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
	"context"

	"k8s.io/apimachinery/pkg/util/sets"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// gangUnit is one constraint-bearing gang: a Job (no SubGroupPolicy) or one
// SubJob, placed together within a single allowed HyperNode domain. It is the
// granularity of the domain-trial relocation and the plan-state commit.
type gangUnit struct {
	job     *schedapi.JobInfo
	subJob  *schedapi.SubJobInfo // nil for a Job unit
	victims []*schedapi.TaskInfo
}

// requiresHyperNodeAllocate reports whether the unit's job hits the scheduler's
// RequiresHyperNodeAllocate predicate AND the session carries a HyperNode tree.
// Without a tree the constraint stack is inert and the unit must keep the
// legacy greedy behavior.
func (u *gangUnit) requiresHyperNodeAllocate(s *SessionSnapshot) bool {
	return u.job != nil && u.job.RequiresHyperNodeAllocate() && s.hasHyperNodeTopology()
}

func (u *gangUnit) subJobID() schedapi.SubJobID {
	if u.subJob == nil {
		return ""
	}
	return u.subJob.UID
}

// hasHyperNodeTopology reports whether the scheduler's HyperNode tree is ready.
// Without a ready tree the constraint stack is inert and the unit keeps the
// legacy greedy path.
func (s *SessionSnapshot) hasHyperNodeTopology() bool {
	return s.ssn != nil && s.ssn.HyperNodesReadyToSchedule
}

// groupVictimsByGang splits victims into gang units in first-encounter order:
// one Job unit, or one unit per SubJob for a SubGroupPolicy job (TaskToSubJob
// always maps a task, the default subJob included).
func (s *SessionSnapshot) groupVictimsByGang(victims []*schedapi.TaskInfo) []*gangUnit {
	type gangKey struct {
		job    schedapi.JobID
		subJob schedapi.SubJobID // "" = Job unit
	}
	var units []*gangUnit
	byKey := map[gangKey]int{}
	for _, v := range victims {
		key := gangKey{job: v.Job}
		var subJob *schedapi.SubJobInfo
		if job := s.ssn.Jobs[v.Job]; job != nil && job.ContainsSubJobPolicy() {
			key.subJob = job.TaskToSubJob[v.UID]
			subJob = job.SubJobs[key.subJob]
		}
		if idx, found := byKey[key]; found {
			units[idx].victims = append(units[idx].victims, v)
			continue
		}
		byKey[key] = len(units)
		units = append(units, &gangUnit{
			job:     s.ssn.Jobs[v.Job],
			subJob:  subJob,
			victims: []*schedapi.TaskInfo{v},
		})
	}
	return units
}

// allowedDomains returns the unit's tier-ascending candidate HyperNode layers
// on the current plan state: the Job-entry gradient, intersected with the
// SubJob-entry gradient for a SubJob unit (Job entry carries the Job-level
// topology the SubGroupPolicy branches do not inherit).
func (s *SessionSnapshot) allowedDomains(unit *gangUnit) ([][]*schedapi.HyperNodeInfo, bool) {
	root := s.ssn.HyperNodes[schedframework.ClusterTopHyperNode]
	if root == nil {
		return nil, false
	}
	jobGradients, _ := s.ssn.HyperNodeGradientForJobFn(unit.job, root, schedapi.PurposeAllocate)
	if unit.subJob == nil {
		return jobGradients, len(jobGradients) > 0
	}
	subJobGradients, _ := s.ssn.HyperNodeGradientForSubJobFn(unit.subJob, root, schedapi.PurposeAllocate)
	return intersectGradientForest(jobGradients, subJobGradients, s.ssn.HyperNodes)
}

// intersectGradientForest keeps the inner-gradient HyperNodes that lie under
// the outer-gradient forest (root-or-ancestor), mirroring
// allocate.filterGradientsByCandidateForest.
func intersectGradientForest(outer, inner [][]*schedapi.HyperNodeInfo, hyperNodes schedapi.HyperNodeInfoMap) ([][]*schedapi.HyperNodeInfo, bool) {
	roots := sets.New[string]()
	for _, layer := range outer {
		for _, hyperNode := range layer {
			if hyperNode != nil {
				roots.Insert(hyperNode.Name)
			}
		}
	}
	if roots.Len() == 0 {
		return nil, false
	}
	var result [][]*schedapi.HyperNodeInfo
	for _, layer := range inner {
		var kept []*schedapi.HyperNodeInfo
		for _, hyperNode := range layer {
			if hyperNode == nil || !underAnyRoot(hyperNodes, hyperNode.Name, roots) {
				continue
			}
			kept = append(kept, hyperNode)
		}
		if len(kept) > 0 {
			result = append(result, kept)
		}
	}
	return result, len(result) > 0
}

func underAnyRoot(hyperNodes schedapi.HyperNodeInfoMap, name string, roots sets.Set[string]) bool {
	for _, ancestor := range hyperNodes.GetAncestors(name) {
		if roots.Has(ancestor) {
			return true
		}
	}
	return false
}

// domainTrialRelocation places a gang unit entirely within one allowed domain,
// tier ascending, first fit wins. A failed trial moves to the next domain; when
// every domain fails the unit is infeasible (nil, false).
func (s *SessionSnapshot) domainTrialRelocation(
	ctx context.Context,
	unit *gangUnit,
	sourceTasksToRemove []*schedapi.TaskInfo,
	receivers []*schedapi.NodeInfo,
	tasksPlacedByNode map[string][]*schedapi.TaskInfo,
) ([]*api.Move, bool) {
	allowed, ok := s.allowedDomainsForTrial(unit)
	if !ok {
		return nil, false
	}
	for _, layer := range allowed {
		for _, domain := range layer {
			if ctx.Err() != nil {
				return nil, false
			}
			if moves, fit := s.trialFitDomain(ctx, unit.victims, domain, sourceTasksToRemove, receivers, tasksPlacedByNode); fit {
				return moves, true
			}
		}
	}
	return nil, false
}

