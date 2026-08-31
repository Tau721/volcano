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
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// gpuJobTask builds a task of gang `gang` requesting g GPUs (gpuRes/gpu come from
// the package's other test files).
func gpuJobTask(name, gang string, g int64) *api.TaskInfo {
	return &api.TaskInfo{Name: name, Job: api.JobID(gang), InitResreq: &api.Resource{
		ScalarResources: map[v1.ResourceName]float64{gpu: float64(g)},
	}}
}

// MoveAggregate counts real relocations per gang; To==From is ignored.
func TestAggregate(t *testing.T) {
	ctx := &PlanContext{TargetResource: gpu}
	cp := NewCandidatePlan(nil, []*Move{
		{Task: gpuJobTask("a", "g1", 2), From: "n0", To: "n1"},
		{Task: gpuJobTask("b", "g1", 3), From: "n0", To: "n1"},
		{Task: gpuJobTask("c", "g2", 4), From: "n0", To: "n1"},
		{Task: gpuJobTask("d", "g2", 1), From: "n0", To: "n0"}, // no-op
	})
	a := cp.MoveAggregate(ctx)
	if a.AffectedPodGroups != 2 {
		t.Errorf("affectedPGs=%d, want 2", a.AffectedPodGroups)
	}
	if a.MovedPods != 3 {
		t.Errorf("movedPods=%d, want 3", a.MovedPods)
	}
	if a.MovedResource != 9 {
		t.Errorf("movedResource=%d, want 9", a.MovedResource)
	}
	if g1 := a.ByPodGroup["g1"]; g1 == nil || g1.MovedPods != 2 || g1.MovedResource != 5 {
		t.Errorf("g1=%+v, want {2 pods, 5 gpu}", g1)
	}
	if g2 := a.ByPodGroup["g2"]; g2 == nil || g2.MovedPods != 1 || g2.MovedResource != 4 {
		t.Errorf("g2=%+v, want {1 pod, 4 gpu}", g2)
	}
}

func TestAggregateIncludesCommittedAndIncrementalMoves(t *testing.T) {
	ctx := &PlanContext{TargetResource: gpu}
	plan := NewCandidatePlan(
		[]*Move{{Task: gpuJobTask("committed", "g1", 2), From: "n0", To: "n1"}},
		[]*Move{{Task: gpuJobTask("candidate", "g2", 3), From: "n2", To: "n3"}},
	)
	aggregate := plan.MoveAggregate(ctx)
	if aggregate.MovedPods != 2 || aggregate.MovedResource != 5 || aggregate.AffectedPodGroups != 2 {
		t.Fatalf("aggregate=%+v, want two moves across two PodGroups", aggregate)
	}
}

func TestCandidatePlanFreezesMoveCountAndScopesAggregateCacheByResource(t *testing.T) {
	npu := v1.ResourceName("huawei.com/ascend-1980")
	task := gpuJobTask("candidate", "g1", 2)
	task.InitResreq.ScalarResources[npu] = 4
	moves := []*Move{{Task: task, From: "n0", To: "n1"}}
	plan := NewCandidatePlan(nil, moves)

	// Later planner progress may append to its committed slice, but must not add
	// moves to an already-created candidate view.
	moves = append(moves, &Move{Task: gpuJobTask("later", "g2", 8), From: "n2", To: "n3"})
	if got := plan.MoveAggregate(&PlanContext{TargetResource: gpu}).MovedResource; got != 2 {
		t.Fatalf("GPU aggregate=%d, want frozen candidate total 2", got)
	}
	if got := plan.MoveAggregate(&PlanContext{TargetResource: npu}).MovedResource; got != 4 {
		t.Fatalf("NPU aggregate=%d, want resource-keyed cache recomputation to 4", got)
	}
}

func TestAggregateDoesNotInventPodGroupForUnownedTask(t *testing.T) {
	task := gpuJobTask("standalone", "", 2)
	aggregate := NewCandidatePlan(nil, []*Move{{Task: task, From: "n0", To: "n1"}}).
		MoveAggregate(&PlanContext{TargetResource: gpu})
	if aggregate.MovedPods != 1 || aggregate.MovedResource != 2 || aggregate.AffectedPodGroups != 0 || len(aggregate.ByPodGroup) != 0 {
		t.Fatalf("aggregate=%+v, want moved Pod/resource without a synthetic empty PodGroup", aggregate)
	}
}

// A To==From move is not actually a relocation (the pod stays on its source
// node), so its From must not be counted as a freed node — consistent with
// PlanMoveAggregate.addMove. This is defensive: drain unit moves never produce
// To==From today.
func TestIncrementalFromNodesExcludesNoopMoves(t *testing.T) {
	plan := NewCandidatePlan(nil, []*Move{
		{Task: gpuJobTask("a", "g1", 2), From: "n0", To: "n1"}, // real relocation, frees n0
		{Task: gpuJobTask("b", "g1", 2), From: "n2", To: "n2"}, // no-op: must not free n2
		{Task: gpuJobTask("c", "g1", 2), From: "n0", To: "n0"}, // no-op
	})
	if got, want := plan.IncrementalFromNodes(), []string{"n0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IncrementalFromNodes=%v, want %v (no-op moves must not count as freed nodes)", got, want)
	}
}

func TestFreedNodesExcludesNoopMoves(t *testing.T) {
	plan := NewCandidatePlan(
		[]*Move{{Task: gpuJobTask("committed", "g1", 2), From: "n0", To: "n1"}},
		[]*Move{
			{Task: gpuJobTask("candidate", "g2", 3), From: "n2", To: "n3"},
			{Task: gpuJobTask("noop", "g2", 1), From: "n0", To: "n0"}, // no-op: must not free n0 again / on its own
		},
	)
	if got, want := plan.FreedNodes(), []string{"n0", "n2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FreedNodes=%v, want %v (no-op move must not add its From)", got, want)
	}
}

// CalculateDisruptionCost summarizes a move set's default dimensions.
func TestCostOf(t *testing.T) {
	c := CalculateDisruptionCost([]*Move{
		{Task: gpuJobTask("a", "g1", 2), From: "n0", To: "n1"},
		{Task: gpuJobTask("c", "g2", 4), From: "n0", To: "n1"},
	}, gpu)
	if c.AffectedPodGroups != 2 || c.MovedResource != 6 || c.MovedPods != 2 {
		t.Errorf("cost=%+v, want {2, 6, 2}", c)
	}
}
