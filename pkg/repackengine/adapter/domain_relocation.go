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

// trialScope is the call-wide evacuation intent: the pods every gang of one
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

// gang is one constraint-bearing gang: a whole Job (no SubGroupPolicy) or one
// SubJob, placed together within a single allowed HyperNode domain. It is the
// granularity of the domain trial and the plan-state commit.
type gang struct {
	subJob  *schedapi.SubJobInfo // nil for the whole-job gang
	victims []*schedapi.TaskInfo
}

// jobGangs is one Job inside one FeasibleRelocation call: the container of its
// gangs and the granularity of the whole-job domain trial. A Job whose victims
// form a single gang is no special case — it is a container with one element.
type jobGangs struct {
	job   *schedapi.JobInfo
	gangs []gang
}

// requiresHyperNodeAllocate reports whether the job hits the scheduler's
// RequiresHyperNodeAllocate predicate AND the session carries a HyperNode tree.
// Without a tree the constraint stack is inert and the job must keep the legacy
// greedy behavior.
func (jg *jobGangs) requiresHyperNodeAllocate(s *SessionSnapshot) bool {
	return jg.job != nil && jg.job.RequiresHyperNodeAllocate() && s.hasHyperNodeTopology()
}

// jobID returns the container's Job ID, or "" when the session has no such Job.
func (jg *jobGangs) jobID() schedapi.JobID {
	if jg.job == nil {
		return ""
	}
	return jg.job.UID
}

// allVictims returns every gang's victims, for logs.
func (jg *jobGangs) allVictims() []*schedapi.TaskInfo {
	var all []*schedapi.TaskInfo
	for i := range jg.gangs {
		all = append(all, jg.gangs[i].victims...)
	}
	return all
}

func (g *gang) subJobID() schedapi.SubJobID {
	if g.subJob == nil {
		return ""
	}
	return g.subJob.UID
}

// hasHyperNodeTopology reports whether the scheduler's HyperNode tree is ready.
// Without a ready tree the constraint stack is inert and a constrained gang
// stays on the legacy greedy path.
func (s *SessionSnapshot) hasHyperNodeTopology() bool {
	return s.ssn != nil && s.ssn.HyperNodesReadyToSchedule
}

// groupVictimsByJobGangs splits victims into Job containers in first-encounter
// order: one gang, or one gang per SubJob for a SubGroupPolicy job (TaskToSubJob
// always maps a task, the default subJob included). Victims of Jobs the session
// does not carry all share one container with job == nil, which always takes the
// greedy path.
func (s *SessionSnapshot) groupVictimsByJobGangs(victims []*schedapi.TaskInfo) []*jobGangs {
	type gangKey struct {
		job    *schedapi.JobInfo
		subJob schedapi.SubJobID // "" = the whole-job gang
	}
	var groups []*jobGangs
	byJob := map[*schedapi.JobInfo]*jobGangs{}
	byGang := map[gangKey]int{}
	for _, v := range victims {
		job := s.ssn.Jobs[v.Job]
		subJobID := schedapi.SubJobID("")
		var subJob *schedapi.SubJobInfo
		if job != nil && job.ContainsSubJobPolicy() {
			subJobID = job.TaskToSubJob[v.UID]
			subJob = job.SubJobs[subJobID]
		}
		jg := byJob[job]
		if jg == nil {
			jg = &jobGangs{job: job}
			byJob[job] = jg
			groups = append(groups, jg)
		}
		key := gangKey{job: job, subJob: subJobID}
		index, found := byGang[key]
		if !found {
			index = len(jg.gangs)
			byGang[key] = index
			jg.gangs = append(jg.gangs, gang{subJob: subJob})
		}
		jg.gangs[index].victims = append(jg.gangs[index].victims, v)
	}
	return groups
}

