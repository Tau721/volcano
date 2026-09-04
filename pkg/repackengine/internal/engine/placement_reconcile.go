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

package engine

import (
	"context"
	"fmt"
	"math"
	"sort"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"

	"volcano.sh/volcano/pkg/repackengine/adapter"
	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

func (e *Engine) reconcilePlacement(ctx context.Context, run *repackv1alpha1.RepackRun) engineframework.RuntimeResult {
	if run == nil {
		return engineframework.RuntimeResult{}
	}
	if executionDeadlinePassed(run, e.now()) {
		return runtimeError(e.timeoutExecution(ctx, run, run.Generation, e.clusterCache.Client()))
	}
	selectedNodePlacements, alternativeNodePlacements, timedOutPlacements := enginestatus.PlacementOutcomeCounts(run)
	klog.V(4).InfoS("repack: reconciling replacement placement",
		"run", run.Name, "relocationCount", len(run.Status.Relocations),
		"selectedNodePlacementCount", selectedNodePlacements,
		"alternativeNodePlacementCount", alternativeNodePlacements,
		"timedOutPlacementCount", timedOutPlacements)
	if err := e.repairRecreatedPodGroupLeasesIfDue(ctx, run); err != nil {
		return runtimeError(fmt.Errorf("reconcile recreated PodGroup leases: %w", err))
	}
	if placementexecutor.Complete(run) {
		if hasRetryableEvictions(run) && !hasTimedOutPlacement(run) {
			changed := state.MarkRunning(run, state.ReasonEvicting,
				"Accepted replacements are restored; resuming remaining eviction retries.")
			if changed {
				if err := e.updateStatus(ctx, run); err != nil {
					return runtimeError(fmt.Errorf("persist eviction retry resume: %w", err))
				}
			}
			if wait := e.evictionRetryWait(run); wait > 0 {
				return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), wait)}
			}
			return engineframework.RuntimeResult{Requeue: true}
		}
		return e.finishPlacement(ctx, run)
	}
	pending := placementexecutor.Candidates(run)
	if len(pending) == 0 {
		// A replacement controller may need time to create the Pod. Keep polling
		// until the durable deadline so an absent replacement cannot bypass the
		// expiration escape hatch.
		klog.V(4).InfoS("repack: no selectable replacement Pod observed yet; placement requeued",
			"run", run.Name, "retryAfter", placementRetryInterval)
		return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
	}

	targetResource := engineconf.ResolveResource(run, e.config.DefaultResource)
	schedulerSession := e.clusterCache.OpenSession(e.tiers, e.configurations)
	defer schedframework.CloseSessionReadOnly(schedulerSession)
	scope, err := enginescope.NewMatcher(run.Spec.Scope, adapter.SessionGangScopeLookup(schedulerSession))
	if err != nil {
		return runtimeError(err)
	}
	snapshot := adapter.NewSessionSnapshot(schedulerSession, targetResource, scope)
	nodes := snapshot.Nodes()
	nodeNames := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if n != nil {
			nodeNames[n.Name] = struct{}{}
		}
	}
	excludedFreedNodes := enginestatus.RealizedFreedNodeNames(run)
	klog.V(4).InfoS("repack: evaluating live placement receivers",
		"run", run.Name, "candidateCount", len(pending), "snapshotNodeCount", len(nodes),
		"excludedFreedNodes", excludedFreedNodes)
	committed := make([]*engineapi.Move, 0, len(pending))
	selected := make(map[placementexecutor.Identity]string, len(pending))
	// HyperNode topology, read once per pass: the planned-domain retry below
	// resolves each whole ==true unit's plan target to its containing HyperNode.
	realNodes := snapshot.RealNodesSet()
	hyperNodesByTier := snapshot.HyperNodesSetByTier()

	// Resolve each candidate to its live, unbound replacement Pod.
	arrived := make([]relocationCandidate, 0, len(pending))
	for _, relocation := range pending {
		pod, err := e.clusterCache.Client().CoreV1().Pods(relocation.Namespace).Get(ctx, relocation.Placement.ReplacementPodName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return runtimeError(err)
		}
		if pod.UID != relocation.Placement.ReplacementPodUID || pod.Spec.NodeName != "" {
			continue
		}
		// TaskInfo from the live unbound Pod carries its real requests into the simulation.
		candidate := relocationCandidate{
			relocation: relocation,
			task:       schedapi.NewTaskInfo(pod),
		}
		// Mirror adapter gangUnit.requiresHyperNodeAllocate.
		if job := schedulerSession.Jobs[candidate.task.Job]; job != nil && job.RequiresHyperNodeAllocate() && schedulerSession.HyperNodesReadyToSchedule {
			candidate.hyperNodeAllocated = true
		}
		arrived = append(arrived, candidate)
	}
	if len(arrived) == 0 {
		// Keep polling; an absent replacement keeps the group gated until the run's ExecutionDeadline.
		return engineframework.RuntimeResult{RequeueAfter: placementRetryInterval}
	}
	arrivedByRelocation := make(map[placementexecutor.Identity]*relocationCandidate, len(arrived))
	for index := range arrived {
		arrivedByRelocation[placementexecutor.IdentityForRelocation(arrived[index].relocation)] = &arrived[index]
	}
	// Whole expected unit set per PodGroup (still-undecided journal members);
	// readiness below is a pure function of it.
	retainedByPodGroup := retainedRelocationsByPodGroup(run)

	held := false
	// ==true units are placed only as a whole, in one FeasibleRelocation over
	// every ready member — never a subset. Incomplete or infeasible units defer
	// to the shared TTL, which expires the whole group together.
	for _, group := range groupRelocationCandidates(arrived) {
		if !group.hyperNodeAllocated {
			continue
		}
		if !podGroupPlannedNodesVisible(group.members, nodeNames) {
			klog.V(3).InfoS("repack: planned receiver node of a HyperNode-constrained gang not yet visible; unit held",
				"run", run.Name, "podGroup", group.key.Namespace+"/"+group.key.Name, "members", len(group.members))
			held = true
			continue
		}
		if !relocationGroupReady(&group, retainedByPodGroup, arrivedByRelocation) {
			klog.V(3).InfoS("repack: HyperNode-constrained gang not fully ready; unit held",
				"run", run.Name, "podGroup", group.key.Namespace+"/"+group.key.Name,
				"arrived", len(group.members), "undecided", len(retainedByPodGroup[group.key]))
			held = true
			continue
		}
		unitTasks := make([]*schedapi.TaskInfo, 0, len(group.members))
		unitMemberByTask := make(map[schedapi.TaskID]*relocationCandidate, len(group.members))
		for memberIndex := range group.members {
			member := &group.members[memberIndex]
			unitTasks = append(unitTasks, member.task)
			unitMemberByTask[member.task.UID] = member
		}
		receivers := unitReceiverUnion(nodes, excludedFreedNodes, group.members)
		// Reproduce the plan's own domain first: a whole ==true unit is retried on
		// the HyperNode holding every member's planned receiver, so a still-feasible
		// plan does not drift to the first gradient domain that fits. The full
		// re-search below stays the fallback when that domain no longer hosts it.
		placements, fit := ([]*engineapi.Move)(nil), false
		if planReceivers := unitPlannedDomainReceivers(receivers, realNodes, hyperNodesByTier, group.members); planReceivers != nil {
			placements, fit = snapshot.FeasibleRelocation(ctx, committed, unitTasks, planReceivers)
		}
		if !fit || len(placements) != len(unitTasks) {
			placements, fit = snapshot.FeasibleRelocation(ctx, committed, unitTasks, receivers)
		}
		if !fit || len(placements) != len(unitTasks) {
			klog.V(3).InfoS("repack: HyperNode-constrained gang has no whole-group receiver; unit held",
				"run", run.Name, "podGroup", group.key.Namespace+"/"+group.key.Name, "members", len(group.members))
			held = true
			continue
		}
		for _, placement := range placements {
			member, found := unitMemberByTask[placement.Task.UID]
			if !found {
				continue
			}
			committed = append(committed, placement)
			selected[placementexecutor.IdentityForRelocation(member.relocation)] = placement.To
			klog.V(4).InfoS("repack: whole HyperNode-constrained gang placed on a shared receiver domain",
				"run", run.Name, "podGroup", group.key.Namespace+"/"+group.key.Name,
				"pod", member.relocation.Namespace+"/"+member.relocation.Placement.ReplacementPodName,
				"plannedNode", member.relocation.PlannedNodeName, "selectedNode", placement.To)
		}
	}

	// ==false members: legacy per-Pod greedy first fit; an unplaceable Pod
	// defers only itself.
	for _, member := range arrived {
		if member.hyperNodeAllocated {
			continue // part of a ==true PodGroup unit handled above
		}
		// A restarted Engine may reconcile before its node cache drains; requeue
		// until the planned receiver is visible rather than silently pick another.
		if relocation := member.relocation; relocation.PlannedNodeName != "" {
			if _, plannedVisible := nodeNames[relocation.PlannedNodeName]; !plannedVisible {
				klog.V(3).InfoS("repack: planned receiver node not yet visible in snapshot; placement requeued",
					"run", run.Name, "pod", relocation.Namespace+"/"+relocation.Placement.ReplacementPodName,
					"plannedNode", relocation.PlannedNodeName, "snapshotNodeCount", len(nodes))
				held = true
				continue
			}
		}
		receivers := placementexecutor.Receivers(nodes, excludedFreedNodes, member.relocation.PlannedNodeName, member.task)
		klog.V(4).InfoS("repack: replacement receiver candidates evaluated",
			"run", run.Name, "pod", member.relocation.Namespace+"/"+member.relocation.Placement.ReplacementPodName,
			"plannedNode", member.relocation.PlannedNodeName, "receiverCount", len(receivers))
		placements, fit := snapshot.FeasibleRelocation(ctx, committed, []*schedapi.TaskInfo{member.task}, receivers)
		if !fit || len(placements) != 1 {
			klog.V(3).InfoS("repack: replacement is waiting for a feasible receiver node",
				"run", run.Name, "pod", member.relocation.Namespace+"/"+member.relocation.Placement.ReplacementPodName,
				"plannedNode", member.relocation.PlannedNodeName, "receiverCount", len(receivers))
			held = true
			continue
		}
		committed = append(committed, placements[0])
		selected[placementexecutor.IdentityForRelocation(member.relocation)] = placements[0].To
		klog.V(4).InfoS("repack: replacement receiver selected in scheduler simulation",
			"run", run.Name, "pod", member.relocation.Namespace+"/"+member.relocation.Placement.ReplacementPodName,
			"plannedNode", member.relocation.PlannedNodeName, "selectedNode", placements[0].To)
	}
	if len(selected) > 0 {
		if err := e.writePlacementSelection(ctx, run.Name, selected); err != nil {
			return runtimeError(err)
		}
		if !held {
			return engineframework.RuntimeResult{}
		}
	}
	if err := e.markWaitingForNodeSelection(ctx, run.Name, pending); err != nil {
		return runtimeError(err)
	}
	return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
}

