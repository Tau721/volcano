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

package adapter

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	schedulingapi "volcano.sh/apis/pkg/apis/scheduling"
	"volcano.sh/volcano/pkg/features"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedcache "volcano.sh/volcano/pkg/scheduler/cache"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
)

func TestFeasibleRelocationStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot := &SessionSnapshot{}
	placements, feasible := snapshot.FeasibleRelocation(ctx, nil, []*schedapi.TaskInfo{{Name: "victim"}}, nil)
	if feasible || placements != nil {
		t.Fatalf("placements=%v feasible=%t, want cancellation before simulation", placements, feasible)
	}
}

// SessionSnapshot.PodGroupView reads MinAvailable/Running/Footprint off
// the live JobInfos; unknown gangs yield a zero view.
func TestSessionSnapshot_PodGroupView(t *testing.T) {
	tasks := schedapi.TasksMap{
		"u0": gpuTask(0, "n0", 4),
		"u1": gpuTask(1, "n0", 4),
	}
	ji := &schedapi.JobInfo{
		MinAvailable:    2,
		Priority:        10,
		Tasks:           tasks,
		TaskStatusIndex: map[schedapi.TaskStatus]schedapi.TasksMap{schedapi.Running: tasks},
		PodGroup: &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
			SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "workers"}},
		}}},
	}
	ssn := &schedframework.Session{Jobs: map[schedapi.JobID]*schedapi.JobInfo{"ns/big": ji}}
	snap := NewSessionSnapshot(ssn, gpu, nil)

	got := snap.PodGroupView("ns/big")
	want := api.PodGroupView{
		MinAvailable: 2, Running: 2, Footprint: 8,
	}
	if got != want {
		t.Errorf("view=%+v want %+v", got, want)
	}
	if !snap.PodGroupUsesSubGroupPolicy("ns/big") {
		t.Error("PodGroupUsesSubGroupPolicy() = false, want true")
	}
	if z := snap.PodGroupView("ns/unknown"); z != (api.PodGroupView{}) {
		t.Errorf("unknown gang should yield zero view, got %+v", z)
	}
	if snap.PodGroupUsesSubGroupPolicy("ns/unknown") {
		t.Error("unknown PodGroup must not use SubGroup policy")
	}
}

// clearNodeBinding must clear node binding so simulated AddTask/filter checks
// treat the pod as scheduling onto a new node.
func TestClearNodeBinding_AddTask(t *testing.T) {
	hosted := gpuTask(0, "n0", 2)
	hosted.Resreq = gpuRes(2)
	hosted.NumaInfo = &schedapi.TopologyInfo{}
	hosted.Pod = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "t0", Namespace: "default"},
		Spec:       v1.PodSpec{NodeName: "n0"},
	}
	dest := capNode("n1", 8)
	dest.Idle = gpuRes(8)
	dest.Node = &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	if err := dest.AddTask(hosted); err == nil {
		t.Fatal("expected AddTask to reject task bound to a different node")
	}
	if err := dest.AddTask(clearNodeBinding(hosted)); err != nil {
		t.Fatalf("AddTask on relocation sim clone: %v", err)
	}
}

// Nodes returns every session node. Scope gates drain targets separately so the
// planning report can retain a cluster-wide target-resource fragmentation rate.
func TestSessionSnapshot_Nodes(t *testing.T) {
	ssn := &schedframework.Session{Nodes: map[string]*schedapi.NodeInfo{
		"n0": capNode("n0", 8), "n1": capNode("n1", 8),
	}}
	snap := NewSessionSnapshot(ssn, gpu, nil)
	if len(snap.Nodes()) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(snap.Nodes()))
	}
}

func TestSessionSnapshot_ListPodDisruptionBudgets(t *testing.T) {
	schedulerCache := schedcache.NewDefaultMockSchedulerCache("test-scheduler")
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "strict"}}
	informer := schedulerCache.SharedInformerFactory().Policy().V1().PodDisruptionBudgets().Informer()
	if err := informer.GetStore().Add(pdb); err != nil {
		t.Fatalf("add PDB to informer store: %v", err)
	}

	ssn := schedframework.OpenSession(schedulerCache, nil, nil)
	defer schedframework.CloseSessionReadOnly(ssn)
	snapshot := NewSessionSnapshot(ssn, gpu, nil)
	pdbs, err := snapshot.ListPodDisruptionBudgets()
	if err != nil {
		t.Fatalf("ListPodDisruptionBudgets() error: %v", err)
	}
	if len(pdbs) != 1 || pdbs[0].Namespace != "ns" || pdbs[0].Name != "strict" {
		t.Fatalf("PDBs=%v, want ns/strict from scheduler informer store", pdbs)
	}
}

func TestSessionSnapshot_ListPodDisruptionBudgetsUnavailable(t *testing.T) {
	if _, err := (&SessionSnapshot{}).ListPodDisruptionBudgets(); err == nil {
		t.Fatal("nil scheduler Session must return an error")
	}

	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodDisruptionBudgetsSupport, false)
	schedulerCache := schedcache.NewDefaultMockSchedulerCache("test-scheduler")
	ssn := schedframework.OpenSession(schedulerCache, nil, nil)
	defer schedframework.CloseSessionReadOnly(ssn)
	snapshot := NewSessionSnapshot(ssn, gpu, nil)
	if _, err := snapshot.ListPodDisruptionBudgets(); err == nil {
		t.Fatal("disabled PodDisruptionBudgetsSupport feature gate must return an error")
	}
}

func TestSessionSnapshot_ReceiverHasTargetResourceCapacity(t *testing.T) {
	snap := &SessionSnapshot{resource: gpu}
	node := capNode("n0", 8)
	node.Used = gpuRes(2)
	node.Idle = gpuRes(6)
	node.Releasing = schedapi.EmptyResource()
	node.Pipelined = schedapi.EmptyResource()

	if !snap.receiverHasTargetResourceCapacity(gpuTask(0, "", 4), node, []*schedapi.TaskInfo{gpuTask(1, "", 2)}) {
		t.Fatal("4 GPUs should fit after 2 GPUs already placed on a receiver with 6 GPUs free")
	}
	if snap.receiverHasTargetResourceCapacity(gpuTask(0, "", 5), node, []*schedapi.TaskInfo{gpuTask(1, "", 2)}) {
		t.Fatal("5 GPUs must not fit after 2 GPUs already placed on a receiver with 6 GPUs free")
	}

	node.Idle = schedapi.EmptyResource()
	if !snap.receiverHasTargetResourceCapacity(gpuTask(0, "", 4), node, nil) {
		t.Fatal("4 GPUs should fit from Allocatable-Used even when Idle lacks the extended resource")
	}
}
