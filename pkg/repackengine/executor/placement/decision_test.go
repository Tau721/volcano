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

package placement

import (
	"testing"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

func TestCandidatesRequireConcreteReplacement(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{Relocations: []repackv1alpha1.PodRelocationStatus{
		{VictimPodName: "ready", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection, ReplacementPodName: "replacement", ReplacementPodUID: "uid"}},
		{VictimPodName: "missing-uid", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection, ReplacementPodName: "replacement"}},
	}}}
	candidates := Candidates(run)
	if len(candidates) != 1 || candidates[0].VictimPodName != "ready" {
		t.Fatalf("Candidates()=%v, want only concrete replacement", candidates)
	}
}

func TestCompleteOnlyWaitsForAcceptedEvictionSubset(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Relocations: []repackv1alpha1.PodRelocationStatus{
			{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementPlaced}},
			{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionInProgress}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}},
		},
	}}
	if !Complete(run) {
		t.Fatal("a placed accepted subset must be complete even while another eviction is retryable")
	}
	run.Status.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementNominated
	if Complete(run) {
		t.Fatal("an accepted replacement must reach a terminal placement phase")
	}
}

func TestReceiversPreferPlannedNodeWhenIdleLedgerIsStale(t *testing.T) {
	resourceName := v1.ResourceName("example.com/accelerator")
	resource := func(value float64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resourceName: value}}
	}
	node := func(name string) *schedapi.NodeInfo {
		return &schedapi.NodeInfo{
			Name: name, Allocatable: resource(8000), Used: resource(0),
			Idle: schedapi.EmptyResource(), Releasing: schedapi.EmptyResource(), Pipelined: schedapi.EmptyResource(),
		}
	}
	planned, alternative := node("planned"), node("alternative")
	task := &schedapi.TaskInfo{InitResreq: resource(2000)}

	receivers := Receivers([]*schedapi.NodeInfo{alternative, planned}, nil, planned.Name, task)
	if len(receivers) != 2 {
		t.Fatalf("Receivers() returned %d nodes, want 2", len(receivers))
	}
	if receivers[0].Name != planned.Name {
		t.Fatalf("first receiver=%q, want planned node %q", receivers[0].Name, planned.Name)
	}
}

func TestEvaluateTerminalRequiresVerifiedBenefit(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan:   &repackv1alpha1.RepackPlan{FreedNodes: []string{"n1"}},
		Result: &repackv1alpha1.RepackResult{FreedNodes: []string{"n1"}, MetricsVerified: false},
	}}
	decision := EvaluateTerminal(run, false)
	if decision.Succeeded || decision.Reason != state.ReasonResultVerificationFailed {
		t.Fatalf("decision=%+v, want result verification failure", decision)
	}
}
