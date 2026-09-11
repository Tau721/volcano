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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

// gatedReplacement builds a journal entry the nominator has already claimed: it
// carries a concrete replacement Pod and waits for a node selection, so
// Candidates() returns exactly these.
func gatedReplacement(podGroup, victim, plannedNode string) repackv1alpha1.PodRelocationStatus {
	return repackv1alpha1.PodRelocationStatus{
		Namespace:       "ns",
		PodGroupName:    podGroup,
		VictimPodName:   victim,
		PlannedNodeName: plannedNode,
		Eviction:        repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted},
		Placement: repackv1alpha1.PodPlacementStatus{
			Phase:              repackv1alpha1.PodPlacementWaitingForNodeSelection,
			ReplacementPodName: "replacement-" + victim,
			ReplacementPodUID:  types.UID("uid-" + victim),
		},
	}
}

func placementRun(relocations ...repackv1alpha1.PodRelocationStatus) *repackv1alpha1.RepackRun {
	run := &repackv1alpha1.RepackRun{ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")}}
	run.Status.Relocations = relocations
	return run
}

// placementEngine builds an Engine with no cluster cache at all: reconcilePlacement
// must nominate from the durable plan without touching live cluster state, so any
// attempt to open a scheduler session or read a Pod would panic here.
func placementEngine(run *repackv1alpha1.RepackRun) (*Engine, *vcfake.Clientset) {
	client := vcfake.NewSimpleClientset(run.DeepCopy())
	return &Engine{volcanoClient: client, now: func() time.Time { return time.Unix(100, 0) }}, client
}

func relocationSelections(t *testing.T, client *vcfake.Clientset) map[string]string {
	t.Helper()
	run, err := client.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	selections := make(map[string]string, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		selections[relocation.VictimPodName] = relocation.Placement.SelectedNodeName
	}
	return selections
}

// TestReconcilePlacementNominatesThePlannedNodeVerbatim pins "the plan is the
// selection": every candidate gets its own PlannedNodeName, never a substitute.
// No snapshot is available to the engine, so a planned node that has since been
// filled or deleted cannot change the outcome — the scheduler owns validation.
func TestReconcilePlacementNominatesThePlannedNodeVerbatim(t *testing.T) {
	run := placementRun(
		gatedReplacement("gA", "a1", "target-a1"),
		gatedReplacement("gA", "a2", "target-a2"),
	)
	engine, client := placementEngine(run)

	result := engine.reconcilePlacement(context.Background(), run)
	if result.Err != nil {
		t.Fatalf("reconcilePlacement() error = %v", result.Err)
	}

	want := map[string]string{"a1": "target-a1", "a2": "target-a2"}
	if got := relocationSelections(t, client); !equalSelections(got, want) {
		t.Fatalf("selections = %v, want %v", got, want)
	}
}

// TestReconcilePlacementLeavesTheNominationToTheController: the engine writes
// only SelectedNodeName. Phase stays WaitingForNodeSelection for the nominator
// to advance, and an already-nominated peer is not rewritten.
func TestReconcilePlacementLeavesTheNominationToTheController(t *testing.T) {
	settled := gatedReplacement("gA", "settled", "target-settled")
	settled.Placement.Phase = repackv1alpha1.PodPlacementNominated
	settled.Placement.SelectedNodeName = "target-settled"
	run := placementRun(settled, gatedReplacement("gA", "pending", "target-pending"))
	engine, client := placementEngine(run)

	if result := engine.reconcilePlacement(context.Background(), run); result.Err != nil {
		t.Fatalf("reconcilePlacement() error = %v", result.Err)
	}

	updated, err := client.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byVictim := make(map[string]repackv1alpha1.PodPlacementStatus, len(updated.Status.Relocations))
	for index := range updated.Status.Relocations {
		relocation := &updated.Status.Relocations[index]
		byVictim[relocation.VictimPodName] = relocation.Placement
	}
	if got := byVictim["pending"]; got.SelectedNodeName != "target-pending" ||
		got.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection {
		t.Errorf("pending placement = %+v, want selection written while still awaiting nomination", got)
	}
	if got := byVictim["settled"]; got.SelectedNodeName != "target-settled" ||
		got.Phase != repackv1alpha1.PodPlacementNominated {
		t.Errorf("settled placement = %+v, want untouched", got)
	}
}

// TestReconcilePlacementWaitsForClaimedReplacements: an unclaimed relocation has
// no replacement Pod yet, so nothing may be written and the run polls until its
// ExecutionDeadline.
func TestReconcilePlacementWaitsForClaimedReplacements(t *testing.T) {
	run := placementRun(gatedReplacement("gA", "a1", "target-a1"))
	run.Status.Relocations[0].Placement = repackv1alpha1.PodPlacementStatus{
		Phase: repackv1alpha1.PodPlacementWaitingForReplacement,
	}
	engine, client := placementEngine(run)

	result := engine.reconcilePlacement(context.Background(), run)
	if result.Err != nil {
		t.Fatalf("reconcilePlacement() error = %v", result.Err)
	}
	if result.RequeueAfter != placementRetryInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, placementRetryInterval)
	}
	if got := relocationSelections(t, client); got["a1"] != "" {
		t.Errorf("selectedNodeName = %q, want no nomination before a replacement Pod is claimed", got["a1"])
	}
}

// TestReconcilePlacementCapsWaitingAtExecutionDeadline: the polling requeue never
// outlives the run's durable deadline, so an unclaimed replacement cannot bypass
// the expiration escape hatch.
func TestReconcilePlacementCapsWaitingAtExecutionDeadline(t *testing.T) {
	run := placementRun(gatedReplacement("gA", "a1", "target-a1"))
	run.Status.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementWaitingForReplacement
	deadline := metav1.NewTime(time.Unix(101, 0))
	run.Status.ExecutionDeadline = &deadline
	engine, _ := placementEngine(run)

	result := engine.reconcilePlacement(context.Background(), run)
	if result.Err != nil {
		t.Fatalf("reconcilePlacement() error = %v", result.Err)
	}
	if want := time.Second; result.RequeueAfter != want {
		t.Errorf("RequeueAfter = %v, want %v (capped at the deadline)", result.RequeueAfter, want)
	}
}

func equalSelections(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for victim, node := range want {
		if got[victim] != node {
			return false
		}
	}
	return true
}
