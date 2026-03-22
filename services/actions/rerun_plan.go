// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"context"

	actions_model "code.gitea.io/gitea/models/actions"
	"code.gitea.io/gitea/modules/container"
	"code.gitea.io/gitea/modules/util"
)

func buildRerunPlan(jobs, targetJobs []*actions_model.ActionRunJob, explicitTargetJobID int64, isRunBlocked bool) *rerunPlan {
	builder := newRerunPlanBuilder(jobs, targetJobs, explicitTargetJobID, isRunBlocked)
	return builder.build()
}

func executeRerunPlan(ctx context.Context, jobs []*actions_model.ActionRunJob, plan *rerunPlan) error {
	for _, job := range jobs {
		if !plan.rerunJobIDs.Contains(job.ID) {
			continue
		}
		shouldBlock, ok := plan.shouldBlock[job.ID]
		if !ok {
			shouldBlock = true
		}
		if err := rerunWorkflowJob(ctx, job, shouldBlock); err != nil {
			return err
		}
	}
	return nil
}

type rerunPlan struct {
	// RerunJobIDs contains the IDs of jobs that should be rerun.
	rerunJobIDs container.Set[int64]

	// ShouldBlock indicates whether a job should be set to StatusBlocked when rerun.
	// If a job ID is not present in this map, it is treated as blocked by default.
	shouldBlock map[int64]bool
}

type rerunPlanBuilder struct {
	targetJobs          []*actions_model.ActionRunJob
	explicitTargetJobID int64
	isRunBlocked        bool

	// jobByID maps job database ID to job model for quick lookup.
	jobByID map[int64]*actions_model.ActionRunJob

	graph *rerunGraph

	rerunIDs          container.Set[int64]
	callerSubtreeIDs  container.Set[int64]
	ancestorCallerIDs container.Set[int64]

	shouldBlockMemo map[int64]bool
}

func newRerunPlanBuilder(jobs, targetJobs []*actions_model.ActionRunJob, explicitTargetJobID int64, isRunBlocked bool) *rerunPlanBuilder {
	jobByID := make(map[int64]*actions_model.ActionRunJob, len(jobs))
	for _, job := range jobs {
		jobByID[job.ID] = job
	}

	return &rerunPlanBuilder{
		targetJobs:          targetJobs,
		explicitTargetJobID: explicitTargetJobID,
		isRunBlocked:        isRunBlocked,
		graph:               newRerunGraph(jobs),
		jobByID:             jobByID,
		rerunIDs:            make(container.Set[int64]),
		callerSubtreeIDs:    make(container.Set[int64]),
		ancestorCallerIDs:   make(container.Set[int64]),
		shouldBlockMemo:     make(map[int64]bool, len(jobs)),
	}
}

func (b *rerunPlanBuilder) addSelectionForSeed(targetJob *actions_model.ActionRunJob) {
	// Always rerun the selected job and all of its downstream jobs within the same scope.
	parentCallJobID := targetJob.ParentCallJobID
	for id := range b.graph.collectDownstreamByParentCallJobID(parentCallJobID, targetJob.JobID) {
		b.rerunIDs.Add(id)
	}

	// If the selected job is inside a reusable call, rerun all ancestor caller jobs (up to root)
	// and their downstream jobs. Ancestor caller jobs are not expanded to their sibling subtrees.
	if targetJob.ParentCallJobID > 0 {
		parentID := targetJob.ParentCallJobID
		for parentID > 0 {
			parentCaller := b.jobByID[parentID]
			if parentCaller == nil {
				break
			}

			b.ancestorCallerIDs.Add(parentCaller.ID)
			b.rerunIDs.Add(parentCaller.ID)

			parentCallJobID := parentCaller.ParentCallJobID
			for id := range b.graph.collectDownstreamByParentCallJobID(parentCallJobID, parentCaller.JobID) {
				b.rerunIDs.Add(id)
			}

			parentID = parentCaller.ParentCallJobID
		}
	}
}

