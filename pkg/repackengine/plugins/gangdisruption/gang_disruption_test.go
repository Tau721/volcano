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

package gangdisruption

import (
	"context"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const gpu = v1.ResourceName("nvidia.com/gpu")

func gpuRes(n int64) *schedapi.Resource {
	return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
}

func tk(name, gang string, g int64) *schedapi.TaskInfo {
	return &schedapi.TaskInfo{Name: name, Job: schedapi.JobID(gang), InitResreq: gpuRes(g)}
}

func mv(t *schedapi.TaskInfo) *api.Move { return &api.Move{Task: t, From: "n0", To: "n1"} }

// fixedView is a test PodGroupViewer returning the same gang facts for every id.
type fixedView struct{ view api.PodGroupView }

func (f fixedView) PodGroupView(schedapi.JobID) api.PodGroupView { return f.view }

type gangSnapshot struct{ nodes []*schedapi.NodeInfo }

func (s *gangSnapshot) Nodes() []*schedapi.NodeInfo                { return s.nodes }
func (*gangSnapshot) NodeInScope(*schedapi.NodeInfo) bool          { return true }
func (*gangSnapshot) PodGroupView(schedapi.JobID) api.PodGroupView { return api.PodGroupView{} }
func (*gangSnapshot) FeasibleRelocation(context.Context, []*api.Move, []*schedapi.TaskInfo, []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}
func (*gangSnapshot) HyperNodesSetByTier() map[int]sets.Set[string] {
	return map[int]sets.Set[string]{}
}
func (*gangSnapshot) RealNodesSet() map[string]sets.Set[string] {
	return map[string]sets.Set[string]{}
}
func (*gangSnapshot) HyperNodeTierNameMap() map[string]int {
	return map[string]int{}
}

// gang g: Running=4, MinAvailable=3 → slack=1, Footprint=8.
func gangCtx() *api.PlanContext {
	return &api.PlanContext{
		TargetResource: gpu,
		PodGroupViews:  fixedView{view: api.PodGroupView{MinAvailable: 3, Running: 4, Footprint: 8}},
	}
}

func TestMeasurePodGroupDisruptionBelongsToGangPolicy(t *testing.T) {
	view := api.PodGroupView{Running: 4, MinAvailable: 2, Footprint: 16}
	tests := []struct {
		name                string
		movedPods           int64
		movedResource       int64
		wantBreached        bool
		wantDamagedResource int64
	}{
		{name: "within running slack", movedPods: 2, movedResource: 8, wantDamagedResource: 8},
		{name: "breaches minAvailable", movedPods: 3, movedResource: 12, wantBreached: true, wantDamagedResource: 16},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := measurePodGroupDisruption(view, test.movedPods, test.movedResource)
			if got.breached != test.wantBreached || got.damagedResource != test.wantDamagedResource {
				t.Fatalf("disruption=%+v, want breached=%v damagedResource=%d",
					got, test.wantBreached, test.wantDamagedResource)
			}
		})
	}
}

// ScoreDamagedResource is a step function: within slack only the moved cards count;
// once minAvailable is breached the whole gang Footprint counts.
func TestScoreDamagedGPU_StepFunction(t *testing.T) {
	ctx := gangCtx()

	within := api.NewCandidatePlan(nil, []*api.Move{mv(tk("p0", "ns/g", 2))}) // 1 pod ≤ slack 1
	if s := scoreDamagedResource(ctx, within); s != 2 {
		t.Errorf("within slack: damaged=%v, want 2 (moved cards)", s)
	}

	breach := api.NewCandidatePlan(nil, []*api.Move{
		mv(tk("p0", "ns/g", 2)), mv(tk("p1", "ns/g", 2)), // 2 pods > slack 1
	})
	if s := scoreDamagedResource(ctx, breach); s != 8 {
		t.Errorf("breach: damaged=%v, want 8 (whole footprint)", s)
	}
}

// ScoreGangBreaches counts gangs pushed below minAvailable.
func TestScoreGangBreaches(t *testing.T) {
	ctx := gangCtx()

	noBreach := api.NewCandidatePlan(nil, []*api.Move{mv(tk("p0", "ns/g", 1))}) // 1 ≤ slack 1
	if s := scoreGangBreaches(ctx, noBreach); s != 0 {
		t.Errorf("no breach: %v, want 0", s)
	}

	breach := api.NewCandidatePlan(nil, []*api.Move{
		mv(tk("p0", "ns/g", 1)), mv(tk("p1", "ns/g", 1)), // 2 > slack 1
	})
	if s := scoreGangBreaches(ctx, breach); s != 1 {
		t.Errorf("breach: %v, want 1", s)
	}
}

