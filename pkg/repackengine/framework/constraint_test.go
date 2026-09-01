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

	"volcano.sh/volcano/pkg/repackengine/api"
)

// The built-in MinNodesFreed benefit gate is registered as a plan constraint, so
// PlanAdmissible rejects a plan that frees fewer nodes than required and admits
// one that meets it.
func TestPlanAdmissible_BuiltinMinNodesFreed(t *testing.T) {
	ssn := OpenSession(SessionConfig{Snapshot: &fakeSnap{}, Resource: gpu, MinNodesFreed: 2}, nil)

	one := &api.RepackPlan{FreedNodes: []string{"n0"}}       // Benefit 1
	two := &api.RepackPlan{FreedNodes: []string{"n0", "n1"}} // Benefit 2

	if ssn.PlanAdmissible(one) {
		t.Error("MinNodesFreed=2 must reject a plan that frees 1 node")
	}
	if !ssn.PlanAdmissible(two) {
		t.Error("MinNodesFreed=2 must admit a plan that frees 2 nodes")
	}
}

// A plugin-registered PlanConstraintFn is a hard veto: it can reject a plan that
// passes the built-in gates, and constraints AND-aggregate.
func TestPlanAdmissible_PluginConstraintVetoes(t *testing.T) {
	ssn := OpenSession(SessionConfig{Snapshot: &fakeSnap{}, Resource: gpu}, nil) // default MinNodesFreed -> 1
	plan := &api.RepackPlan{FreedNodes: []string{"n0", "n1"}}

	if !ssn.PlanAdmissible(plan) {
		t.Fatal("plan should be admissible before adding a vetoing constraint")
	}
	ssn.AddConstraintFn(func(_ *api.PlanContext, _ *api.RepackPlan) (bool, string) { return false, "TestVeto" })
	if ssn.PlanAdmissible(plan) {
		t.Error("a PlanConstraintFn returning false must reject the plan")
	}
	// The first failing constraint's reason is recorded on the session so the
	// action can report the binding gate, not a generic fallback.
	if got := ssn.ConstraintRejection(); got != "TestVeto" {
		t.Errorf("constraintRejection=%q, want %q", got, "TestVeto")
	}
}
