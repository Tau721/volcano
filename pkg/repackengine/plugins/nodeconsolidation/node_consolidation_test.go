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

package nodeconsolidation

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

const testResource = v1.ResourceName("example.com/accelerator")

type consolidationSnapshot struct {
	nodes []*schedapi.NodeInfo
}

func (s consolidationSnapshot) Nodes() []*schedapi.NodeInfo       { return s.nodes }
func (consolidationSnapshot) NodeInScope(*schedapi.NodeInfo) bool { return true }
func (consolidationSnapshot) PodGroupView(schedapi.JobID) api.PodGroupView {
	return api.PodGroupView{}
}
func (consolidationSnapshot) FeasibleRelocation(context.Context, []*api.Move, []*schedapi.TaskInfo, []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}
func (consolidationSnapshot) HyperNodesSetByTier() map[int]sets.Set[string] {
	return map[int]sets.Set[string]{}
}
func (consolidationSnapshot) RealNodesSet() map[string]sets.Set[string] {
	return map[string]sets.Set[string]{}
}
func (consolidationSnapshot) HyperNodeTierNameMap() map[string]int {
	return map[string]int{}
}

func consolidationNode(name string, capacity, used int64) *schedapi.NodeInfo {
	resource := func(value int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{testResource: float64(value)}}
	}
	return &schedapi.NodeInfo{Name: name, Allocatable: resource(capacity), Used: resource(used)}
}

func TestNodeConsolidationContributesOnlyPartiallyOccupiedNodes(t *testing.T) {
	snapshot := consolidationSnapshot{nodes: []*schedapi.NodeInfo{
		consolidationNode("unavailable", 0, 0),
		consolidationNode("empty", 8, 0),
		consolidationNode("partial", 8, 4),
		consolidationNode("full", 8, 8),
	}}
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: snapshot,
		Resource: testResource,
	}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	units := ssn.FreeableUnits()
	if len(units) != 1 || !reflect.DeepEqual(units[0].Nodes, []string{"partial"}) {
		t.Fatalf("freeable units=%+v, want only the partially occupied node", units)
	}
}
