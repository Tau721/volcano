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
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	state "volcano.sh/repack-controller/pkg/state"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

func TestPlacementReceiversExcludeFreedNodesAndRequireImmediateIdleCapacity(t *testing.T) {
	resource := v1.ResourceName("example.com/accelerator")
	resourceOf := func(quantity int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resource: float64(quantity)}}
	}
	node := func(name string, capacity int64) *schedapi.NodeInfo {
		return &schedapi.NodeInfo{Name: name, Allocatable: resourceOf(capacity), Used: resourceOf(0), Idle: resourceOf(capacity)}
	}
	nodes := []*schedapi.NodeInfo{
		node("planned", 2),
		node("freeing", 8),
		node("too-small", 1),
		node("alternative", 4),
	}
	task := &schedapi.TaskInfo{InitResreq: resourceOf(2)}

	receivers := placementexecutor.Receivers(nodes, []string{"freeing"}, "planned", task)
	if len(receivers) != 2 {
		t.Fatalf("receiver count = %d, want 2", len(receivers))
	}
	if receivers[0].Name != "planned" || receivers[1].Name != "alternative" {
		t.Errorf("receiver order = [%s, %s], want [planned, alternative]", receivers[0].Name, receivers[1].Name)
	}
}

func TestPlacementCandidatesRequireConcreteGatedReplacement(t *testing.T) {
	run := &repackv1alpha1.RepackRun{}
	run.Status.Relocations = []repackv1alpha1.PodRelocationStatus{
		{Namespace: "ns", PodGroupName: "g", VictimPodName: "prepared", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}},
		{Namespace: "ns", PodGroupName: "g", VictimPodName: "selected", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{ReplacementPodName: "p-selected", ReplacementPodUID: "uid", SelectedNodeName: "n2", Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection}},
		{Namespace: "ns", PodGroupName: "g", VictimPodName: "awaiting", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{ReplacementPodName: "p-awaiting", ReplacementPodUID: "uid", Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection}},
		{Namespace: "ns", PodGroupName: "g", VictimPodName: "gated", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{ReplacementPodName: "p-gated", ReplacementPodUID: "uid", Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection}},
	}

	candidates := placementexecutor.Candidates(run)
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(candidates))
	}
	if candidates[0].VictimPodName != "awaiting" || candidates[1].VictimPodName != "gated" {
		t.Errorf("candidates = [%s, %s], want deterministic [awaiting, gated]", candidates[0].VictimPodName, candidates[1].VictimPodName)
	}
}

func TestPreparePlacementLeaseReclaimsTerminalLease(t *testing.T) {
	oldRun := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "old", UID: types.UID("old-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase:       repackv1alpha1.RepackFailed,
			Relocations: []repackv1alpha1.PodRelocationStatus{{Namespace: "ns", PodGroupName: "pg", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementTimedOut}}},
		},
	}
	newRun := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "new", UID: types.UID("new-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status:     repackv1alpha1.RepackRunStatus{Relocations: []repackv1alpha1.PodRelocationStatus{{Namespace: "ns", PodGroupName: "pg", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}}}},
	}
	pg := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pg", Annotations: map[string]string{
		repackv1alpha1.PlacementLeaseAnnotation: "old/old-uid",
	}}}
	client := vcfake.NewSimpleClientset(oldRun, newRun, pg)
	e := &Engine{volcanoClient: client}

	if err := e.preparePlacementLeases(context.Background(), newRun); err != nil {
		t.Fatalf("prepare placement lease: %v", err)
	}
	updated, err := client.SchedulingV1beta1().PodGroups("ns").Get(context.Background(), "pg", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := updated.Annotations[repackv1alpha1.PlacementLeaseAnnotation], "new/new-uid"; got != want {
		t.Fatalf("lease = %q, want %q", got, want)
	}
}

