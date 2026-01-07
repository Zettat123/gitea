// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	actions_model "code.gitea.io/gitea/models/actions"
	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	org_model "code.gitea.io/gitea/models/organization"
	packages_model "code.gitea.io/gitea/models/packages"
	perm_model "code.gitea.io/gitea/models/perm"
	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	unit_model "code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/test"
	"code.gitea.io/gitea/modules/util"
	actions_service "code.gitea.io/gitea/services/actions"

	"github.com/nektos/act/pkg/jobparser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionsJobTokenAccess(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		t.Run("Write Access", testActionsJobTokenAccess(u, false))
		t.Run("Read Access", testActionsJobTokenAccess(u, true))
	})
}

func testActionsJobTokenAccess(u *url.URL, isFork bool) func(t *testing.T) {
	return func(t *testing.T) {
		task := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 47})

		// Ensure the Actions unit exists for the repository with default permissive mode
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: task.RepoID})
		actionsUnit, err := repo.GetUnit(t.Context(), unit_model.TypeActions)
		if repo_model.IsErrUnitTypeNotExist(err) {
			// Insert Actions unit if it doesn't exist
			err = db.Insert(t.Context(), &repo_model.RepoUnit{
				RepoID: repo.ID,
				Type:   unit_model.TypeActions,
				Config: &repo_model.ActionsConfig{},
			})
			require.NoError(t, err)
		} else {
			require.NoError(t, err)
			// Ensure permissive mode for this test
			actionsCfg := actionsUnit.ActionsConfig()
			actionsCfg.TokenPermissionMode = repo_model.ActionsTokenPermissionModePermissive
			actionsCfg.MaxTokenPermissions = nil
			actionsUnit.Config = actionsCfg
			require.NoError(t, repo_model.UpdateRepoUnit(t.Context(), actionsUnit))
		}

		require.NoError(t, task.GenerateToken())
		task.Status = actions_model.StatusRunning
		task.IsForkPullRequest = isFork
		err = actions_model.UpdateTask(t.Context(), task, "token_hash", "token_salt", "token_last_eight", "status", "is_fork_pull_request")
		require.NoError(t, err)

		session := emptyTestSession(t)
		context := APITestContext{
			Session:  session,
			Token:    task.Token,
			Username: "user5",
			Reponame: "repo4",
		}
		dstPath := t.TempDir()

		u.Path = context.GitPath()
		u.User = url.UserPassword("gitea-actions", task.Token)

		t.Run("Git Clone", doGitClone(dstPath, u))

		t.Run("API Get Repository", doAPIGetRepository(context, func(t *testing.T, r api.Repository) {
			require.Equal(t, "repo4", r.Name)
			require.Equal(t, "user5", r.Owner.UserName)
		}))

		context.ExpectedCode = util.Iif(isFork, http.StatusForbidden, http.StatusCreated)
		t.Run("API Create File", doAPICreateFile(context, "test.txt", &api.CreateFileOptions{
			FileOptions: api.FileOptions{
				NewBranchName: "new-branch",
				Message:       "Create File",
			},
			ContentBase64: base64.StdEncoding.EncodeToString([]byte(`This is a test file created using job token.`)),
		}))

		context.ExpectedCode = http.StatusForbidden
		t.Run("Fail to Create Repository", doAPICreateRepository(context, true))

		context.ExpectedCode = http.StatusForbidden
		t.Run("Fail to Delete Repository", doAPIDeleteRepository(context))

		t.Run("Fail to Create Organization", doAPICreateOrganization(context, &api.CreateOrgOption{
			UserName: "actions",
			FullName: "Gitea Actions",
		}))
	}
}

func TestActionsJobTokenAccessLFS(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		httpContext := NewAPITestContext(t, "user2", "repo-lfs-test", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteRepository)
		t.Run("Create Repository", doAPICreateRepository(httpContext, false, func(t *testing.T, repository api.Repository) {
			task := &actions_model.ActionTask{}
			require.NoError(t, task.GenerateToken())
			task.Status = actions_model.StatusRunning
			task.IsForkPullRequest = false
			task.RepoID = repository.ID
			err := db.Insert(t.Context(), task)
			require.NoError(t, err)

			// Enable Actions unit for the repository
			err = db.Insert(t.Context(), &repo_model.RepoUnit{
				RepoID: repository.ID,
				Type:   unit_model.TypeActions,
				Config: &repo_model.ActionsConfig{},
			})
			require.NoError(t, err)
			session := emptyTestSession(t)
			httpContext := APITestContext{
				Session:  session,
				Token:    task.Token,
				Username: "user2",
				Reponame: "repo-lfs-test",
			}

			u.Path = httpContext.GitPath()
			dstPath := t.TempDir()

			u.Path = httpContext.GitPath()
			u.User = url.UserPassword("gitea-actions", task.Token)

			t.Run("Clone", doGitClone(dstPath, u))

			dstPath2 := t.TempDir()

			t.Run("Partial Clone", doPartialGitClone(dstPath2, u))

			lfs := lfsCommitAndPushTest(t, dstPath, testFileSizeSmall)[0]

			reqLFS := NewRequest(t, "GET", "/api/v1/repos/user2/repo-lfs-test/media/"+lfs).AddTokenAuth(task.Token)
			respLFS := MakeRequestNilResponseRecorder(t, reqLFS, http.StatusOK)
			assert.Equal(t, testFileSizeSmall, respLFS.Length)
		}))
	})
}

func TestActionsTokenPermissionsModes(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		t.Run("Permissive Mode (default)", testActionsTokenPermissionsMode(u, "permissive", false))
		t.Run("Restricted Mode", testActionsTokenPermissionsMode(u, "restricted", true))
	})
}

