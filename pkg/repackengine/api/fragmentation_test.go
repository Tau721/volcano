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

package api

import (
	"math"
	"testing"

	v1 "k8s.io/api/core/v1"

	"volcano.sh/volcano/pkg/scheduler/api"
)

const gpu = v1.ResourceName("nvidia.com/gpu")

func TestFragmentationRate(t *testing.T) {
	fragmentation := ResourceFragmentation{
		Resource: gpu, ProvidingNodeCount: 20, OccupiedNodeCount: 18, OptimalOccupiedNodeCount: 16,
	}
	if got := fragmentation.FragmentationRate(); math.Abs(got-0.10) > 1e-9 {
		t.Errorf("gpu FragmentationRate=%v want 0.10", got)
	}
}

func TestMeasureResource(t *testing.T) {
	mkRes := func(n int64) *api.Resource {
		return &api.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
	}
	mkTask := func(g int64) *api.TaskInfo { return &api.TaskInfo{Resreq: mkRes(g)} }
	node := func(cap, used int64, reqs ...int64) *api.NodeInfo {
		tasks := map[api.TaskID]*api.TaskInfo{}
		for i, g := range reqs {
			tasks[api.TaskID(string(rune('a'+i)))] = mkTask(g)
		}
		return &api.NodeInfo{Allocatable: mkRes(cap), Used: mkRes(used), Tasks: tasks}
	}
	nodes := []*api.NodeInfo{
		node(8, 4, 4),       // 8-TargetResource node, 4 used by one 4-TargetResource task
		node(8, 4, 2, 1, 1), // fragmented: three small tasks summing 4
		node(8, 0),          // empty 8-TargetResource node
		{Allocatable: &api.Resource{ScalarResources: map[v1.ResourceName]float64{"cpu": 0}}}, // non-TargetResource node, ignored
	}
	f := MeasureResourceFragmentation(nodes, gpu)
	// Three nodes provide the target resource; two are occupied; demand {4,2,1,1}=8
	// means the optimal occupied-node count is ceil(8/8)=1.
	if f.ProvidingNodeCount != 3 || f.OccupiedNodeCount != 2 || f.OptimalOccupiedNodeCount != 1 || !f.Exact {
		t.Fatalf("MeasureResourceFragmentation = %+v; want providing=3 occupied=2 optimal=1 Exact=true", f)
	}
	if got := f.FragmentationRate(); math.Abs(got-1.0/3.0) > 1e-9 { // (2-1)/3
		t.Errorf("FragmentationRate=%v want %v", got, 1.0/3.0)
	}
}

func TestMeasureResourceHeterogeneousIsOrderIndependentAndBounded(t *testing.T) {
	mkRes := func(n int64) *api.Resource {
		return &api.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
	}
	node := func(name string, cap, used, req int64) *api.NodeInfo {
		tasks := map[api.TaskID]*api.TaskInfo{}
		if req > 0 {
			tasks[api.TaskID(name+"-pod")] = &api.TaskInfo{Resreq: mkRes(req)}
		}
		return &api.NodeInfo{Name: name, Allocatable: mkRes(cap), Used: mkRes(used), Tasks: tasks}
	}
	four := node("four", 4, 4, 4)
	eight := node("eight", 8, 8, 8)
	emptyEight := node("empty-eight", 8, 0, 0)

	a := MeasureResourceFragmentation([]*api.NodeInfo{four, eight, emptyEight}, gpu)
	b := MeasureResourceFragmentation([]*api.NodeInfo{emptyEight, eight, four}, gpu)
	if a.OptimalOccupiedNodeCount != b.OptimalOccupiedNodeCount || a.OccupiedNodeCount != b.OccupiedNodeCount || a.ProvidingNodeCount != b.ProvidingNodeCount {
		t.Fatalf("heterogeneous metric depends on node order: first=%+v reversed=%+v", a, b)
	}
	if a.Exact || a.OptimalOccupiedNodeCount < 0 || a.OptimalOccupiedNodeCount > a.OccupiedNodeCount || a.FragmentationRate() < 0 || a.FragmentationRate() > 1 {
		t.Fatalf("heterogeneous metric must be inexact and bounded: %+v rate=%v", a, a.FragmentationRate())
	}
}
