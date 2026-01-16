// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"context"
	"fmt"
	"slices"

	actions_model "code.gitea.io/gitea/models/actions"
	"code.gitea.io/gitea/models/db"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/modules/container"
	"code.gitea.io/gitea/modules/util"
	notify_service "code.gitea.io/gitea/services/notify"

	"github.com/nektos/act/pkg/model"
	"go.yaml.in/yaml/v4"
	"xorm.io/builder"
)

// buildRerunPlan builds a rerun plan for the given root run and optional selected job ID.
func buildRerunPlan(ctx context.Context, repo *repo_model.Repository, rootRun *actions_model.ActionRun, selectedJobID int64) (*rerunPlan, error) {
	plan := &rerunPlan{
		singleRunRerunPlans: make(map[int64]*singleRunRerunPlan),
	}

	if selectedJobID == 0 {
		if err := plan.addRunAndJobs(ctx, repo.ID, rootRun.ID, true, false, 0, nil); err != nil {
			return nil, err
		}

		rootJobs, err := actions_model.GetRunJobsByRunID(ctx, rootRun.ID)
		if err != nil {
			return nil, err
		}
		if err := plan.expandChildRuns(ctx, repo.ID, rootJobs); err != nil {
			return nil, err
		}
	} else {
		selectedJob, err := actions_model.GetRunJobByRepoAndID(ctx, repo.ID, selectedJobID)
		if err != nil {
			return nil, err
		}

		selectedRun, err := actions_model.GetRunByRepoAndID(ctx, repo.ID, selectedJob.RunID)
		if err != nil {
			return nil, err
		}
		selectedRunJobs, err := actions_model.GetRunJobsByRunID(ctx, selectedRun.ID)
		if err != nil {
			return nil, err
		}

		rerunJobs := GetAllRerunJobs(selectedJob, selectedRunJobs)
		rerunJobIDs := make([]int64, 0, len(rerunJobs))
		for _, j := range rerunJobs {
			rerunJobIDs = append(rerunJobIDs, j.ID)
		}
		if err := plan.addRunAndJobs(ctx, repo.ID, selectedRun.ID, false, false, selectedJob.ID, rerunJobIDs); err != nil {
			return nil, err
		}
		if err := plan.expandChildRuns(ctx, repo.ID, rerunJobs); err != nil {
			return nil, err
		}

		currentRun := selectedRun
		for currentRun.ParentJobID > 0 {
			if err := currentRun.LoadParentJob(ctx); err != nil {
				return nil, err
			}
			parentJob := currentRun.ParentJob

			parentRun, err := actions_model.GetRunByRepoAndID(ctx, repo.ID, parentJob.RunID)
			if err != nil {
				return nil, err
			}
			parentRunJobs, err := actions_model.GetRunJobsByRunID(ctx, parentRun.ID)
			if err != nil {
				return nil, err
			}

			parentRerunJobs := GetAllRerunJobs(parentJob, parentRunJobs)
			parentRerunJobIDs := make([]int64, 0, len(parentRerunJobs))
			for _, j := range parentRerunJobs {
				parentRerunJobIDs = append(parentRerunJobIDs, j.ID)
			}
			if err := plan.addRunAndJobs(ctx, repo.ID, parentRun.ID, false, false, parentJob.ID, parentRerunJobIDs); err != nil {
				return nil, err
			}
			if err := plan.expandChildRuns(ctx, repo.ID, parentRerunJobs); err != nil {
				return nil, err
			}

			currentRun = parentRun
		}
	}

	order, err := computeRerunPlanOrder(ctx, repo.ID, plan.singleRunRerunPlans)
	if err != nil {
		return nil, err
	}
	plan.order = order

	return plan, nil
}

func executeRerunPlan(ctx context.Context, repo *repo_model.Repository, plan *rerunPlan) error {
	for _, runID := range plan.order {
		rp, ok := plan.singleRunRerunPlans[runID]
		if !ok {
			continue
		}
		if err := executeSingleRunRerunPlan(ctx, repo, rp); err != nil {
			return err
		}
	}

	return nil
}

func executeSingleRunRerunPlan(ctx context.Context, repo *repo_model.Repository, rp *singleRunRerunPlan) error {
	run := rp.Run

	if err := prepareRunForRerun(ctx, repo, run); err != nil {
		return err
	}

	jobs, err := actions_model.GetRunJobsByRunID(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		job.Run = run
	}

	isRunBlocked := run.Status == actions_model.StatusBlocked

	if rp.AllJobs {
		forceBlockAllJobs := rp.ForceBlockAllJobs
		if !forceBlockAllJobs && run.ParentJobID > 0 {
			if err := run.LoadParentJob(ctx); err != nil {
				return err
			}
			if !run.ParentJob.Status.In(actions_model.StatusWaiting, actions_model.StatusRunning) {
				forceBlockAllJobs = true
			}
		}

		for _, job := range jobs {
			shouldBlockJob := forceBlockAllJobs || len(job.Needs) > 0 || isRunBlocked
			if err := rerunOneJob(ctx, job, shouldBlockJob); err != nil {
				return err
			}
		}
		return nil
	}

	for _, job := range jobs {
		if rp.JobIDs == nil || !rp.JobIDs.Contains(job.ID) {
			continue
		}
		isStart := rp.StartJobIDs != nil && rp.StartJobIDs.Contains(job.ID)
		shouldBlockJob := !isStart || isRunBlocked
		if err := rerunOneJob(ctx, job, shouldBlockJob); err != nil {
			return err
		}
	}

	return nil
}

