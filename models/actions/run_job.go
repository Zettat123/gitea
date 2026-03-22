// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"context"
	"fmt"
	"slices"
	"time"

	"code.gitea.io/gitea/models/db"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/actions/jobparser"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"

	"xorm.io/builder"
)

// ActionRunJob represents a job of a run
type ActionRunJob struct {
	ID                int64
	RunID             int64                  `xorm:"index"`
	Run               *ActionRun             `xorm:"-"`
	RepoID            int64                  `xorm:"index(repo_concurrency)"`
	Repo              *repo_model.Repository `xorm:"-"`
	OwnerID           int64                  `xorm:"index"`
	CommitSHA         string                 `xorm:"index"`
	IsForkPullRequest bool
	Name              string `xorm:"VARCHAR(255)"`
	Attempt           int64

	// WorkflowPayload is act/jobparser.SingleWorkflow for act/jobparser.Parse
	// it should contain exactly one job with global workflow fields for this model
	WorkflowPayload []byte

	JobID  string   `xorm:"VARCHAR(255)"` // job id in workflow, not job's id
	Needs  []string `xorm:"JSON TEXT"`
	RunsOn []string `xorm:"JSON TEXT"`
	TaskID int64    // the latest task of the job
	Status Status   `xorm:"index"`

	RawConcurrency string // raw concurrency from job YAML's "concurrency" section

	// IsConcurrencyEvaluated is only valid/needed when this job's RawConcurrency is not empty.
	// If RawConcurrency can't be evaluated (e.g. depend on other job's outputs or have errors), this field will be false.
	// If RawConcurrency has been successfully evaluated, this field will be true, ConcurrencyGroup and ConcurrencyCancel are also set.
	IsConcurrencyEvaluated bool

	ConcurrencyGroup  string `xorm:"index(repo_concurrency) NOT NULL DEFAULT ''"` // evaluated concurrency.group
	ConcurrencyCancel bool   `xorm:"NOT NULL DEFAULT FALSE"`                      // evaluated concurrency.cancel-in-progress

	// TokenPermissions stores the explicit permissions from workflow/job YAML (no org/repo clamps applied).
	// Org/repo clamps are enforced when the token is used at runtime.
	// It is JSON-encoded repo_model.ActionsTokenPermissions and may be empty if not specified.
	TokenPermissions *repo_model.ActionsTokenPermissions `xorm:"JSON TEXT"`

	// IsReusableCall indicates this job is a reusable workflow caller job ("uses: ./.gitea/workflows/*.yml@...").
	// It doesn't run on a runner, but groups the reusable workflow's expanded jobs.
	IsReusableCall bool `xorm:"index NOT NULL DEFAULT FALSE"`
	// ReusableWorkflowUses stores the raw "uses" value of a reusable workflow caller job.
	// It should only be set for reusable workflow caller jobs (IsReusableCall == true).
	ReusableWorkflowUses string `xorm:"VARCHAR(255)"`
	// ParentCallJobID indicates this job belongs to a reusable workflow caller job.
	// It's the ID of the parent ActionRunJob (the caller job). 0 means this job is not a child job of a reusable call.
	ParentCallJobID int64 `xorm:"index NOT NULL DEFAULT 0"`
	// RootCallJobID indicates the outermost reusable workflow caller job this job belongs to.
	// It's the ID of the root caller ActionRunJob. 0 means this job is not inside a reusable call.
	RootCallJobID int64 `xorm:"index NOT NULL DEFAULT 0"`
	// CallDepth is the nested depth of reusable workflow calls.
	// 0 means this job is in the root workflow (including caller jobs). Child jobs have depth >= 1.
	CallDepth int `xorm:"index NOT NULL DEFAULT 0"`
	// CallEventPayload is the event payload for reusable workflow call.
	// It should only be set for reusable workflow caller jobs (IsReusableCall == true).
	CallEventPayload string `xorm:"LONGTEXT"`
	// CallSecretsInherit indicates the caller job uses "secrets: inherit" when calling a reusable workflow.
	// It should only be set for reusable workflow caller jobs (IsReusableCall == true).
	CallSecretsInherit bool `xorm:"NOT NULL DEFAULT FALSE"`
	// CallSecretNames stores the reusable workflow call secrets mapping, encoded as JSON.
	// Key is the secret name expected by the called workflow (declared in "on.workflow_call.secrets"),
	// value is the secret name referenced from the caller workflow ("${{ secrets.NAME }}"), e.g. {"parent_token":"mysecret"}.
	// It should only be set for reusable workflow caller jobs (IsReusableCall == true).
	CallSecretNames string `xorm:"LONGTEXT"`

	Started timeutil.TimeStamp
	Stopped timeutil.TimeStamp
	Created timeutil.TimeStamp `xorm:"created"`
	Updated timeutil.TimeStamp `xorm:"updated index"`
}