func (b *rerunPlanBuilder) build() *rerunPlan {
	// 1) Seed selection union.
	if len(b.targetJobs) == 0 {
		for id := range b.jobByID {
			b.rerunIDs.Add(id)
		}
	} else {
		for _, targetJob := range b.targetJobs {
			b.addSelectionForSeed(targetJob)
		}
	}

	// 2) Expand reusable call subtrees for caller jobs that are part of this rerun selection,
	// except for ancestor callers (their siblings should not be rerun).
	expandSubtreeCallers := make(container.Set[int64])
	for id := range b.rerunIDs {
		job := b.jobByID[id]
		if job == nil {
			continue
		}
		if job.IsReusableCall && !b.ancestorCallerIDs.Contains(job.ID) {
			expandSubtreeCallers.Add(job.ID)
		}
	}

	// 3) Expand caller subtrees.
	for callerID := range expandSubtreeCallers {
		b.rerunIDs.Add(callerID)
		subtree := b.graph.collectCallerSubtreeJobs(callerID)
		for id := range subtree {
			b.rerunIDs.Add(id)
			b.callerSubtreeIDs.Add(id)
		}
	}

	// 4) Compute initial statuses (Blocked vs Waiting) for all selected jobs and build the plan.
	plan := &rerunPlan{rerunJobIDs: make(container.Set[int64]), shouldBlock: make(map[int64]bool)}

	for id := range b.rerunIDs {
		job := b.jobByID[id]
		if job == nil {
			continue
		}

		shouldBlock := b.shouldBlockByNeedsAndCaller(job.ID)

		if b.explicitTargetJobID > 0 {
			// "Explicit rerun" semantics: rerun the target job and its related jobs, but block everything else by default.
			shouldBlock = true
			if job.IsReusableCall && b.ancestorCallerIDs.Contains(job.ID) {
				shouldBlock = b.isRunBlocked
			} else if job.ID == b.explicitTargetJobID {
				shouldBlock = util.Iif(job.IsReusableCall, true, b.isRunBlocked)
			} else if b.callerSubtreeIDs.Contains(job.ID) {
				shouldBlock = b.shouldBlockByNeedsAndCaller(job.ID)
			}
		} else if job.IsReusableCall && b.ancestorCallerIDs.Contains(job.ID) {
			// Ancestor caller jobs are rerun for status propagation/downstream selection, but their sibling subtrees
			// should not be rerun. Unblock them unless the run is blocked by concurrency.
			shouldBlock = b.isRunBlocked
		}

		plan.rerunJobIDs.Add(job.ID)
		plan.shouldBlock[job.ID] = shouldBlock
	}

	return plan
}

func (b *rerunPlanBuilder) shouldBlockByNeedsAndCaller(jobID int64) bool {
	if b.isRunBlocked {
		return true
	}
	if v, ok := b.shouldBlockMemo[jobID]; ok {
		return v
	}
	job := b.jobByID[jobID]
	if job == nil {
		// Shouldn't happen. Be conservative to avoid running child jobs while their caller can't be resolved.
		b.shouldBlockMemo[jobID] = true
		return true
	}

	// Block if any needed job is not ready.
	// "Ready" means the needed job is not being rerun (so it remains done), and it has succeeded or been skipped.
	for _, need := range job.Needs {
		needJobs := b.graph.jobsByJobIDByParentCallJobID[job.ParentCallJobID][need]
		if len(needJobs) == 0 {
			b.shouldBlockMemo[jobID] = true
			return true
		}

		needJob := needJobs[0]
		if b.rerunIDs.Contains(needJob.ID) {
			b.shouldBlockMemo[jobID] = true
			return true
		}
		if needJob.Status != actions_model.StatusSuccess && needJob.Status != actions_model.StatusSkipped {
			b.shouldBlockMemo[jobID] = true
			return true
		}
	}
	if job.ParentCallJobID > 0 {
		b.shouldBlockMemo[jobID] = b.shouldBlockByNeedsAndCaller(job.ParentCallJobID)
		return b.shouldBlockMemo[jobID]
	}
	b.shouldBlockMemo[jobID] = false
	return false
}

