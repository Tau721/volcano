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

// Package adapter isolates scheduler-framework details from Repack planning:
// it adapts a live volcano-scheduler Session into the framework's Snapshot
// abstraction, keeping api/ and framework/ independently testable.
package adapter

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	fwk "k8s.io/kube-scheduler/framework"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
)

// SessionSnapshot adapts a live scheduler Session to framework.Snapshot (the
// in-scheduler-cache implementation). It applies the scope.nodes filter so the
// planner only ever sees in-scope drain candidates.
type SessionSnapshot struct {
	ssn      *schedframework.Session
	resource v1.ResourceName
	scope    *enginescope.Matcher // nil = all nodes in scope
	plan     PlanStateCarrier     // job-side plan state for hypernode constraint evaluation
}

var _ framework.Snapshot = (*SessionSnapshot)(nil)

// NewSessionSnapshot wraps a Session for the given target resource. scope gates
// drain targets (nil = all in scope), not the receiver set. plan is the
// plan-state carrier for hypernode constraint evaluation; nil uses the
// live-session implementation (injectable for tests).
func NewSessionSnapshot(ssn *schedframework.Session, resource v1.ResourceName, scope *enginescope.Matcher) *SessionSnapshot {
	return &SessionSnapshot{ssn: ssn, resource: resource, scope: scope, plan: NewSessionPlanState(ssn)}
}

// Nodes returns ALL session nodes (the receiver universe). scope.nodes gates
// drain targets via NodeInScope, not the receiver set.
func (s *SessionSnapshot) Nodes() []*schedapi.NodeInfo {
	out := make([]*schedapi.NodeInfo, 0, len(s.ssn.Nodes))
	for _, n := range s.ssn.Nodes {
		if n != nil {
			out = append(out, n)
		}
	}
	return out
}

// NodeInScope reports whether a node may be a drain target (nil scope = all).
func (s *SessionSnapshot) NodeInScope(n *schedapi.NodeInfo) bool {
	return s.scope == nil || s.scope.NodeInScope(n)
}

// HyperNodesSetByTier is a thin pass-through of the scheduler Session's
// HyperNode tier topology: tier -> set of HyperNode names. Each inner set is
// cloned so the Snapshot stays read-only.
func (s *SessionSnapshot) HyperNodesSetByTier() map[int]sets.Set[string] {
	out := make(map[int]sets.Set[string], len(s.ssn.HyperNodesSetByTier))
	for tier, row := range s.ssn.HyperNodesSetByTier {
		out[tier] = row.Clone()
	}
	return out
}

// RealNodesSet is a thin pass-through of the scheduler Session's HyperNode
// membership: HyperNode name -> set of real node names under it. Inner sets are
// cloned, keeping the Snapshot read-only.
func (s *SessionSnapshot) RealNodesSet() map[string]sets.Set[string] {
	out := make(map[string]sets.Set[string], len(s.ssn.RealNodesSet))
	for name, row := range s.ssn.RealNodesSet {
		out[name] = row.Clone()
	}
	return out
}

// HyperNodeTierNameMap is a thin pass-through of the scheduler Session's
// tierName -> tier index (map copied so callers cannot alias session storage).
func (s *SessionSnapshot) HyperNodeTierNameMap() map[string]int {
	out := make(map[string]int, len(s.ssn.HyperNodeTierNameMap))
	for name, tier := range s.ssn.HyperNodeTierNameMap {
		out[name] = tier
	}
	return out
}