func testActionsTokenPermissionsMode(u *url.URL, mode string, expectReadOnly bool) func(t *testing.T) {
	return func(t *testing.T) {
		// Update repository settings to the requested mode
		if mode != "" {
			repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{Name: "repo4", OwnerName: "user5"})
			require.NoError(t, repo.LoadUnits(t.Context()))
			actionsUnit, err := repo.GetUnit(t.Context(), unit_model.TypeActions)
			require.NoError(t, err, "Actions unit should exist for repo4")
			actionsCfg := actionsUnit.ActionsConfig()
			actionsCfg.TokenPermissionMode = repo_model.ActionsTokenPermissionMode(mode)
			actionsCfg.DefaultTokenPermissions = nil // Ensure no custom permissions override the mode
			actionsCfg.MaxTokenPermissions = nil     // Ensure no max permissions interfere
			// Update the config
			actionsUnit.Config = actionsCfg
			require.NoError(t, repo_model.UpdateRepoUnit(t.Context(), actionsUnit))
		}

		// Load a task that can be used for testing
		task := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 47})
		// Regenerate token to pick up new permissions if any (though currently permissions are checked at runtime)
		require.NoError(t, task.GenerateToken())
		task.Status = actions_model.StatusRunning
		task.IsForkPullRequest = false // Not a fork PR
		err := actions_model.UpdateTask(t.Context(), task, "token_hash", "token_salt", "token_last_eight", "status", "is_fork_pull_request")
		require.NoError(t, err)

		session := emptyTestSession(t)
		context := APITestContext{
			Session:  session,
			Token:    task.Token,
			Username: "user5",
			Reponame: "repo4",
		}
		dstPath := t.TempDir()

		u.Path = context.GitPath()
		u.User = url.UserPassword("gitea-actions", task.Token)

		// Git clone should always work (read access)
		t.Run("Git Clone", doGitClone(dstPath, u))

		// API Get should always work (read access)
		t.Run("API Get Repository", doAPIGetRepository(context, func(t *testing.T, r api.Repository) {
			require.Equal(t, "repo4", r.Name)
			require.Equal(t, "user5", r.Owner.UserName)
		}))

		var sha string

		// Test Write Access
		if expectReadOnly {
			context.ExpectedCode = http.StatusForbidden
		} else {
			context.ExpectedCode = 0
		}
		t.Run("API Create File", doAPICreateFile(context, "test-permissions.txt", &api.CreateFileOptions{
			FileOptions: api.FileOptions{
				BranchName: "master",
				Message:    "Create File",
			},
			ContentBase64: base64.StdEncoding.EncodeToString([]byte(`This is a test file for permissions.`)),
		}, func(t *testing.T, resp api.FileResponse) {
			sha = resp.Content.SHA
			require.NotEmpty(t, sha, "SHA should not be empty")
		}))

		// Test Delete Access
		if expectReadOnly {
			context.ExpectedCode = http.StatusForbidden
		} else {
			context.ExpectedCode = 0
		}
		if !expectReadOnly {
			// Clean up created file if we had write access
			t.Run("API Delete File", func(t *testing.T) {
				t.Logf("Deleting file with SHA: %s", sha)
				require.NotEmpty(t, sha, "SHA must be captured before deletion")
				deleteOpts := &api.DeleteFileOptions{
					FileOptions: api.FileOptions{
						BranchName: "master",
						Message:    "Delete File",
					},
					SHA: sha,
				}
				req := NewRequestWithJSON(t, "DELETE", fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", context.Username, context.Reponame, "test-permissions.txt"), deleteOpts).
					AddTokenAuth(context.Token)
				if context.ExpectedCode != 0 {
					context.Session.MakeRequest(t, req, context.ExpectedCode)
					return
				}
				context.Session.MakeRequest(t, req, http.StatusOK)
			})
		}
	}
}

func TestActionsTokenPermissionsClamping(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		httpContext := NewAPITestContext(t, "user2", "repo-clamping", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteRepository)
		t.Run("Create Repository", doAPICreateRepository(httpContext, false, func(t *testing.T, repository api.Repository) {
			// Enable Actions unit with Clamping Config
			err := db.Insert(t.Context(), &repo_model.RepoUnit{
				RepoID: repository.ID,
				Type:   unit_model.TypeActions,
				Config: &repo_model.ActionsConfig{
					TokenPermissionMode: repo_model.ActionsTokenPermissionModePermissive,
					MaxTokenPermissions: &repo_model.ActionsTokenPermissions{
						Code: perm_model.AccessModeRead, // Max is Read - will clamp default Write to Read
					},
				},
			})
			require.NoError(t, err)

			// Create Task and Token
			task := &actions_model.ActionTask{
				RepoID:            repository.ID,
				Status:            actions_model.StatusRunning,
				IsForkPullRequest: false,
			}
			require.NoError(t, task.GenerateToken())
			require.NoError(t, db.Insert(t.Context(), task))

			// Verify Token Permissions
			session := emptyTestSession(t)
			testCtx := APITestContext{
				Session:  session,
				Token:    task.Token,
				Username: "user2",
				Reponame: "repo-clamping",
			}

			// 1. Try to Write (Create File) - Should Fail (403) because Max is Read
			testCtx.ExpectedCode = http.StatusForbidden
			t.Run("Fail to Create File (Max Clamping)", doAPICreateFile(testCtx, "clamping.txt", &api.CreateFileOptions{
				ContentBase64: base64.StdEncoding.EncodeToString([]byte("test")),
			}))

			// 2. Try to Read (Get Repository) - Should Succeed (200)
			testCtx.ExpectedCode = http.StatusOK
			t.Run("Get Repository (Read Allowed)", doAPIGetRepository(testCtx, func(t *testing.T, r api.Repository) {
				assert.Equal(t, "repo-clamping", r.Name)
			}))
		}))
	})
}