func hasTimedOutPlacement(run *repackv1alpha1.RepackRun) bool {
	if run == nil {
		return false
	}
	for index := range run.Status.Relocations {
		if run.Status.Relocations[index].Placement.Phase == repackv1alpha1.PodPlacementTimedOut {
			return true
		}
	}
	return false
}

func (e *Engine) writePlacementSelection(
	ctx context.Context,
	runName string,
	selected map[placementexecutor.Identity]string,
) error {
	var updatedRun *repackv1alpha1.RepackRun
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range run.Status.Relocations {
			relocation := &run.Status.Relocations[index]
			if node, found := selected[placementexecutor.IdentityForRelocation(relocation)]; found && relocation.Placement.SelectedNodeName == "" {
				relocation.Placement.SelectedNodeName = node
				changed = true
			}
		}
		if !changed {
			return nil
		}
		updatedRun, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
	if err != nil || updatedRun == nil {
		return err
	}
	klog.V(3).InfoS("repack: live replacement receivers persisted",
		"run", runName, "selectionCount", len(selected))
	e.recordRunEvent(updatedRun, v1.EventTypeNormal, eventReasonPlacementSelected,
		fmt.Sprintf("Selected live receiver nodes for %d replacement Pods.", len(selected)))
	return nil
}

func (e *Engine) markWaitingForNodeSelection(ctx context.Context, runName string, relocations []*repackv1alpha1.PodRelocationStatus) error {
	keys := make(map[placementexecutor.Identity]struct{}, len(relocations))
	for _, relocation := range relocations {
		keys[placementexecutor.IdentityForRelocation(relocation)] = struct{}{}
	}
	var updatedRun *repackv1alpha1.RepackRun
	placementStateChanged := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range run.Status.Relocations {
			relocation := &run.Status.Relocations[index]
			if _, found := keys[placementexecutor.IdentityForRelocation(relocation)]; found && relocation.Placement.SelectedNodeName == "" && relocation.Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection {
				relocation.Placement.Phase = repackv1alpha1.PodPlacementWaitingForNodeSelection
				changed = true
				placementStateChanged = true
			}
		}
		if state.MarkRunning(run, state.ReasonReconcilingPlacements,
			enginestatus.PlacementProgressMessage(run, engineconf.ResolveResource(run, e.config.DefaultResource))) {
			changed = true
		}
		if !changed {
			return nil
		}
		updatedRun, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
	if err == nil && placementStateChanged && updatedRun != nil {
		message := enginestatus.PlacementProgressMessage(updatedRun, engineconf.ResolveResource(updatedRun, e.config.DefaultResource))
		klog.V(3).InfoS("repack: replacement placement waiting for node selection",
			"run", runName, "pendingCount", len(relocations), "retryAfter", placementRetryInterval)
		e.recordRunEvent(updatedRun, v1.EventTypeWarning, eventReasonWaitingForNodeSelection, message)
	}
	return err
}