// FeasibleRelocation simulates evicting `victims` and relocating them onto
// `receivers`, on clones only — never mutating the shared session. Feasibility
// uses the full filter stack (SimulatePredicateFn); `committed` are earlier
// moves this pass, their pods counted as present so capacity/topology stay
// consistent; receivers are tried in the given order (first fit wins).
//
// Dual-mode: victims of gangs hitting RequiresHyperNodeAllocate land within a
// single allowed HyperNode domain (tier ascending, first fit); other victims
// keep per-victim greedy placement. A failing gang reverts the whole unit's
// plan-state commits (mixed gang atomicity). Returns the placements and whether
// every victim fit.
func (s *SessionSnapshot) FeasibleRelocation(ctx context.Context, committed []*api.Move, victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo) ([]*api.Move, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	// Reconcile plan state with committed moves (normally a no-op: the previous
	// call applied them; ==false moves never enter plan state).
	s.applyCommittedToPlanState(committed)
	// Baseline: a failing gang trial later reverts plan state to exactly this point.
	baseline := s.planState().Save()

	// Pods already placed on each receiver in this pass (prior committed moves).
	tasksPlacedByNode := map[string][]*schedapi.TaskInfo{}
	sourceTasksToRemove := make([]*schedapi.TaskInfo, 0, len(committed)+len(victims))
	for _, committedMove := range committed {
		if committedMove != nil && committedMove.Task != nil {
			tasksPlacedByNode[committedMove.To] = append(tasksPlacedByNode[committedMove.To], committedMove.Task)
			sourceTasksToRemove = append(sourceTasksToRemove, committedMove.Task)
		}
	}
	sourceTasksToRemove = append(sourceTasksToRemove, victims...)

	relocationMoves := make([]*api.Move, 0, len(victims))

	// ==true gang units first, committed to plan state before the next is tried;
	// the serial commit narrows a Required-affinity-linked peer to the settled
	// domain, keeping co-migrating members co-located. Others follow greedily.
	units := s.groupVictimsByGang(victims)
	for _, unit := range units {
		if !unit.requiresHyperNodeAllocate(s) {
			continue
		}
		moves, fit := s.domainTrialRelocation(ctx, unit, sourceTasksToRemove, receivers, tasksPlacedByNode)
		if !fit {
			s.planState().Restore(baseline)
			return nil, false
		}
		s.planState().ApplyCommit(moves)
		relocationMoves = append(relocationMoves, moves...)
	}

	var greedyVictims []*schedapi.TaskInfo
	for _, victim := range victims {
		job := s.ssn.Jobs[victim.Job]
		if job != nil && job.RequiresHyperNodeAllocate() && s.hasHyperNodeTopology() {
			continue
		}
		greedyVictims = append(greedyVictims, victim)
	}
	if len(greedyVictims) > 0 {
		moves, fit := s.greedyRelocation(ctx, greedyVictims, sourceTasksToRemove, receivers, tasksPlacedByNode)
		if !fit {
			s.planState().Restore(baseline)
			return nil, false
		}
		relocationMoves = append(relocationMoves, moves...)
	}
	return relocationMoves, true
}

// applyCommittedToPlanState applies the ==true moves among committed to plan
// state — a safety net: prior calls normally already applied them, and ==false
// moves never enter plan state.
func (s *SessionSnapshot) applyCommittedToPlanState(committed []*api.Move) {
	var planMoves []*api.Move
	for _, m := range committed {
		if m == nil || m.Task == nil {
			continue
		}
		job := s.ssn.Jobs[m.Task.Job]
		if job != nil && job.RequiresHyperNodeAllocate() && s.hasHyperNodeTopology() {
			planMoves = append(planMoves, m)
		}
	}
	if len(planMoves) > 0 {
		s.planState().ApplyCommit(planMoves)
	}
}

// planState lazily returns the plan-state carrier, defaulting to the live
// session implementation when none was injected.
func (s *SessionSnapshot) planState() PlanStateCarrier {
	if s.plan == nil {
		s.plan = NewSessionPlanState(s.ssn)
	}
	return s.plan
}

func (s *SessionSnapshot) buildRelocationCycleState(ctx context.Context, victim *schedapi.TaskInfo, sourceTasksToRemove []*schedapi.TaskInfo, tasksPlacedByNode map[string][]*schedapi.TaskInfo) (fwk.CycleState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.ssn.PrePredicateFn(victim); err != nil {
		return nil, err
	}
	state := s.ssn.GetCycleState(victim.UID).Clone()
	// Clone each source node once: several victims may leave it, and cloning per
	// task is expensive and would make simulation hooks see stale co-located pods.
	sourceNodeCopies := make(map[string]*schedapi.NodeInfo)
	for _, task := range sourceTasksToRemove {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if task == nil || task.NodeName == "" {
			continue
		}
		sourceNodeCopy := sourceNodeCopies[task.NodeName]
		if sourceNodeCopy == nil {
			source := s.ssn.Nodes[task.NodeName]
			if source == nil {
				continue
			}
			sourceNodeCopy = source.Clone()
			sourceNodeCopies[task.NodeName] = sourceNodeCopy
		}
		if err := s.ssn.SimulateRemoveTaskFn(ctx, state, victim, task, sourceNodeCopy); err != nil {
			return nil, err
		}
		sourceNodeCopy.RemoveTask(task)
	}
	for nodeName, pods := range tasksPlacedByNode {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node := s.ssn.Nodes[nodeName]
		if node == nil {
			continue
		}
		nodeCopy := node.Clone()
		for _, task := range pods {
			simulatedPlacement := clearNodeBinding(task)
			if err := s.ssn.SimulateAddTaskFn(ctx, state, victim, simulatedPlacement, nodeCopy); err != nil {
				return nil, err
			}
			if err := nodeCopy.AddTask(simulatedPlacement); err != nil {
				return nil, err
			}
		}
	}
	return state, nil
}

