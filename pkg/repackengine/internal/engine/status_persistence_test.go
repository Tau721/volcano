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
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	state "volcano.sh/volcano/pkg/controllers/repack/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

const gpuResource = v1.ResourceName("nvidia.com/gpu")

func mkMove(name, job string, cards float64, from, to string) *engineapi.Move {
	return &engineapi.Move{
		Task: &schedapi.TaskInfo{
			Name: name, Job: schedapi.JobID(job),
			Resreq: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpuResource: cards * 1000}},
		},
		From: from, To: to,
	}
}

func TestResolveMoveOwners(t *testing.T) {
	controller := true
	client := vcfake.NewSimpleClientset(
		&schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "owned",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "worker", Controller: &controller}},
		}},
		&schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "non-controller",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "helper"}},
		}},
	)
	plan := &engineapi.RepackPlan{Moves: []*engineapi.Move{
		mkMove("owned-0", "ns/owned", 1, "n0", "n1"),
		mkMove("plain-0", "ns/non-controller", 1, "n0", "n1"),
		mkMove("missing-0", "ns/missing", 1, "n0", "n1"),
	}}
	owners := (&Engine{volcanoClient: client}).resolveMoveOwners(context.Background(), plan)
	if len(owners) != 1 {
		t.Fatalf("resolved owners=%v, want one controller owner", owners)
	}
	got := owners["ns/owned"]
	if got == nil || got.APIVersion != "apps/v1" || got.Kind != "StatefulSet" || got.Name != "worker" {
		t.Errorf("owner=%+v, want apps/v1 StatefulSet worker", got)
	}
}

func TestWriteStatusRetriesConflictAndPreservesBoundNomination(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "status-conflict"},
		Status: repackv1alpha1.RepackRunStatus{
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementPlaced},
			}},
		},
	}
	volcanoClient := vcfake.NewSimpleClientset(run)
	updateAttempts := 0
	volcanoClient.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		updateAttempts++
		if updateAttempts == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
				"status-conflict", errors.New("simulated conflict"))
		}
		return false, nil, nil
	})

	desired := run.Status.DeepCopy()
	desired.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementWaitingForReplacement // engine's stale view must not undo Placed.
	desired.Phase = repackv1alpha1.RepackSucceeded
	engine := &Engine{volcanoClient: volcanoClient}
	if err := engine.writeStatus(context.Background(), run.Name, desired); err != nil {
		t.Fatalf("writeStatus() error = %v", err)
	}
	if updateAttempts != 2 {
		t.Fatalf("status update attempts = %d, want conflict retry", updateAttempts)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if updated.Status.Phase != repackv1alpha1.RepackSucceeded {
		t.Errorf("phase = %q, want Succeeded", updated.Status.Phase)
	}
	if updated.Status.Relocations[0].Placement.Phase != repackv1alpha1.PodPlacementPlaced {
		t.Errorf("placement phase = %q, want controller-owned Placed", updated.Status.Relocations[0].Placement.Phase)
	}
}

func TestUpdateStatusTerminalPersistsMessageAndCompletionTime(t *testing.T) {
	startTime := metav1.NewTime(time.Now().Add(-time.Minute))
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal-status"},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeDryRun},
		Status: repackv1alpha1.RepackRunStatus{
			Phase:     repackv1alpha1.RepackSucceeded,
			Message:   "operator-readable result",
			StartTime: &startTime,
			Conditions: []metav1.Condition{{
				Type: state.CondComplete, Status: metav1.ConditionTrue, Reason: state.ReasonNoFragmentation,
			}},
		},
	}
	client := vcfake.NewSimpleClientset(run.DeepCopy())
	recorder := record.NewFakeRecorder(10)
	engine := &Engine{volcanoClient: client, recorder: recorder}
	if err := engine.updateStatusTerminal(context.Background(), run); err != nil {
		t.Fatalf("updateStatusTerminal() error = %v", err)
	}
	updated, err := client.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status.Message != "operator-readable result" {
		t.Errorf("message=%q, want operator-readable result", updated.Status.Message)
	}
	if updated.Status.CompletionTime == nil {
		t.Fatal("completionTime was not persisted")
	}
	if updated.Status.CompletionTime.Time.Before(startTime.Time) {
		t.Errorf("completionTime=%v precedes startTime=%v", updated.Status.CompletionTime, startTime)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, state.ReasonNoFragmentation) ||
			!strings.Contains(event, "operator-readable result") {
			t.Fatalf("terminal event = %q, want reason and operator-readable message", event)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal RepackRun event was not recorded")
	}
}

func TestUpdateStatusTerminalYieldsAfterBoundedFailures(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal-status-retry"},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeDryRun},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackSucceeded,
			Conditions: []metav1.Condition{{
				Type: state.CondComplete, Status: metav1.ConditionTrue, Reason: state.ReasonNoFragmentation,
			}},
		},
	}
	client := vcfake.NewSimpleClientset(run.DeepCopy())
	failWrites := true
	updateAttempts := 0
	client.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		updateAttempts++
		if failWrites {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
				run.Name, errors.New("simulated persistent RBAC failure"))
		}
		return false, nil, nil
	})
	engine := &Engine{
		volcanoClient:           client,
		pendingTerminalStatuses: make(map[string]*repackv1alpha1.RepackRunStatus),
	}

	err := engine.updateStatusTerminal(context.Background(), run.DeepCopy())
	if !isTerminalStatusPersistenceError(err) {
		t.Fatalf("updateStatusTerminal() error = %v, want terminal persistence error", err)
	}
	if updateAttempts != terminalStatusWriteAttempts {
		t.Fatalf("status update attempts = %d, want bounded %d", updateAttempts, terminalStatusWriteAttempts)
	}
	if reconcileErrorConsumesRetryBudget(err) {
		t.Fatal("terminal persistence error must yield and requeue without consuming poison-pill budget")
	}
	if _, found := engine.pendingTerminalStatus(run.Name); !found {
		t.Fatal("terminal projection was not retained for the queued retry")
	}

	failWrites = false
	desired, found := engine.pendingTerminalStatus(run.Name)
	if !found {
		t.Fatal("pending terminal projection disappeared")
	}
	retryRun := run.DeepCopy()
	desired.DeepCopyInto(&retryRun.Status)
	if err := engine.updateStatusTerminal(context.Background(), retryRun); err != nil {
		t.Fatalf("terminal status retry failed: %v", err)
	}
	if _, found := engine.pendingTerminalStatus(run.Name); found {
		t.Fatal("terminal projection was not cleared after persistence succeeded")
	}
}