// relocationCandidate is one live, unbound replacement Pod resolved this pass.
// hyperNodeAllocated mirrors the adapter's gangUnit.requiresHyperNodeAllocate.
type relocationCandidate struct {
	relocation         *repackv1alpha1.PodRelocationStatus
	task               *schedapi.TaskInfo
	hyperNodeAllocated bool
}

// relocationGroup is the arrived subset of one PodGroup; members share the same
// Job, hence the same ==true classification.
type relocationGroup struct {
	key                types.NamespacedName
	members            []relocationCandidate
	hyperNodeAllocated bool
}

// groupRelocationCandidates buckets arrived members by PodGroup in first-encounter order.
func groupRelocationCandidates(arrived []relocationCandidate) []relocationGroup {
	groups := make([]relocationGroup, 0)
	indexByKey := make(map[types.NamespacedName]int, len(arrived))
	for _, member := range arrived {
		key := types.NamespacedName{Namespace: member.relocation.Namespace, Name: member.relocation.PodGroupName}
		if groupIndex, found := indexByKey[key]; found {
			groups[groupIndex].members = append(groups[groupIndex].members, member)
			continue
		}
		indexByKey[key] = len(groups)
		groups = append(groups, relocationGroup{
			key:                key,
			hyperNodeAllocated: member.hyperNodeAllocated,
			members:            []relocationCandidate{member},
		})
	}
	return groups
}