type rerunGraph struct {
	// jobsByJobIDByParentCallJobID groups jobs by ParentCallJobID and workflow JobID.
	// It allows scope-aware selection of jobs by JobID, to avoid conflicts across reusable workflow scopes.
	jobsByJobIDByParentCallJobID map[int64]map[string][]*actions_model.ActionRunJob
	// dependentsByParentCallJobID is the reverse dependency graph within a ParentCallJobID scope:
	// dependentsByParentCallJobID[parentCallJobID][needJobID] lists jobs that declare "needs: [needJobID]".
	dependentsByParentCallJobID map[int64]map[string][]*actions_model.ActionRunJob
	// childrenByCaller groups jobs by their direct reusable workflow caller job (job.ParentCallJobID).
	// It is used to expand a reusable caller job to its child job subtree (including nested reusable calls).
	childrenByCaller map[int64][]*actions_model.ActionRunJob
}

func newRerunGraph(jobs []*actions_model.ActionRunJob) *rerunGraph {
	g := &rerunGraph{
		jobsByJobIDByParentCallJobID: make(map[int64]map[string][]*actions_model.ActionRunJob),
		dependentsByParentCallJobID:  make(map[int64]map[string][]*actions_model.ActionRunJob),
		childrenByCaller:             make(map[int64][]*actions_model.ActionRunJob),
	}

	for _, job := range jobs {
		parentCallJobID := job.ParentCallJobID
		if g.jobsByJobIDByParentCallJobID[parentCallJobID] == nil {
			g.jobsByJobIDByParentCallJobID[parentCallJobID] = make(map[string][]*actions_model.ActionRunJob)
		}
		g.jobsByJobIDByParentCallJobID[parentCallJobID][job.JobID] = append(g.jobsByJobIDByParentCallJobID[parentCallJobID][job.JobID], job)

		if g.dependentsByParentCallJobID[parentCallJobID] == nil {
			g.dependentsByParentCallJobID[parentCallJobID] = make(map[string][]*actions_model.ActionRunJob)
		}
		for _, need := range job.Needs {
			g.dependentsByParentCallJobID[parentCallJobID][need] = append(g.dependentsByParentCallJobID[parentCallJobID][need], job)
		}

		if job.ParentCallJobID > 0 {
			g.childrenByCaller[job.ParentCallJobID] = append(g.childrenByCaller[job.ParentCallJobID], job)
		}
	}

	return g
}

func (g *rerunGraph) collectDownstreamByParentCallJobID(parentCallJobID int64, seedJobID string) container.Set[int64] {
	ret := make(container.Set[int64])
	if seedJobID == "" {
		return ret
	}

	queue := make([]string, 0, 4)
	enqueued := make(container.Set[string])
	if enqueued.Add(seedJobID) {
		queue = append(queue, seedJobID)
	}

	for len(queue) > 0 {
		jobID := queue[0]
		queue = queue[1:]

		for _, v := range g.jobsByJobIDByParentCallJobID[parentCallJobID][jobID] {
			ret.Add(v.ID)
		}

		for _, dependent := range g.dependentsByParentCallJobID[parentCallJobID][jobID] {
			if ret.Add(dependent.ID) && enqueued.Add(dependent.JobID) {
				queue = append(queue, dependent.JobID)
			}
		}
	}

	return ret
}

func (g *rerunGraph) collectCallerSubtreeJobs(callerID int64) container.Set[int64] {
	ret := make(container.Set[int64])
	if callerID <= 0 {
		return ret
	}

	stack := []int64{callerID}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		for _, child := range g.childrenByCaller[id] {
			if !ret.Add(child.ID) {
				continue
			}
			if child.IsReusableCall {
				stack = append(stack, child.ID)
			}
		}
	}

	return ret
}