// clearNodeBinding returns a task clone with node binding cleared so relocation
// simulation can AddTask onto a different node and run filter plugins as if the
// pod were unbound.
func clearNodeBinding(task *schedapi.TaskInfo) *schedapi.TaskInfo {
	if task == nil {
		return nil
	}
	t := task.Clone()
	t.NodeName = ""
	if t.Pod != nil {
		p := t.Pod.DeepCopy()
		p.Spec.NodeName = ""
		p.Status.NominatedNodeName = ""
		t.Pod = p
	}
	return t
}

// firstFeasibleReceiver returns the first receiver (in the caller's preference
// order) that passes the full scheduler filters for victim, or "" if none fit.
func (s *SessionSnapshot) firstFeasibleReceiver(ctx context.Context, victim *schedapi.TaskInfo, baseState fwk.CycleState, receivers []*schedapi.NodeInfo, tasksPlacedByNode map[string][]*schedapi.TaskInfo) string {
	for _, node := range receivers {
		if ctx.Err() != nil {
			return ""
		}
		if s.victimFitsReceiver(ctx, victim, baseState, node, tasksPlacedByNode[node.Name]) {
			return node.Name
		}
	}
	return ""
}

// victimFitsReceiver checks, on clones only, whether victim fits on node after
// the pods already placed there this pass: resource via FutureIdle, everything
// else via the full SimulatePredicateFn stack.
func (s *SessionSnapshot) victimFitsReceiver(ctx context.Context, victim *schedapi.TaskInfo, baseState fwk.CycleState, node *schedapi.NodeInfo, previouslyPlacedTasks []*schedapi.TaskInfo) bool {
	if ctx.Err() != nil {
		return false
	}
	// Cheap preflight before cloning NodeInfo/CycleState: if the target-accelerator
	// request cannot fit after prior placements, the full predicate cannot pass
	// either. Other resources and predicates stay authoritative below.
	if !s.receiverHasTargetResourceCapacity(victim, node, previouslyPlacedTasks) {
		return false
	}
	nodeCopy := node.Clone()
	stateCopy := baseState.Clone()
	for _, task := range previouslyPlacedTasks {
		simulatedPlacement := clearNodeBinding(task)
		if err := nodeCopy.AddTask(simulatedPlacement); err != nil {
			return false
		}
	}
	if !victim.InitResreq.LessEqual(nodeCopy.FutureIdle(), schedapi.Zero) {
		return false
	}
	return s.ssn.SimulatePredicateFn(ctx, stateCopy, victim, nodeCopy) == nil
}

// receiverHasTargetResourceCapacity is a cheap necessary preflight for one
// receiver, using the target accelerator only.
func (s *SessionSnapshot) receiverHasTargetResourceCapacity(victim *schedapi.TaskInfo, node *schedapi.NodeInfo, previouslyPlacedTasks []*schedapi.TaskInfo) bool {
	if victim == nil || node == nil || node.Idle == nil || node.Releasing == nil || node.Pipelined == nil {
		return false
	}
	available := api.Scalar(api.NodeFreeCapacity(node), s.resource)
	for _, task := range previouslyPlacedTasks {
		if task != nil {
			available -= api.Scalar(task.InitResreq, s.resource)
		}
	}
	return api.Scalar(victim.InitResreq, s.resource) <= available
}

// PodGroupView reads plan-scoring facts off JobInfo.
func (s *SessionSnapshot) PodGroupView(id schedapi.JobID) api.PodGroupView {
	ji, ok := s.ssn.Jobs[id]
	if !ok || ji == nil {
		return api.PodGroupView{}
	}
	var running int32
	if m, ok := ji.TaskStatusIndex[schedapi.Running]; ok {
		running = int32(len(m))
	}
	var footprint int64
	for _, t := range ji.Tasks {
		if t != nil && t.InitResreq != nil {
			footprint += scalar(t.InitResreq, s.resource)
		}
	}
	return api.PodGroupView{
		MinAvailable: ji.MinAvailable,
		Running:      running,
		Footprint:    footprint,
	}
}

// PodGroupUsesSubGroupPolicy reports whether replacement Pods require
// scheduling-requirements matching instead of homogeneous PodGroup matching.
func (s *SessionSnapshot) PodGroupUsesSubGroupPolicy(id schedapi.JobID) bool {
	job := s.ssn.Jobs[id]
	return job != nil && job.ContainsSubJobPolicy()
}

// scalar returns the count of a single scalar resource on r.
func scalar(r *schedapi.Resource, name v1.ResourceName) int64 {
	if r == nil || r.ScalarResources == nil {
		return 0
	}
	return int64(r.ScalarResources[name] + 0.5)
}