// retainedRelocationsByPodGroup returns, per PodGroup, the durable journal
// members still undecided (no node selected, not terminal) — the whole expected
// unit set, read fresh every pass.
func retainedRelocationsByPodGroup(run *repackv1alpha1.RepackRun) map[types.NamespacedName][]*repackv1alpha1.PodRelocationStatus {
	result := make(map[types.NamespacedName][]*repackv1alpha1.PodRelocationStatus)
	if run == nil {
		return result
	}
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if relocation.Placement.SelectedNodeName != "" {
			continue
		}
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementWaitingForReplacement,
			repackv1alpha1.PodPlacementWaitingForNodeSelection,
			repackv1alpha1.PodPlacementNominated:
		default:
			continue
		}
		key := types.NamespacedName{Namespace: relocation.Namespace, Name: relocation.PodGroupName}
		result[key] = append(result[key], relocation)
	}
	return result
}

// relocationGroupReady reports whether every still-undecided journal member
// arrived this pass. Never place a subset: while any member is missing the
// whole unit waits, bounded by the run's ExecutionDeadline.
func relocationGroupReady(
	group *relocationGroup,
	retained map[types.NamespacedName][]*repackv1alpha1.PodRelocationStatus,
	arrivedByRelocation map[placementexecutor.Identity]*relocationCandidate,
) bool {
	if group == nil {
		return false
	}
	for _, relocation := range retained[group.key] {
		if _, found := arrivedByRelocation[placementexecutor.IdentityForRelocation(relocation)]; !found {
			return false
		}
	}
	return true
}