func TestCleanupPlacementFindsUnclaimedWebhookLeaseAndClearsDiscoveryLabel(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "run", UID: types.UID("run-uid"),
			Labels: map[string]string{repackv1alpha1.PlacementActiveLabel: "true"},
		},
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackFailed,
			Plan: &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "old",
			}}},
		},
	}
	unclaimed := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "scale-out",
		Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid"},
	}}
	client := vcfake.NewSimpleClientset(run.DeepCopy(), unclaimed)
	engine := &Engine{volcanoClient: client}
	if err := engine.cleanupPlacement(context.Background(), run.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	updatedPodGroup, err := client.SchedulingV1beta1().PodGroups("ns").Get(
		context.Background(), "scale-out", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedPodGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] != "" {
		t.Fatalf("unclaimed admission lease was not removed: %+v", updatedPodGroup.Annotations)
	}
	updatedRun, err := client.RepackV1alpha1().RepackRuns().Get(
		context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedRun.Labels[repackv1alpha1.PlacementActiveLabel] != "" {
		t.Fatalf("placement discovery label was not removed: %+v", updatedRun.Labels)
	}
}

func TestCleanupPlacementClearsUnperformedJournalOnlyAfterLeaseCleanup(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackFailed,
			Plan: &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "pg",
			}}},
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "pg", VictimPodName: "pod",
				Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending},
			}},
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "pg",
		Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid"},
	}}
	client := vcfake.NewSimpleClientset(run.DeepCopy(), podGroup)
	engine := &Engine{volcanoClient: client}

	if err := engine.cleanupPlacement(context.Background(), run.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	updatedPodGroup, err := client.SchedulingV1beta1().PodGroups("ns").Get(
		context.Background(), "pg", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedPodGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] != "" {
		t.Fatalf("placement lease was not removed: %+v", updatedPodGroup.Annotations)
	}
	updatedRun, err := client.RepackV1alpha1().RepackRuns().Get(
		context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(updatedRun.Status.Relocations) != 0 || updatedRun.Status.Result != nil {
		t.Fatalf("unperformed execution state was not cleared after cleanup: %+v", updatedRun.Status)
	}
	if enginestatus.ResolveStage(updatedRun) != enginestatus.StageNone {
		t.Fatalf("stage=%q, want cleanup to converge to StageNone", enginestatus.ResolveStage(updatedRun))
	}
	if err := engine.cleanupPlacement(context.Background(), updatedRun.DeepCopy()); err != nil {
		t.Fatalf("repeated cleanup must remain idempotent: %v", err)
	}
}

func TestCleanupPlacementRetainsUnperformedJournalWhenLeaseCleanupFails(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackFailed,
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "pg", VictimPodName: "pod",
				Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending},
			}},
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "pg",
		Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid"},
	}}
	client := vcfake.NewSimpleClientset(run.DeepCopy(), podGroup)
	client.PrependReactor("update", "podgroups", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated PodGroup API outage")
	})
	engine := &Engine{volcanoClient: client}

	if err := engine.cleanupPlacement(context.Background(), run.DeepCopy()); err == nil {
		t.Fatal("cleanupPlacement() error = nil, want lease cleanup failure")
	}
	updatedRun, err := client.RepackV1alpha1().RepackRuns().Get(
		context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(updatedRun.Status.Relocations) != 1 ||
		updatedRun.Status.Relocations[0].Eviction.Phase != repackv1alpha1.PodEvictionPending {
		t.Fatalf("unperformed journal must remain as cleanup recovery evidence: %+v", updatedRun.Status.Relocations)
	}
	if enginestatus.ResolveStage(updatedRun) != enginestatus.StageCleanup {
		t.Fatalf("stage=%q, want failed cleanup to remain recoverable", enginestatus.ResolveStage(updatedRun))
	}
}

func TestCleanupPlacementPreservesAttemptedExecutionJournal(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "run", UID: types.UID("run-uid"),
			Labels: map[string]string{repackv1alpha1.PlacementActiveLabel: "true"},
		},
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackSucceeded,
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted},
			}},
		},
	}
	client := vcfake.NewSimpleClientset(run.DeepCopy())
	engine := &Engine{volcanoClient: client}

	if err := engine.cleanupPlacement(context.Background(), run.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	updated, err := client.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.Relocations) != 1 ||
		updated.Status.Relocations[0].Eviction.Phase != repackv1alpha1.PodEvictionAccepted {
		t.Fatalf("attempted execution journal must be retained: %+v", updated.Status.Relocations)
	}
	if updated.Labels[repackv1alpha1.PlacementActiveLabel] != "" {
		t.Fatalf("placement-active label was not cleared: %+v", updated.Labels)
	}
	if enginestatus.ResolveStage(updated) != enginestatus.StageNone {
		t.Fatalf("stage=%q, want completed cleanup to stop reconciling", enginestatus.ResolveStage(updated))
	}
}