func init() {
	db.RegisterModel(new(ActionRunJob))
}

func (job *ActionRunJob) Duration() time.Duration {
	return calculateDuration(job.Started, job.Stopped, job.Status)
}

func (job *ActionRunJob) LoadRun(ctx context.Context) error {
	if job.Run == nil {
		run, err := GetRunByRepoAndID(ctx, job.RepoID, job.RunID)
		if err != nil {
			return err
		}
		job.Run = run
	}
	return nil
}

func (job *ActionRunJob) LoadRepo(ctx context.Context) error {
	if job.Repo == nil {
		repo, err := repo_model.GetRepositoryByID(ctx, job.RepoID)
		if err != nil {
			return err
		}
		job.Repo = repo
	}
	return nil
}

// LoadAttributes load Run if not loaded
func (job *ActionRunJob) LoadAttributes(ctx context.Context) error {
	if job == nil {
		return nil
	}

	if err := job.LoadRun(ctx); err != nil {
		return err
	}

	return job.Run.LoadAttributes(ctx)
}

// ParseJob parses the job structure from the ActionRunJob.WorkflowPayload
func (job *ActionRunJob) ParseJob() (*jobparser.Job, error) {
	// job.WorkflowPayload is a SingleWorkflow created from an ActionRun's workflow, which exactly contains this job's YAML definition.
	// Ideally it shouldn't be called "Workflow", it is just a job with global workflow fields + trigger
	parsedWorkflows, err := jobparser.Parse(job.WorkflowPayload)
	if err != nil {
		return nil, fmt.Errorf("job %d single workflow: unable to parse: %w", job.ID, err)
	} else if len(parsedWorkflows) != 1 {
		return nil, fmt.Errorf("job %d single workflow: not single workflow", job.ID)
	}
	_, workflowJob := parsedWorkflows[0].Job()
	if workflowJob == nil {
		// it shouldn't happen, and since the callers don't check nil, so return an error instead of nil
		return nil, util.ErrorWrap(util.ErrNotExist, "job %d single workflow: payload doesn't contain a job", job.ID)
	}
	return workflowJob, nil
}

func GetRunJobByRepoAndID(ctx context.Context, repoID, jobID int64) (*ActionRunJob, error) {
	var job ActionRunJob
	has, err := db.GetEngine(ctx).Where("id=? AND repo_id=?", jobID, repoID).Get(&job)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, fmt.Errorf("run job with id %d: %w", jobID, util.ErrNotExist)
	}

	return &job, nil
}

func GetRunJobByRunAndID(ctx context.Context, runID, jobID int64) (*ActionRunJob, error) {
	var job ActionRunJob
	has, err := db.GetEngine(ctx).Where("id=? AND run_id=?", jobID, runID).Get(&job)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, fmt.Errorf("run job with id %d: %w", jobID, util.ErrNotExist)
	}

	return &job, nil
}

