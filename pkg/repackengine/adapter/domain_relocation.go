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
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// trialScope is the call-wide evacuation intent: the pods every unit of one
// FeasibleRelocation call is about to evict. A gang's own victims cannot tell
// whether the job as a whole is being drained — its sibling gangs can.
type trialScope struct {
	victims sets.Set[schedapi.TaskID]
}

func newTrialScope(victims []*schedapi.TaskInfo) *trialScope {
	ids := sets.New[schedapi.TaskID]()
	for _, victim := range victims {
		if victim != nil {
			ids.Insert(victim.UID)
		}
	}
	return &trialScope{victims: ids}
}

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

// jobID returns the unit's Job ID, or "" when the session has no such Job.
func (u *gangUnit) jobID() schedapi.JobID {
	if u.job == nil {
		return ""
	}
	return u.job.UID
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
	scope *trialScope,
	unit *gangUnit,
	sourceTasksToRemove []*schedapi.TaskInfo,
	receivers []*schedapi.NodeInfo,
	tasksPlacedByNode map[string][]*schedapi.TaskInfo,
) ([]*api.Move, bool) {
	allowed, ok := s.allowedDomainsForTrial(scope, unit)
	if !ok {
		klog.V(4).InfoS("repack relocation: unit INFEASIBLE — no allowed HyperNode domain",
			"job", unit.jobID(), "subJob", unit.subJobID(), "victims", taskNames(unit.victims))
		return nil, false
	}
	for _, layer := range allowed {
		for _, domain := range layer {
			if ctx.Err() != nil {
				return nil, false
			}
			if moves, fit := s.trialFitDomain(ctx, unit.victims, domain, sourceTasksToRemove, receivers, tasksPlacedByNode); fit {
				klog.V(4).InfoS("repack relocation: unit placed inside one domain",
					"job", unit.jobID(), "subJob", unit.subJobID(), "domain", domain.Name, "tier", domain.Tier(),
					"placements", moveSummary(moves))
				return moves, true
			}
		}
	}
	klog.V(4).InfoS("repack relocation: unit INFEASIBLE — every allowed domain failed the trial",
		"job", unit.jobID(), "subJob", unit.subJobID(), "victims", taskNames(unit.victims),
		"domainsTried", domainsByTier(allowed))
	return nil, false
}

// allowedDomainsForTrial evaluates the unit's domains on the anchors the plan
// intends once this pass's evacuations are done: a stale anchor pins the trial to
// the drained subtree — the source only, never a receiver.
func (s *SessionSnapshot) allowedDomainsForTrial(scope *trialScope, unit *gangUnit) ([][]*schedapi.HyperNodeInfo, bool) {
	gangVacated := s.gangFullyVacated(unit)
	jobVacated := false
	if gangVacated {
		anchor := s.plan.Save()
		defer s.plan.Restore(anchor)
		if unit.subJob != nil {
			s.plan.SetGangAnchor(unit.job.UID, unit.subJobID(), "")
		}
		if s.jobFullyVacated(scope, unit) {
			jobVacated = true
			s.plan.SetGangAnchor(unit.job.UID, "", s.jobAnchorAfterEvacuation(scope, unit))
		}
	}
	allowed, ok := s.allowedDomains(unit)
	// Read before the deferred restore: these are the values the gradient saw.
	klog.V(4).InfoS("repack relocation: unit allowed domains", "job", unit.jobID(), "subJob", unit.subJobID(),
		"victims", taskNames(unit.victims), "gangFullyVacated", gangVacated, "jobFullyVacated", jobVacated,
		"jobAnchor", s.planState().JobAllocatedHyperNode(unit.job.UID),
		"subJobAnchor", s.planState().SubJobAllocatedHyperNode(unit.job.UID, unit.subJobID()),
		"allowedCount", len(allowed), "allowed", domainsByTier(allowed), "usable", ok)
	return allowed, ok
}

// gangFullyVacated reports whether every plan-state allocated task of the unit's
// gang is a victim of this unit (no residual pod anchors it).
func (s *SessionSnapshot) gangFullyVacated(unit *gangUnit) bool {
	index := unit.job.TaskStatusIndex
	if unit.subJob != nil {
		index = unit.subJob.TaskStatusIndex
	}
	return unitFullyVacated(unit, index)
}