func TestScoreFutureReceiverImpactUsesMarginalGangCost(t *testing.T) {
	ctx := gangCtx()
	candidate := &framework.PlanningCandidate{Plan: api.NewCandidatePlan(nil,
		[]*api.Move{mv(tk("p0", "ns/g", 2))})} // consumes the gang's one-pod slack
	receiver := &framework.ReceiverCandidate{}
	futureMoves := map[schedapi.JobID]api.PodGroupMoveAggregate{
		"ns/g": {MovedPods: 1, MovedResource: 2},
	}

	// Filling this receiver prevents a future drain that would newly breach the
	// gang. Damaged resource grows from the two moved cards to the full footprint.
	want := framework.ReceiverPreference{1, 0, 6, 2, 1}
	if got := scoreFutureReceiverImpact(ctx, candidate, receiver, futureMoves); !reflect.DeepEqual(got, want) {
		t.Fatalf("future receiver preference=%v, want %v", got, want)
	}
}

func TestAggregateTasksByPodGroupBelongsToGangDisruptionPlugin(t *testing.T) {
	aggregates := aggregateTasksByPodGroup([]*schedapi.TaskInfo{
		tk("a", "ns/g", 2), tk("b", "ns/g", 3), tk("c", "ns/other", 1),
	}, gpu)
	if got := aggregates["ns/g"]; got.MovedPods != 2 || got.MovedResource != 5 {
		t.Fatalf("ns/g aggregate=%+v, want 2 pods and 5 resources", got)
	}
	if got := aggregates["ns/other"]; got.MovedPods != 1 || got.MovedResource != 1 {
		t.Fatalf("ns/other aggregate=%+v, want 1 pod and 1 resource", got)
	}
}

func TestFutureMovesCacheOnlyScansPreferredReceiver(t *testing.T) {
	preferred := &schedapi.NodeInfo{
		Name: "preferred",
		Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
			"preferred-task": tk("preferred-task", "ns/g", 2),
		},
	}
	notEvaluated := &schedapi.NodeInfo{
		Name: "not-evaluated",
		Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
			"not-evaluated-task": tk("not-evaluated-task", "ns/other", 2),
		},
	}
	ssn := framework.OpenSession(framework.SessionConfig{
		Resource: gpu,
		Snapshot: &gangSnapshot{nodes: []*schedapi.NodeInfo{preferred, notEvaluated}},
	}, nil)
	defer framework.CloseSession(ssn)
	movableCalls := 0
	ssn.AddMovableFn(func(*schedapi.TaskInfo) bool {
		movableCalls++
		return true
	})
	plugin := &gangDisruptionPlugin{}

	first := plugin.futureMovesForReceiver(ssn, preferred)
	second := plugin.futureMovesForReceiver(ssn, preferred)
	if first["ns/g"].MovedPods != 1 || second["ns/g"].MovedPods != 1 {
		t.Fatalf("preferred receiver cache=%v/%v, want one ns/g move", first, second)
	}
	if movableCalls != 1 {
		t.Fatalf("movable calls=%d, want preferred receiver scanned exactly once", movableCalls)
	}
	if _, found := plugin.futureMovesByNode[notEvaluated.Name]; found {
		t.Fatal("receiver outside preference evaluation must not be scanned or cached")
	}
}

func TestConfiguredGangWeights(t *testing.T) {
	arguments := framework.Arguments{
		argGangBreachesWeight:    15,
		argDamagedResourceWeight: 0,
	}
	if err := framework.ValidatePluginArguments(Name, arguments); err != nil {
		t.Fatalf("valid weights rejected: %v", err)
	}
	plugin, ok := framework.GetPlugin(Name, arguments)
	if !ok {
		t.Fatal("gangdisruption plugin is not registered")
	}
	configured := plugin.(*gangDisruptionPlugin)
	if configured.gangBreachesWeight != 15 || configured.damagedResourceWeight != 0 {
		t.Fatalf("configured weights=%+v, want 15/0", configured)
	}
	if err := framework.ValidatePluginArguments(Name, framework.Arguments{argGangBreachesWeight: -1}); err == nil {
		t.Fatal("negative gang weight should be rejected")
	}
	if err := framework.ValidatePluginArguments(Name, framework.Arguments{argGangBreachesWeight: 1.5}); err == nil {
		t.Fatal("fractional gang weight should be rejected")
	}
}