func TestActionsCrossRepoAccess(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		session := loginUser(t, "user2")
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteOrganization)

		// 1. Create Organization
		orgName := "org-cross-test"
		req := NewRequestWithJSON(t, "POST", "/api/v1/orgs", &api.CreateOrgOption{
			UserName: orgName,
		}).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusCreated)

		// 2. Create Two Repositories in Org
		createRepoInOrg := func(name string) int64 {
			req := NewRequestWithJSON(t, "POST", fmt.Sprintf("/api/v1/orgs/%s/repos", orgName), &api.CreateRepoOption{
				Name:     name,
				AutoInit: true,
				Private:  true, // Must be private for potential restrictions
			}).AddTokenAuth(token)
			resp := MakeRequest(t, req, http.StatusCreated)
			var repo api.Repository
			DecodeJSON(t, resp, &repo)
			return repo.ID
		}

		repoAID := createRepoInOrg("repo-A")
		repoBID := createRepoInOrg("repo-B")

		// 3. Enable Actions in Repo A (Source) and Repo B (Target)
		enableActions := func(repoID int64) {
			err := db.Insert(t.Context(), &repo_model.RepoUnit{
				RepoID: repoID,
				Type:   unit_model.TypeActions,
				Config: &repo_model.ActionsConfig{
					TokenPermissionMode: repo_model.ActionsTokenPermissionModePermissive,
				},
			})
			require.NoError(t, err)
		}

		enableActions(repoAID)
		enableActions(repoBID)

		// 4. Create Task in Repo A
		task := &actions_model.ActionTask{
			RepoID:            repoAID,
			Status:            actions_model.StatusRunning,
			IsForkPullRequest: false,
		}
		require.NoError(t, task.GenerateToken())
		require.NoError(t, db.Insert(t.Context(), task))

		// 5. Verify Access to Repo B (Target)
		testCtx := APITestContext{
			Session:  emptyTestSession(t),
			Token:    task.Token,
			Username: orgName,
			Reponame: "repo-B",
		}

		// Case A: Default (AllowCrossRepoAccess = false/unset) -> Should Fail (404 Not Found)
		// API returns 404 for private repos you can't access, not 403, to avoid leaking existence.
		testCtx.ExpectedCode = http.StatusNotFound
		t.Run("Cross-Repo Access Denied (Default)", doAPIGetRepository(testCtx, nil))

		// Case B: Enable AllowCrossRepoAccess
		org, err := org_model.GetOrgByName(t.Context(), orgName)
		require.NoError(t, err)

		cfg := &repo_model.ActionsConfig{
			AllowCrossRepoAccess: true,
		}
		err = actions_model.SetOrgActionsConfig(t.Context(), org.ID, cfg)
		require.NoError(t, err)

		// Retry -> Should Succeed (200) - Read Only
		testCtx.ExpectedCode = http.StatusOK
		t.Run("Cross-Repo Access Allowed", doAPIGetRepository(testCtx, func(t *testing.T, r api.Repository) {
			assert.Equal(t, "repo-B", r.Name)
		}))

		// 6. Test Cross-Repo Package Access
		t.Run("Cross-Repo Package Access", func(t *testing.T) {
			packageName := "cross-test-pkg"
			packageVersion := "1.0.0"
			fileName := "test-file.bin"
			content := []byte{1, 2, 3, 4, 5}

			// First, upload a package to the org using basic auth (user2 is org owner)
			packageURL := fmt.Sprintf("/api/packages/%s/generic/%s/%s/%s", orgName, packageName, packageVersion, fileName)
			uploadReq := NewRequestWithBody(t, "PUT", packageURL, bytes.NewReader(content)).AddBasicAuth("user2")
			MakeRequest(t, uploadReq, http.StatusCreated)

			// Link the package to repo-B (per reviewer feedback: packages must be linked to repos)
			pkg, err := packages_model.GetPackageByName(t.Context(), org.ID, packages_model.TypeGeneric, packageName)
			require.NoError(t, err)
			require.NoError(t, packages_model.SetRepositoryLink(t.Context(), pkg.ID, repoBID))

			// By default, cross-repo is disabled
			// Explicitly set it to false to ensure test determinism (in case defaults change)
			require.NoError(t, actions_model.SetOrgActionsConfig(t.Context(), org.ID, &repo_model.ActionsConfig{
				AllowCrossRepoAccess: false,
			}))

			// Try to download with cross-repo disabled - should fail
			downloadReqDenied := NewRequest(t, "GET", packageURL)
			downloadReqDenied.Header.Set("Authorization", "Bearer "+task.Token)
			MakeRequest(t, downloadReqDenied, http.StatusForbidden)

			// Enable cross-repo access
			require.NoError(t, actions_model.SetOrgActionsConfig(t.Context(), org.ID, &repo_model.ActionsConfig{
				AllowCrossRepoAccess: true,
			}))

			// Try to download with cross-repo enabled - should succeed
			downloadReq := NewRequest(t, "GET", packageURL)
			downloadReq.Header.Set("Authorization", "Bearer "+task.Token)
			resp := MakeRequest(t, downloadReq, http.StatusOK)
			assert.Equal(t, content, resp.Body.Bytes(), "Should be able to read package from other repo in same org")

			// Try to upload a package with task token (cross-repo write)
			// Cross-repo access should be read-only, write attempts return 401 Unauthorized
			writePackageURL := fmt.Sprintf("/api/packages/%s/generic/%s/%s/write-test.bin", orgName, packageName, packageVersion)
			writeReq := NewRequestWithBody(t, "PUT", writePackageURL, bytes.NewReader(content))
			writeReq.Header.Set("Authorization", "Bearer "+task.Token)
			MakeRequest(t, writeReq, http.StatusUnauthorized)
		})
	})
}