func prepareRunForRerun(ctx context.Context, repo *repo_model.Repository, run *actions_model.ActionRun) error {
	// Rerun is not allowed if the run is not done.
	if !run.Status.IsDone() {
		return util.NewInvalidArgumentErrorf("this workflow run is not done")
	}

	cfgUnit := repo.MustGetUnit(ctx, unit.TypeActions)

	// Rerun is not allowed when workflow is disabled.
	cfg := cfgUnit.ActionsConfig()
	if cfg.IsWorkflowDisabled(run.WorkflowID) {
		return util.NewInvalidArgumentErrorf("workflow %s is disabled", run.WorkflowID)
	}

	// Reset run's timestamps and status.
	run.PreviousDuration = run.Duration()
	run.Started = 0
	run.Stopped = 0
	run.Status = actions_model.StatusWaiting

	if run.RawConcurrency != "" {
		var rawConcurrency model.RawConcurrency
		if err := yaml.Unmarshal([]byte(run.RawConcurrency), &rawConcurrency); err != nil {
			return fmt.Errorf("unmarshal raw concurrency: %w", err)
		}

		vars, err := actions_model.GetVariablesOfRun(ctx, run)
		if err != nil {
			return fmt.Errorf("get run %d variables: %w", run.ID, err)
		}

		if err := EvaluateRunConcurrencyFillModel(ctx, run, &rawConcurrency, vars, nil); err != nil {
			return fmt.Errorf("evaluate run concurrency: %w", err)
		}

		run.Status, err = PrepareToStartRunWithConcurrency(ctx, run)
		if err != nil {
			return err
		}
	}

	if err := actions_model.UpdateRun(ctx, run, "started", "stopped", "previous_duration", "status", "concurrency_group", "concurrency_cancel"); err != nil {
		return err
	}

	if err := run.LoadAttributes(ctx); err != nil {
		return err
	}

	notify_service.WorkflowRunStatusUpdate(ctx, run.Repo, run.TriggerUser, run)
	return nil
}

func rerunOneJob(ctx context.Context, job *actions_model.ActionRunJob, shouldBlock bool) error {
	status := job.Status
	if !status.IsDone() {
		return nil
	}

	job.TaskID = 0
	job.Status = util.Iif(shouldBlock, actions_model.StatusBlocked, actions_model.StatusWaiting)
	job.Started = 0
	job.Stopped = 0
	job.ConcurrencyGroup = ""
	job.ConcurrencyCancel = false
	job.IsConcurrencyEvaluated = false

	if err := job.LoadRun(ctx); err != nil {
		return err
	}
	if err := job.Run.LoadAttributes(ctx); err != nil {
		return err
	}

	if job.RawConcurrency != "" && !shouldBlock {
		vars, err := actions_model.GetVariablesOfRun(ctx, job.Run)
		if err != nil {
			return fmt.Errorf("get run %d variables: %w", job.Run.ID, err)
		}

		if err := EvaluateJobConcurrencyFillModel(ctx, job.Run, job, vars, nil); err != nil {
			return fmt.Errorf("evaluate job concurrency: %w", err)
		}

		job.Status, err = PrepareToStartJobWithConcurrency(ctx, job)
		if err != nil {
			return err
		}
	}

	if err := db.WithTx(ctx, func(ctx context.Context) error {
		updateCols := []string{"task_id", "status", "started", "stopped", "concurrency_group", "concurrency_cancel", "is_concurrency_evaluated"}
		_, err := actions_model.UpdateRunJob(ctx, job, builder.Eq{"status": status}, updateCols...)
		return err
	}); err != nil {
		return err
	}

	CreateCommitStatusForRunJobs(ctx, job.Run, job)
	notify_service.WorkflowJobStatusUpdate(ctx, job.Run.Repo, job.Run.TriggerUser, job, nil)
	return nil
}

type rerunPlan struct {
	singleRunRerunPlans map[int64]*singleRunRerunPlan
	order               []int64
}

