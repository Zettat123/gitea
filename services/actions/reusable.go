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
	"code.gitea.io/gitea/modules/actions/jobparser"
	"code.gitea.io/gitea/modules/container"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/convert"

	"go.yaml.in/yaml/v4"
	"xorm.io/builder"
)

// MaxReusableCallDepth is the hard limit on nested reusable workflow calls.
// Caller jobs at depth >= MaxReusableCallDepth cannot be expanded further.
const MaxReusableCallDepth = 10

// callerSpec is the parsed information needed to expand a single caller job.
type callerSpec struct {
	uses        *jobparser.UsesRef
	with        map[string]any
	secretsNode yaml.Node // raw "secrets:" node from caller YAML; may be zero
}

// extractCallerSpec parses a caller job (one with "uses:") and returns its uses ref + with + secrets node.
func extractCallerSpec(job *jobparser.Job) (*callerSpec, error) {
	if job == nil {
		return nil, errors.New("extractCallerSpec called with nil job")
	}
	ref, err := jobparser.ParseUses(job.Uses)
	if err != nil {
		return nil, fmt.Errorf("parse uses %q: %w", job.Uses, err)
	}
	return &callerSpec{
		uses:        ref,
		with:        job.With,
		secretsNode: job.RawSecrets,
	}, nil
}

// readCallerSecretsFromYAML decodes a caller's "secrets:" raw node.
func readCallerSecretsFromYAML(node yaml.Node) (bool, map[string]string, error) {
	if node.IsZero() {
		return false, nil, nil
	}
	if node.Kind == yaml.ScalarNode && strings.TrimSpace(node.Value) == "inherit" {
		return true, nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return false, nil, errors.New("invalid secrets: section, expected mapping or 'inherit'")
	}
	out := make(map[string]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		v := node.Content[i+1]
		var sv string
		if err := v.Decode(&sv); err != nil {
			return false, nil, fmt.Errorf("decode secret %q: %w", k.Value, err)
		}
		out[k.Value] = sv
	}
	return false, out, nil
}

// loadReusableWorkflowSource resolves the workflow file referenced by a caller and returns its raw bytes.
func loadReusableWorkflowSource(ctx context.Context, run *actions_model.ActionRun, ref *jobparser.UsesRef) ([]byte, error) {
	if err := run.LoadAttributes(ctx); err != nil {
		return nil, err
	}

	switch ref.Kind {
	case jobparser.UsesKindLocalSameRepo:
		// Same-repo: pin to the run's commit SHA, no @ref support.
		return readWorkflowFromRepo(ctx, run.Repo, run.CommitSHA, ref.Path)

	case jobparser.UsesKindLocalCrossRepo:
		repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, ref.Owner, ref.Repo)
		if err != nil {
			return nil, fmt.Errorf("look up cross-repo workflow source %q: %w", ref.Owner+"/"+ref.Repo, err)
		}
		ok, err := access_model.CanReadWorkflowCrossRepo(ctx, repo, run)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("no permission to read reusable workflow from %s/%s", ref.Owner, ref.Repo)
		}
		return readWorkflowFromRepo(ctx, repo, ref.Ref, ref.Path)
	}
	return nil, fmt.Errorf("unsupported uses kind %d", ref.Kind)
}

func readWorkflowFromRepo(ctx context.Context, repo *repo_model.Repository, refOrSHA, path string) ([]byte, error) {
	gitRepo, err := gitrepo.OpenRepository(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("open repo %s: %w", repo.FullName(), err)
	}
	defer gitRepo.Close()

	commit, err := gitRepo.GetCommit(refOrSHA)
	if err != nil {
		return nil, fmt.Errorf("get commit %q in %s: %w", refOrSHA, repo.FullName(), err)
	}
	str, err := commit.GetFileContent(path, 1024*1024)
	if err != nil {
		return nil, fmt.Errorf("read %s@%s:%s: %w", repo.FullName(), refOrSHA, path, err)
	}
	return []byte(str), nil
}

// buildWorkflowCallPayload constructs the WorkflowCallPayload that will be exposed to a reusable workflow's child jobs as gitea.event.
func buildWorkflowCallPayload(ctx context.Context, run *actions_model.ActionRun, inputs map[string]any) ([]byte, error) {
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
	return payload.JSONPayload()
}