func TestReleasePlacementLeasesPreservesAnotherRunOwner(t *testing.T) {
	run := &repackv1alpha1.RepackRun{ObjectMeta: metav1.ObjectMeta{
		Name: "run", UID: types.UID("run-uid"),
	}}
	owned := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "owned",
		Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid"},
	}}
	notOwned := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "not-owned",
		Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "other/other-uid"},
	}}
	client := vcfake.NewSimpleClientset(owned, notOwned)
	engine := &Engine{volcanoClient: client}
	groups := map[types.NamespacedName]struct{}{
		{Namespace: "ns", Name: "owned"}:     {},
		{Namespace: "ns", Name: "not-owned"}: {},
		{Namespace: "ns", Name: "missing"}:   {},
	}
	if err := engine.releasePlacementLeases(context.Background(), run, groups); err != nil {
		t.Fatal(err)
	}
	updatedOwned, err := client.SchedulingV1beta1().PodGroups("ns").Get(
		context.Background(), "owned", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedOwned.Annotations[repackv1alpha1.PlacementLeaseAnnotation] != "" {
		t.Fatalf("owned lease was not released: %+v", updatedOwned.Annotations)
	}
	updatedNotOwned, err := client.SchedulingV1beta1().PodGroups("ns").Get(
		context.Background(), "not-owned", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updatedNotOwned.Annotations[repackv1alpha1.PlacementLeaseAnnotation]; got != "other/other-uid" {
		t.Fatalf("another Run's lease changed to %q", got)
	}
}

func TestRecreatedPodGroupLeaseRepairIsIndependentlyRateLimited(t *testing.T) {
	controller := true
	start := metav1.NewTime(time.Unix(100, 0))
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase:     repackv1alpha1.RepackRunning,
			StartTime: &start,
			Plan: &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "old",
				Owner: &repackv1alpha1.WorkloadRef{
					APIVersion: "serving.example/v1", Kind: "Serving", Name: "model",
				},
			}}},
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "old", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	candidate := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "new", CreationTimestamp: metav1.NewTime(time.Unix(101, 0)),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	client := vcfake.NewSimpleClientset(run.DeepCopy(), candidate)
	now := time.Unix(101, 0)
	engine := &Engine{volcanoClient: client, now: func() time.Time { return now }}
	listCount := func() int {
		count := 0
		for _, action := range client.Actions() {
			if action.GetVerb() == "list" && action.GetResource().Resource == "podgroups" {
				count++
			}
		}
		return count
	}

	if err := engine.repairRecreatedPodGroupLeasesIfDue(context.Background(), run.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := engine.repairRecreatedPodGroupLeasesIfDue(context.Background(), run.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if got := listCount(); got != 1 {
		t.Fatalf("PodGroup LIST count before repair interval = %d, want 1", got)
	}

	now = now.Add(placementLeaseRepairInterval + time.Second)
	if err := engine.repairRecreatedPodGroupLeasesIfDue(context.Background(), run.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if got := listCount(); got != 2 {
		t.Fatalf("PodGroup LIST count after repair interval = %d, want 2", got)
	}
	updated, err := client.SchedulingV1beta1().PodGroups("ns").Get(context.Background(), "new", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Annotations[repackv1alpha1.PlacementLeaseAnnotation]; got != "run/run-uid" {
		t.Fatalf("repaired placement lease = %q, want run/run-uid", got)
	}
}

func TestRecreatedPodGroupLeaseRepairSkipsCompletedWorkloads(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")},
		Status: repackv1alpha1.RepackRunStatus{
			Plan: &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "old",
				Owner: &repackv1alpha1.WorkloadRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "workload"},
			}}},
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "old", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementPlaced},
			}},
		},
	}
	client := vcfake.NewSimpleClientset(run.DeepCopy())
	engine := &Engine{volcanoClient: client}
	if err := engine.repairRecreatedPodGroupLeasesIfDue(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "podgroups" {
			t.Fatalf("completed workload triggered unnecessary PodGroup LIST: %#v", action)
		}
	}
}

func TestPlacementBindingsVisible(t *testing.T) {
	nodes := []*schedapi.NodeInfo{{
		Name: "n1",
		Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
			"replacement-uid": {UID: "replacement-uid"},
		},
	}}
	relocations := []repackv1alpha1.PodRelocationStatus{{Placement: repackv1alpha1.PodPlacementStatus{ReplacementPodUID: "replacement-uid",
		ActualNodeName: "n1",
		Phase:          repackv1alpha1.PodPlacementPlaced},
	}}
	if !placementexecutor.BindingsVisible(nodes, relocations) {
		t.Fatal("expected replacement binding to be visible")
	}
	relocations[0].Placement.ActualNodeName = "n2"
	if placementexecutor.BindingsVisible(nodes, relocations) {
		t.Fatal("binding on a different node must not be treated as visible")
	}
}

