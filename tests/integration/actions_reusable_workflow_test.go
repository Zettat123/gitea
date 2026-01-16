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

		t.Run("Check initialized jobs", func(t *testing.T) {
			assert.Equal(t, 1, unittest.GetCount(t, &actions_model.ActionRun{RepoID: repo.ID}))
			callerRun := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{RepoID: repo.ID})
			assert.Equal(t, 3, unittest.GetCount(t, &actions_model.ActionRunJob{RunID: callerRun.ID}))
			prepareJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{RunID: callerRun.ID, JobID: "prepare"})
			assert.Equal(t, actions_model.StatusWaiting, prepareJob.Status)
			job1 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{RunID: callerRun.ID, JobID: "caller_job1"})
			assert.Equal(t, actions_model.StatusBlocked, job1.Status)
			job2 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{RunID: callerRun.ID, JobID: "caller_job2"})
			assert.Equal(t, actions_model.StatusBlocked, job2.Status)
		})

		var (
			rootRunID       int64 // "Caller" run
			job1ID          int64 // "prepare" job
			run2ID          int64 // "Reusable1" run
			run2ParentJobID int64 // "caller_job1" job
			job3ID          int64 // "reusable1_job2" job
		)
		t.Run("First run", func(t *testing.T) {
			task1 := defaultRunner.fetchTask(t) // for "prepare" job
			_, job1, run1 := getTaskAndJobAndRunByTaskID(t, task1.Id)
			job1ID = job1.ID
			rootRunID = run1.ID
			assert.Equal(t, "prepare", job1.JobID)
			defaultRunner.fetchNoTask(t)
			defaultRunner.execTask(t, task1, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"prepare_out": "prepared_data",
				},
			})

			task2 := defaultRunner.fetchTask(t) // for "reusable1_job1" job
			_, job2, run2 := getTaskAndJobAndRunByTaskID(t, task2.Id)
			assert.Equal(t, "reusable1_job1", job2.JobID)
			run2ID = run2.ID
			payload := getWorkflowCallPayloadFromTask(t, task2)
			if assert.Len(t, payload.Inputs, 5) {
				assert.Equal(t, "from caller job1", payload.Inputs["str_input"])
				assert.EqualValues(t, 230, payload.Inputs["num_input"])
				assert.Equal(t, true, payload.Inputs["bool_input"])
				assert.Equal(t, "abc123", payload.Inputs["parent_var"])
				assert.Equal(t, "prepared_data", payload.Inputs["needs_out"])
			}
			if assert.Len(t, task2.Secrets, 3) {
				assert.Contains(t, task2.Secrets, "GITEA_TOKEN")
				assert.Contains(t, task2.Secrets, "GITHUB_TOKEN")
				assert.Equal(t, "secRET-t0Ken", task2.Secrets["parent_token"])
			}
			run2ParentJobID = run2.ParentJobID
			run2ParentJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: run2.ParentJobID})
			assert.Equal(t, run2.ID, run2ParentJob.ChildRunID)
			assert.Equal(t, "caller_job1", run2ParentJob.JobID)
			assert.Equal(t, actions_model.StatusRunning, run2ParentJob.Status)
			customRunner.fetchNoTask(t)
			defaultRunner.execTask(t, task2, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})

			task3 := customRunner.fetchTask(t) // for "reusable1_job2" job
			_, job3, run3 := getTaskAndJobAndRunByTaskID(t, task3.Id)
			job3ID = job3.ID
			assert.Equal(t, "reusable1_job2", job3.JobID)
			assert.Equal(t, run2.ID, run3.ID)
			if assert.Len(t, task3.Needs, 1) {
				assert.Contains(t, task3.Needs, "reusable1_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, task3.Needs["reusable1_job1"].Result)
			}
			customRunner.execTask(t, task3, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"r1j2_out": "r1j2_out_data",
				},
			})
			run3ParentJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: run3.ParentJobID})
			assert.Equal(t, actions_model.StatusSuccess, run3ParentJob.Status)

			task4 := defaultRunner.fetchTask(t) // for "caller_job2" job
			_, job4, _ := getTaskAndJobAndRunByTaskID(t, task4.Id)
			assert.Equal(t, "caller_job2", job4.JobID)
			if assert.Len(t, task4.Needs, 1) {
				assert.Contains(t, task4.Needs, "caller_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, task4.Needs["caller_job1"].Result)
				if assert.Len(t, task4.Needs["caller_job1"].Outputs, 1) {
					assert.Equal(t, "r1j2_out_data", task4.Needs["caller_job1"].Outputs["r1_out"])
				}
			}
			defaultRunner.execTask(t, task4, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})
			run1 = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: run1.ID})
			assert.Equal(t, actions_model.StatusSuccess, run1.Status)
		})

		t.Run("Rerun 'reusable1_job2'", func(t *testing.T) {
			req = NewRequest(t, "POST", fmt.Sprintf("/%s/%s/actions/runs/%d/jobs/%d/rerun", repo.OwnerName, repo.Name, rootRunID, job3ID))
			user2Session.MakeRequest(t, req, http.StatusOK)

			run1 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: rootRunID})
			assert.Equal(t, actions_model.StatusWaiting, run1.Status)
			run2 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: run2ID})
			assert.Equal(t, actions_model.StatusWaiting, run2.Status)
			run2ParentJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: run2ParentJobID})
			assert.Equal(t, actions_model.StatusWaiting, run2ParentJob.Status)

			defaultRunner.fetchNoTask(t)

			task3 := customRunner.fetchTask(t)
			_, job3, run3 := getTaskAndJobAndRunByTaskID(t, task3.Id)
			assert.Equal(t, "reusable1_job2", job3.JobID)
			assert.Equal(t, actions_model.StatusRunning, job3.Status)
			run1 = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: run1.ID})
			assert.Equal(t, actions_model.StatusRunning, run1.Status)
			customRunner.execTask(t, task3, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"r1j2_out": "r1j2_out_data_updated",
				},
			})
			run3ParentJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: run3.ParentJobID})
			assert.Equal(t, actions_model.StatusSuccess, run3ParentJob.Status)

			task4 := defaultRunner.fetchTask(t)
			_, job4, _ := getTaskAndJobAndRunByTaskID(t, task4.Id)
			assert.Equal(t, "caller_job2", job4.JobID)
			if assert.Len(t, task4.Needs, 1) {
				assert.Contains(t, task4.Needs, "caller_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, task4.Needs["caller_job1"].Result)
				if assert.Len(t, task4.Needs["caller_job1"].Outputs, 1) {
					assert.Equal(t, "r1j2_out_data_updated", task4.Needs["caller_job1"].Outputs["r1_out"])
				}
			}
			defaultRunner.execTask(t, task4, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})
			run1 = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: run1.ID})
			assert.Equal(t, actions_model.StatusSuccess, run1.Status)
		})

		t.Run("Rerun 'prepare'", func(t *testing.T) {
			req = NewRequest(t, "POST", fmt.Sprintf("/%s/%s/actions/runs/%d/jobs/%d/rerun", repo.OwnerName, repo.Name, rootRunID, job1ID))
			user2Session.MakeRequest(t, req, http.StatusOK)

			run1 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: rootRunID})
			assert.Equal(t, actions_model.StatusWaiting, run1.Status)
			run2 := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: run2ID})
			assert.Equal(t, actions_model.StatusBlocked, run2.Status)
			run2ParentJob := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRunJob{ID: run2ParentJobID})
			assert.Equal(t, actions_model.StatusBlocked, run2ParentJob.Status)

			task1 := defaultRunner.fetchTask(t)
			_, job1, _ := getTaskAndJobAndRunByTaskID(t, task1.Id)
			assert.Equal(t, "prepare", job1.JobID)
			run1 = unittest.AssertExistsAndLoadBean(t, &actions_model.ActionRun{ID: run1.ID})
			assert.Equal(t, actions_model.StatusRunning, run1.Status)
			defaultRunner.fetchNoTask(t)
			defaultRunner.execTask(t, task1, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"prepare_out": "prepared_data_updated",
				},
			})

			task2 := defaultRunner.fetchTask(t)
			_, job2, _ := getTaskAndJobAndRunByTaskID(t, task2.Id)
			assert.Equal(t, "reusable1_job1", job2.JobID)
			customRunner.fetchNoTask(t)
			payload := getWorkflowCallPayloadFromTask(t, task2)
			assert.Equal(t, "prepared_data_updated", payload.Inputs["needs_out"])
			defaultRunner.execTask(t, task2, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
			})

			task3 := customRunner.fetchTask(t)
			_, job3, _ := getTaskAndJobAndRunByTaskID(t, task3.Id)
			assert.Equal(t, "reusable1_job2", job3.JobID)
			defaultRunner.fetchNoTask(t)
			customRunner.execTask(t, task3, &mockTaskOutcome{
				result: runnerv1.Result_RESULT_SUCCESS,
				outputs: map[string]string{
					"r1j2_out": "r1j2_out_data_updated",
				},
			})

			task4 := defaultRunner.fetchTask(t)
			_, job4, _ := getTaskAndJobAndRunByTaskID(t, task4.Id)
			assert.Equal(t, "caller_job2", job4.JobID)
			if assert.Len(t, task4.Needs, 1) {
				assert.Contains(t, task4.Needs, "caller_job1")
				assert.Equal(t, runnerv1.Result_RESULT_SUCCESS, task4.Needs["caller_job1"].Result)
				if assert.Len(t, task4.Needs["caller_job1"].Outputs, 1) {
					assert.Equal(t, "r1j2_out_data_updated", task4.Needs["caller_job1"].Outputs["r1_out"])
				}
			}
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
