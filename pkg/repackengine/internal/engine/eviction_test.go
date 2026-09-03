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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	state "volcano.sh/repack-controller/pkg/state"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

func TestExecutePreparedEvictionsRecoversAcceptedRequestAfterStatusFailure(t *testing.T) {
	const (
		runName      = "run"
		namespace    = "ns"
		podGroupName = "pg"
		podName      = "victim"
		podUID       = types.UID("victim-uid")
	)
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: runName, UID: "run-uid"},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode:  repackv1alpha1.RepackModeExecute,
			Goals: []repackv1alpha1.RepackGoal{{Resource: "example.com/accelerator"}},
		},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Conditions: []metav1.Condition{{
				Type: state.CondProgressing, Status: metav1.ConditionTrue, Reason: state.ReasonEvicting,
			}},
			Plan: &repackv1alpha1.RepackPlan{
				Summary:    &repackv1alpha1.RepackSummary{FragBeforePercent: 50, FreedNodeCount: 1, MovedCardCount: 2},
				FreedNodes: []string{"node-a"},
				Moves: []repackv1alpha1.RepackMove{{
					Namespace: namespace, PodGroupName: podGroupName, Cards: 2,
					Pods: []repackv1alpha1.PodMove{{
						Name: podName, FromNode: "node-a", ToNode: "node-b", Cards: 2,
					}},
				}},
			},
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: namespace, PodGroupName: podGroupName,
				VictimPodName: podName, VictimPodUID: podUID,
				PlannedNodeName: "node-b", Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: podGroupName,
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid",
		},
	}}
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy(), podGroup)
	statusUpdates := 0
	failAcceptedStatusOnce := true
	volcanoClient.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		statusUpdates++
		updated := action.(k8stesting.UpdateAction).GetObject().(*repackv1alpha1.RepackRun)
		if failAcceptedStatusOnce &&
			updated.Status.Relocations[0].Eviction.Phase == repackv1alpha1.PodEvictionAccepted {
			failAcceptedStatusOnce = false
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
				runName, errors.New("simulated status outage after eviction acceptance"))
		}
		return false, nil, nil
	})

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: podName, UID: podUID,
	}}
	kubeClient := kubefake.NewSimpleClientset(pod)
	evictionCalls := 0
	kubeClient.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		evictionCalls++
		eviction := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		current, err := kubeClient.Tracker().Get(v1.SchemeGroupVersion.WithResource("pods"), namespace, podName)
		if err != nil {
			return true, nil, err
		}
		terminating := current.(*v1.Pod).DeepCopy()
		now := metav1.Now()
		terminating.DeletionTimestamp = &now
		if err := kubeClient.Tracker().Update(v1.SchemeGroupVersion.WithResource("pods"), terminating, namespace); err != nil {
			return true, nil, err
		}
		return true, eviction, nil
	})

	engine := &Engine{
		volcanoClient: volcanoClient,
		workQueue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
		recorder:                record.NewFakeRecorder(20),
		now:                     time.Now,
		pendingTerminalStatuses: make(map[string]*repackv1alpha1.RepackRunStatus),
	}
	t.Cleanup(engine.workQueue.ShutDown)

	result := engine.executePreparedEvictionsWithClient(
		context.Background(), run.DeepCopy(), run.Generation, "example.com/accelerator", kubeClient)
	if result.Err == nil {
		t.Fatal("first execution unexpectedly succeeded despite simulated accepted-status outage")
	}
	if evictionCalls != 1 {
		t.Fatalf("Eviction API calls = %d, want 1", evictionCalls)
	}
	persisted, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(
		context.Background(), runName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Status.Relocations[0].Eviction.Phase; got != repackv1alpha1.PodEvictionInProgress {
		t.Fatalf("phase after failed accepted write = %q, want InProgress", got)
	}

	if result := engine.executePreparedEvictionsWithClient(
		context.Background(), persisted.DeepCopy(), persisted.Generation,
		"example.com/accelerator", kubeClient); result.Err != nil {
		t.Fatalf("recovery execution failed: %v", result.Err)
	}
	if evictionCalls != 1 {
		t.Fatalf("Eviction API calls after recovery = %d, want no replay", evictionCalls)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(
		context.Background(), runName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Status.Relocations[0].Eviction.Phase; got != repackv1alpha1.PodEvictionAccepted {
		t.Fatalf("recovered eviction phase = %q, want Accepted", got)
	}
	if updated.Status.Result == nil || updated.Status.Result.MovedCardCount != 2 {
		t.Fatalf("result = %#v, want movedCardCount=2", updated.Status.Result)
	}
	if !hasProgressingReason(updated, state.ReasonReconcilingPlacements) {
		t.Fatalf("conditions = %#v, want ReconcilingPlacements", updated.Status.Conditions)
	}
	if statusUpdates < 4 {
		t.Fatalf("status updates = %d, want durable intent, outcome, result and placement barriers", statusUpdates)
	}
}

func TestClassifyMissingVictimsRequiresAcceptedSibling(t *testing.T) {
	relocations := []repackv1alpha1.PodRelocationStatus{
		{Namespace: "ns", PodGroupName: "group-a", Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}},
		{Namespace: "ns", PodGroupName: "group-a"},
		{Namespace: "ns", PodGroupName: "group-b"},
	}
	if !classifyMissingVictims(relocations, map[int]string{
		1: "Victim Pod was not found.",
		2: "Victim Pod was not found.",
	}) {
		t.Fatal("classification unexpectedly reported no change")
	}
	if got := relocations[1].Eviction.Phase; got != repackv1alpha1.PodEvictionIndirectlyRemoved {
		t.Fatalf("accepted sibling phase = %q, want IndirectlyRemoved", got)
	}
	if got := relocations[2].Eviction.Phase; got != repackv1alpha1.PodEvictionRejected {
		t.Fatalf("unrelated missing victim phase = %q, want Rejected", got)
	}
}

func TestExecutePreparedEvictionsBatchesPDBRetriesWithoutStatusChurn(t *testing.T) {
	now := time.Unix(1000, 0)
	run := testEvictionRun("batch-pdb", []string{"victim-a", "victim-b"})
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	statusUpdates := 0
	volcanoClient.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			statusUpdates++
		}
		return false, nil, nil
	})
	kubeClient := kubefake.NewSimpleClientset(
		&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "victim-a", UID: "uid-a"}},
		&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "victim-b", UID: "uid-b"}},
	)
	evictionCalls := 0
	kubeClient.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		evictionCalls++
		return true, nil, apierrors.NewTooManyRequests("cannot evict pod as it would violate the pod's disruption budget", 0)
	})
	engine := &Engine{
		volcanoClient:   volcanoClient,
		config:          Config{ExecutionTimeout: 10 * time.Minute},
		now:             func() time.Time { return now },
		evictionRetries: make(map[string]evictionRetryState),
	}

	result := engine.executePreparedEvictionsWithClient(context.Background(), run.DeepCopy(), 0,
		"example.com/accelerator", kubeClient)
	if result.Err != nil {
		t.Fatalf("first PDB-blocked batch failed: %v", result.Err)
	}
	if evictionCalls != 2 {
		t.Fatalf("Eviction calls=%d, want one batch of 2", evictionCalls)
	}
	if statusUpdates != 2 {
		t.Fatalf("status updates=%d, want one intent and one outcome checkpoint", statusUpdates)
	}
	if result.RequeueAfter < 1600*time.Millisecond || result.RequeueAfter > 2400*time.Millisecond {
		t.Fatalf("first retry delay=%s, want 2s with ±20%% jitter", result.RequeueAfter)
	}
	latest, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status.ExecutionDeadline == nil || !latest.Status.ExecutionDeadline.Time.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("executionDeadline=%v, want %v", latest.Status.ExecutionDeadline, now.Add(10*time.Minute))
	}
	for index := range latest.Status.Relocations {
		if latest.Status.Relocations[index].Eviction.Phase != repackv1alpha1.PodEvictionInProgress {
			t.Fatalf("relocation %d phase=%q, want InProgress", index, latest.Status.Relocations[index].Eviction.Phase)
		}
		if latest.Status.Relocations[index].Eviction.Message == "" {
			t.Fatalf("relocation %d must retain the PDB rejection detail", index)
		}
	}

	second := engine.executePreparedEvictionsWithClient(context.Background(), latest.DeepCopy(), 0,
		"example.com/accelerator", kubeClient)
	if second.Err != nil || second.RequeueAfter <= 0 {
		t.Fatalf("early retry result=%+v, want delayed requeue", second)
	}
	if evictionCalls != 2 || statusUpdates != 2 {
		t.Fatalf("early retry changed state: evictionCalls=%d statusUpdates=%d", evictionCalls, statusUpdates)
	}
}