func TestPlacementObservationDeadlinePassed(t *testing.T) {
	deadline := metav1.NewTime(time.Unix(100, 0))
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		ExecutionDeadline: &deadline,
		Relocations: []repackv1alpha1.PodRelocationStatus{{
			Placement: repackv1alpha1.PodPlacementStatus{},
		}},
	}}
	if placementexecutor.ObservationDeadlinePassed(run, time.Unix(99, 0)) {
		t.Fatal("deadline must not pass early")
	}
	if !placementexecutor.ObservationDeadlinePassed(run, time.Unix(100, 0)) {
		t.Fatal("deadline must pass at executionDeadline")
	}
}

func TestPlannedNodeFreeingCanConvergeUntilPlacementDeadline(t *testing.T) {
	deadline := metav1.NewTime(time.Unix(100, 0))
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan:              &repackv1alpha1.RepackPlan{FreedNodes: []string{"source"}},
		Result:            &repackv1alpha1.RepackResult{MetricsVerified: true},
		ExecutionDeadline: &deadline,
		Relocations: []repackv1alpha1.PodRelocationStatus{{
			Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementPlaced},
		}},
	}}

	comparison, pending := placementexecutor.FreedNodeVerificationPending(run, time.Unix(99, 0))
	if comparison.Equal {
		t.Fatal("planned and actual freed-node sets must initially differ")
	}
	if !pending {
		t.Fatal("a transient missing node must be retried before the deadline")
	}

	run.Status.Result.FreedNodes = []string{"source"}
	if comparison, pending = placementexecutor.FreedNodeVerificationPending(run, time.Unix(99, 0)); !comparison.Equal || pending {
		t.Fatalf("comparison=%+v, want converged planned and actual node sets", comparison)
	}

	run.Status.Result.FreedNodes = nil
	if comparison, pending = placementexecutor.FreedNodeVerificationPending(run, time.Unix(100, 0)); comparison.Equal || pending {
		t.Fatal("a persistently occupied planned node must become terminal at the deadline")
	}
	decision := placementexecutor.EvaluateTerminal(run, false)
	if decision.Succeeded || decision.Reason != state.ReasonBenefitNotRealized {
		t.Fatalf("decision=%+v, want failed %s", decision, state.ReasonBenefitNotRealized)
	}
}

func TestUpdateActualExecuteResult(t *testing.T) {
	resource := v1.ResourceName("example.com/accelerator")
	resourceOf := func(cards int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resource: float64(cards * 1000)}}
	}
	nodes := []*schedapi.NodeInfo{
		{Name: "freed", Allocatable: resourceOf(8), Used: resourceOf(0), Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{}},
		{
			Name:        "receiver",
			Allocatable: resourceOf(8),
			Used:        resourceOf(6),
			Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
				"a": {Resreq: resourceOf(4)},
				"b": {Resreq: resourceOf(2)},
			},
		},
		{Name: "empty", Allocatable: resourceOf(8), Used: resourceOf(0), Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{}},
	}
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan: &repackv1alpha1.RepackPlan{
			Summary:    &repackv1alpha1.RepackSummary{FragBeforePercent: 33, FragAfterPercent: 11, FreedNodeCount: 1},
			FreedNodes: []string{"freed"},
			Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "pg",
				Pods: []repackv1alpha1.PodMove{{Name: "victim", FromNode: "freed", ToNode: "receiver"}},
			}},
		},
		Result: &repackv1alpha1.RepackResult{MovedCardCount: 2},
		Relocations: []repackv1alpha1.PodRelocationStatus{{
			Namespace: "ns", PodGroupName: "pg", VictimPodName: "victim", PlannedNodeName: "receiver",
		}},
	}}
	updateActualExecuteResult(run, nodes, resource)
	if run.Status.Result.FragAfterPercent != 0 {
		t.Errorf("fragAfterPercent=%d, want actual 0", run.Status.Result.FragAfterPercent)
	}
	if run.Status.Result.FreedNodeCount != 1 || !run.Status.Result.MetricsVerified {
		t.Errorf("result=%+v, want actual freedNodeCount=1 and verified", run.Status.Result)
	}
	if got, want := run.Status.Result.FreedNodes, []string{"freed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("result.freedNodes=%v, want %v; an unrelated already-empty node must not be counted", got, want)
	}
	if run.Status.Plan.Summary.FragAfterPercent != 11 || run.Status.Plan.Summary.FreedNodeCount != 1 {
		t.Errorf("plan summary was overwritten: %+v", run.Status.Plan.Summary)
	}
}

