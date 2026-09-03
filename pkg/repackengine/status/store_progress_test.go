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

package status_test

import (
	"testing"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

func TestMergeRelocationProgressPreservesControllerOwnedPlacementPhase(t *testing.T) {
	desired := []repackv1alpha1.PodRelocationStatus{{Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}}}
	latest := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection,
			ReplacementPodName: "replacement", ReplacementPodUID: "replacement-uid"},
	}}
	enginestatus.MergeRelocationProgress(desired, latest)
	if desired[0].Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection {
		t.Fatalf("phase=%q, want Gated", desired[0].Placement.Phase)
	}
	if desired[0].Placement.ReplacementPodName != "replacement" || desired[0].Placement.ReplacementPodUID != "replacement-uid" {
		t.Fatalf("replacement identity=%q/%q, want replacement/replacement-uid", desired[0].Placement.ReplacementPodName, desired[0].Placement.ReplacementPodUID)
	}
}

func TestMergeRelocationProgressAllowsEngineOwnedSamePhaseMessage(t *testing.T) {
	desired := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n",
		VictimPodUID: "uid", Eviction: repackv1alpha1.PodEvictionStatus{
			Phase: repackv1alpha1.PodEvictionInProgress, Message: "PDB temporarily blocked eviction",
		},
	}}
	latest := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n",
		VictimPodUID: "uid", Eviction: repackv1alpha1.PodEvictionStatus{
			Phase: repackv1alpha1.PodEvictionInProgress, Message: "durable intent",
		},
	}}

	enginestatus.MergeRelocationProgress(desired, latest)
	if desired[0].Eviction.Message != "PDB temporarily blocked eviction" {
		t.Fatalf("message=%q, want current batch outcome", desired[0].Eviction.Message)
	}
}

func TestMergeRelocationProgressDoesNotOverwriteTerminalPlacementWithOlderObservation(t *testing.T) {
	for _, olderPhase := range []repackv1alpha1.PodPlacementPhase{
		repackv1alpha1.PodPlacementWaitingForReplacement,
		repackv1alpha1.PodPlacementWaitingForNodeSelection,
		repackv1alpha1.PodPlacementNominated,
	} {
		t.Run(string(olderPhase), func(t *testing.T) {
			desired := []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n",
				Placement: repackv1alpha1.PodPlacementStatus{
					Phase: repackv1alpha1.PodPlacementTimedOut,
				},
			}}
			latest := []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n",
				Placement: repackv1alpha1.PodPlacementStatus{
					Phase:              olderPhase,
					ReplacementPodName: "replacement",
					ReplacementPodUID:  "replacement-uid",
				},
			}}

			enginestatus.MergeRelocationProgress(desired, latest)
			if desired[0].Placement.Phase != repackv1alpha1.PodPlacementTimedOut {
				t.Fatalf("phase=%q, want TimedOut", desired[0].Placement.Phase)
			}
		})
	}
}

func TestMergeRelocationProgressPreservesConcurrentTerminalPlacement(t *testing.T) {
	desired := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n",
		Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementTimedOut},
	}}
	latest := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "g", VictimPodName: "p", PlannedNodeName: "n",
		Placement: repackv1alpha1.PodPlacementStatus{
			Phase: repackv1alpha1.PodPlacementPlaced, ReplacementPodName: "replacement",
			ReplacementPodUID: "uid", ActualNodeName: "node-a",
		},
	}}

	enginestatus.MergeRelocationProgress(desired, latest)
	if desired[0].Placement.Phase != repackv1alpha1.PodPlacementPlaced ||
		desired[0].Placement.ActualNodeName != "node-a" {
		t.Fatalf("placement=%+v, want concurrent Placed result", desired[0].Placement)
	}
}