func TestActionsTokenPermissionsWorkflowScenario(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		// Step 1: Create a new repository with Actions enabled
		httpContext := NewAPITestContext(t, "user2", "repo-workflow-token-test", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteRepository)
		t.Run("Create Repository and Test Token Permissions", doAPICreateRepository(httpContext, false, func(t *testing.T, repository api.Repository) {
			// Step 2: Enable Actions unit with Permissive mode (the mode the reviewer set)
			err := db.Insert(t.Context(), &repo_model.RepoUnit{
				RepoID: repository.ID,
				Type:   unit_model.TypeActions,
				Config: &repo_model.ActionsConfig{
					TokenPermissionMode: repo_model.ActionsTokenPermissionModePermissive,
					// No MaxTokenPermissions - allows full write access
				},
			})
			require.NoError(t, err)

			// Step 3: Create an Actions task (simulates a running workflow)
			task := &actions_model.ActionTask{
				RepoID:            repository.ID,
				Status:            actions_model.StatusRunning,
				IsForkPullRequest: false,
			}
			require.NoError(t, task.GenerateToken())
			require.NoError(t, db.Insert(t.Context(), task))

			// Step 4: Use the GITEA_TOKEN to create a file via API (exactly as the reviewer's workflow did)
			session := emptyTestSession(t)
			testCtx := APITestContext{
				Session:  session,
				Token:    task.Token,
				Username: "user2",
				Reponame: "repo-workflow-token-test",
			}

			// The create file should succeed with permissive mode
			testCtx.ExpectedCode = http.StatusCreated
			t.Run("GITEA_TOKEN Create File (Permissive Mode)", doAPICreateFile(testCtx, fmt.Sprintf("test-file-%d.txt", time.Now().Unix()), &api.CreateFileOptions{
				FileOptions: api.FileOptions{
					BranchName: "master",
					Message:    "test actions token",
				},
				ContentBase64: base64.StdEncoding.EncodeToString([]byte("Test Content")),
			}))

			// Verify that the API also works for reading (should always work)
			testCtx.ExpectedCode = http.StatusOK
			t.Run("GITEA_TOKEN Get Repository", doAPIGetRepository(testCtx, func(t *testing.T, r api.Repository) {
				assert.Equal(t, "repo-workflow-token-test", r.Name)
			}))

			// Now test with Restricted mode - file creation should fail
			repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: repository.ID})
			actionsUnit, err := repo.GetUnit(t.Context(), unit_model.TypeActions)
			require.NoError(t, err)
			actionsCfg := actionsUnit.ActionsConfig()
			actionsCfg.TokenPermissionMode = repo_model.ActionsTokenPermissionModeRestricted
			actionsUnit.Config = actionsCfg
			require.NoError(t, repo_model.UpdateRepoUnit(t.Context(), actionsUnit))

			// Regenerate token to get fresh permissions
			require.NoError(t, task.GenerateToken())
			task.Status = actions_model.StatusRunning
			require.NoError(t, actions_model.UpdateTask(t.Context(), task, "token_hash", "token_salt", "token_last_eight", "status"))

			testCtx.Token = task.Token
			testCtx.ExpectedCode = http.StatusForbidden
			t.Run("GITEA_TOKEN Create File (Restricted Mode - Should Fail)", doAPICreateFile(testCtx, "should-fail.txt", &api.CreateFileOptions{
				FileOptions: api.FileOptions{
					BranchName: "master",
					Message:    "this should fail",
				},
				ContentBase64: base64.StdEncoding.EncodeToString([]byte("Should Not Be Created")),
			}))
		}))
	})
}

// TestActionsWorkflowPermissionsKeyword tests that the `permissions:` keyword in a workflow YAML
// restricts the token even when the repository is in permissive mode.
func TestActionsWorkflowPermissionsKeyword(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		httpContext := NewAPITestContext(t, "user2", "repo-workflow-perms-kw", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteRepository)
		t.Run("Workflow Permissions Keyword", doAPICreateRepository(httpContext, false, func(t *testing.T, repository api.Repository) {
			// Enable Actions unit with PERMISSIVE mode (default write access)
			err := db.Insert(t.Context(), &repo_model.RepoUnit{
				RepoID: repository.ID,
				Type:   unit_model.TypeActions,
				Config: &repo_model.ActionsConfig{
					TokenPermissionMode: repo_model.ActionsTokenPermissionModePermissive,
				},
			})
			require.NoError(t, err)

			// Define Workflow YAML with two jobs:
			// 1. job-read-only: Inherits `permissions: read-all` (write should fail)
			// 2. job-override: Overrides with `permissions: contents: write` (write should succeed)
			workflowYAML := `
name: Test Permissions
on: workflow_dispatch
permissions: read-all

jobs:
  job-read-only:
    runs-on: ubuntu-latest
    steps:
      - run: echo "Full read-only"

  job-override:
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - run: echo "Override to write"
`
			// Parse the workflow using the actual parsing logic (this verifies the parser works as expected)
			singleWorkflows, err := jobparser.Parse([]byte(workflowYAML))
			require.NoError(t, err)
			// jobparser.Parse returns one SingleWorkflow per job

			// Get default permissions for the repo (Permissive)
			repo, err := repo_model.GetRepositoryByID(t.Context(), repository.ID)
			require.NoError(t, err)
			actionsUnit, err := repo.GetUnit(t.Context(), unit_model.TypeActions)
			require.NoError(t, err)
			cfg := actionsUnit.ActionsConfig()
			defaultPerms := cfg.GetEffectiveTokenPermissions(false)

			// Create Run (shared)
			run := &actions_model.ActionRun{
				RepoID:        repository.ID,
				OwnerID:       repository.Owner.ID,
				Title:         "Test workflow permissions",
				Status:        actions_model.StatusRunning,
				Ref:           "refs/heads/master",
				CommitSHA:     "abc123456",
				TriggerUserID: repository.Owner.ID,
			}
			require.NoError(t, db.Insert(t.Context(), run))

			// Iterate over jobs and create them matching the parser logic
			for _, flow := range singleWorkflows {
				jobID, jobDef := flow.Job()
				jobName := jobDef.Name

				// Parse workflow-level permissions from the flow
				workflowPerms := actions_service.ParseWorkflowPermissions(flow, defaultPerms)

				// Parse job-level permissions(jobDef, workflowPerms)
				jobPerms := actions_service.ParseJobPermissions(jobDef, workflowPerms)
				finalPerms := cfg.ClampPermissions(jobPerms)
				permsJSON := repo_model.MarshalTokenPermissions(finalPerms)

				job := &actions_model.ActionRunJob{
					RunID:            run.ID,
					RepoID:           repository.ID,
					OwnerID:          repository.Owner.ID,
					CommitSHA:        "abc123456",
					Name:             jobName,
					JobID:            jobID,
					Status:           actions_model.StatusRunning,
					TokenPermissions: permsJSON,
				}
				require.NoError(t, db.Insert(t.Context(), job))

				task := &actions_model.ActionTask{
					JobID:             job.ID,
					RepoID:            repository.ID,
					Status:            actions_model.StatusRunning,
					IsForkPullRequest: false,
				}
				require.NoError(t, task.GenerateToken())
				require.NoError(t, db.Insert(t.Context(), task))

				// Link task to job
				job.TaskID = task.ID
				_, err = db.GetEngine(t.Context()).ID(job.ID).Cols("task_id").Update(job)
				require.NoError(t, err)

				// Test API Access
				session := emptyTestSession(t)
				testCtx := APITestContext{
					Session:  session,
					Token:    task.Token,
					Username: "user2",
					Reponame: "repo-workflow-perms-kw",
				}

				if jobID == "job-read-only" {
					// Should match 'read-all' -> Write Forbidden
					testCtx.ExpectedCode = http.StatusForbidden
					t.Run("Job [read-only] Create File (Should Fail)", doAPICreateFile(testCtx, "fail-readonly.txt", &api.CreateFileOptions{
						ContentBase64: base64.StdEncoding.EncodeToString([]byte("fail")),
					}))

					testCtx.ExpectedCode = http.StatusOK
					t.Run("Job [read-only] Get Repo (Should Succeed)", doAPIGetRepository(testCtx, func(t *testing.T, r api.Repository) {
						assert.Equal(t, repository.Name, r.Name)
					}))
				} else if jobID == "job-override" {
					// Should have 'contents: write' -> Write Created
					testCtx.ExpectedCode = http.StatusCreated
					t.Run("Job [override] Create File (Should Succeed)", doAPICreateFile(testCtx, "succeed-override.txt", &api.CreateFileOptions{
						FileOptions: api.FileOptions{
							BranchName: "master",
							Message:    "override success",
						},
						ContentBase64: base64.StdEncoding.EncodeToString([]byte("success")),
					}))
				}
			}
		}))
	})
}