// allowedDomainsForTrial clears the gang anchor when this unit fully vacates
// the gang, so the whole-cluster branch applies as the real scheduler would
// after eviction. Save/Restore keeps the clear leak-free even on a panic.
func (s *SessionSnapshot) allowedDomainsForTrial(unit *gangUnit) ([][]*schedapi.HyperNodeInfo, bool) {
	if s.gangFullyVacated(unit) {
		anchor := s.plan.Save()
		defer s.plan.Restore(anchor)
		s.plan.ClearGangAnchor(unit.job.UID, unit.subJobID())
		return s.allowedDomains(unit)
	}
	return s.allowedDomains(unit)
}

// gangFullyVacated reports whether every plan-state allocated task of the gang is
// a victim of this unit (no residual pod anchors it). Set membership, not count
// equality: on the Execute-side reconcile a victim is the live Pending replacement
// pod, so counts could match while a residual allocated pod still anchors the gang.
func (s *SessionSnapshot) gangFullyVacated(unit *gangUnit) bool {
	if len(unit.victims) == 0 {
		return false
	}
	victimIDs := sets.New[schedapi.TaskID]()
	for _, v := range unit.victims {
		if v != nil {
			victimIDs.Insert(v.UID)
		}
	}
	index := unit.job.TaskStatusIndex
	if unit.subJob != nil {
		index = unit.subJob.TaskStatusIndex
	}
	for status, tasks := range index {
		if !schedapi.AllocatedStatus(status) {
			continue
		}
		for taskID := range tasks {
			if !victimIDs.Has(taskID) {
				return false
			}
		}
	}
	return true
}

// trialFitDomain places every victim into receivers under domain's real node
// set, one pod at a time (full SimulatePredicateFn per pod), recording into
// tasksPlacedByNode. Any failure discards the whole trial — no partial residue.
func (s *SessionSnapshot) trialFitDomain(
	ctx context.Context,
	victims []*schedapi.TaskInfo,
	domain *schedapi.HyperNodeInfo,
	sourceTasksToRemove []*schedapi.TaskInfo,
	receivers []*schedapi.NodeInfo,
	tasksPlacedByNode map[string][]*schedapi.TaskInfo,
) ([]*api.Move, bool) {
	domainNodes := s.ssn.RealNodesSet[domain.Name]
	domainReceivers := make([]*schedapi.NodeInfo, 0, len(receivers))
	for _, r := range receivers {
		if domainNodes.Has(r.Name) {
			domainReceivers = append(domainReceivers, r)
		}
	}
	if len(domainReceivers) == 0 {
		return nil, false
	}
	saved := clonePlaced(tasksPlacedByNode)
	moves := make([]*api.Move, 0, len(victims))
	for _, victim := range victims {
		if ctx.Err() != nil {
			restorePlaced(tasksPlacedByNode, saved)
			return nil, false
		}
		simulatedVictim := clearNodeBinding(victim)
		baseState, err := s.buildRelocationCycleState(ctx, simulatedVictim, sourceTasksToRemove, tasksPlacedByNode)
		if err != nil {
			restorePlaced(tasksPlacedByNode, saved)
			return nil, false
		}
		target := s.firstFeasibleReceiver(ctx, simulatedVictim, baseState, domainReceivers, tasksPlacedByNode)
		if target == "" {
			restorePlaced(tasksPlacedByNode, saved)
			return nil, false
		}
		tasksPlacedByNode[target] = append(tasksPlacedByNode[target], victim)
		moves = append(moves, &api.Move{Task: victim, From: victim.NodeName, To: target})
	}
	return moves, true
}

// greedyRelocation is the legacy per-victim first-fit over all receivers for
// ==false victims (cross-domain placement).
func (s *SessionSnapshot) greedyRelocation(
	ctx context.Context,
	victims []*schedapi.TaskInfo,
	sourceTasksToRemove []*schedapi.TaskInfo,
	receivers []*schedapi.NodeInfo,
	tasksPlacedByNode map[string][]*schedapi.TaskInfo,
) ([]*api.Move, bool) {
	moves := make([]*api.Move, 0, len(victims))
	for _, victim := range victims {
		if ctx.Err() != nil {
			return nil, false
		}
		simulatedVictim := clearNodeBinding(victim)
		baseState, err := s.buildRelocationCycleState(ctx, simulatedVictim, sourceTasksToRemove, tasksPlacedByNode)
		if err != nil {
			return nil, false
		}
		target := s.firstFeasibleReceiver(ctx, simulatedVictim, baseState, receivers, tasksPlacedByNode)
		if target == "" {
			return nil, false
		}
		tasksPlacedByNode[target] = append(tasksPlacedByNode[target], victim)
		moves = append(moves, &api.Move{Task: victim, From: victim.NodeName, To: target})
	}
	return moves, true
}

// clonePlaced snapshots the placed-by-node accounting for trial rollback.
func clonePlaced(in map[string][]*schedapi.TaskInfo) map[string][]*schedapi.TaskInfo {
	out := make(map[string][]*schedapi.TaskInfo, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// restorePlaced returns the placed-by-node accounting to a snapshot.
func restorePlaced(m, saved map[string][]*schedapi.TaskInfo) {
	for k := range m {
		delete(m, k)
	}
	for k, v := range saved {
		m[k] = v
	}
}