func TestUpdateActualExecuteResultDoesNotClaimOccupiedPlannedNode(t *testing.T) {
	resource := v1.ResourceName("example.com/accelerator")
	resourceOf := func(cards int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resource: float64(cards * 1000)}}
	}
	nodes := []*schedapi.NodeInfo{{
		Name: "planned", Allocatable: resourceOf(8), Used: resourceOf(2),
		Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{"concurrent": {Resreq: resourceOf(2)}},
	}}
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan: &repackv1alpha1.RepackPlan{
			Summary:    &repackv1alpha1.RepackSummary{FragBeforePercent: 25, FreedNodeCount: 1},
			FreedNodes: []string{"planned"},
			Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "pg",
				Pods: []repackv1alpha1.PodMove{{Name: "victim", FromNode: "planned", ToNode: "receiver"}},
			}},
		},
		Result: &repackv1alpha1.RepackResult{},
		Relocations: []repackv1alpha1.PodRelocationStatus{{
			Namespace: "ns", PodGroupName: "pg", VictimPodName: "victim", PlannedNodeName: "receiver", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementPlaced},
		}},
	}}

	updateActualExecuteResult(run, nodes, resource)
	if len(run.Status.Result.FreedNodes) != 0 || run.Status.Result.FreedNodeCount != 0 {
		t.Fatalf("result=%+v, want no freed node while planned node remains occupied", run.Status.Result)
	}
	decision := placementexecutor.EvaluateTerminal(run, false)
	if decision.Succeeded || decision.Reason != state.ReasonBenefitNotRealized {
		t.Fatalf("decision=%+v, want failed %s", decision, state.ReasonBenefitNotRealized)
	}
	if got, want := decision.Nodes.Missing, []string{"planned"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("missing=%v, want %v", got, want)
	}
}

func TestEvaluatePlacementTerminal(t *testing.T) {
	tests := []struct {
		name                      string
		placementPhases           []repackv1alpha1.PodPlacementPhase
		plannedNodes              []string
		actualNodes               []string
		metricsVerified           bool
		resultSnapshotUnavailable bool
		alternativeNode           bool
		wantSucceeded             bool
		wantReason                string
		wantMissing               []string
		wantUnexpected            []string
	}{
		{
			name: "exact planned node set",
			placementPhases: []repackv1alpha1.PodPlacementPhase{
				repackv1alpha1.PodPlacementPlaced,
			},
			plannedNodes: []string{"node-b", "node-a"}, actualNodes: []string{"node-a", "node-b"},
			metricsVerified: true, wantSucceeded: true, wantReason: state.ReasonExecutionCompleted,
		},
		{
			name: "alternative node is diagnostic when benefit is realized",
			placementPhases: []repackv1alpha1.PodPlacementPhase{
				repackv1alpha1.PodPlacementPlaced,
			},
			plannedNodes: []string{"node-a"}, actualNodes: []string{"node-a"},
			metricsVerified: true, alternativeNode: true,
			wantSucceeded: true, wantReason: state.ReasonExecutionCompletedWithAlternativePlacement,
		},
		{
			name: "same count but different node set",
			placementPhases: []repackv1alpha1.PodPlacementPhase{
				repackv1alpha1.PodPlacementPlaced,
			},
			plannedNodes: []string{"node-a"}, actualNodes: []string{"node-b"},
			metricsVerified: true, wantReason: state.ReasonBenefitNotRealized,
			wantMissing: []string{"node-a"}, wantUnexpected: []string{"node-b"},
		},
		{
			name: "planned node remains occupied",
			placementPhases: []repackv1alpha1.PodPlacementPhase{
				repackv1alpha1.PodPlacementPlaced,
			},
			plannedNodes: []string{"node-a", "node-b"}, actualNodes: []string{"node-a"},
			metricsVerified: true, wantReason: state.ReasonBenefitNotRealized,
			wantMissing: []string{"node-b"},
		},
		{
			name: "replacement placement timed out",
			placementPhases: []repackv1alpha1.PodPlacementPhase{
				repackv1alpha1.PodPlacementTimedOut,
			},
			plannedNodes: []string{"node-a"}, actualNodes: nil,
			wantReason: state.ReasonPlacementTimedOut, wantMissing: []string{"node-a"},
		},
		{
			name: "terminal scheduler metrics unverified",
			placementPhases: []repackv1alpha1.PodPlacementPhase{
				repackv1alpha1.PodPlacementPlaced,
			},
			plannedNodes: []string{"node-a"}, actualNodes: nil,
			resultSnapshotUnavailable: true, wantReason: state.ReasonResultVerificationFailed,
			wantMissing: []string{"node-a"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			relocations := make([]repackv1alpha1.PodRelocationStatus, len(test.placementPhases))
			for index, phase := range test.placementPhases {
				relocations[index].Placement.Phase = phase
				if phase == repackv1alpha1.PodPlacementPlaced {
					relocations[index].Placement.SelectedNodeName = "selected"
					relocations[index].Placement.ActualNodeName = "selected"
					if test.alternativeNode {
						relocations[index].Placement.ActualNodeName = "alternative"
					}
				}
			}
			run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
				Plan:        &repackv1alpha1.RepackPlan{FreedNodes: test.plannedNodes},
				Result:      &repackv1alpha1.RepackResult{FreedNodes: test.actualNodes, MetricsVerified: test.metricsVerified},
				Relocations: relocations,
			}}
			got := placementexecutor.EvaluateTerminal(run, test.resultSnapshotUnavailable)
			if got.Succeeded != test.wantSucceeded || got.Reason != test.wantReason {
				t.Fatalf("decision=%+v, want succeeded=%t reason=%s", got, test.wantSucceeded, test.wantReason)
			}
			if !reflect.DeepEqual(got.Nodes.Missing, test.wantMissing) {
				t.Errorf("missing=%v, want %v", got.Nodes.Missing, test.wantMissing)
			}
			if !reflect.DeepEqual(got.Nodes.Unexpected, test.wantUnexpected) {
				t.Errorf("unexpected=%v, want %v", got.Nodes.Unexpected, test.wantUnexpected)
			}
		})
	}
}

