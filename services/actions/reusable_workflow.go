// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"

	actions_model "code.gitea.io/gitea/models/actions"
	"code.gitea.io/gitea/models/db"
	perm_model "code.gitea.io/gitea/models/perm"
	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/actions/jobparser"
	"code.gitea.io/gitea/modules/container"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/json"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/convert"

	act_pkg_model "github.com/nektos/act/pkg/model"
	act_pkg_runner "github.com/nektos/act/pkg/runner"
	"go.yaml.in/yaml/v4"
	"xorm.io/builder"
)

func buildReusableWorkflowCallSecrets(workflow *act_pkg_model.Workflow, workflowJob *jobparser.Job) (inherit bool, secretNamesJSON string, _ error) {
	if workflowJob != nil && workflowJob.RawSecrets.Kind == yaml.ScalarNode && workflowJob.RawSecrets.Value == "inherit" {
		return true, "", nil
	}

	mapping, err := jobparser.EvaluateWorkflowCallSecrets(workflow, workflowJob)
	if err != nil {
		return false, "", err
	}
	if len(mapping) == 0 {
		return false, "", nil
	}
	b, err := json.Marshal(mapping)
	if err != nil {
		return false, "", fmt.Errorf("marshal workflow_call secrets mapping: %w", err)
	}
	return false, string(b), nil
}