func TestPlacementPhaseAdvances(t *testing.T) {
	tests := []struct {
		name              string
		current, observed repackv1alpha1.PodPlacementPhase
		want              bool
	}{
		{name: "initial observation", observed: repackv1alpha1.PodPlacementWaitingForReplacement, want: true},
		{name: "replacement identified", current: repackv1alpha1.PodPlacementWaitingForReplacement, observed: repackv1alpha1.PodPlacementWaitingForNodeSelection, want: true},
		{name: "skipped directly to placed", current: repackv1alpha1.PodPlacementWaitingForReplacement, observed: repackv1alpha1.PodPlacementPlaced, want: true},
		{name: "node nominated", current: repackv1alpha1.PodPlacementWaitingForNodeSelection, observed: repackv1alpha1.PodPlacementNominated, want: true},
		{name: "nomination timed out", current: repackv1alpha1.PodPlacementNominated, observed: repackv1alpha1.PodPlacementTimedOut, want: true},
		{name: "same phase is not advancement", current: repackv1alpha1.PodPlacementNominated, observed: repackv1alpha1.PodPlacementNominated},
		{name: "cannot move backward", current: repackv1alpha1.PodPlacementNominated, observed: repackv1alpha1.PodPlacementWaitingForNodeSelection},
		{name: "placed cannot become timed out", current: repackv1alpha1.PodPlacementPlaced, observed: repackv1alpha1.PodPlacementTimedOut},
		{name: "timed out cannot become placed", current: repackv1alpha1.PodPlacementTimedOut, observed: repackv1alpha1.PodPlacementPlaced},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := enginestatus.PlacementPhaseAdvances(test.current, test.observed); got != test.want {
				t.Fatalf("enginestatus.PlacementPhaseAdvances(%q, %q) = %t, want %t", test.current, test.observed, got, test.want)
			}
		})
	}
}

func TestEvictionPhaseAdvances(t *testing.T) {
	tests := []struct {
		name              string
		current, observed repackv1alpha1.PodEvictionPhase
		want              bool
	}{
		{name: "initial pending", observed: repackv1alpha1.PodEvictionPending, want: true},
		{name: "request in progress", current: repackv1alpha1.PodEvictionPending, observed: repackv1alpha1.PodEvictionInProgress, want: true},
		{name: "skipped directly to accepted", current: repackv1alpha1.PodEvictionPending, observed: repackv1alpha1.PodEvictionAccepted, want: true},
		{name: "indirect removal observed", current: repackv1alpha1.PodEvictionInProgress, observed: repackv1alpha1.PodEvictionIndirectlyRemoved, want: true},
		{name: "same phase is not advancement", current: repackv1alpha1.PodEvictionInProgress, observed: repackv1alpha1.PodEvictionInProgress},
		{name: "cannot move backward", current: repackv1alpha1.PodEvictionInProgress, observed: repackv1alpha1.PodEvictionPending},
		{name: "accepted cannot become rejected", current: repackv1alpha1.PodEvictionAccepted, observed: repackv1alpha1.PodEvictionRejected},
		{name: "rejected cannot become indirectly removed", current: repackv1alpha1.PodEvictionRejected, observed: repackv1alpha1.PodEvictionIndirectlyRemoved},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := enginestatus.EvictionPhaseAdvances(test.current, test.observed); got != test.want {
				t.Fatalf("enginestatus.EvictionPhaseAdvances(%q, %q) = %t, want %t", test.current, test.observed, got, test.want)
			}
		})
	}
}

func TestMergeRelocationProgressPreservesPodGroupReplacement(t *testing.T) {
	desired := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "old", VictimPodName: "pod", PlannedNodeName: "node", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
	}}
	latest := append([]repackv1alpha1.PodRelocationStatus(nil), desired...)
	latest[0].ReplacementPodGroupName = "new"

	enginestatus.MergeRelocationProgress(desired, latest)
	if desired[0].ReplacementPodGroupName != "new" {
		t.Fatalf("replacementPodGroupName = %q, want new", desired[0].ReplacementPodGroupName)
	}
}