// unitReceiverUnion is the deterministic union of each member's eligible
// receivers (immediately idle, not freed by this Run) — a superset the
// whole-unit domain trial narrows to the trial domain's nodes.
func unitReceiverUnion(nodes []*schedapi.NodeInfo, excludedFreedNodes []string, members []relocationCandidate) []*schedapi.NodeInfo {
	byName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, node := range nodes {
		if node != nil {
			byName[node.Name] = node
		}
	}
	included := make(map[string]struct{}, len(nodes))
	for _, member := range members {
		for _, receiver := range placementexecutor.Receivers(nodes, excludedFreedNodes, member.relocation.PlannedNodeName, member.task) {
			included[receiver.Name] = struct{}{}
		}
	}
	result := make([]*schedapi.NodeInfo, 0, len(included))
	for name := range included {
		if node, found := byName[name]; found {
			result = append(result, node)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Name < result[right].Name
	})
	return result
}

// podGroupPlannedNodesVisible holds a unit until every planned receiver is
// visible in the snapshot (a restarted Engine can reconcile before its node
// cache drains, and committing then would silently pick a different receiver).
func podGroupPlannedNodesVisible(members []relocationCandidate, nodeNames map[string]struct{}) bool {
	for _, member := range members {
		if plannedNode := member.relocation.PlannedNodeName; plannedNode != "" {
			if _, visible := nodeNames[plannedNode]; !visible {
				return false
			}
		}
	}
	return true
}

// unitPlannedDomainReceivers narrows the whole-unit receiver union to the plan's
// own HyperNode: the smallest one whose real-node set still covers every member's
// planned receiver. A ==true unit is planned co-located in one domain, so retrying
// it there first reproduces the plan while that domain can still host it. nil when
// no planned receiver resolves to a domain or that domain has no eligible receiver.
func unitPlannedDomainReceivers(
	receivers []*schedapi.NodeInfo,
	realNodes map[string]sets.Set[string],
	hyperNodesByTier map[int]sets.Set[string],
	members []relocationCandidate,
) []*schedapi.NodeInfo {
	planned := sets.New[string]()
	for _, member := range members {
		if node := member.relocation.PlannedNodeName; node != "" {
			planned.Insert(node)
		}
	}
	if planned.Len() == 0 {
		return nil
	}
	domain := smallestHyperNodeCovering(realNodes, hyperNodesByTier, planned)
	if domain == "" {
		return nil
	}
	domainNodes := realNodes[domain]
	byName := make(map[string]*schedapi.NodeInfo, len(receivers))
	for _, receiver := range receivers {
		byName[receiver.Name] = receiver
	}
	restricted := make([]*schedapi.NodeInfo, 0, len(receivers))
	added := sets.New[string]()
	for _, member := range members { // planned receivers first, in member order
		if node := member.relocation.PlannedNodeName; node != "" && domainNodes.Has(node) {
			if receiver, found := byName[node]; found && !added.Has(node) {
				restricted = append(restricted, receiver)
				added.Insert(node)
			}
		}
	}
	for _, receiver := range receivers { // then the domain's other eligible receivers
		if domainNodes.Has(receiver.Name) && !added.Has(receiver.Name) {
			restricted = append(restricted, receiver)
		}
	}
	if len(restricted) == 0 {
		return nil // the plan domain currently has no eligible receiver; fall back to the full re-search
	}
	return restricted
}

func smallestHyperNodeCovering(realNodes map[string]sets.Set[string], hyperNodesByTier map[int]sets.Set[string], planned sets.Set[string]) string {
	best := ""
	bestTier := math.MaxInt
	for name, nodes := range realNodes {
		if name == schedframework.ClusterTopHyperNode || !nodes.IsSuperset(planned) {
			continue
		}
		tier := tierOfHyperNode(hyperNodesByTier, name)
		if best == "" || nodes.Len() < len(realNodes[best]) ||
			(nodes.Len() == len(realNodes[best]) && (tier < bestTier || (tier == bestTier && name < best))) {
			best, bestTier = name, tier
		}
	}
	return best
}

func tierOfHyperNode(hyperNodesByTier map[int]sets.Set[string], name string) int {
	for tier, row := range hyperNodesByTier {
		if row.Has(name) {
			return tier
		}
	}
	return math.MaxInt
}