func ExpandReusableCallJob(ctx context.Context, callerJob *actions_model.ActionRunJob) error {
	if err := callerJob.LoadAttributes(ctx); err != nil {
		return err
	}
	workflowJob, err := callerJob.ParseJob()
	if err != nil {
		return err
	}
	if strings.TrimSpace(workflowJob.Uses) == "" {
		return nil
	}

	if err := ensureNoReusableWorkflowCallCycle(ctx, callerJob, workflowJob.Uses); err != nil {
		return err
	}

	ref, err := act_pkg_runner.ParseReusableWorkflowRef(workflowJob.Uses)
	if err != nil {
		return err
	}

	content, err := loadReusableWorkflowContent(ctx, callerJob.Run, ref)
	if err != nil {
		return err
	}

	vars, err := actions_model.GetVariablesOfRun(ctx, callerJob.Run)
	if err != nil {
		return err
	}
	runInputs, err := getInputsFromRun(callerJob.Run)
	if err != nil {
		return fmt.Errorf("get inputs: %w", err)
	}

	singleWorkflow := &jobparser.SingleWorkflow{}
	if err := yaml.Unmarshal(content, singleWorkflow); err != nil {
		return fmt.Errorf("unmarshal workflow: %w", err)
	}
	workflow := &act_pkg_model.Workflow{
		RawOn: singleWorkflow.RawOn,
	}
	actionsJobCtx, err := GenerateGiteaContext(ctx, callerJob.Run, callerJob)
	if err != nil {
		return err
	}
	jobResults, err := findJobNeedsAndFillJobResults(ctx, callerJob)
	if err != nil {
		return err
	}

	inputsWithDefaults, err := jobparser.EvaluateWorkflowCallInputs(workflow, callerJob.JobID, workflowJob, actionsJobCtx, jobResults, vars, runInputs)
	if err != nil {
		return err
	}

	eventPayload, err := buildWorkflowCallEventPayload(ctx, callerJob.Run, inputsWithDefaults)
	if err != nil {
		return err
	}

	// Check if the caller job has already been expanded.
	exist, err := db.GetEngine(ctx).Exist(&actions_model.ActionRunJob{
		RunID:           callerJob.RunID,
		ParentCallJobID: callerJob.ID,
	})
	if err != nil {
		return err
	}
	if exist {
		callerJob.CallEventPayload = string(eventPayload)
		_, err := actions_model.UpdateRunJob(ctx, callerJob, nil, "call_event_payload")
		return err
	}

	callSecretsInherit, callSecretNamesJSON, err := buildReusableWorkflowCallSecrets(workflow, workflowJob)
	if err != nil {
		return err
	}

	giteaCtx, err := GenerateGiteaContext(ctx, callerJob.Run, callerJob)
	if err != nil {
		return err
	}

	calledJobs, err := jobparser.Parse(content,
		jobparser.WithVars(vars),
		jobparser.WithGitContext(giteaCtx.ToGitHubContext()),
		jobparser.WithInputs(inputsWithDefaults),
	)
	if err != nil {
		return err
	}

	rootCallJobID := util.Iif(callerJob.RootCallJobID == 0, callerJob.ID, callerJob.RootCallJobID)

	now := timeutil.TimeStampNow()
	var readyCallJobs []*actions_model.ActionRunJob
	if err := db.WithTx(ctx, func(ctx context.Context) error {
		// Update the caller job itself.
		callerJob.IsReusableCall = true
		callerJob.ReusableWorkflowUses = workflowJob.Uses
		callerJob.CallEventPayload = string(eventPayload)
		callerJob.CallSecretsInherit = callSecretsInherit
		callerJob.CallSecretNames = callSecretNamesJSON
		if callerJob.Started.IsZero() {
			callerJob.Started = now
		}
		callerJob.Status = actions_model.StatusRunning
		if _, err := actions_model.UpdateRunJob(ctx, callerJob, builder.In("status", []actions_model.Status{actions_model.StatusWaiting, actions_model.StatusBlocked}), "is_reusable_call", "reusable_workflow_uses", "call_event_payload", "call_secrets_inherit", "call_secret_names", "status", "started"); err != nil {
			return err
		}

		var hasWaitingJobs bool
		for _, v := range calledJobs {
			id, job := v.Job()
			needs := job.Needs()
			if err := v.SetJob(id, job.EraseNeeds()); err != nil {
				return err
			}
			payload, _ := v.Marshal()

			shouldBlockJob := len(needs) > 0

			job.Name = util.EllipsisDisplayString(job.Name, 255)
			runJob := &actions_model.ActionRunJob{
				RunID:             callerJob.RunID,
				RepoID:            callerJob.RepoID,
				OwnerID:           callerJob.OwnerID,
				CommitSHA:         callerJob.CommitSHA,
				IsForkPullRequest: callerJob.IsForkPullRequest,
				Name:              job.Name,
				WorkflowPayload:   payload,
				JobID:             id,
				Needs:             needs,
				RunsOn:            job.RunsOn(),
				Status:            util.Iif(shouldBlockJob, actions_model.StatusBlocked, actions_model.StatusWaiting),

				ParentCallJobID: callerJob.ID,
				RootCallJobID:   rootCallJobID,
				CallDepth:       callerJob.CallDepth + 1,
			}
			if job.Uses != "" {
				runJob.IsReusableCall = true
				runJob.ReusableWorkflowUses = job.Uses
			}

			if job.RawConcurrency != nil {
				rawConcurrency, err := yaml.Marshal(job.RawConcurrency)
				if err != nil {
					return fmt.Errorf("marshal raw concurrency: %w", err)
				}
				runJob.RawConcurrency = string(rawConcurrency)

				if len(needs) == 0 {
					if err := EvaluateJobConcurrencyFillModel(ctx, callerJob.Run, runJob, vars, inputsWithDefaults); err != nil {
						return fmt.Errorf("evaluate job concurrency: %w", err)
					}
				}

				if runJob.Status == actions_model.StatusWaiting {
					runJob.Status, err = PrepareToStartJobWithConcurrency(ctx, runJob)
					if err != nil {
						return fmt.Errorf("prepare to start job with concurrency: %w", err)
					}
				}
			}

			if err := db.Insert(ctx, runJob); err != nil {
				return err
			}
			if runJob.Status == actions_model.StatusWaiting {
				if runJob.IsReusableCall {
					readyCallJobs = append(readyCallJobs, runJob)
				} else {
					hasWaitingJobs = true
				}
			}
		}

		if hasWaitingJobs {
			if err := actions_model.IncreaseTaskVersion(ctx, callerJob.OwnerID, callerJob.RepoID); err != nil {
				return err
			}
		}

		allJobs, err := actions_model.GetRunJobsByRunID(ctx, callerJob.RunID)
		if err != nil {
			return err
		}

		// UpdateRun requires an up-to-date Version, and the run may have already been updated by UpdateRunJob above.
		// Reload it to avoid optimistic locking failures like "run has changed".
		run, err := actions_model.GetRunByRepoAndID(ctx, callerJob.RepoID, callerJob.RunID)
		if err != nil {
			return err
		}
		run.Status = actions_model.AggregateJobStatus(allJobs)
		return actions_model.UpdateRun(ctx, run, "status")
	}); err != nil {
		return err
	}

	for _, callJob := range readyCallJobs {
		if err := ExpandReusableCallJob(ctx, callJob); err != nil {
			return err
		}
	}

	return nil
}