func TestActionsRerunPermissions(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		session := loginUser(t, "user2")
		httpContext := NewAPITestContext(t, "user2", "repo-rerun-perms", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteRepository)

		t.Run("Rerun Permissions", doAPICreateRepository(httpContext, false, func(t *testing.T, repository api.Repository) {
			// 1. Enable Actions with PERMISSIVE mode
			err := db.Insert(t.Context(), &repo_model.RepoUnit{
				RepoID: repository.ID,
				Type:   unit_model.TypeActions,
				Config: &repo_model.ActionsConfig{
					TokenPermissionMode: repo_model.ActionsTokenPermissionModePermissive,
				},
			})
			require.NoError(t, err)

			// 2. Create Run and Job with implicit permissions (no parsed perms stored yet)
			// or with parsed perms that allow write (Permissive default)
			workflowPayload := `
name: Test Rerun
on: workflow_dispatch
jobs:
  test-rerun:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`
			run := &actions_model.ActionRun{
				RepoID:        repository.ID,
				OwnerID:       repository.Owner.ID,
				Title:         "Test Rerun",
				Status:        actions_model.StatusSuccess, // Run finished
				Ref:           "refs/heads/master",
				CommitSHA:     "abc123",
				WorkflowID:    "test-rerun.yaml",
				TriggerUserID: repository.Owner.ID,
			}
			require.NoError(t, db.Insert(t.Context(), run))

			// Initial permissions: Permissive (Write)
			initialPerms := repo_model.ActionsTokenPermissions{
				Code: perm_model.AccessModeWrite,
			}

			job := &actions_model.ActionRunJob{
				RunID:            run.ID,
				RepoID:           repository.ID,
				OwnerID:          repository.Owner.ID,
				CommitSHA:        "abc123",
				Name:             "test-rerun",
				JobID:            "test-rerun",
				Status:           actions_model.StatusSuccess, // Job finished
				WorkflowPayload:  []byte(workflowPayload),
				TokenPermissions: repo_model.MarshalTokenPermissions(initialPerms),
			}
			require.NoError(t, db.Insert(t.Context(), job))

			// 3. Change Repo Settings to RESTRICTED
			unitConfig := &repo_model.ActionsConfig{
				TokenPermissionMode: repo_model.ActionsTokenPermissionModeRestricted,
			}
			// Update the specific unit
			// Need to find the unit first
			repo, err := repo_model.GetRepositoryByID(t.Context(), repository.ID)
			require.NoError(t, err)
			unit, err := repo.GetUnit(t.Context(), unit_model.TypeActions)
			require.NoError(t, err)

			unit.Config = unitConfig
			require.NoError(t, repo_model.UpdateRepoUnit(t.Context(), unit))

			// 4. Trigger Rerun via Web Handler
			run.Index = 1
			_, err = db.GetEngine(t.Context()).ID(run.ID).Cols("index").Update(run)
			require.NoError(t, err)

			req := NewRequest(t, "POST", fmt.Sprintf("/%s/%s/actions/runs/%d/rerun", "user2", "repo-rerun-perms", run.Index))
			session.MakeRequest(t, req, http.StatusOK)

			// 5. Verify TokenPermissions in DB are now Restricted (Read-only)
			// Reload job
			jobReload := new(actions_model.ActionRunJob)
			has, err := db.GetEngine(t.Context()).ID(job.ID).Get(jobReload)
			require.NoError(t, err)
			assert.True(t, has)

			// Check permissions
			perms, err := repo_model.UnmarshalTokenPermissions(jobReload.TokenPermissions)
			require.NoError(t, err)

			// Should be restricted (Read)
			assert.Equal(t, perm_model.AccessModeRead, perms.Code, "Permissions should be restricted to Read after rerun in restricted mode")
		}))
	})
}