func TestPlacementStatusMessageExplainsMissingPlannedNodes(t *testing.T) {
	resource := v1.ResourceName("example.com/accelerator")
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan: &repackv1alpha1.RepackPlan{Summary: &repackv1alpha1.RepackSummary{
			FragBeforePercent: 50,
		}},
		Relocations: []repackv1alpha1.PodRelocationStatus{
			{
				Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "new",
				Placement: repackv1alpha1.PodPlacementStatus{
					Phase: repackv1alpha1.PodPlacementPlaced, SelectedNodeName: "node-a", ActualNodeName: "node-a",
				},
			},
			{Placement: repackv1alpha1.PodPlacementStatus{
				Phase: repackv1alpha1.PodPlacementPlaced, SelectedNodeName: "node-a", ActualNodeName: "node-b",
			}},
		},
	}}
	decision := placementexecutor.TerminalDecision{
		Reason: state.ReasonBenefitNotRealized,
		Nodes: placementexecutor.FreedNodeComparison{
			Planned: []string{"node-a", "node-b"},
			Actual:  []string{"node-a"},
			Missing: []string{"node-b"},
		},
	}

	message := enginestatus.PlacementMessage(run, resource, decision)
	for _, want := range []string{
		"did not realize the planned benefit",
		"node-a, node-b",
		"node-b",
		"2 replacement Pods were scheduled",
		"1 alternative placement",
		"inspect target-resource usage",
		"ns/old -> ns/new",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not contain operator detail %q", message, want)
		}
	}
}

func TestExpiredPlacementDoesNotClaimUnverifiedBenefit(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan: &repackv1alpha1.RepackPlan{Summary: &repackv1alpha1.RepackSummary{
			FragBeforePercent: 40,
			FragAfterPercent:  20,
			FreedNodeCount:    2,
		}},
		Result: &repackv1alpha1.RepackResult{
			FragAfterPercent: 30, FreedNodeCount: 1, FreedNodes: []string{"node-a"},
			MovedCardCount: 6, MetricsVerified: true,
		},
	}}
	placementexecutor.MarkBenefitUnverified(run)
	if run.Status.Result.FragAfterPercent != 40 || run.Status.Result.FreedNodeCount != 0 ||
		len(run.Status.Result.FreedNodes) != 0 || run.Status.Result.MovedCardCount != 6 || run.Status.Result.MetricsVerified {
		t.Fatalf("expired placement result=%+v, want conservative 40%%/0 with accepted cards retained", run.Status.Result)
	}
	if run.Status.Plan.Summary.FragAfterPercent != 20 || run.Status.Plan.Summary.FreedNodeCount != 2 {
		t.Fatalf("plan summary was overwritten: %+v", run.Status.Plan.Summary)
	}
}