// ensureCallerChainOK walks `caller`'s ancestor chain (via ParentCallJobID) and:
//   - rejects cycles (caller.CallUses appearing in any ancestor's CallUses)
//   - enforces MaxReusableCallDepth on caller's depth (top-level = 0)
func ensureCallerChainOK(ctx context.Context, caller *actions_model.ActionRunJob) error {
	if caller.ParentCallJobID == 0 {
		return nil // top-level caller: depth 0, no ancestors to walk
	}

	visited := container.Set[string]{}
	visited.Add(caller.CallUses)

	depth := 0
	current := caller
	for current.ParentCallJobID != 0 {
		next, err := actions_model.GetRunJobByRunAndID(ctx, current.RunID, current.ParentCallJobID)
		if err != nil {
			return fmt.Errorf("walk caller chain: %w", err)
		}
		current = next
		depth++
		if depth >= MaxReusableCallDepth {
			return fmt.Errorf("reusable workflow call depth exceeds limit (%d) at %q", MaxReusableCallDepth, caller.CallUses)
		}
		if current.IsReusableCaller && current.CallUses != "" {
			if visited.Contains(current.CallUses) {
				return fmt.Errorf("reusable workflow call cycle detected: %q", current.CallUses)
			}
			visited.Add(current.CallUses)
		}
	}
	return nil
}

// expandReusableCaller writes the called workflow's child rows (and recursively, their reusable descendants) into the same ActionRunAttempt.
func expandReusableCaller(
	ctx context.Context,
	run *actions_model.ActionRun,
	attempt *actions_model.ActionRunAttempt,
	caller *actions_model.ActionRunJob,
	parsedJob *jobparser.Job,
	nextAttemptJobID *int64,
) error {
	// Cycle + depth check via the ParentCallJobID chain.
	if err := ensureCallerChainOK(ctx, caller); err != nil {
		return err
	}

	spec, err := extractCallerSpec(parsedJob)
	if err != nil {
		return err
	}

	// load the reusable workflow content
	content, err := loadReusableWorkflowSource(ctx, run, spec.uses)
	if err != nil {
		return err
	}
	// Parse the called workflow's workflow_call schema for secret-name validation here;
	// inputs/outputs schema will be re-read when caller becomes ready.
	wcSpec, err := jobparser.ParseWorkflowCallSpec(content)
	if err != nil {
		return fmt.Errorf("called workflow %q: %w", parsedJob.Uses, err)
	}

	// Resolve secrets section (inherit / mapping). At expansion time we only validate the
	// schema and remember the structural mapping; values are deferred to PickTask time.
	inherit, secretsMap, err := readCallerSecretsFromYAML(spec.secretsNode)
	if err != nil {
		return fmt.Errorf("caller secrets %q: %w", caller.JobID, err)
	}

	// Validate secrets against the spec — values themselves are not resolved here.
	if !inherit {
		if _, _, err := jobparser.EvaluateWorkflowCallSecrets(wcSpec, false, secretsMap); err != nil {
			return fmt.Errorf("caller secrets %q: %w", caller.JobID, err)
		}
	}

	// Persist caller metadata: source content cached for later steps, secrets mapping;
	// CallUses is already set; inputs payload is deferred to caller-ready.
	switch {
	case inherit:
		caller.CallSecrets = "inherit"
	case len(secretsMap) > 0:
		mapBytes, err := json.Marshal(secretsMap)
		if err != nil {
			return fmt.Errorf("marshal caller secret map: %w", err)
		}
		caller.CallSecrets = string(mapBytes)
	}
	caller.ReusableWorkflowContent = content
	if _, err := actions_model.UpdateRunJob(ctx, caller, nil, "call_secrets", "reusable_workflow_content"); err != nil {
		return fmt.Errorf("update caller job %d: %w", caller.ID, err)
	}

	// Parse called workflow's jobs.
	childWorkflows, err := jobparser.Parse(content)
	if err != nil {
		return fmt.Errorf("parse called workflow %q: %w", parsedJob.Uses, err)
	}

	// Reject empty called workflows: a workflow_call schema with no jobs has no meaningful execution
	if len(childWorkflows) == 0 {
		return fmt.Errorf("called workflow %q has no jobs", parsedJob.Uses)
	}

	// Insert child rows. Children start in StatusBlocked: they are eligible to run only after the caller becomes "ready" (CallPayload is not empty)
	for _, sw := range childWorkflows {
		id, childParsed := sw.Job()
		if childParsed == nil {
			continue
		}
		needs := childParsed.Needs()
		if err := sw.SetJob(id, childParsed.EraseNeeds()); err != nil {
			return err
		}
		payload, err := sw.Marshal()
		if err != nil {
			return fmt.Errorf("marshal child job %q: %w", id, err)
		}

		childParsed.Name = util.EllipsisDisplayString(childParsed.Name, 255)
		child := &actions_model.ActionRunJob{
			RunID:             run.ID,
			RunAttemptID:      attempt.ID,
			RepoID:            run.RepoID,
			OwnerID:           run.OwnerID,
			CommitSHA:         run.CommitSHA,
			IsForkPullRequest: run.IsForkPullRequest,
			Name:              childParsed.Name,
			Attempt:           attempt.Attempt,
			WorkflowPayload:   payload,
			JobID:             id,
			AttemptJobID:      *nextAttemptJobID,
			Needs:             needs,
			RunsOn:            childParsed.RunsOn(),
			Status:            actions_model.StatusBlocked,
			ParentCallJobID:   caller.ID,
		}
		*nextAttemptJobID++
		if perms := ExtractJobPermissionsFromWorkflow(sw, childParsed); perms != nil {
			child.TokenPermissions = perms
		}
		if childParsed.Uses != "" {
			child.IsReusableCaller = true
			child.CallUses = childParsed.Uses
		}
		if err := db.Insert(ctx, child); err != nil {
			return err
		}
		if child.IsReusableCaller {
			if err := expandReusableCaller(ctx, run, attempt, child, childParsed, nextAttemptJobID); err != nil {
				return err
			}
		}
	}

	return nil
}