func loadReusableWorkflowContent(ctx context.Context, parentRun *actions_model.ActionRun, ref *act_pkg_runner.ReusableWorkflowRef) ([]byte, error) {
	if ref.Kind == act_pkg_runner.ReusableWorkflowKindLocalSameRepo {
		if err := parentRun.LoadRepo(ctx); err != nil {
			return nil, err
		}
		return readWorkflowContentFromLocalRepo(ctx, parentRun.Repo, parentRun.Ref, ref.WorkflowPath)
	}

	if ref.Kind == act_pkg_runner.ReusableWorkflowKindLocalOtherRepo {
		repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, ref.Owner, ref.Repo)
		if err != nil {
			return nil, err
		}
		if repo.IsPrivate {
			perm, err := access_model.GetActionsUserRepoPermissionByActionRun(ctx, repo, user_model.NewActionsUser(), parentRun)
			if err != nil {
				return nil, err
			}
			if !perm.CanRead(unit.TypeCode) {
				return nil, errors.New("actions user has no access to reusable workflow repo")
			}
		}
		return readWorkflowContentFromLocalRepo(ctx, repo, ref.Ref, ref.WorkflowPath)
	}

	content, err := ref.FetchReusableWorkflowContent(ctx)
	if err != nil {
		return nil, err
	}
	return content, nil
}

func readWorkflowContentFromLocalRepo(ctx context.Context, repo *repo_model.Repository, ref, workflowPath string) ([]byte, error) {
	gitRepo, err := gitrepo.OpenRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer gitRepo.Close()

	commit, err := gitRepo.GetCommit(ref)
	if err != nil {
		return nil, err
	}
	content, err := commit.GetFileContent(workflowPath, 1024*1024)
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

func buildWorkflowCallEventPayload(ctx context.Context, run *actions_model.ActionRun, inputs map[string]any) ([]byte, error) {
	if err := run.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	payload := &api.WorkflowCallPayload{
		Workflow:   run.WorkflowID,
		Ref:        run.Ref,
		Repository: convert.ToRepo(ctx, run.Repo, access_model.Permission{AccessMode: perm_model.AccessModeNone}),
		Sender:     convert.ToUserWithAccessMode(ctx, run.TriggerUser, perm_model.AccessModeNone),
		Inputs:     inputs,
	}
	eventPayload, err := payload.JSONPayload()
	if err != nil {
		return nil, fmt.Errorf("JSONPayload: %w", err)
	}
	return eventPayload, nil
}

func ensureNoReusableWorkflowCallCycle(ctx context.Context, callerJob *actions_model.ActionRunJob, uses string) error {
	visited := make(container.Set[string])
	visited.Add(uses)

	parentID := callerJob.ParentCallJobID
	for parentID > 0 {
		parentJob, err := actions_model.GetRunJobByRepoAndID(ctx, callerJob.RepoID, parentID)
		if err != nil {
			return err
		}
		if parentJob.ReusableWorkflowUses != "" {
			if visited.Contains(parentJob.ReusableWorkflowUses) {
				return fmt.Errorf("reusable workflow call cycle detected: %q", parentJob.ReusableWorkflowUses)
			}
			visited.Add(parentJob.ReusableWorkflowUses)
		}
		parentID = parentJob.ParentCallJobID
	}

	return nil
}