func TestActionsPermission(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		session := loginUser(t, user2.Name)
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteUser)

		// create a new repo
		apiRepo := createActionsTestRepo(t, token, "actions-permission", false)
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: apiRepo.ID})
		httpContext := NewAPITestContext(t, user2.Name, repo.Name, auth_model.AccessTokenScopeWriteRepository)
		defer doAPIDeleteRepository(httpContext)(t)

		// create a mock runner
		runner := newMockRunner()
		runner.registerAsRepoRunner(t, repo.OwnerName, repo.Name, "mock-runner", []string{"ubuntu-latest"}, false)

		// set actions token permission mode to "permissive"
		req := NewRequestWithValues(t, "POST", fmt.Sprintf("/%s/%s/settings/actions/general/token_permissions", repo.OwnerName, repo.Name), map[string]string{
			"token_permission_mode": "permissive",
		})
		resp := session.MakeRequest(t, req, http.StatusSeeOther)
		require.Equal(t, fmt.Sprintf("/%s/%s/settings/actions/general", repo.OwnerName, repo.Name), test.RedirectURL(resp))

		// create a workflow file with "permission" keyword
		wfTreePath := ".gitea/workflows/test_permissions.yml"
		wfFileContent := `name: Test Permissions
on:
  push:
    paths:
      - '.gitea/workflows/test_permissions.yml'

permissions: read-all

jobs:
  job-override:
    runs-on: ubuntu-latest
    permissions: write-all
    steps:
      - run: echo "Override to write"
`
		opts := getWorkflowCreateFileOptions(user2, repo.DefaultBranch, "create "+wfTreePath, wfFileContent)
		createWorkflowFile(t, token, user2.Name, repo.Name, wfTreePath, opts)

		// fetch a task(*runnerv1.Task) and get its token
		runnerTask := runner.fetchTask(t)
		taskToken := runnerTask.Secrets["GITEA_TOKEN"]
		require.NotEmpty(t, taskToken)
		// get the task(*actions_model.ActionTask) by token
		task, err := actions_model.GetRunningTaskByToken(t.Context(), taskToken)
		require.NoError(t, err)
		require.Equal(t, repo.ID, task.RepoID)
		require.False(t, task.IsForkPullRequest)
		require.Equal(t, actions_model.StatusRunning, task.Status)
		actionsPerm, err := access_model.GetActionsUserRepoPermission(t.Context(), repo, user_model.NewActionsUser(), task.ID)
		require.NoError(t, err)
		require.NoError(t, task.LoadJob(t.Context()))
		t.Logf("TokenPermissions: %s", task.Job.TokenPermissions)
		t.Logf("Computed Units Mode: %+v", actionsPerm)
		require.True(t, actionsPerm.CanWrite(unit_model.TypeCode), "Should have write access to Code. Got: %v", actionsPerm.AccessMode) // the token should have the "write" permission on "Code" unit
		// test creating a file with the token
		actionsTokenContext := APITestContext{
			Session:      emptyTestSession(t),
			Token:        taskToken,
			Username:     repo.OwnerName,
			Reponame:     repo.Name,
			ExpectedCode: 0,
		}
		t.Run("API Create File", doAPICreateFile(actionsTokenContext, "test-permissions.txt", &api.CreateFileOptions{
			FileOptions: api.FileOptions{
				BranchName: repo.DefaultBranch,
				Message:    "Create File",
			},
			ContentBase64: base64.StdEncoding.EncodeToString([]byte(`This is a test file for permissions.`)),
		}, func(t *testing.T, resp api.FileResponse) {
			require.NotEmpty(t, resp.Content.SHA)
		}))
	})
}

func TestRepoActionsTokenPermission(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		session := loginUser(t, user2.Name)
		user2Token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteUser)

		// create a new repo
		apiRepo := createActionsTestRepo(t, user2Token, "actions-token-permission", false)
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: apiRepo.ID})
		httpContext := NewAPITestContext(t, user2.Name, repo.Name, auth_model.AccessTokenScopeWriteRepository)
		defer doAPIDeleteRepository(httpContext)(t)

		// create a mock runner
		runner := newMockRunner()
		runner.registerAsRepoRunner(t, repo.OwnerName, repo.Name, "mock-runner", []string{"ubuntu-latest"}, false)

		repoTestCases := []struct {
			name                    string
			tokenPermissionsPayload map[string]string
		}{
			{
				name: "Restricted Mode",
				tokenPermissionsPayload: map[string]string{
					"token_permission_mode": "restricted",
				},
			},
			{
				name: "Permissive Mode",
				tokenPermissionsPayload: map[string]string{
					"token_permission_mode": "permissive",
				},
			},
			{
				name: "Write All",
				tokenPermissionsPayload: map[string]string{
					"token_permission_mode": "custom",
					"max_contents":          "write",
					"max_issues":            "write",
					"max_pull_requests":     "write",
					"max_wiki":              "write",
					"max_releases":          "write",
					"max_projects":          "write",
					"max_packages":          "write",
					"max_actions":           "write",
				},
			},
			{
				name: "Custom Mode",
				tokenPermissionsPayload: map[string]string{
					"token_permission_mode": "custom",
					"max_contents":          "write",
					"max_issues":            "write",
					"max_pull_requests":     "none",
					"max_wiki":              "read",
					"max_releases":          "read",
					"max_projects":          "read",
					"max_packages":          "write",
					"max_actions":           "read",
				},
			},
		}

		workflowTestCases := []struct {
			name       string
			wfTreepath string
			wfContent  string
			perms      map[unit_model.Type]perm_model.AccessMode
		}{
			{
				name:       "read_code",
				wfTreepath: ".gitea/workflows/read_code.yml",
				wfContent: `name: Test Permissions
on:
  push:
    paths:
      - '.gitea/workflows/read_code.yml'

jobs:
  test:
    runs-on: ubuntu-latest
    permissions:
      code: read
    steps:
      - run: echo "test"
`,
				perms: map[unit_model.Type]perm_model.AccessMode{
					unit_model.TypeCode: perm_model.AccessModeRead,
				},
			},
		}

		for _, rtc := range repoTestCases {
			t.Run(rtc.name, func(t *testing.T) {
				// set actions token permission mode
				req := NewRequestWithValues(t, "POST", fmt.Sprintf("/%s/%s/settings/actions/general/token_permissions", repo.OwnerName, repo.Name), rtc.tokenPermissionsPayload)
				resp := session.MakeRequest(t, req, http.StatusSeeOther)
				require.Equal(t, fmt.Sprintf("/%s/%s/settings/actions/general", repo.OwnerName, repo.Name), test.RedirectURL(resp))

				for _, wtc := range workflowTestCases {
					t.Run(wtc.name, func(t *testing.T) {
						opts := getWorkflowCreateFileOptions(user2, repo.DefaultBranch, "create "+wtc.wfTreepath, wtc.wfContent)
						createWorkflowFile(t, user2Token, user2.Name, repo.Name, wtc.wfTreepath, opts)

						task := runner.fetchTask(t)
						taskToken := task.Secrets["GITEA_TOKEN"]

						expectedTokenPerms := getExpectedActionsTokenPerms(rtc.tokenPermissionsPayload, wtc.perms)

						testActionsTokenPermission(t, taskToken, repo, expectedTokenPerms)

						runner.execTask(t, task, &mockTaskOutcome{
							result: runnerv1.Result_RESULT_SUCCESS,
						})

						// delete the workflow file
						getReq := NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", repo.OwnerName, repo.Name, wtc.wfTreepath)).
							AddTokenAuth(user2Token)
						getResp := session.MakeRequest(t, getReq, http.StatusOK)
						var contentsResp api.ContentsResponse
						DecodeJSON(t, getResp, &contentsResp)
						req := NewRequestWithJSON(t, "DELETE", fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", repo.OwnerName, repo.Name, wtc.wfTreepath), &api.DeleteFileOptions{
							SHA: contentsResp.SHA,
						}).AddTokenAuth(user2Token)
						session.MakeRequest(t, req, http.StatusOK)
					})
				}
			})
		}
	})
}

