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

package binpack

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const testGPU = v1.ResourceName("nvidia.com/gpu")

type binpackSnapshot struct{}

func (binpackSnapshot) Nodes() []*schedapi.NodeInfo                  { return nil }
func (binpackSnapshot) NodeInScope(*schedapi.NodeInfo) bool          { return true }
func (binpackSnapshot) PodGroupView(schedapi.JobID) api.PodGroupView { return api.PodGroupView{} }
func (binpackSnapshot) FeasibleRelocation(context.Context, []*api.Move, []*schedapi.TaskInfo, []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}
func (binpackSnapshot) HyperNodesSetByTier() map[int]sets.Set[string] {
	return map[int]sets.Set[string]{}
}
func (binpackSnapshot) RealNodesSet() map[string]sets.Set[string] {
	return map[string]sets.Set[string]{}
}
func (binpackSnapshot) HyperNodeTierNameMap() map[string]int {
	return map[string]int{}
}

func binpackNode(name string, used int64) *schedapi.NodeInfo {
	return &schedapi.NodeInfo{
		Name: name,
		Used: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{testGPU: float64(used)}},
	}
}

func binpackTask(name string, requested int64) *schedapi.TaskInfo {
	return &schedapi.TaskInfo{
		Name: name,
		InitResreq: &schedapi.Resource{
			ScalarResources: map[v1.ResourceName]float64{testGPU: float64(requested)},
		},
	}
}

func TestBinpackPluginOrdersVictimsAndComposesReceiverPhases(t *testing.T) {
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: binpackSnapshot{}, Resource: testGPU,
	}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	pool := ssn.ReceiverPool([]*schedapi.NodeInfo{binpackNode("empty", 0), binpackNode("used", 2)})
	if len(pool) != 2 {
		t.Fatalf("receiver pool size=%d, want binpack not to own base receiver filtering", len(pool))
	}

	victims := ssn.OrderVictims([]*schedapi.TaskInfo{
		binpackTask("small", 1), binpackTask("large", 4), binpackTask("medium", 2),
	})
	if victims[0].Name != "large" || victims[1].Name != "medium" || victims[2].Name != "small" {
		t.Fatalf("victim order=[%s %s %s], want [large medium small]",
			victims[0].Name, victims[1].Name, victims[2].Name)
	}

	receivers := []*framework.ReceiverCandidate{
		{Node: binpackNode("free-small", 1), AvailableResource: 1},
		{Node: binpackNode("stays-large", 1), StaysOccupied: true, AvailableResource: 6},
		{Node: binpackNode("stays-small", 1), StaysOccupied: true, AvailableResource: 2},
	}
	ordered := ssn.OrderReceivers(&framework.PlanningCandidate{}, receivers)
	if ordered[0].Receiver.Node.Name != "stays-small" ||
		ordered[1].Receiver.Node.Name != "stays-large" ||
		ordered[2].Receiver.Node.Name != "free-small" {
		t.Fatalf("receiver order=[%s %s %s], want [stays-small stays-large free-small]",
			ordered[0].Receiver.Node.Name, ordered[1].Receiver.Node.Name, ordered[2].Receiver.Node.Name)
	}
}
