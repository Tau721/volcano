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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
)

func TestResolveStageUsesOneDurableWorkflowClassification(t *testing.T) {
	plan := &repackv1alpha1.RepackPlan{}
	tests := []struct {
		name string
		run  *repackv1alpha1.RepackRun
		want Stage
	}{
		{name: "new", run: &repackv1alpha1.RepackRun{}, want: StagePlanning},
		{name: "pending", run: &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackPending}}, want: StagePlanning},
		{name: "evicting", run: &repackv1alpha1.RepackRun{
			Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning, Plan: plan,
				Relocations: []repackv1alpha1.PodRelocationStatus{{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending}}}},
		}, want: StageEvicting},
		{name: "placing", run: &repackv1alpha1.RepackRun{
			Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning, Plan: plan,
				Relocations: []repackv1alpha1.PodRelocationStatus{{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}}},
				Conditions:  []metav1.Condition{{Type: state.CondProgressing, Status: metav1.ConditionTrue, Reason: state.ReasonReconcilingPlacements}}},
		}, want: StagePlacing},
		{name: "mixed batch places accepted subset first", run: &repackv1alpha1.RepackRun{
			Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning, Plan: plan,
				Relocations: []repackv1alpha1.PodRelocationStatus{
					{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}},
					{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionInProgress}},
				}},
		}, want: StagePlacing},
		{name: "mixed batch resumes eviction after accepted placement", run: &repackv1alpha1.RepackRun{
			Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning, Plan: plan,
				Relocations: []repackv1alpha1.PodRelocationStatus{
					{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementPlaced}},
					{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionInProgress}},
				}},
		}, want: StageEvicting},
		{name: "terminal cleanup", run: &repackv1alpha1.RepackRun{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{repackv1alpha1.PlacementActiveLabel: "true"}},
			Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackSucceeded,
				Relocations: []repackv1alpha1.PodRelocationStatus{{}}},
		}, want: StageCleanup},
		{name: "terminal preparation cleanup without active label", run: &repackv1alpha1.RepackRun{
			Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackFailed,
				Relocations: []repackv1alpha1.PodRelocationStatus{{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending}}}},
		}, want: StageCleanup},
		{name: "terminal completed execution keeps journal without cleanup", run: &repackv1alpha1.RepackRun{
			Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackSucceeded,
				Relocations: []repackv1alpha1.PodRelocationStatus{{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}}}},
		}, want: StageNone},
		{name: "terminal clean", run: &repackv1alpha1.RepackRun{
			Spec:   repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackSucceeded},
		}, want: StageNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ResolveStage(test.run); got != test.want {
				t.Fatalf("stage=%q, want %q", got, test.want)
			}
		})
	}
}
