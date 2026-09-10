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

package status

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

func TestMergeRelocationProgressPreservesControllerPlacement(t *testing.T) {
	desired := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "pg", VictimPodName: "pod", PlannedNodeName: "n2",
		Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted},
	}}
	latest := []repackv1alpha1.PodRelocationStatus{{
		Namespace: "ns", PodGroupName: "pg", VictimPodName: "pod", PlannedNodeName: "n2",
		Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted},
		Placement: repackv1alpha1.PodPlacementStatus{
			Phase: repackv1alpha1.PodPlacementPlaced, SelectedNodeName: "n2", ActualNodeName: "n3",
		},
	}}

	MergeRelocationProgress(desired, latest)
	if desired[0].Placement.Phase != repackv1alpha1.PodPlacementPlaced || desired[0].Placement.ActualNodeName != "n3" {
		t.Fatalf("merged placement=%+v, want controller-owned Placed result", desired[0].Placement)
	}
}

func TestTerminalPhasesDoNotReplaceSiblingTerminalPhases(t *testing.T) {
	if PlacementPhaseAdvances(repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementTimedOut) {
		t.Fatal("TimedOut must not replace sibling terminal phase Placed")
	}
	if EvictionPhaseAdvances(repackv1alpha1.PodEvictionAccepted, repackv1alpha1.PodEvictionRejected) {
		t.Fatal("Rejected must not replace sibling terminal phase Accepted")
	}
}

// A writer holding a pre-terminal observation (the informer cache had not yet
// observed the terminal write) must not re-open the Run; a non-terminal write
// still lands unchanged.
func TestWriteKeepsTerminalPhaseFinal(t *testing.T) {
	terminal := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal-run"},
		Status:     repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackSucceeded},
	}
	running := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "running-run"},
		Status:     repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning},
	}
	client := vcfake.NewSimpleClientset(terminal, running)
	store := NewStore(client)

	if err := store.Write(context.Background(), terminal.Name, &repackv1alpha1.RepackRunStatus{
		Phase: repackv1alpha1.RepackPending, Message: "deferred by execute gate",
	}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := store.Write(context.Background(), running.Name, &repackv1alpha1.RepackRunStatus{
		Phase: repackv1alpha1.RepackPending, Message: "deferred by execute gate",
	}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := client.RepackV1alpha1().RepackRuns().Get(context.Background(), terminal.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != repackv1alpha1.RepackSucceeded || got.Status.Message != "" {
		t.Errorf("status = %+v, want the terminal status untouched", got.Status)
	}
	got, err = client.RepackV1alpha1().RepackRuns().Get(context.Background(), running.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != repackv1alpha1.RepackPending {
		t.Errorf("phase = %q, want the non-terminal write to land", got.Status.Phase)
	}
}
