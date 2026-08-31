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

package repackbudget

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

type budgetSnapshot struct{}

func (budgetSnapshot) Nodes() []*schedapi.NodeInfo                  { return nil }
func (budgetSnapshot) NodeInScope(*schedapi.NodeInfo) bool          { return true }
func (budgetSnapshot) PodGroupView(schedapi.JobID) api.PodGroupView { return api.PodGroupView{} }
func (budgetSnapshot) FeasibleRelocation(context.Context, []*api.Move, []*schedapi.TaskInfo, []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}
func (budgetSnapshot) HyperNodesSetByTier() map[int]sets.Set[string] {
	return map[int]sets.Set[string]{}
}
func (budgetSnapshot) RealNodesSet() map[string]sets.Set[string] {
	return map[string]sets.Set[string]{}
}
func (budgetSnapshot) HyperNodeTierNameMap() map[string]int {
	return map[string]int{}
}

func budgetMove(name string, job schedapi.JobID, cards int64) *api.Move {
	return &api.Move{From: "source", To: "receiver", Task: &schedapi.TaskInfo{
		Name: name,
		Job:  job,
		InitResreq: &schedapi.Resource{
			ScalarResources: map[v1.ResourceName]float64{testGPU: float64(cards)},
		},
	}}
}

func TestRepackBudgetPluginChecksWholeProspectivePlan(t *testing.T) {
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot:       budgetSnapshot{},
		Resource:       testGPU,
		MaxPodGroups:   1,
		MaxResource:    4,
		LimitPodGroups: true,
		LimitResource:  true,
	}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	candidate := &framework.PlanningCandidate{Plan: api.NewCandidatePlan(
		[]*api.Move{budgetMove("committed", "ns/a", 2)},
		[]*api.Move{budgetMove("candidate", "ns/b", 2)},
	)}
	if got := ssn.CandidateAdmissible(candidate); got == nil || got.Reason != "max_pod_groups" {
		t.Fatalf("result=%+v, want whole-plan max_pod_groups rejection", got)
	}

	resourceOnly := framework.OpenSession(framework.SessionConfig{
		Snapshot: budgetSnapshot{}, Resource: testGPU, MaxResource: 3, LimitResource: true,
	}, framework.PluginOptions(Name))
	defer framework.CloseSession(resourceOnly)
	if got := resourceOnly.CandidateAdmissible(candidate); got == nil || got.Reason != "max_resource" {
		t.Fatalf("result=%+v, want whole-plan max_resource rejection", got)
	}
}

func TestRepackBudgetIsOptional(t *testing.T) {
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: budgetSnapshot{}, Resource: testGPU,
		MaxPodGroups: 0, MaxResource: 0, LimitPodGroups: true, LimitResource: true,
	}, nil)
	defer framework.CloseSession(ssn)

	candidate := &framework.PlanningCandidate{Plan: api.NewCandidatePlan(nil,
		[]*api.Move{budgetMove("candidate", "ns/a", 2)})}
	if got := ssn.CandidateAdmissible(candidate); got != nil {
		t.Fatalf("repack budget must not apply when plugin is disabled, got %+v", got)
	}
}
