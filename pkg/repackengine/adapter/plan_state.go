/*
Copyright 2026 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package adapter

import (
	"k8s.io/apimachinery/pkg/util/sets"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// PlanStateSnapshot captures the job-side plan state at one point: every
// rewritten task binding (with its pre-rewrite original) and every gang
// AllocatedHyperNode anchor. Restore returns the session exactly to that point.
type PlanStateSnapshot struct {
	rewritten []planStateTask
	anchors   []planStateAnchor
}

// planStateTask records one job-side task binding rewrite for symmetric rollback.
type planStateTask struct {
	jobID    schedapi.JobID
	subJobID schedapi.SubJobID // "" when the task is not in any subJob index
	status   schedapi.TaskStatus
	taskID   schedapi.TaskID
	original *schedapi.TaskInfo // pre-rewrite (real) task; the job side points at its clone
}

// planStateAnchor records a gang AllocatedHyperNode value at snapshot time.
type planStateAnchor struct {
	jobID    schedapi.JobID
	subJobID schedapi.SubJobID // "" = job-level anchor
	value    string
}

// PlanStateCarrier mutates the JOB side of a scheduler Session for one repack
// pass: it rewrites committed moves' task placements into ssn.Jobs and
// recomputes gang AllocatedHyperNode anchors. The NODE side is never touched,
// so the planner reads real capacity while gradients read plan state. Save/
// Restore brackets a trial for symmetric rollback; the baseline advances only
// on ApplyCommit.
type PlanStateCarrier interface {
	// Save captures the job-side plan state for later Restore.
	Save() PlanStateSnapshot
	// Restore returns the session to a previously captured state: tasks rewritten
	// after the snapshot are swapped back and anchors restored — symmetric, so a
	// failed trial leaves no residue.
	Restore(PlanStateSnapshot)
	// ApplyCommit rewrites the moves onto the job side and recomputes gang anchors
	// via the scheduler's SyncJobAllocatedHyperNode. To=="" or To==From moves are
	// ignored. The caller re-Saves to advance the rollback baseline.
	ApplyCommit(moves []*api.Move)
	// ClearGangAnchor temporarily clears a gang's AllocatedHyperNode for the
	// no-anchor evaluation of a fully-vacated unit (subJobID == "" = job anchor).
	// The caller must bracket it with Save/Restore.
	ClearGangAnchor(jobID schedapi.JobID, subJobID schedapi.SubJobID)
	// JobAllocatedHyperNode returns the current plan-state job anchor.
	JobAllocatedHyperNode(jobID schedapi.JobID) string
	// SubJobAllocatedHyperNode returns the current plan-state subJob anchor.
	SubJobAllocatedHyperNode(jobID schedapi.JobID, subJobID schedapi.SubJobID) string
}

// SessionPlanState is the live-session implementation of PlanStateCarrier. It
// rewrites committed moves' task placements into ssn.Jobs (job + subJob maps
// point at task clones carrying the plan-state NodeName) and recomputes gang
// anchors via the scheduler's SyncJobAllocatedHyperNode — which writes only the
// job/subJob AllocatedHyperNode fields, so Save/Restore of those is faithful.
type SessionPlanState struct {
	ssn *schedframework.Session
	// rewritten tracks every task whose job-side binding has been rewritten this
	// pass, taskID -> the pre-rewrite original (for Restore). At most once per pass.
	rewritten map[schedapi.TaskID]planStateTask
}

var _ PlanStateCarrier = (*SessionPlanState)(nil)

// NewSessionPlanState builds a plan-state carrier over a live scheduler session.
func NewSessionPlanState(ssn *schedframework.Session) *SessionPlanState {
	return &SessionPlanState{ssn: ssn, rewritten: map[schedapi.TaskID]planStateTask{}}
}

// Save captures the current job-side plan state.
func (p *SessionPlanState) Save() PlanStateSnapshot {
	snap := PlanStateSnapshot{}
	for taskID, entry := range p.rewritten {
		snap.rewritten = append(snap.rewritten, planStateTask{
			jobID: entry.jobID, subJobID: entry.subJobID,
			status: entry.status, taskID: taskID, original: entry.original,
		})
	}
	for jobID, job := range p.ssn.Jobs {
		if job == nil {
			continue
		}
		snap.anchors = append(snap.anchors, planStateAnchor{jobID: jobID, value: job.AllocatedHyperNode})
		for subJobID, subJob := range job.SubJobs {
			if subJob != nil {
				snap.anchors = append(snap.anchors, planStateAnchor{jobID: jobID, subJobID: subJobID, value: subJob.AllocatedHyperNode})
			}
		}
	}
	return snap
}

// Restore returns the session to a previously saved state: tasks rewritten
// after the snapshot are swapped back to their originals (rewrites accumulate,
// so the snapshot set is always a subset), anchors restored from the snapshot.
func (p *SessionPlanState) Restore(s PlanStateSnapshot) {
	snapshotTasks := sets.New[schedapi.TaskID]()
	for _, e := range s.rewritten {
		snapshotTasks.Insert(e.taskID)
	}
	for taskID, entry := range p.rewritten {
		if snapshotTasks.Has(taskID) {
			continue
		}
		p.restoreTask(taskID, entry)
		delete(p.rewritten, taskID)
	}
	for _, a := range s.anchors {
		job := p.ssn.Jobs[a.jobID]
		if job == nil {
			continue
		}
		if a.subJobID == "" {
			job.AllocatedHyperNode = a.value
		} else if subJob := job.SubJobs[a.subJobID]; subJob != nil {
			subJob.AllocatedHyperNode = a.value
		}
	}
}

// ApplyCommit rewrites the moves onto the job side and recomputes gang anchors.
func (p *SessionPlanState) ApplyCommit(moves []*api.Move) {
	affectedJobs := sets.New[schedapi.JobID]()
	for _, move := range moves {
		if move == nil || move.Task == nil || move.To == "" || move.To == move.From {
			continue
		}
		job := p.ssn.Jobs[move.Task.Job]
		if job == nil {
			continue
		}
		p.rewriteTask(job, move)
		affectedJobs.Insert(move.Task.Job)
	}
	for _, jobID := range affectedJobs.UnsortedList() {
		if job := p.ssn.Jobs[jobID]; job != nil {
			schedapi.SyncJobAllocatedHyperNode(job, p.ssn.HyperNodes, p.ssn.RealNodesSet)
		}
	}
}

// rewriteTask replaces the job-side references of a task with a clone carrying
// the plan-state NodeName, leaving the node-side pointer untouched. The
// original is retained for symmetric rollback.
func (p *SessionPlanState) rewriteTask(job *schedapi.JobInfo, move *api.Move) {
	task := move.Task
	if _, already := p.rewritten[task.UID]; already {
		return // a task is committed at most once per pass; defensive
	}
	subJobID := job.TaskToSubJob[task.UID]
	clone := cloneTaskWithNode(task, move.To)
	job.Tasks[task.UID] = clone
	if m, ok := job.TaskStatusIndex[task.Status]; ok {
		m[task.UID] = clone
	}
	if subJobID != "" {
		if subJob := job.SubJobs[subJobID]; subJob != nil {
			subJob.Tasks[task.UID] = clone
			if m, ok := subJob.TaskStatusIndex[task.Status]; ok {
				m[task.UID] = clone
			}
		}
	}
	p.rewritten[task.UID] = planStateTask{
		jobID: job.UID, subJobID: subJobID, status: task.Status,
		taskID: task.UID, original: task,
	}
}

// restoreTask swaps a rewritten job-side task back to its pre-rewrite original.
func (p *SessionPlanState) restoreTask(taskID schedapi.TaskID, e planStateTask) {
	job := p.ssn.Jobs[e.jobID]
	if job == nil {
		return
	}
	job.Tasks[taskID] = e.original
	if m, ok := job.TaskStatusIndex[e.status]; ok {
		m[taskID] = e.original
	}
	if e.subJobID != "" {
		if subJob := job.SubJobs[e.subJobID]; subJob != nil {
			subJob.Tasks[taskID] = e.original
			if m, ok := subJob.TaskStatusIndex[e.status]; ok {
				m[taskID] = e.original
			}
		}
	}
}

// ClearGangAnchor temporarily clears a gang's AllocatedHyperNode for no-anchor
// evaluation. subJobID == "" clears the job anchor. Bracket with Save/Restore.
func (p *SessionPlanState) ClearGangAnchor(jobID schedapi.JobID, subJobID schedapi.SubJobID) {
	job := p.ssn.Jobs[jobID]
	if job == nil {
		return
	}
	if subJobID == "" {
		job.AllocatedHyperNode = ""
	} else if subJob := job.SubJobs[subJobID]; subJob != nil {
		subJob.AllocatedHyperNode = ""
	}
}

// JobAllocatedHyperNode returns the current plan-state job anchor.
func (p *SessionPlanState) JobAllocatedHyperNode(jobID schedapi.JobID) string {
	if job := p.ssn.Jobs[jobID]; job != nil {
		return job.AllocatedHyperNode
	}
	return ""
}

// SubJobAllocatedHyperNode returns the current plan-state subJob anchor.
func (p *SessionPlanState) SubJobAllocatedHyperNode(jobID schedapi.JobID, subJobID schedapi.SubJobID) string {
	if job := p.ssn.Jobs[jobID]; job != nil {
		if subJob := job.SubJobs[subJobID]; subJob != nil {
			return subJob.AllocatedHyperNode
		}
	}
	return ""
}

// cloneTaskWithNode returns a clone of task carrying nodeName as its binding.
// The clone keeps the task identity so the job-side maps keep indexing it; the
// original task (the node-side view) is untouched.
func cloneTaskWithNode(task *schedapi.TaskInfo, nodeName string) *schedapi.TaskInfo {
	t := task.Clone()
	t.NodeName = nodeName
	if t.Pod != nil {
		p := t.Pod.DeepCopy()
		p.Spec.NodeName = nodeName
		t.Pod = p
	}
	return t
}