func GetRunJobsByRunID(ctx context.Context, runID int64) (ActionJobList, error) {
	var jobs []*ActionRunJob
	if err := db.GetEngine(ctx).Where("run_id=?", runID).OrderBy("id").Find(&jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

func GetReusableCallerChildJobs(ctx context.Context, callerJob *ActionRunJob) (ActionJobList, error) {
	if callerJob == nil || callerJob.ID <= 0 || callerJob.RunID <= 0 || callerJob.RepoID <= 0 {
		return nil, util.NewInvalidArgumentErrorf("invalid caller job")
	}
	var jobs []*ActionRunJob
	if err := db.GetEngine(ctx).
		Where(builder.Eq{
			"run_id":             callerJob.RunID,
			"repo_id":            callerJob.RepoID,
			"parent_call_job_id": callerJob.ID,
		}).
		OrderBy("id").
		Find(&jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

func UpdateRunJob(ctx context.Context, job *ActionRunJob, cond builder.Cond, cols ...string) (int64, error) {
	e := db.GetEngine(ctx)

	sess := e.ID(job.ID)
	if len(cols) > 0 {
		sess.Cols(cols...)
	}

	if cond != nil {
		sess.Where(cond)
	}

	affected, err := sess.Update(job)
	if err != nil {
		return 0, err
	}

	if affected == 0 || (!slices.Contains(cols, "status") && job.Status == 0) {
		return affected, nil
	}

	if slices.Contains(cols, "status") && job.Status.IsWaiting() {
		// if the status of job changes to waiting again, increase tasks version.
		if err := IncreaseTaskVersion(ctx, job.OwnerID, job.RepoID); err != nil {
			return 0, err
		}
	}

	if job.RunID == 0 {
		var err error
		if job, err = GetRunJobByRepoAndID(ctx, job.RepoID, job.ID); err != nil {
			return 0, err
		}
	}

	{
		// Other goroutines may aggregate the status of the run and update it too.
		// So we need load the run and its jobs before updating the run.
		run, err := GetRunByRepoAndID(ctx, job.RepoID, job.RunID)
		if err != nil {
			return 0, err
		}
		jobs, err := GetRunJobsByRunID(ctx, job.RunID)
		if err != nil {
			return 0, err
		}
		run.Status = AggregateJobStatus(jobs)
		if run.Started.IsZero() && run.Status.IsRunning() {
			run.Started = timeutil.TimeStampNow()
		}
		if run.Stopped.IsZero() && run.Status.IsDone() {
			run.Stopped = timeutil.TimeStampNow()
		}
		if err := UpdateRun(ctx, run, "status", "started", "stopped"); err != nil {
			return 0, fmt.Errorf("update run %d: %w", run.ID, err)
		}
	}

	return affected, nil
}

func AggregateJobStatus(jobs []*ActionRunJob) Status {
	allSuccessOrSkipped := len(jobs) != 0
	allSkipped := len(jobs) != 0
	var hasFailure, hasCancelled, hasWaiting, hasRunning, hasBlocked bool
	for _, job := range jobs {
		allSuccessOrSkipped = allSuccessOrSkipped && (job.Status == StatusSuccess || job.Status == StatusSkipped)
		allSkipped = allSkipped && job.Status == StatusSkipped
		hasFailure = hasFailure || job.Status == StatusFailure
		hasCancelled = hasCancelled || job.Status == StatusCancelled
		hasWaiting = hasWaiting || job.Status == StatusWaiting
		hasRunning = hasRunning || job.Status == StatusRunning
		hasBlocked = hasBlocked || job.Status == StatusBlocked
	}
	switch {
	case allSkipped:
		return StatusSkipped
	case allSuccessOrSkipped:
		return StatusSuccess
	case hasCancelled:
		return StatusCancelled
	case hasRunning:
		return StatusRunning
	case hasWaiting:
		return StatusWaiting
	case hasFailure:
		return StatusFailure
	case hasBlocked:
		return StatusBlocked
	default:
		return StatusUnknown // it shouldn't happen
	}
}

func CancelPreviousJobsByJobConcurrency(ctx context.Context, job *ActionRunJob) (jobsToCancel []*ActionRunJob, _ error) {
	if job.RawConcurrency == "" {
		return nil, nil
	}
	if !job.IsConcurrencyEvaluated {
		return nil, nil
	}
	if job.ConcurrencyGroup == "" {
		return nil, nil
	}

	statusFindOption := []Status{StatusWaiting, StatusBlocked}
	if job.ConcurrencyCancel {
		statusFindOption = append(statusFindOption, StatusRunning)
	}
	runs, jobs, err := GetConcurrentRunsAndJobs(ctx, job.RepoID, job.ConcurrencyGroup, statusFindOption)
	if err != nil {
		return nil, fmt.Errorf("find concurrent runs and jobs: %w", err)
	}
	jobs = slices.DeleteFunc(jobs, func(j *ActionRunJob) bool { return j.ID == job.ID })
	jobsToCancel = append(jobsToCancel, jobs...)

	// cancel runs in the same concurrency group
	for _, run := range runs {
		jobs, err := db.Find[ActionRunJob](ctx, FindRunJobOptions{
			RunID: run.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("find run %d jobs: %w", run.ID, err)
		}
		jobsToCancel = append(jobsToCancel, jobs...)
	}

	return CancelJobs(ctx, jobsToCancel)
}
