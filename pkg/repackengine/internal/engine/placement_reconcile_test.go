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
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

// reconcileRelocation builds a durable journal relocation. WaitingForNodeSelection
// members carry a concrete replacement Pod so Candidates() can select them.
func reconcileRelocation(namespace, podGroup, victim string, phase repackv1alpha1.PodPlacementPhase, selectedNode string) *repackv1alpha1.PodRelocationStatus {
	placementStatus := repackv1alpha1.PodPlacementStatus{Phase: phase, SelectedNodeName: selectedNode}
	if phase == repackv1alpha1.PodPlacementWaitingForNodeSelection {
		placementStatus.ReplacementPodName = "replacement-" + victim
		placementStatus.ReplacementPodUID = types.UID("uid-" + victim)
	}
	return &repackv1alpha1.PodRelocationStatus{
		Namespace:       namespace,
		PodGroupName:    podGroup,
		VictimPodName:   victim,
		PlannedNodeName: "target-" + victim,
		Placement:       placementStatus,
	}
}

func victimNames(relocations []*repackv1alpha1.PodRelocationStatus) []string {
	names := make([]string, 0, len(relocations))
	for _, relocation := range relocations {
		if relocation != nil {
			names = append(names, relocation.VictimPodName)
		}
	}
	return names
}

// TestRetainedRelocationsByPodGroup: only undecided journal members (no
// SelectedNodeName, non-terminal phase) count, grouped per PodGroup.
func TestRetainedRelocationsByPodGroup(t *testing.T) {
	run := &repackv1alpha1.RepackRun{}
	run.Status.Relocations = []repackv1alpha1.PodRelocationStatus{
		// PodGroup gA: a1+a2 still undecided, a3 finished, a4 has a selection.
		*reconcileRelocation("ns", "gA", "a1", repackv1alpha1.PodPlacementWaitingForReplacement, ""),
		*reconcileRelocation("ns", "gA", "a2", repackv1alpha1.PodPlacementWaitingForNodeSelection, ""),
		*reconcileRelocation("ns", "gA", "a3", repackv1alpha1.PodPlacementPlaced, "target-a3"),
		*reconcileRelocation("ns", "gA", "a4", repackv1alpha1.PodPlacementWaitingForNodeSelection, "target-a4"),
		// PodGroup gB: b1 nominated (still undecided), b2 timed out.
		*reconcileRelocation("ns", "gB", "b1", repackv1alpha1.PodPlacementNominated, ""),
		*reconcileRelocation("ns", "gB", "b2", repackv1alpha1.PodPlacementTimedOut, ""),
	}

	retained := retainedRelocationsByPodGroup(run)
	if len(retained) != 2 {
		t.Fatalf("retained PodGroup count = %d, want 2", len(retained))
	}
	gA := retained[types.NamespacedName{Namespace: "ns", Name: "gA"}]
	gB := retained[types.NamespacedName{Namespace: "ns", Name: "gB"}]
	if names := victimNames(gA); !reflect.DeepEqual(names, []string{"a1", "a2"}) {
		t.Errorf("gA retained victims = %v, want [a1 a2]", names)
	}
	if names := victimNames(gB); !reflect.DeepEqual(names, []string{"b1"}) {
		t.Errorf("gB retained victims = %v, want [b1]", names)
	}
}