func testActionsTokenPermission(t *testing.T, token string, repo *repo_model.Repository, expectedPerms map[unit_model.Type]perm_model.AccessMode) {
	perms := map[unit_model.Type]perm_model.AccessMode{
		unit_model.TypeCode:         perm_model.AccessModeNone,
		unit_model.TypeIssues:       perm_model.AccessModeNone,
		unit_model.TypePullRequests: perm_model.AccessModeNone,
		unit_model.TypeWiki:         perm_model.AccessModeNone,
		unit_model.TypeReleases:     perm_model.AccessModeNone,
		unit_model.TypeProjects:     perm_model.AccessModeNone,
		unit_model.TypePackages:     perm_model.AccessModeNone,
		unit_model.TypeActions:      perm_model.AccessModeNone,
	}
	for unit, accessMode := range expectedPerms {
		// accessMode can only be None/Read/Write
		require.Contains(t, []perm_model.AccessMode{perm_model.AccessModeNone, perm_model.AccessModeRead, perm_model.AccessModeWrite}, accessMode)
		perms[unit] = accessMode
	}

	for unit, accessMode := range perms {
		switch unit {
		case unit_model.TypeCode:
			t.Run("Code Read", func(t *testing.T) {
				expectedCode := util.Iif(accessMode == perm_model.AccessModeNone, http.StatusNotFound, http.StatusOK)
				readReq := NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/%s/%s", repo.OwnerName, repo.Name)).
					AddTokenAuth(token)
				MakeRequest(t, readReq, expectedCode)
			})
			t.Run("Code Write", func(t *testing.T) {
				expectedCode := http.StatusCreated
				switch accessMode {
				case perm_model.AccessModeNone:
					expectedCode = http.StatusNotFound
				case perm_model.AccessModeRead:
					expectedCode = http.StatusForbidden
				}
				writeReq := NewRequestWithJSON(t, "POST", fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", repo.OwnerName, repo.Name, "test-permissions.txt"), &api.CreateFileOptions{
					FileOptions: api.FileOptions{
						BranchName: repo.DefaultBranch,
						Message:    "Create File",
					},
					ContentBase64: base64.StdEncoding.EncodeToString([]byte(`This is a test file for permissions.`)),
				}).
					AddTokenAuth(token)
				writeResp := MakeRequest(t, writeReq, expectedCode)
				if writeResp.Code == http.StatusCreated {
					var fileResp api.FileResponse
					DecodeJSON(t, writeResp, &fileResp)
					require.NotNil(t, fileResp.Content)
					deleteReq := NewRequestWithJSON(t, "DELETE", fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", repo.OwnerName, repo.Name, "test-permissions.txt"), &api.DeleteFileOptions{
						SHA: fileResp.Content.SHA,
					}).AddTokenAuth(token)
					MakeRequest(t, deleteReq, http.StatusOK)
				}
			})

		case unit_model.TypeIssues:
			t.Run("Issues Read", func(t *testing.T) {
				expectedCode := util.Iif(accessMode == perm_model.AccessModeNone, http.StatusNotFound, http.StatusOK)
				readReq := NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/%s/%s/issues", repo.OwnerName, repo.Name)).
					AddTokenAuth(token)
				MakeRequest(t, readReq, expectedCode)
			})
			t.Run("Issues Write", func(t *testing.T) {
				expectedCode := http.StatusCreated
				switch accessMode {
				case perm_model.AccessModeNone:
					expectedCode = http.StatusNotFound
				case perm_model.AccessModeRead:
					expectedCode = http.StatusForbidden
				}
				writeReq := NewRequestWithJSON(t, "POST", fmt.Sprintf("/api/v1/repos/%s/%s/labels", repo.OwnerName, repo.Name), &api.CreateLabelOption{
					Name:        fmt.Sprintf("test-label-%d", time.Now().UnixNano()),
					Color:       "abcdef",
					Description: "test label for permissions",
				}).AddTokenAuth(token)
				writeResp := MakeRequest(t, writeReq, expectedCode)
				if writeResp.Code == http.StatusCreated {
					var label api.Label
					DecodeJSON(t, writeResp, &label)
					deleteReq := NewRequest(t, "DELETE", fmt.Sprintf("/api/v1/repos/%s/%s/labels/%d", repo.OwnerName, repo.Name, label.ID)).
						AddTokenAuth(token)
					MakeRequest(t, deleteReq, http.StatusNoContent)
				}
			})

		case unit_model.TypeReleases:
			t.Run("Releases Read", func(t *testing.T) {
				expectedCode := util.Iif(accessMode == perm_model.AccessModeNone, http.StatusForbidden, http.StatusOK)
				readReq := NewRequest(t, "GET", fmt.Sprintf("/api/v1/repos/%s/%s/releases", repo.OwnerName, repo.Name)).
					AddTokenAuth(token)
				MakeRequest(t, readReq, expectedCode)
			})
			t.Run("Releases Write", func(t *testing.T) {
				expectedCode := http.StatusCreated
				switch accessMode {
				case perm_model.AccessModeNone, perm_model.AccessModeRead:
					expectedCode = http.StatusForbidden
				}
				tagName := fmt.Sprintf("test-release-%d", time.Now().UnixNano())
				writeReq := NewRequestWithJSON(t, "POST", fmt.Sprintf("/api/v1/repos/%s/%s/releases", repo.OwnerName, repo.Name), &api.CreateReleaseOption{
					TagName: tagName,
					Title:   tagName,
					Target:  repo.DefaultBranch,
					Note:    "test release",
				}).AddTokenAuth(token)
				MakeRequest(t, writeReq, expectedCode)
			})

		case unit_model.TypePackages:
			t.Run("Packages Read", func(t *testing.T) {
				// Prepare a generic package owned by the repo owner using the owner's token (not the task token)
				packageName := fmt.Sprintf("test-pkg-read-%d", time.Now().UnixNano())
				packageVersion := "1.0.0"
				fileName := "read-test.bin"
				content := []byte("read package content")
				uploadURL := fmt.Sprintf("/api/packages/%s/generic/%s/%s/%s", repo.OwnerName, packageName, packageVersion, fileName)
				uploadReq := NewRequestWithBody(t, "PUT", uploadURL, bytes.NewReader(content)).
					AddBasicAuth(repo.OwnerName)
				MakeRequest(t, uploadReq, http.StatusCreated)

				// Link the package to the repo
				pkg := unittest.AssertExistsAndLoadBean(t, &packages_model.Package{
					OwnerID: repo.OwnerID,
					Type:    packages_model.TypeGeneric,
					Name:    packageName,
				})
				linkReq := NewRequest(t, "POST", fmt.Sprintf("/api/v1/packages/%s/generic/%s/-/link/%s", repo.OwnerName, pkg.Name, repo.Name)).
					AddBasicAuth(repo.OwnerName)
				MakeRequest(t, linkReq, http.StatusCreated)

				expectedCode := http.StatusOK
				if accessMode == perm_model.AccessModeNone {
					expectedCode = http.StatusUnauthorized
				}
				// Read uses bearer token auth against the packages API (not /api/v1).
				readReq := NewRequest(t, "GET", uploadURL)
				readReq.Header.Set("Authorization", "Bearer "+token)
				MakeRequest(t, readReq, expectedCode)

				// Cleanup via owner basic auth to avoid depending on actions token write rights.
				deleteReq := NewRequest(t, "DELETE", uploadURL).AddBasicAuth(repo.OwnerName)
				MakeRequest(t, deleteReq, http.StatusNoContent)
			})
			t.Run("Packages Write", func(t *testing.T) {
				// Write path uses bearer auth and expects success only with write access.
				packageName := fmt.Sprintf("test-pkg-write-%d", time.Now().UnixNano())
				packageVersion := "1.0.0"
				fileName := "write-test.bin"
				content := []byte("write package content")
				uploadURL := fmt.Sprintf("/api/packages/%s/generic/%s/%s/%s", repo.OwnerName, packageName, packageVersion, fileName)

				expectedCode := http.StatusUnauthorized
				if accessMode == perm_model.AccessModeWrite {
					expectedCode = http.StatusCreated
				}
				writeReq := NewRequestWithBody(t, "PUT", uploadURL, bytes.NewReader(content))
				writeReq.Header.Set("Authorization", "Bearer "+token)
				MakeRequest(t, writeReq, expectedCode)

				if expectedCode == http.StatusCreated {
					// Cleanup via owner basic auth to avoid leaking packages across test cases.
					deleteReq := NewRequest(t, "DELETE", uploadURL).AddBasicAuth(repo.OwnerName)
					MakeRequest(t, deleteReq, http.StatusNoContent)
				}
			})
		}
	}

}

