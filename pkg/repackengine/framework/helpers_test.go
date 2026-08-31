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

package framework

import (
	"context"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

const gpu = v1.ResourceName("nvidia.com/gpu")

// fakeSnap is a minimal Snapshot for framework tests.
type fakeSnap struct {
	nodes []*schedapi.NodeInfo
	views map[schedapi.JobID]api.PodGroupView
}

func (f *fakeSnap) Nodes() []*schedapi.NodeInfo                     { return f.nodes }
func (f *fakeSnap) NodeInScope(*schedapi.NodeInfo) bool             { return true }
func (f *fakeSnap) PodGroupView(id schedapi.JobID) api.PodGroupView { return f.views[id] }

// FeasibleRelocation is not exercised by the framework-level tests (they cover
// session plumbing, not the drain planner), so this is an inert stand-in.

func (f *fakeSnap) FeasibleRelocation(ctx context.Context, _ []*api.Move, _ []*schedapi.TaskInfo, _ []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}

// HyperNodesSetByTier / RealNodesSet / HyperNodeTierNameMap are stubs required
// by the extended Snapshot interface: framework-level tests do not plan against
// HyperNode topology, so they all return empty views.
func (f *fakeSnap) HyperNodesSetByTier() map[int]sets.Set[string] { return map[int]sets.Set[string]{} }
func (f *fakeSnap) RealNodesSet() map[string]sets.Set[string]     { return map[string]sets.Set[string]{} }
func (f *fakeSnap) HyperNodeTierNameMap() map[string]int          { return map[string]int{} }

func node(name string, labels map[string]string) *schedapi.NodeInfo {
	return &schedapi.NodeInfo{Name: name, Node: &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}}
}

func gpuRes(n int64) *schedapi.Resource {
	return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(n)}}
}

func task(name, gang string, g int64) *schedapi.TaskInfo {
	return &schedapi.TaskInfo{Name: name, Job: schedapi.JobID(gang), InitResreq: gpuRes(g)}
}

func move(t *schedapi.TaskInfo, from, to string) *api.Move {
	return &api.Move{Task: t, From: from, To: to}
}

func newSession(snap Snapshot) *Session {
	return OpenSession(SessionConfig{Snapshot: snap, Resource: gpu}, nil)
}