// jobFullyVacated reports whether this pass evacuates the whole job, not just one
// of its gangs: the Job-entry gradient stays anchored to the job until its last
// pod leaves.
func (s *SessionSnapshot) jobFullyVacated(scope *trialScope, unit *gangUnit) bool {
	if unit.job == nil {
		return false
	}
	allocated := 0
	for status, tasks := range unit.job.TaskStatusIndex {
		if !schedapi.AllocatedStatus(status) {
			continue
		}
		for taskID := range tasks {
			allocated++
			if !scope.victims.Has(taskID) {
				return false
			}
		}
	}
	return allocated > 0
}

// jobAnchorAfterEvacuation returns the Job-entry anchor the plan intends once this
// pass's evictions are done: the LCA over the gangs that still hold a pod — the
// gangs the plan has already placed included. Empty when the job keeps no pod, so
// a fully drained job is bound by nothing.
func (s *SessionSnapshot) jobAnchorAfterEvacuation(scope *trialScope, unit *gangUnit) string {
	if unit.job == nil {
		return ""
	}
	displaced := s.plan.DisplacedTasks(unit.job.UID)
	anchor := ""
	for _, subJob := range unit.job.SubJobs {
		if !subJobKeepsPods(subJob, scope.victims, displaced) {
			continue
		}
		anchor = s.ssn.HyperNodes.GetLCAHyperNode(anchor, subJob.AllocatedHyperNode)
	}
	return anchor
}

// subJobKeepsPods reports whether the subJob still holds a pod the plan keeps: an
// allocated task this pass does not evict, or one it has already placed.
func subJobKeepsPods(subJob *schedapi.SubJobInfo, victims, displaced sets.Set[schedapi.TaskID]) bool {
	if subJob == nil {
		return false
	}
	for status, tasks := range subJob.TaskStatusIndex {
		if !schedapi.AllocatedStatus(status) {
			continue
		}
		for taskID := range tasks {
			if !victims.Has(taskID) || displaced.Has(taskID) {
				return true
			}
		}
	}
	return false
}

// unitFullyVacated compares victim set membership against statusIndex's allocated
// tasks: set membership, not count equality — on the Execute-side reconcile a
// victim is the live Pending replacement pod, so counts could match while a
// residual allocated pod still anchors the gang.
func unitFullyVacated(unit *gangUnit, statusIndex map[schedapi.TaskStatus]schedapi.TasksMap) bool {
	if len(unit.victims) == 0 {
		return false
	}
	victimIDs := sets.New[schedapi.TaskID]()
	for _, v := range unit.victims {
		if v != nil {
			victimIDs.Insert(v.UID)
		}
	}
	for status, tasks := range statusIndex {
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
		klog.V(5).InfoS("repack relocation: domain has no receiver", "domain", domain.Name, "tier", domain.Tier(),
			"receiversOutsideDomain", nodeNames(receivers))
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
			klog.V(5).InfoS("repack relocation: victim has no feasible receiver in domain",
				"domain", domain.Name, "tier", domain.Tier(), "victim", simulatedVictim.Name,
				"from", simulatedVictim.NodeName, "domainReceivers", nodeNames(domainReceivers))
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

func taskNames(tasks []*schedapi.TaskInfo) []string {
	names := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if task != nil {
			names = append(names, task.Name)
		}
	}
	return names
}

func nodeNames(nodes []*schedapi.NodeInfo) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil {
			names = append(names, node.Name)
		}
	}
	return names
}

// domainsByTier renders a gradient forest for logs, e.g. "tier-1: rt-s0, rt-s1; tier-2: rt-s2".
func domainsByTier(layers [][]*schedapi.HyperNodeInfo) string {
	parts := make([]string, 0, len(layers))
	for _, layer := range layers {
		names := make([]string, 0, len(layer))
		tier := 0
		for _, hyperNode := range layer {
			if hyperNode == nil {
				continue
			}
			tier = hyperNode.Tier()
			names = append(names, hyperNode.Name)
		}
		if len(names) > 0 {
			sort.Strings(names)
			parts = append(parts, fmt.Sprintf("tier-%d: %s", tier, strings.Join(names, ", ")))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// moveSummary renders planned placements for logs, e.g. "pod-a: from n0 to n1".
// An arrow would be escaped to > by klog's text output.
func moveSummary(moves []*api.Move) []string {
	pairs := make([]string, 0, len(moves))
	for _, move := range moves {
		if move != nil && move.Task != nil {
			pairs = append(pairs, fmt.Sprintf("%s: from %s to %s", move.Task.Name, move.From, move.To))
		}
	}
	return pairs
}