func getExpectedActionsTokenPerms(tokenPermissionsPayload map[string]string, workflowPerms map[unit_model.Type]perm_model.AccessMode) map[unit_model.Type]perm_model.AccessMode {
	mode := repo_model.ActionsTokenPermissionMode(tokenPermissionsPayload["token_permission_mode"])

	var defaultPerms map[unit_model.Type]perm_model.AccessMode

	switch mode {
	case repo_model.ActionsTokenPermissionModePermissive:
		defaultPerms = map[unit_model.Type]perm_model.AccessMode{
			unit_model.TypeCode:         perm_model.AccessModeWrite,
			unit_model.TypeIssues:       perm_model.AccessModeWrite,
			unit_model.TypePullRequests: perm_model.AccessModeWrite,
			unit_model.TypePackages:     perm_model.AccessModeRead,
			unit_model.TypeActions:      perm_model.AccessModeWrite,
			unit_model.TypeWiki:         perm_model.AccessModeWrite,
			unit_model.TypeReleases:     perm_model.AccessModeWrite,
			unit_model.TypeProjects:     perm_model.AccessModeWrite,
		}
	case repo_model.ActionsTokenPermissionModeRestricted:
		defaultPerms = map[unit_model.Type]perm_model.AccessMode{
			unit_model.TypeCode:         perm_model.AccessModeRead,
			unit_model.TypeIssues:       perm_model.AccessModeRead,
			unit_model.TypePullRequests: perm_model.AccessModeRead,
			unit_model.TypePackages:     perm_model.AccessModeRead,
			unit_model.TypeActions:      perm_model.AccessModeRead,
			unit_model.TypeWiki:         perm_model.AccessModeRead,
			unit_model.TypeReleases:     perm_model.AccessModeRead,
			unit_model.TypeProjects:     perm_model.AccessModeRead,
		}
	case repo_model.ActionsTokenPermissionModeCustom:
		parsePerm := func(key string) perm_model.AccessMode {
			switch tokenPermissionsPayload[key] {
			case "write":
				return perm_model.AccessModeWrite
			case "read":
				return perm_model.AccessModeRead
			default:
				return perm_model.AccessModeNone
			}
		}
		defaultPerms = map[unit_model.Type]perm_model.AccessMode{
			unit_model.TypeCode:         parsePerm("max_contents"),
			unit_model.TypeIssues:       parsePerm("max_issues"),
			unit_model.TypePullRequests: parsePerm("max_pull_requests"),
			unit_model.TypeWiki:         parsePerm("max_wiki"),
			unit_model.TypeReleases:     parsePerm("max_releases"),
			unit_model.TypeProjects:     parsePerm("max_projects"),
			unit_model.TypePackages:     parsePerm("max_packages"),
			unit_model.TypeActions:      parsePerm("max_actions"),
		}
	}

	perms := map[unit_model.Type]perm_model.AccessMode{}
	for unit, defaultAccess := range defaultPerms {
		accessMode := defaultAccess
		if workflowAccess, ok := workflowPerms[unit]; ok {
			if workflowAccess < accessMode {
				accessMode = workflowAccess
			}
		}
		perms[unit] = accessMode
	}

	return perms
}