// TestRelocationGroupReady pins whole-or-nothing: a ==true group is ready only
// when every undecided journal member arrived; a missing member blocks the unit
// (never a subset).
func TestRelocationGroupReady(t *testing.T) {
	gA := types.NamespacedName{Namespace: "ns", Name: "gA"}
	a1 := reconcileRelocation("ns", "gA", "a1", repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
	a2 := reconcileRelocation("ns", "gA", "a2", repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
	a3 := reconcileRelocation("ns", "gA", "a3", repackv1alpha1.PodPlacementWaitingForReplacement, "")
	group := &relocationGroup{key: gA, hyperNodeAllocated: true, members: []relocationCandidate{
		{relocation: a1, task: nil, hyperNodeAllocated: true},
		{relocation: a2, task: nil, hyperNodeAllocated: true},
	}}

	arrivedFor := func(relocations ...*repackv1alpha1.PodRelocationStatus) map[placementexecutor.Identity]*relocationCandidate {
		arrived := make(map[placementexecutor.Identity]*relocationCandidate, len(relocations))
		for _, relocation := range relocations {
			arrived[placementexecutor.IdentityForRelocation(relocation)] = &relocationCandidate{relocation: relocation}
		}
		return arrived
	}

	// Whole unit arrived: gate opens.
	retainedAllArrived := map[types.NamespacedName][]*repackv1alpha1.PodRelocationStatus{
		gA: {a1, a2},
	}
	if !relocationGroupReady(group, retainedAllArrived, arrivedFor(a1, a2)) {
		t.Errorf("group with all retained members arrived should be ready")
	}

	// Sibling still waiting for replacement (a3): gate must block the whole unit.
	retainedWithMissingSibling := map[types.NamespacedName][]*repackv1alpha1.PodRelocationStatus{
		gA: {a1, a2, a3},
	}
	if relocationGroupReady(group, retainedWithMissingSibling, arrivedFor(a1, a2)) {
		t.Errorf("group with an un-arrived retained member must NOT be ready (never place a subset)")
	}

	// No durable members still undecided: nothing to wait for.
	if !relocationGroupReady(group, map[types.NamespacedName][]*repackv1alpha1.PodRelocationStatus{}, arrivedFor(a1, a2)) {
		t.Errorf("group with no retained members should be ready")
	}

	// Nil group is never ready.
	if relocationGroupReady(nil, retainedAllArrived, arrivedFor(a1, a2)) {
		t.Errorf("nil group must not be ready")
	}
}

// TestGroupRelocationCandidates: arrived members bucket by PodGroup in
// first-encounter order; ==true classification propagates from members.
func TestGroupRelocationCandidates(t *testing.T) {
	a1 := reconcileRelocation("ns", "gA", "a1", repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
	a2 := reconcileRelocation("ns", "gA", "a2", repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
	b1 := reconcileRelocation("ns", "gB", "b1", repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
	arrived := []relocationCandidate{
		{relocation: a1, hyperNodeAllocated: true},
		{relocation: b1, hyperNodeAllocated: false},
		{relocation: a2, hyperNodeAllocated: true},
	}

	groups := groupRelocationCandidates(arrived)
	if len(groups) != 2 {
		t.Fatalf("group count = %d, want 2", len(groups))
	}
	if groups[0].key != (types.NamespacedName{Namespace: "ns", Name: "gA"}) || !groups[0].hyperNodeAllocated {
		t.Errorf("groups[0] = %+v, want gA HyperNode-constrained", groups[0])
	}
	if got := victimNames([]*repackv1alpha1.PodRelocationStatus{groups[0].members[0].relocation, groups[0].members[1].relocation}); !reflect.DeepEqual(got, []string{"a1", "a2"}) {
		t.Errorf("gA members = %v, want [a1 a2]", got)
	}
	if groups[1].key != (types.NamespacedName{Namespace: "ns", Name: "gB"}) || groups[1].hyperNodeAllocated {
		t.Errorf("groups[1] = %+v, want gB non-HyperNode-constrained", groups[1])
	}
	if got := groups[1].members[0].relocation.VictimPodName; got != "b1" {
		t.Errorf("gB member = %s, want b1", got)
	}
}

// TestPodGroupPlannedNodesVisible: a unit is held while any member's planned
// receiver is absent from the scheduler snapshot.
func TestPodGroupPlannedNodesVisible(t *testing.T) {
	member := func(victim, plannedNode string) relocationCandidate {
		relocation := reconcileRelocation("ns", "gA", victim, repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
		relocation.PlannedNodeName = plannedNode
		return relocationCandidate{relocation: relocation, hyperNodeAllocated: true}
	}
	allVisible := map[string]struct{}{"target-a1": {}, "target-a2": {}}

	if !podGroupPlannedNodesVisible([]relocationCandidate{member("a1", "target-a1"), member("a2", "target-a2")}, allVisible) {
		t.Errorf("unit with all planned receivers visible should pass the gate")
	}
	if podGroupPlannedNodesVisible([]relocationCandidate{member("a1", "target-a1"), member("a2", "target-a3")}, allVisible) {
		t.Errorf("unit with an invisible planned receiver must NOT pass the gate")
	}
	// A member with no planned receiver (pending workload) does not block.
	relocation := reconcileRelocation("ns", "gA", "a3", repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
	relocation.PlannedNodeName = ""
	if !podGroupPlannedNodesVisible([]relocationCandidate{{relocation: relocation, hyperNodeAllocated: true}}, allVisible) {
		t.Errorf("member with empty planned receiver should not block the unit")
	}
}

// TestUnitReceiverUnion: deterministic union of each member's immediately-idle
// receivers, freed nodes excluded and deduped.
func TestUnitReceiverUnion(t *testing.T) {
	resource := v1.ResourceName("example.com/accelerator")
	resourceOf := func(quantity int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resource: float64(quantity)}}
	}
	node := func(name string, capacity int64) *schedapi.NodeInfo {
		return &schedapi.NodeInfo{Name: name, Allocatable: resourceOf(capacity), Used: resourceOf(0), Idle: resourceOf(capacity)}
	}
	nodes := []*schedapi.NodeInfo{
		node("planned-a1", 2),
		node("planned-a2", 2),
		node("freeing", 8),
		node("too-small", 1),
		node("alternative", 4),
	}
	receiverTask := func(victim, plannedNode string) relocationCandidate {
		relocation := reconcileRelocation("ns", "gA", victim, repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
		relocation.PlannedNodeName = plannedNode
		return relocationCandidate{
			relocation:         relocation,
			task:               &schedapi.TaskInfo{InitResreq: resourceOf(2)},
			hyperNodeAllocated: true,
		}
	}

	receivers := unitReceiverUnion(nodes, []string{"freeing"},
		[]relocationCandidate{receiverTask("a1", "planned-a1"), receiverTask("a2", "planned-a2")})
	names := make([]string, 0, len(receivers))
	for _, receiver := range receivers {
		names = append(names, receiver.Name)
	}
	// Freed node excluded, too-small excluded, deduped union sorted by name.
	if want := []string{"alternative", "planned-a1", "planned-a2"}; !reflect.DeepEqual(names, want) {
		t.Errorf("receiver names = %v, want %v", names, want)
	}
}

// TestUnitPlannedDomainReceivers: the plan-domain retry narrows the receiver
// union to the smallest HyperNode covering every member's planned receiver
// (planned receivers first), nil when none is resolvable. Same geometry as the
// E21 tree: rt-s0 {n0,n1}, rt-s1 {n2,n3}, rt-s2 aggregates both at tier 2.
func TestUnitPlannedDomainReceivers(t *testing.T) {
	receiver := func(name string) *schedapi.NodeInfo {
		return &schedapi.NodeInfo{Name: name}
	}
	member := func(victim, plannedNode string) relocationCandidate {
		relocation := reconcileRelocation("ns", "gA", victim, repackv1alpha1.PodPlacementWaitingForNodeSelection, "")
		relocation.PlannedNodeName = plannedNode
		return relocationCandidate{relocation: relocation, hyperNodeAllocated: true}
	}
	names := func(nodes []*schedapi.NodeInfo) []string {
		out := make([]string, 0, len(nodes))
		for _, node := range nodes {
			out = append(out, node.Name)
		}
		return out
	}
	realNodes := map[string]sets.Set[string]{
		"rt-s0":                            sets.New("n0", "n1"),
		"rt-s1":                            sets.New("n2", "n3"),
		"rt-s2":                            sets.New("n0", "n1", "n2", "n3"),
		schedframework.ClusterTopHyperNode: sets.New("n0", "n1", "n2", "n3"), // never a retry domain
	}
	tiers := map[int]sets.Set[string]{
		1: sets.New("rt-s0", "rt-s1"),
		2: sets.New("rt-s2"),
	}

	// Whole unit planned on n3: the retry keeps rt-s1's idle receivers only,
	// planned node first — a still-feasible plan is reproduced, not drifted.
	receivers := []*schedapi.NodeInfo{receiver("n0"), receiver("n2"), receiver("n3")}
	got := unitPlannedDomainReceivers(receivers, realNodes, tiers,
		[]relocationCandidate{member("a1", "n3"), member("a2", "n3")})
	if want := []string{"n3", "n2"}; !reflect.DeepEqual(names(got), want) {
		t.Errorf("planned-n3 receivers = %v, want %v", names(got), want)
	}

	// Exact planned node no longer eligible, a domain sibling is: still rt-s1,
	// never drifting to rt-s0's n0.
	got = unitPlannedDomainReceivers([]*schedapi.NodeInfo{receiver("n2")}, realNodes, tiers,
		[]relocationCandidate{member("a1", "n3")})
	if want := []string{"n2"}; !reflect.DeepEqual(names(got), want) {
		t.Errorf("domain-sibling fallback = %v, want %v", names(got), want)
	}

	// Plan spread across rt-s0 and rt-s1 (a tier-2 co-location): the smallest
	// covering HyperNode is rt-s2, both planned nodes kept first.
	allReceivers := []*schedapi.NodeInfo{receiver("n0"), receiver("n1"), receiver("n2"), receiver("n3")}
	got = unitPlannedDomainReceivers(allReceivers, realNodes, tiers,
		[]relocationCandidate{member("a1", "n0"), member("a2", "n3")})
	if want := []string{"n0", "n3", "n1", "n2"}; !reflect.DeepEqual(names(got), want) {
		t.Errorf("cross-domain planned receivers = %v, want %v", names(got), want)
	}

	// No planned receiver, or one outside the topology: nil -> the caller keeps
	// the full union (no narrower retry).
	if got := unitPlannedDomainReceivers(receivers, realNodes, tiers,
		[]relocationCandidate{member("a1", "")}); got != nil {
		t.Errorf("empty planned receiver must yield nil, got %v", names(got))
	}
	if got := unitPlannedDomainReceivers(receivers, realNodes, tiers,
		[]relocationCandidate{member("a1", "elsewhere")}); got != nil {
		t.Errorf("planned receiver outside the topology must yield nil, got %v", names(got))
	}
}
