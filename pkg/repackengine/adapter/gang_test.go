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
	"testing"

	v1 "k8s.io/api/core/v1"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
)

// A running task may reference a Job ID that is not cloned into the scheduler
// session snapshot (queue/PodGroup gating), but repack scope must still see it.
func TestSessionGangScopeLookup_FallsBackToTasksOnNodes(t *testing.T) {
	gpu := v1.ResourceName("nvidia.com/gpu")
	task := &schedapi.TaskInfo{
		Name: "pod-a",
		Job:  "ns/frag-a",
		TransactionContext: schedapi.TransactionContext{
			NodeName: "n0",
		},
		InitResreq: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: 4}},
	}
	node := &schedapi.NodeInfo{
		Name:  "n0",
		Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{"pod-a": task},
	}
	ssn := &schedframework.Session{Nodes: map[string]*schedapi.NodeInfo{"n0": node}}

	lookup := SessionGangScopeLookup(ssn)
	matcher, err := enginescope.NewMatcher(nil, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.InScope("ns/frag-a") {
		t.Fatal("gang on a node must be in scope even when absent from ssn.Jobs")
	}
	if matcher.InScope("ns/does-not-exist") {
		t.Fatal("unknown gang must stay out of scope")
	}
}