type singleRunRerunPlan struct {
	// Run is the action run to rerun. It may be nil if it wasn't loaded yet.
	Run *actions_model.ActionRun
	// AllJobs indicates that all jobs in this run should be rerun.
	// When true, StartJobIDs and JobIDs are ignored and may be nil.
	AllJobs bool
	// ForceBlockAllJobs indicates that all jobs in this run should be set to StatusBlocked when rerunning.
	// It is used for child runs that are rerun because their parent job is rerun: child run jobs must not start before the parent job is unblocked.
	ForceBlockAllJobs bool
	// StartJobIDs contains the IDs of the "start" jobs for this run.
	// These jobs are rerun in StatusWaiting, while other jobs in JobIDs are rerun in StatusBlocked to wait for dependencies.
	StartJobIDs container.Set[int64]
	// JobIDs contains the IDs of jobs in this run that should be rerun.
	// If AllJobs is false, only jobs in this set will be rerun.
	JobIDs container.Set[int64]
}

func (p *rerunPlan) addRunAndJobs(ctx context.Context, repoID, runID int64, allJobs, forceBlockAllJobs bool, startJobID int64, jobIDs []int64) error {
	rp, ok := p.singleRunRerunPlans[runID]
	if !ok {
		run, err := actions_model.GetRunByRepoAndID(ctx, repoID, runID)
		if err != nil {
			return err
		}
		rp = &singleRunRerunPlan{
			Run: run,
		}
		p.singleRunRerunPlans[runID] = rp
	}

	rp.ForceBlockAllJobs = rp.ForceBlockAllJobs || forceBlockAllJobs

	if rp.AllJobs {
		return nil
	}

	if allJobs {
		rp.AllJobs = true
		rp.StartJobIDs = nil
		rp.JobIDs = nil
		return nil
	}

	if rp.StartJobIDs == nil {
		rp.StartJobIDs = make(container.Set[int64])
	}
	rp.StartJobIDs.Add(startJobID)
	if rp.JobIDs == nil {
		rp.JobIDs = make(container.Set[int64])
	}
	for _, jobID := range jobIDs {
		rp.JobIDs.Add(jobID)
	}
	return nil
}

func (p *rerunPlan) isJobBlockedByPlan(job *actions_model.ActionRunJob) bool {
	rp, ok := p.singleRunRerunPlans[job.RunID]
	if !ok || rp == nil {
		return false
	}

	if rp.AllJobs {
		return len(job.Needs) > 0
	}

	if rp.StartJobIDs != nil && rp.StartJobIDs.Contains(job.ID) {
		return false
	}

	return true
}

func (p *rerunPlan) expandChildRuns(ctx context.Context, repoID int64, jobs []*actions_model.ActionRunJob) error {
	for _, job := range jobs {
		if job.ChildRunID <= 0 {
			continue
		}

		if _, ok := p.singleRunRerunPlans[job.ChildRunID]; ok {
			continue
		}

		forceBlockAllJobs := p.isJobBlockedByPlan(job)
		if err := p.addRunAndJobs(ctx, repoID, job.ChildRunID, true, forceBlockAllJobs, 0, nil); err != nil {
			return err
		}

		childRunJobs, err := actions_model.GetRunJobsByRunID(ctx, job.ChildRunID)
		if err != nil {
			return err
		}

		if err := p.expandChildRuns(ctx, repoID, childRunJobs); err != nil {
			return err
		}
	}
	return nil
}

func computeRerunPlanOrder(ctx context.Context, repoID int64, runs map[int64]*singleRunRerunPlan) ([]int64, error) {
	parentRunID := make(map[int64]int64, len(runs)) // run_id => parent_run_id
	for runID, rp := range runs {
		run := rp.Run
		if run.ParentJobID == 0 {
			parentRunID[runID] = 0
			continue
		}
		parentJob, err := actions_model.GetRunJobByRepoAndID(ctx, repoID, run.ParentJobID)
		if err != nil {
			return nil, err
		}
		parentRunID[runID] = parentJob.RunID
	}

	depthByRunID := make(map[int64]int64, len(runs))
	for runID := range runs {
		if _, ok := depthByRunID[runID]; ok {
			continue
		}

		var ancestorRunIDs []int64
		var baseDepth int64
		cur := runID
		for {
			if d, ok := depthByRunID[cur]; ok {
				baseDepth = d + 1
				break
			}
			ancestorRunIDs = append(ancestorRunIDs, cur)
			parent, ok := parentRunID[cur]
			if !ok || parent == 0 {
				baseDepth = 0
				break
			}
			cur = parent
		}

		depth := baseDepth
		for i := len(ancestorRunIDs) - 1; i >= 0; i-- {
			depthByRunID[ancestorRunIDs[i]] = depth
			depth++
		}
	}

	runIDs := util.KeysOfMap(parentRunID)
	slices.SortFunc(runIDs, func(runID1, runID2 int64) int {
		return int(depthByRunID[runID1] - depthByRunID[runID2])
	})

	return runIDs, nil
}
