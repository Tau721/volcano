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
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Movable is the AND of every registered MovableFn (any plugin may veto).
func TestSession_MovableAND(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddMovableFn(func(t *schedapi.TaskInfo) bool { return t.Name != "x" })
	ssn.AddMovableFn(func(t *schedapi.TaskInfo) bool { return t.Job != "ns/frozen" })
	mv := ssn.Movable()
	if !mv(task("a", "ns/g", 1)) {
		t.Error("a should be movable")
	}
	if mv(task("x", "ns/g", 1)) {
		t.Error("x vetoed by first fn")
	}
	if mv(task("a", "ns/frozen", 1)) {
		t.Error("frozen vetoed by second fn")
	}
}

// A task whose workload template pins spec.nodeName (resolved by the Engine into
// PinnedTasks) is immovable, while an otherwise-identical free task stays movable.
// The planner-side guarantee: an evicted pinned pod is recreated glued to the same
// node by the template, so the drain could never complete. The pin cannot be read
// off the live pod (the scheduler writes spec.nodeName into every bound pod), hence
// the explicit set.
func TestSession_MovableExcludesPinnedTasks(t *testing.T) {
	ssn := OpenSession(SessionConfig{
		Snapshot:    &fakeSnap{},
		Resource:    gpu,
		PinnedTasks: sets.New[schedapi.TaskID]("ns/g/pinned"),
	}, nil)
	mv := ssn.Movable()

	pinned := &schedapi.TaskInfo{UID: "ns/g/pinned", Name: "pinned", Job: "ns/g"}
	if mv(pinned) {
		t.Error("pinned task must be immovable")
	}
	free := &schedapi.TaskInfo{UID: "ns/g/free", Name: "free", Job: "ns/g"}
	if !mv(free) {
		t.Error("free task must stay movable")
	}
}

func TestSession_MovableAlwaysEnforcesPodGroupIdentity(t *testing.T) {
	movable := newSession(&fakeSnap{}).Movable()
	if !movable(task("a", "ns/g", 1)) {
		t.Error("valid PodGroup task should be movable when no policy callbacks are registered")
	}
	tests := []struct {
		name string
		task *schedapi.TaskInfo
	}{
		{name: "nil task"},
		{name: "no PodGroup", task: task("a", "", 1)},
		{name: "missing namespace", task: task("a", "g", 1)},
		{name: "missing name", task: task("a", "ns/", 1)},
		{name: "multiple separators", task: task("a", "ns/g/extra", 1)},
		{name: "namespace mismatch", task: &schedapi.TaskInfo{Name: "a", Namespace: "other", Job: "ns/g"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if movable(testCase.task) {
				t.Fatalf("task %+v should be rejected by the mandatory PodGroup boundary", testCase.task)
			}
		})
	}
}

// Node feasibility (taints/affinity/topology/resources) now lives in the scheduler-
// faithful Snapshot.FeasibleRelocation feasibility check (adapter), exercised by the drain and
// e2e suites — the session no longer has a Predicate path to unit-test here.

// FreeableUnits is the union across domain plugins (node + hypernode here).
func TestSession_FreeableUnitsUnion(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddDomainFn(func(Snapshot) []api.FreeableUnit {
		return []api.FreeableUnit{{Level: "node", Nodes: []string{"n0"}, Weight: 1}}
	})
	ssn.AddDomainFn(func(Snapshot) []api.FreeableUnit {
		return []api.FreeableUnit{{Level: "hypernode", Nodes: []string{"n0", "n1"}, Weight: 3}}
	})
	if u := ssn.FreeableUnits(); len(u) != 2 {
		t.Fatalf("union len=%d, want 2", len(u))
	}
}