// allowedDomains returns the gang's tier-ascending candidate HyperNode layers
// on the current plan state: the Job-entry gradient, intersected with the
// SubJob-entry gradient for a SubJob gang (Job entry carries the Job-level
// topology the SubGroupPolicy branches do not inherit).
func (s *SessionSnapshot) allowedDomains(jg *jobGangs, g *gang) ([][]*schedapi.HyperNodeInfo, bool) {
	root := s.ssn.HyperNodes[schedframework.ClusterTopHyperNode]
	if root == nil {
		return nil, false
	}
	jobGradients, _ := s.ssn.HyperNodeGradientForJobFn(jg.job, root, schedapi.PurposeAllocate)
	if g.subJob == nil {
		return sortLayersByName(jobGradients), len(jobGradients) > 0
	}
	subJobGradients, _ := s.ssn.HyperNodeGradientForSubJobFn(g.subJob, root, schedapi.PurposeAllocate)
	layers, ok := intersectGradientForest(jobGradients, subJobGradients, s.ssn.HyperNodes)
	return sortLayersByName(layers), ok
}

// sortLayersByName makes the domain order reproducible: the plugin builds each
// tier by walking map-backed Children, so first-fit would otherwise pick a
// different domain of the same tier from run to run.
func sortLayersByName(layers [][]*schedapi.HyperNodeInfo) [][]*schedapi.HyperNodeInfo {
	for _, layer := range layers {
		sort.Slice(layer, func(i, j int) bool { return layer[i].Name < layer[j].Name })
	}
	return layers
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

// shouldUsePodGroupAntiAffinityCandidateForest reports whether the Job's hard
// boundary is required PodGroup anti-affinity rather than a real Job-level hard
// network topology — a copy of allocate's function of the same name.
func shouldUsePodGroupAntiAffinityCandidateForest(job *schedapi.JobInfo) bool {
	if job == nil || !job.ContainsHardPodGroupAntiAffinity() {
		return false
	}
	hardMode, _ := job.IsHardTopologyMode()
	return !hardMode
}

// layerCandidateDomainSets turns one candidate layer into the domain sets to try,
// one set per attempt. By default each domain is its own set: the container has to
// fit inside that one domain. A required-anti-affinity forest takes the layer as
// one set over all its roots, so sibling domains can split the container — the
// same reading of a layer as allocate's candidate forest
// (allocate.normalizeCandidateForestRoots and filterGradientsByCandidateForest).
func layerCandidateDomainSets(layer []*schedapi.HyperNodeInfo, useCandidateForest bool, hyperNodes schedapi.HyperNodeInfoMap) [][]*schedapi.HyperNodeInfo {
	if !useCandidateForest {
		sets := make([][]*schedapi.HyperNodeInfo, 0, len(layer))
		for _, domain := range layer {
			if domain == nil {
				continue
			}
			sets = append(sets, []*schedapi.HyperNodeInfo{domain})
		}
		return sets
	}
	roots := distinctDomainsByName(layer)
	if len(roots) == 0 {
		return nil
	}
	// The roots' union is the semantic bound; the envelope is only the guard
	// allocate applies too: a layer whose roots the tree does not join is skipped.
	if candidateForestEnvelope(hyperNodes, roots) == nil {
		klog.V(4).InfoS("repack relocation: candidate forest layer skipped — its roots share no HyperNode ancestor",
			"roots", domainsByTier([][]*schedapi.HyperNodeInfo{roots}))
		return nil
	}
	return [][]*schedapi.HyperNodeInfo{roots}
}

// candidateForestEnvelope is the LCA of a layer's roots, nil when the tree does
// not join them (allocate.candidateForestEnvelope).
func candidateForestEnvelope(hyperNodes schedapi.HyperNodeInfoMap, roots []*schedapi.HyperNodeInfo) *schedapi.HyperNodeInfo {
	if len(roots) == 0 {
		return nil
	}
	envelope := roots[0].Name
	for _, root := range roots[1:] {
		envelope = hyperNodes.GetLCAHyperNode(envelope, root.Name)
		if envelope == "" {
			return nil
		}
	}
	return hyperNodes[envelope]
}

// distinctDomainsByName dedupes a layer's domains in name order.
func distinctDomainsByName(layer []*schedapi.HyperNodeInfo) []*schedapi.HyperNodeInfo {
	unique := make(map[string]*schedapi.HyperNodeInfo, len(layer))
	for _, domain := range layer {
		if domain != nil {
			unique[domain.Name] = domain
		}
	}
	names := make([]string, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	roots := make([]*schedapi.HyperNodeInfo, 0, len(names))
	for _, name := range names {
		roots = append(roots, unique[name])
	}
	return roots
}

// trialWholeJob settles every gang of one Job container inside the Job's allowed
// HyperNode domains: the candidates come from the Job-entry gradient, tried tier
// ascending, first fit wins. A domain set that cannot hold the whole container is
// rolled back and the next one is tried — without this, the first gang's first
// fit decides where the rest of the job can go, and a job that fits another
// domain is refused. Inside a set each gang keeps its own allowed domains and its
// own per-pod trial, so subjob-level and relational constraints still hold.
func (s *SessionSnapshot) trialWholeJob(
	ctx context.Context,
	jg *jobGangs,
	scope *trialScope,
	sourceTasksToRemove []*schedapi.TaskInfo,
	receivers []*schedapi.NodeInfo,
	tasksPlacedByNode map[string][]*schedapi.TaskInfo,
) ([]*api.Move, bool) {
	layers, ok := s.jobEntryDomains(jg, scope)
	if !ok {
		klog.V(4).InfoS("repack relocation: whole-job INFEASIBLE — no allowed HyperNode domain",
			"job", jg.job.UID, "gangs", len(jg.gangs), "victims", taskNames(jg.allVictims()))
		return nil, false
	}
	// Rollback base is the container's own start, not the call's baseline: the
	// gangs before this container stay committed.
	jobBase := s.planState().Save()
	savedPlaced := clonePlaced(tasksPlacedByNode)
	useCandidateForest := shouldUsePodGroupAntiAffinityCandidateForest(jg.job)
	for _, layer := range layers {
		for _, domains := range layerCandidateDomainSets(layer, useCandidateForest, s.ssn.HyperNodes) {
			if ctx.Err() != nil {
				return nil, false
			}
			moves, fit := s.trialWholeJobInDomains(ctx, jg, domains, scope, sourceTasksToRemove, receivers, tasksPlacedByNode)
			if fit {
				klog.V(4).InfoS("repack relocation: whole job placed in one domain set",
					"job", jg.job.UID, "allowedDomains", domainsByTier([][]*schedapi.HyperNodeInfo{domains}),
					"gangs", len(jg.gangs), "placements", moveSummary(moves))
				return moves, true
			}
			s.planState().Restore(jobBase)
			restorePlaced(tasksPlacedByNode, savedPlaced)
		}
	}
	klog.V(4).InfoS("repack relocation: whole-job INFEASIBLE — every allowed domain failed the trial",
		"job", jg.job.UID, "gangs", len(jg.gangs), "victims", taskNames(jg.allVictims()),
		"domainsTried", domainsByTier(layers))
	return nil, false
}

// jobEntryDomains returns the container's candidate domain layers: the Job-entry
// gradient under the anchors this pass intends. A Job the entry leaves
// unconstrained yields the tree root as its one candidate — a domain that
// excludes nothing, so every gang keeps its own domains; that is today's per-gang
// path, and the whole-job retry only pays off when a plugin pins gangs to the
// Job anchor.
func (s *SessionSnapshot) jobEntryDomains(jg *jobGangs, scope *trialScope) ([][]*schedapi.HyperNodeInfo, bool) {
	root := s.ssn.HyperNodes[schedframework.ClusterTopHyperNode]
	if root == nil {
		return nil, false
	}
	plan := s.planState()
	anchor := plan.Save()
	defer plan.Restore(anchor)
	if s.jobFullyVacated(scope, jg.job) {
		plan.SetGangAnchor(jg.job.UID, "", s.jobAnchorAfterEvacuation(scope, jg.job))
	}
	jobGradients, stats := s.ssn.HyperNodeGradientForJobFn(jg.job, root, schedapi.PurposeAllocate)
	if len(jobGradients) == 0 {
		return nil, false
	}
	if noPluginApplied(stats) {
		return [][]*schedapi.HyperNodeInfo{{root}}, true
	}
	return sortLayersByName(jobGradients), true
}

// noPluginApplied reports whether no gradient plugin claimed the Job entry: the
// framework fills PluginEligibleByTier only for Applied plugins, and a nil stats
// means none is registered. The returned forest is then the bare candidate
// universe, so the caller uses the tree root as its one candidate.
func noPluginApplied(stats *schedapi.HyperNodeGradientStats) bool {
	return stats == nil || len(stats.PluginEligibleByTier) == 0
}

// trialWholeJobInDomains places every gang of the container in one domain set, in
// gang order, committing each gang so the next one is graded against the previous
// gang's plan-state placement — the serial pinning that keeps Required-linked
// gangs co-located. Any failure discards the whole set; the caller rolls back.
func (s *SessionSnapshot) trialWholeJobInDomains(
	ctx context.Context,
	jg *jobGangs,
	domains []*schedapi.HyperNodeInfo,
	scope *trialScope,
	sourceTasksToRemove []*schedapi.TaskInfo,
	receivers []*schedapi.NodeInfo,
	tasksPlacedByNode map[string][]*schedapi.TaskInfo,
) ([]*api.Move, bool) {
	var moves []*api.Move
	for i := range jg.gangs {
		gangMoves, fit := s.trialGangInDomains(ctx, jg, &jg.gangs[i], domains, scope, sourceTasksToRemove, receivers, tasksPlacedByNode)
		if !fit {
			return nil, false
		}
		s.planState().ApplyCommit(gangMoves)
		moves = append(moves, gangMoves...)
	}
	return moves, true
}

// trialGangInDomains places one gang entirely inside one set's allowed domains,
// tier ascending, first fit wins. A failed trial moves to the next domain; when
// every domain fails the gang is infeasible (nil, false).
func (s *SessionSnapshot) trialGangInDomains(
	ctx context.Context,
	jg *jobGangs,
	g *gang,
	domains []*schedapi.HyperNodeInfo,
	scope *trialScope,
	sourceTasksToRemove []*schedapi.TaskInfo,
	receivers []*schedapi.NodeInfo,
	tasksPlacedByNode map[string][]*schedapi.TaskInfo,
) ([]*api.Move, bool) {
	allowed, ok := s.allowedDomainsForTrial(jg, g, scope)
	if !ok {
		klog.V(5).InfoS("repack relocation: gang has no allowed HyperNode domain",
			"job", jg.jobID(), "subJob", g.subJobID(), "victims", taskNames(g.victims))
		return nil, false
	}
	// Keep the gang's own allowed domains inside the set: the gradient is still the
	// gang's, only its candidates narrow.
	allowed, ok = intersectGradientForest([][]*schedapi.HyperNodeInfo{domains}, allowed, s.ssn.HyperNodes)
	if !ok {
		klog.V(5).InfoS("repack relocation: gang has no allowed HyperNode domain inside the container's domains",
			"job", jg.jobID(), "subJob", g.subJobID(), "allowedDomains", domainsByTier([][]*schedapi.HyperNodeInfo{domains}),
			"victims", taskNames(g.victims))
		return nil, false
	}
	for _, layer := range allowed {
		for _, domain := range layer {
			if ctx.Err() != nil {
				return nil, false
			}
			if moves, fit := s.trialFitDomain(ctx, g.victims, domain, sourceTasksToRemove, receivers, tasksPlacedByNode); fit {
				klog.V(4).InfoS("repack relocation: gang placed inside one domain",
					"job", jg.jobID(), "subJob", g.subJobID(), "domain", domain.Name, "tier", domain.Tier(),
					"placements", moveSummary(moves))
				return moves, true
			}
		}
	}
	klog.V(4).InfoS("repack relocation: gang INFEASIBLE — every allowed domain failed the trial",
		"job", jg.jobID(), "subJob", g.subJobID(), "victims", taskNames(g.victims),
		"domainsTried", domainsByTier(allowed))
	return nil, false
}

// allowedDomainsForTrial evaluates the gang's domains on the anchors the plan
// intends once this pass's evacuations are done: a stale anchor pins the trial to
// the drained subtree — the source only, never a receiver.
func (s *SessionSnapshot) allowedDomainsForTrial(jg *jobGangs, g *gang, scope *trialScope) ([][]*schedapi.HyperNodeInfo, bool) {
	gangVacated := jg.gangFullyVacated(g)
	jobVacated := false
	if gangVacated {
		plan := s.planState()
		anchor := plan.Save()
		defer plan.Restore(anchor)
		if g.subJob != nil {
			plan.SetGangAnchor(jg.job.UID, g.subJobID(), "")
		}
		if s.jobFullyVacated(scope, jg.job) {
			jobVacated = true
			plan.SetGangAnchor(jg.job.UID, "", s.jobAnchorAfterEvacuation(scope, jg.job))
		}
	}
	allowed, ok := s.allowedDomains(jg, g)
	if klog.V(4).Enabled() {
		// Read before the deferred restore: these are the values the gradient saw.
		klog.V(4).InfoS("repack relocation: gang allowed domains", "job", jg.jobID(), "subJob", g.subJobID(),
			"victims", taskNames(g.victims), "gangFullyVacated", gangVacated, "jobFullyVacated", jobVacated,
			"jobAnchor", s.planState().JobAllocatedHyperNode(jg.job.UID),
			"subJobAnchor", s.planState().SubJobAllocatedHyperNode(jg.job.UID, g.subJobID()),
			"allowedCount", len(allowed), "allowed", domainsByTier(allowed), "usable", ok)
	}
	return allowed, ok
}

// gangFullyVacated reports whether every plan-state allocated task of the gang is
// a victim of that gang (no residual pod anchors it).
func (jg *jobGangs) gangFullyVacated(g *gang) bool {
	index := jg.job.TaskStatusIndex
	if g.subJob != nil {
		index = g.subJob.TaskStatusIndex
	}
	return victimsCoverAllocatedTasks(g.victims, index)
}

// jobFullyVacated reports whether this pass evacuates the whole job, not just one
// of its gangs: the Job-entry gradient stays anchored to the job until its last
// pod leaves.
func (s *SessionSnapshot) jobFullyVacated(scope *trialScope, job *schedapi.JobInfo) bool {
	if job == nil {
		return false
	}
	allocated := 0
	for status, tasks := range job.TaskStatusIndex {
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
func (s *SessionSnapshot) jobAnchorAfterEvacuation(scope *trialScope, job *schedapi.JobInfo) string {
	if job == nil {
		return ""
	}
	displaced := s.planState().DisplacedTasks(job.UID)
	anchor := ""
	kept := 0
	for _, subJob := range job.SubJobs {
		if !subJobKeepsPods(subJob, scope.victims, displaced) {
			continue
		}
		kept++
		anchor = s.ssn.HyperNodes.GetLCAHyperNode(anchor, subJob.AllocatedHyperNode)
	}
	if kept > 0 && anchor == "" {
		// Downstream reads "" as unconstrained, so a kept gang with no resolvable
		// HyperNode widens the trial silently instead of failing it.
		klog.V(4).InfoS("repack relocation: kept gangs resolve to no HyperNode ancestor — the job anchor stays empty (unconstrained)",
			"job", job.UID, "keptGangs", kept)
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

// victimsCoverAllocatedTasks compares victim set membership against statusIndex's
// allocated tasks: set membership, not count equality — on the Execute-side
// reconcile a victim is the live Pending replacement pod, so counts could match
// while a residual allocated pod still anchors the gang.
func victimsCoverAllocatedTasks(victims []*schedapi.TaskInfo, statusIndex map[schedapi.TaskStatus]schedapi.TasksMap) bool {
	if len(victims) == 0 {
		return false
	}
	victimIDs := sets.New[schedapi.TaskID]()
	for _, v := range victims {
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
