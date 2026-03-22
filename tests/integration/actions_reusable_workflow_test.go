// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	actions_model "code.gitea.io/gitea/models/actions"
	auth_model "code.gitea.io/gitea/models/auth"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/json"
	api "code.gitea.io/gitea/modules/structs"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"github.com/stretchr/testify/assert"
)

func TestJobUsesReusableWorkflow(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		user2Session := loginUser(t, user2.Name)
		user2Token := getTokenForLoggedInUser(t, user2Session, auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteUser)

		apiRepo := createActionsTestRepo(t, user2Token, "workflow-call-test", false)
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: apiRepo.ID})

		defaultRunner := newMockRunner()
		defaultRunner.registerAsRepoRunner(t, repo.OwnerName, repo.Name, "mock-default-runner", []string{"ubuntu-latest"}, false)
		customRunner := newMockRunner()
		customRunner.registerAsRepoRunner(t, repo.OwnerName, repo.Name, "mock-custom-runner", []string{"custom-os"}, false)

		// add a variable for test
		req := NewRequestWithJSON(t, "POST",
			fmt.Sprintf("/api/v1/repos/%s/%s/actions/variables/myvar", repo.OwnerName, repo.Name), &api.CreateVariableOption{
				Value: "abc123",
			}).
			AddTokenAuth(user2Token)
		MakeRequest(t, req, http.StatusCreated)
		// add a secret for test
		req = NewRequestWithJSON(t, "PUT", fmt.Sprintf("/api/v1/repos/%s/%s/actions/secrets/mysecret", repo.OwnerName, repo.Name), api.CreateOrUpdateSecretOption{
			Data: "secRET-t0Ken",
		}).AddTokenAuth(user2Token)
		MakeRequest(t, req, http.StatusCreated)

		createRepoWorkflowFile(t, user2, repo, ".gitea/workflows/reusable1.yaml",
			`name: Reusable1
on:
  workflow_call:
    inputs:
      str_input:
        type: string
      num_input:
       type: number
      bool_input:
       type: boolean
      parent_var:
        type: string
      needs_out:
        type: string
    secrets:
      parent_token:
    outputs:
      r1_out:
        value: ${{ jobs.reusable1_job2.outputs.r1j2_out }}

jobs:
  reusable1_job1:
    runs-on: ubuntu-latest
    steps:
      - run: echo 'reusable1_job1'

  reusable1_job2:
    needs: [reusable1_job1]
    outputs:
      r1j2_out: ${{ steps.gen_r1j2_output.outputs.out }}
    runs-on: custom-os
    steps:
      - id: gen_r1j2_output
        run: |
          echo "out=r1j2_out_data" >> "$GITHUB_OUTPUT"
`)

		createRepoWorkflowFile(t, user2, repo, ".gitea/workflows/caller.yaml",
			`name: Caller
on:
  push:
    paths:
      - '.gitea/workflows/caller.yaml'
jobs:
  prepare:
    runs-on: ubuntu-latest
    outputs:
      prepare_out: ${{ steps.gen_output.outputs.po }}
    steps:
      - id: gen_output
        run: |
          echo "po=prepared_data" >> "$GITHUB_OUTPUT"

  caller_job1:
    needs: [prepare]
    uses: './.gitea/workflows/reusable1.yaml'
    with:
      str_input: 'from caller job1'
      num_input: ${{ 2.3e2 }}
      bool_input: ${{ gitea.event_name == 'push' }}
      parent_var: ${{ vars.myvar }}
      needs_out: ${{ needs.prepare.outputs.prepare_out }}
    secrets:
      parent_token: ${{ secrets.mysecret }}

  caller_job2:
    needs: [caller_job1]
    runs-on: ubuntu-latest
    steps:
      - run: |
          echo ${{ needs.caller_job1.outputs.r1_out }}
`)

		var (
			callerRunID     int64
			callerJob1ID    int64
			reusable1Job2ID int64
			callerJob2ID    int64
		)

		t.Run("Check initialized jobs", func(t *testing.T) {
			assert.Equal(t, 1, unittest.GetCount(t, &actions_model.ActionRun{RepoID: repo.ID}))
			callerRun := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{RepoID: repo.ID})
			callerRunID = callerRun.ID
			assert.Equal(t, 3, unittest.GetCount(t, &actions_model.ActionRunJob{RunID: callerRun.ID}))
			prepareJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{RunID: callerRun.ID, JobID: "prepare"})
			assert.Equal(t, actions_model.StatusWaiting, prepareJob.Status)
			assert.False(t, prepareJob.IsReusableCall)
			callerJob1 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{RunID: callerRun.ID, JobID: "caller_job1"})
			assert.Equal(t, actions_model.StatusBlocked, callerJob1.Status)
			assert.True(t, callerJob1.IsReusableCall)
			callerJob1ID = callerJob1.ID
			callerJob2 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{RunID: callerRun.ID, JobID: "caller_job2"})
			callerJob2ID = callerJob2.ID
			assert.Equal(t, actions_model.StatusBlocked, callerJob2.Status)
			assert.False(t, callerJob2.IsReusableCall)
		})

		t.Run("First run", func(t *testing.T) {
			prepareTask := defaultRunner.fetchTask(t) // for "prepare" job
			_, prepareJob, _ := getTaskAndJobAndRunByTaskID(t, prepareTask.Id)
			assert.Equal(t, "prepare", prepareJob.JobID)
			defaultRunner.fetchNoTask(t)
			defaultRunner.execTask(t, prepareTask, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"prepare_out": "prepared_data",
				},
			})

			reusable1Job1Task := defaultRunner.fetchTask(t) // for "reusable1_job1" job
			_, reusable1Job1, _ := getTaskAndJobAndRunByTaskID(t, reusable1Job1Task.Id)
			assert.Equal(t, "reusable1_job1", reusable1Job1.JobID)
			assert.Equal(t, callerJob1ID, reusable1Job1.ParentCallJobID)
			assert.Equal(t, callerJob1ID, reusable1Job1.RootCallJobID)
			payload := getWorkflowCallPayloadFromTask(t, reusable1Job1Task)
			if assert.Len(t, payload.Inputs, 5) {
				assert.Equal(t, "from caller job1", payload.Inputs["str_input"])
				assert.EqualValues(t, 230, payload.Inputs["num_input"])
				assert.Equal(t, true, payload.Inputs["bool_input"])
				assert.Equal(t, "abc123", payload.Inputs["parent_var"])
				assert.Equal(t, "prepared_data", payload.Inputs["needs_out"])
			}
			if assert.Len(t, reusable1Job1Task.Secrets, 3) {
				assert.Contains(t, reusable1Job1Task.Secrets, "GITEA_TOKEN")
				assert.Contains(t, reusable1Job1Task.Secrets, "GITHUB_TOKEN")
				assert.Equal(t, "secRET-t0Ken", reusable1Job1Task.Secrets["parent_token"])
			}
			customRunner.fetchNoTask(t)
			defaultRunner.execTask(t, reusable1Job1Task, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})

			reusable1Job2Task := customRunner.fetchTask(t) // for "reusable1_job2" job
			_, reusable1Job2, _ := getTaskAndJobAndRunByTaskID(t, reusable1Job2Task.Id)
			assert.Equal(t, "reusable1_job2", reusable1Job2.JobID)
			reusable1Job2ID = reusable1Job2.ID
			if assert.Len(t, reusable1Job2Task.Needs, 1) {
				assert.Contains(t, reusable1Job2Task.Needs, "reusable1_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, reusable1Job2Task.Needs["reusable1_job1"].Result)
			}
			customRunner.execTask(t, reusable1Job2Task, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"r1j2_out": "r1j2_out_data",
				},
			})
			callerJob1 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: callerJob1ID})
			assert.Equal(t, actions_model.StatusSuccess, callerJob1.Status)

			callerJob2Task := defaultRunner.fetchTask(t) // for "caller_job2" job
			_, callerJob2, _ := getTaskAndJobAndRunByTaskID(t, callerJob2Task.Id)
			assert.Equal(t, "caller_job2", callerJob2.JobID)
			if assert.Len(t, callerJob2Task.Needs, 1) {
				assert.Contains(t, callerJob2Task.Needs, "caller_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, callerJob2Task.Needs["caller_job1"].Result)
				if assert.Len(t, callerJob2Task.Needs["caller_job1"].Outputs, 1) {
					assert.Equal(t, "r1j2_out_data", callerJob2Task.Needs["caller_job1"].Outputs["r1_out"])
				}
			}
			defaultRunner.execTask(t, callerJob2Task, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})
			callerRun := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: callerRunID})
			assert.Equal(t, actions_model.StatusSuccess, callerRun.Status)
		})

		t.Run("Rerun 'reusable1_job2'", func(t *testing.T) {
			req = NewRequest(t, "POST", fmt.Sprintf("/%s/%s/actions/runs/%d/jobs/%d/rerun", repo.OwnerName, repo.Name, callerRunID, reusable1Job2ID))
			user2Session.MakeRequest(t, req, http.StatusOK)

			callerRun := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: callerRunID})
			assert.Equal(t, actions_model.StatusWaiting, callerRun.Status)
			callerJob1 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: callerJob1ID})
			assert.Equal(t, actions_model.StatusWaiting, callerJob1.Status)
			callerJob2 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: callerJob2ID})
			assert.Equal(t, actions_model.StatusBlocked, callerJob2.Status)

			defaultRunner.fetchNoTask(t)
			reusable1Job2Task := customRunner.fetchTask(t)
			_, reusable1Job2, _ := getTaskAndJobAndRunByTaskID(t, reusable1Job2Task.Id)
			assert.Equal(t, "reusable1_job2", reusable1Job2.JobID)
			assert.Equal(t, reusable1Job2ID, reusable1Job2.ID)
			assert.Equal(t, actions_model.StatusRunning, reusable1Job2.Status)
			callerRun = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: callerRunID})
			assert.Equal(t, actions_model.StatusRunning, callerRun.Status)
			customRunner.execTask(t, reusable1Job2Task, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"r1j2_out": "r1j2_out_data_updated",
				},
			})
			callerJob1 = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: callerJob1ID})
			assert.Equal(t, actions_model.StatusSuccess, callerJob1.Status)

			callerJob2Task := defaultRunner.fetchTask(t)
			_, callerJob2, _ = getTaskAndJobAndRunByTaskID(t, callerJob2Task.Id)
			assert.Equal(t, "caller_job2", callerJob2.JobID)
			if assert.Len(t, callerJob2Task.Needs, 1) {
				assert.Contains(t, callerJob2Task.Needs, "caller_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, callerJob2Task.Needs["caller_job1"].Result)
				if assert.Len(t, callerJob2Task.Needs["caller_job1"].Outputs, 1) {
					assert.Equal(t, "r1j2_out_data_updated", callerJob2Task.Needs["caller_job1"].Outputs["r1_out"])
				}
			}
			defaultRunner.execTask(t, callerJob2Task, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})
			callerRun = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: callerRunID})
			assert.Equal(t, actions_model.StatusSuccess, callerRun.Status)
		})

		t.Run("Rerun 'caller_job1'", func(t *testing.T) {
			req = NewRequest(t, "POST", fmt.Sprintf("/%s/%s/actions/runs/%d/jobs/%d/rerun", repo.OwnerName, repo.Name, callerRunID, callerJob1ID))
			user2Session.MakeRequest(t, req, http.StatusOK)

			callerRun := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: callerRunID})
			assert.Equal(t, actions_model.StatusWaiting, callerRun.Status)
			callerJob2 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: callerJob2ID})
			assert.Equal(t, actions_model.StatusBlocked, callerJob2.Status)

			reusable1Job1Task := defaultRunner.fetchTask(t)
			_, reusable1Job1, _ := getTaskAndJobAndRunByTaskID(t, reusable1Job1Task.Id)
			assert.Equal(t, "reusable1_job1", reusable1Job1.JobID)
			assert.Equal(t, callerJob1ID, reusable1Job1.ParentCallJobID)
			defaultRunner.execTask(t, reusable1Job1Task, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})

			reusable1Job2Task := customRunner.fetchTask(t)
			_, reusable1Job2, _ := getTaskAndJobAndRunByTaskID(t, reusable1Job2Task.Id)
			assert.Equal(t, "reusable1_job2", reusable1Job2.JobID)
			if assert.Len(t, reusable1Job2Task.Needs, 1) {
				assert.Contains(t, reusable1Job2Task.Needs, "reusable1_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, reusable1Job2Task.Needs["reusable1_job1"].Result)
			}
			customRunner.execTask(t, reusable1Job2Task, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"r1j2_out": "r1j2_out_data_rerun_caller_job1",
				},
			})

			callerJob2Task := defaultRunner.fetchTask(t)
			_, callerJob2, _ = getTaskAndJobAndRunByTaskID(t, callerJob2Task.Id)
			assert.Equal(t, "caller_job2", callerJob2.JobID)
			if assert.Len(t, callerJob2Task.Needs, 1) {
				assert.Contains(t, callerJob2Task.Needs, "caller_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, callerJob2Task.Needs["caller_job1"].Result)
				if assert.Len(t, callerJob2Task.Needs["caller_job1"].Outputs, 1) {
					assert.Equal(t, "r1j2_out_data_rerun_caller_job1", callerJob2Task.Needs["caller_job1"].Outputs["r1_out"])
				}
			}
			defaultRunner.execTask(t, callerJob2Task, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})

			callerRun = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: callerRunID})
			assert.Equal(t, actions_model.StatusSuccess, callerRun.Status)
		})
	})
}

func createRepoWorkflowFile(t *testing.T, u *user_model.User, repo *repo_model.Repository, treePath, content string) {
	token := getTokenForLoggedInUser(t, loginUser(t, u.Name), auth_model.AccessTokenScopeWriteRepository)
	opts := getWorkflowCreateFileOptions(u, repo.DefaultBranch, "create "+treePath, content)
	createWorkflowFile(t, token, repo.OwnerName, repo.Name, treePath, opts)
}

func getWorkflowCallPayloadFromTask(t *testing.T, runnerTask *runnerv1.Task) *api.WorkflowCallPayload {
	eventJSON, err := runnerTask.GetContext().Fields["event"].GetStructValue().MarshalJSON()
	assert.NoError(t, err)
	var payload api.WorkflowCallPayload
	assert.NoError(t, json.Unmarshal(eventJSON, &payload))
	return &payload
}