func TestExecutePreparedEvictionsPlacesAcceptedSubsetBeforeRetry(t *testing.T) {
	now := time.Unix(2000, 0)
	run := testEvictionRun("mixed-batch", []string{"accepted", "pdb-blocked"})
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	kubeClient := kubefake.NewSimpleClientset(
		&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "accepted", UID: "accepted-uid"}},
		&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pdb-blocked", UID: "blocked-uid"}},
	)
	kubeClient.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		eviction := action.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		if eviction.Name == "pdb-blocked" {
			return true, nil, apierrors.NewTooManyRequests("PDB currently has no disruptions allowed", 0)
		}
		return true, eviction, nil
	})
	engine := &Engine{
		volcanoClient:   volcanoClient,
		config:          Config{ExecutionTimeout: 10 * time.Minute},
		now:             func() time.Time { return now },
		evictionRetries: make(map[string]evictionRetryState),
	}

	result := engine.executePreparedEvictionsWithClient(context.Background(), run.DeepCopy(), 0,
		"example.com/accelerator", kubeClient)
	if result.Err != nil {
		t.Fatalf("mixed batch failed: %v", result.Err)
	}
	latest, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status.Relocations[0].Eviction.Phase != repackv1alpha1.PodEvictionAccepted ||
		latest.Status.Relocations[1].Eviction.Phase != repackv1alpha1.PodEvictionInProgress {
		t.Fatalf("mixed phases=%q/%q, want Accepted/InProgress",
			latest.Status.Relocations[0].Eviction.Phase, latest.Status.Relocations[1].Eviction.Phase)
	}
	if enginestatus.ResolveStage(latest) != enginestatus.StagePlacing {
		t.Fatalf("stage=%q, want accepted subset placement before retry", enginestatus.ResolveStage(latest))
	}
	if latest.Status.Result == nil || latest.Status.Result.MovedCardCount != 1 {
		t.Fatalf("result=%+v, want accepted subset movedCardCount=1", latest.Status.Result)
	}
}