func triggerCallerReady(ctx context.Context, run *actions_model.ActionRun, attempt *actions_model.ActionRunAttempt, caller *actions_model.ActionRunJob, jobs []*actions_model.ActionRunJob, vars map[string]string) (updatedJobs, cancelledJobs []*actions_model.ActionRunJob, err error) {
	parsedJob, err := caller.ParseJob()
	if err != nil {
		return nil, nil, fmt.Errorf("parse caller job %d: %w", caller.ID, err)
	}

	// Resolve the caller's `with:`
	jobResults, err := findJobNeedsAndFillJobResults(ctx, caller)
	if err != nil {
		return nil, nil, fmt.Errorf("find caller needs: %w", err)
	}
	runInputs, err := getInputsFromRun(run)
	if err != nil {
		return nil, nil, err
	}
	gitCtx := GenerateGiteaContext(ctx, run, attempt, caller)
	inputs, err := jobparser.EvaluateWorkflowCallInput(
		caller.JobID, parsedJob, caller.ReusableWorkflowContent,
		gitCtx, jobResults, vars, runInputs,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("evaluate caller with: %w", err)
	}

	payload, err := buildWorkflowCallPayload(ctx, run, inputs)
	if err != nil {
		return nil, nil, err
	}

	// Generate and persist CallPayload
	caller.CallPayload = string(payload)
	if _, err := actions_model.UpdateRunJob(ctx, caller, nil, "call_payload"); err != nil {
		return nil, nil, fmt.Errorf("update caller job %d: %w", caller.ID, err)
	}

	// Re-render child WorkflowPayloads
	if err := refreshChildPayloads(ctx, run, attempt, caller, jobs, vars, inputs); err != nil {
		return nil, nil, err
	}

	for _, child := range jobs {
		if child.ParentCallJobID != caller.ID || child.Status != actions_model.StatusBlocked {
			continue
		}
		if len(child.Needs) > 0 {
			continue // leave Blocked; job_emitter will check it
		}
		subUpdated, subCancelled, err := updateReusableCallerChild(ctx, run, attempt, child, jobs, vars)
		if err != nil {
			return nil, nil, err
		}
		updatedJobs = append(updatedJobs, subUpdated...)
		cancelledJobs = append(cancelledJobs, subCancelled...)
	}
	return updatedJobs, cancelledJobs, nil
}