func TestExecutionDeadlineStopsFurtherEvictionAndMarksRunFailed(t *testing.T) {
	now := time.Unix(3000, 0)
	run := testEvictionRun("execution-timeout", []string{"blocked"})
	deadline := metav1.NewTime(now.Add(-time.Second))
	run.Status.ExecutionDeadline = &deadline
	run.Status.Relocations[0].VictimPodUID = "blocked-uid"
	run.Status.Relocations[0].Eviction.Phase = repackv1alpha1.PodEvictionInProgress
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	kubeClient := kubefake.NewSimpleClientset(&v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "blocked", UID: "blocked-uid",
	}})
	evictionCalls := 0
	kubeClient.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		evictionCalls++
		return true, nil, nil
	})
	engine := &Engine{
		volcanoClient:   volcanoClient,
		config:          Config{ExecutionTimeout: 10 * time.Minute},
		now:             func() time.Time { return now },
		evictionRetries: make(map[string]evictionRetryState),
	}

	result := engine.executePreparedEvictionsWithClient(context.Background(), run.DeepCopy(), 0,
		"example.com/accelerator", kubeClient)
	if result.Err != nil {
		t.Fatalf("timeout convergence returned error: %v", result.Err)
	}
	if evictionCalls != 0 {
		t.Fatalf("Eviction calls=%d, want no request after deadline", evictionCalls)
	}
	latest, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Status.Phase != repackv1alpha1.RepackFailed ||
		!hasFailedReason(latest, state.ReasonExecutionTimedOut) {
		t.Fatalf("terminal status=%+v, want Failed/%s", latest.Status, state.ReasonExecutionTimedOut)
	}
	if latest.Status.Relocations[0].Eviction.Phase != repackv1alpha1.PodEvictionRejected {
		t.Fatalf("eviction phase=%q, want Rejected", latest.Status.Relocations[0].Eviction.Phase)
	}
	if latest.Status.Result == nil || latest.Status.Result.MetricsVerified {
		t.Fatalf("result=%+v, want unverified timeout result", latest.Status.Result)
	}
}

func testEvictionRun(name string, podNames []string) *repackv1alpha1.RepackRun {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode:  repackv1alpha1.RepackModeExecute,
			Goals: []repackv1alpha1.RepackGoal{{Resource: "example.com/accelerator"}},
		},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Conditions: []metav1.Condition{{
				Type: state.CondProgressing, Status: metav1.ConditionTrue, Reason: state.ReasonEvicting,
			}},
			Plan: &repackv1alpha1.RepackPlan{
				Summary:    &repackv1alpha1.RepackSummary{FragBeforePercent: 50, FreedNodeCount: 1, MovedCardCount: int64(len(podNames))},
				FreedNodes: []string{"source"},
			},
		},
	}
	for _, podName := range podNames {
		run.Status.Plan.Moves = append(run.Status.Plan.Moves, repackv1alpha1.RepackMove{
			Namespace: "ns", PodGroupName: "pg", Cards: 1,
			Pods: []repackv1alpha1.PodMove{{Name: podName, FromNode: "source", ToNode: "target", Cards: 1}},
		})
		run.Status.Relocations = append(run.Status.Relocations, repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "pg", VictimPodName: podName, PlannedNodeName: "target",
			Eviction:  repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending},
			Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
		})
	}
	return run
}

func hasProgressingReason(run *repackv1alpha1.RepackRun, reason string) bool {
	for index := range run.Status.Conditions {
		condition := &run.Status.Conditions[index]
		if condition.Type == state.CondProgressing &&
			condition.Status == metav1.ConditionTrue &&
			condition.Reason == reason {
			return true
		}
	}
	return false
}

func hasFailedReason(run *repackv1alpha1.RepackRun, reason string) bool {
	for index := range run.Status.Conditions {
		condition := &run.Status.Conditions[index]
		if condition.Type == state.CondFailed && condition.Status == metav1.ConditionTrue && condition.Reason == reason {
			return true
		}
	}
	return false
}