func updateReusableCallerChild(ctx context.Context, run *actions_model.ActionRun, attempt *actions_model.ActionRunAttempt, child *actions_model.ActionRunJob, jobs []*actions_model.ActionRunJob, vars map[string]string) (updatedJobs, cancelledJobs []*actions_model.ActionRunJob, err error) {
	shouldStart, err := evaluateJobIf(ctx, run, attempt, child, vars, true)
	if err != nil {
		log.Debug("evaluate child if %d: %v — leaving Blocked", child.ID, err)
		return nil, nil, nil
	}

	if !shouldStart {
		if child.IsReusableCaller {
			if err := skipCallerSubtree(ctx, child, jobs); err != nil {
				return nil, nil, err
			}
			return nil, nil, nil
		}
		child.Status = actions_model.StatusSkipped
		if _, err := actions_model.UpdateRunJob(ctx, child, builder.Eq{"status": actions_model.StatusBlocked}, "status"); err != nil {
			return nil, nil, fmt.Errorf("skip child %d: %w", child.ID, err)
		}
		return []*actions_model.ActionRunJob{child}, nil, nil
	}

	if child.IsReusableCaller {
		subUpdated, subCancelled, err := triggerCallerReady(ctx, run, attempt, child, jobs, vars)
		if err != nil {
			return nil, nil, err
		}
		return subUpdated, subCancelled, nil
	}

	if child.RawConcurrency != "" {
		if err := EvaluateJobConcurrencyFillModel(ctx, run, attempt, child, vars, nil); err != nil {
			log.Debug("evaluate child concurrency %d: %v — leaving Blocked", child.ID, err)
			return nil, nil, nil
		}
		if _, err := actions_model.UpdateRunJob(ctx, child, nil, "concurrency_group", "concurrency_cancel", "is_concurrency_evaluated"); err != nil {
			return nil, nil, fmt.Errorf("update child concurrency %d: %w", child.ID, err)
		}
	}
	newStatus, cancelled, err := PrepareToStartJobWithConcurrency(ctx, child)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare child %d: %w", child.ID, err)
	}
	if newStatus == actions_model.StatusBlocked {
		return nil, cancelled, nil
	}
	child.Status = newStatus
	if _, err := actions_model.UpdateRunJob(ctx, child, builder.Eq{"status": actions_model.StatusBlocked}, "status"); err != nil {
		return nil, nil, fmt.Errorf("update child status %d: %w", child.ID, err)
	}
	return []*actions_model.ActionRunJob{child}, cancelled, nil
}

// refreshChildPayloads re-parses the called workflow with the resolved inputs
func refreshChildPayloads(ctx context.Context, run *actions_model.ActionRun, attempt *actions_model.ActionRunAttempt, caller *actions_model.ActionRunJob, allJobs []*actions_model.ActionRunJob, vars map[string]string, inputs map[string]any) error {
	directChildren := make([]*actions_model.ActionRunJob, 0)
	for _, j := range allJobs {
		if j.ParentCallJobID == caller.ID {
			directChildren = append(directChildren, j)
		}
	}
	if len(directChildren) == 0 {
		return nil
	}
	if len(caller.ReusableWorkflowContent) == 0 {
		return fmt.Errorf("caller %d has no cached source content", caller.ID)
	}

	// Use a direct child (not the caller) for gitCtx
	gitCtx := GenerateGiteaContext(ctx, run, attempt, directChildren[0])
	parsed, err := jobparser.Parse(caller.ReusableWorkflowContent,
		jobparser.WithVars(vars),
		jobparser.WithGitContext(gitCtx.ToGitHubContext()),
		jobparser.WithInputs(inputs),
	)
	if err != nil {
		return fmt.Errorf("re-parse called workflow for caller %d: %w", caller.ID, err)
	}
	parsedByID := make(map[string]*jobparser.SingleWorkflow, len(parsed))
	for _, sw := range parsed {
		id, _ := sw.Job()
		parsedByID[id] = sw
	}
	for _, child := range directChildren {
		sw, ok := parsedByID[child.JobID]
		if !ok {
			log.Warn("refreshChildPayloads: child %d (job %q) missing from re-parse of caller %d's workflow — leaving payload unresolved", child.ID, child.JobID, caller.ID)
			continue
		}
		_, parsedChild := sw.Job()
		if parsedChild == nil {
			log.Warn("refreshChildPayloads: child %d (job %q) parsed to nil — leaving payload unresolved", child.ID, child.JobID)
			continue
		}
		if err := sw.SetJob(child.JobID, parsedChild.EraseNeeds()); err != nil {
			return err
		}
		payload, err := sw.Marshal()
		if err != nil {
			return err
		}
		child.WorkflowPayload = payload
		child.Name = util.EllipsisDisplayString(parsedChild.Name, 255)
		child.RunsOn = parsedChild.RunsOn()
		if _, err := actions_model.UpdateRunJob(ctx, child, nil, "workflow_payload", "name", "runs_on"); err != nil {
			return fmt.Errorf("update child job %d: %w", child.ID, err)
		}
	}
	return nil
}

// skipCallerSubtree marks every descendant of the caller (recursively) as Skipped.
func skipCallerSubtree(ctx context.Context, caller *actions_model.ActionRunJob, allJobs []*actions_model.ActionRunJob) error {
	for _, j := range actions_model.CollectReusableCallerAllChildJobs(caller, allJobs) {
		j.Status = actions_model.StatusSkipped
		if _, err := actions_model.UpdateRunJob(ctx, j, nil, "status"); err != nil {
			return err
		}
	}
	return nil
}
