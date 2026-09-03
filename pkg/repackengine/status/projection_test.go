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

package status_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

const gpuResource = v1.ResourceName("nvidia.com/gpu")

type testPodGroupPlacementPolicies map[schedapi.JobID]bool

func (policies testPodGroupPlacementPolicies) PodGroupUsesSubGroupPolicy(id schedapi.JobID) bool {
	return policies[id]
}

func mkMove(name, job string, cards float64, from, to string) *engineapi.Move {
	return &engineapi.Move{
		Task: &schedapi.TaskInfo{
			Name: name, Job: schedapi.JobID(job),
			// Volcano stores extended resources in milli (1 device = 1000); mirror
			// production so BuildStatusMoves' milli-to-whole conversion is exercised faithfully.
			Resreq: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpuResource: cards * 1000}},
		},
		From: from, To: to,
	}
}

// PercentagePoints rounds a 0-1 fraction to a percentage point, clamped to [0,100].
func TestPct(t *testing.T) {
	cases := []struct {
		in   float64
		want int32
	}{
		{0, 0}, {0.5, 50}, {1, 100},
		{0.004, 0}, {0.005, 1}, // round-half-up at the 0.5pp boundary
		{-0.3, 0},  // clamp low
		{1.7, 100}, // clamp high
	}
	for _, c := range cases {
		if got := enginestatus.PercentagePoints(c.in); got != c.want {
			t.Errorf("percentagePoints(%v)=%d, want %d", c.in, got, c.want)
		}
	}
}

func TestSplitJobID(t *testing.T) {
	if ns, n := enginestatus.SplitPodGroupID("team-a/gang-1"); ns != "team-a" || n != "gang-1" {
		t.Errorf("split=%q/%q, want team-a/gang-1", ns, n)
	}
	if ns, n := enginestatus.SplitPodGroupID("bare"); ns != "" || n != "bare" {
		t.Errorf("split no-slash=%q/%q, want ''/bare", ns, n)
	}
	if ns, n := enginestatus.SplitPodGroupID(""); ns != "" || n != "" {
		t.Errorf("split empty=%q/%q, want ''/''", ns, n)
	}
}

func TestFreedNodesOf(t *testing.T) {
	if enginestatus.SortedFreedNodeNames(nil) != nil {
		t.Error("nil plan -> nil")
	}
	got := enginestatus.SortedFreedNodeNames(&engineapi.RepackPlan{FreedNodes: []string{"n2", "n0", "n1"}})
	if len(got) != 3 || got[0] != "n0" || got[1] != "n1" || got[2] != "n2" {
		t.Errorf("freedNodes=%v, want sorted [n0 n1 n2]", got)
	}
}

func TestMovesOf(t *testing.T) {
	// nil plan / no moves.
	if enginestatus.BuildStatusMoves(nil, gpuResource, nil) != nil {
		t.Error("nil plan -> nil")
	}

	// A gang whose pods spread across nodes; a no-op move (To==From) is dropped;
	// two gangs come out namespace/name-sorted.
	plan := &engineapi.RepackPlan{Moves: []*engineapi.Move{
		mkMove("w1", "ns/gb", 2, "n0", "n2"),
		mkMove("w0", "ns/gb", 2, "n1", "n2"), // same gang, different source node
		mkMove("a0", "ns/ga", 4, "n0", "n3"),
		mkMove("noop", "ns/ga", 1, "n5", "n5"), // To==From: filtered
	}}
	owners := map[string]*repackv1alpha1.WorkloadRef{
		"ns/ga": {APIVersion: "apps/v1", Kind: "Deployment", Name: "trainer"},
	}
	moves := enginestatus.BuildStatusMoves(plan, gpuResource, owners)
	if len(moves) != 2 {
		t.Fatalf("got %d podgroup moves, want 2", len(moves))
	}
	// sorted by (namespace, podGroupName): ga before gb.
	if moves[0].PodGroupName != "ga" || moves[1].PodGroupName != "gb" {
		t.Fatalf("order=%s,%s want ga,gb", moves[0].PodGroupName, moves[1].PodGroupName)
	}
	ga, gb := moves[0], moves[1]
	if ga.Namespace != "ns" || ga.Cards != 4 || len(ga.Pods) != 1 {
		t.Errorf("ga=%+v", ga)
	}
	if ga.Owner == nil || ga.Owner.APIVersion != "apps/v1" || ga.Owner.Kind != "Deployment" || ga.Owner.Name != "trainer" {
		t.Errorf("ga owner=%+v, want apps/v1 Deployment trainer", ga.Owner)
	}
	if gb.Owner != nil {
		t.Errorf("gb owner=%+v, want nil", gb.Owner)
	}
	if gb.Cards != 4 || len(gb.Pods) != 2 { // 2+2, two pods from two source nodes
		t.Errorf("gb cards=%d pods=%d, want 4/2", gb.Cards, len(gb.Pods))
	}
	// gb pods are name-sorted: w0 then w1.
	if gb.Pods[0].Name != "w0" || gb.Pods[0].FromNode != "n1" || gb.Pods[1].Name != "w1" {
		t.Errorf("gb pods not sorted: %+v", gb.Pods)
	}
}

func TestSummaryOf(t *testing.T) {
	s := enginestatus.BuildRepackSummary(engineapi.Report{FragmentationRateBefore: 0.4, FragmentationRateAfter: 0.2, NodesFreed: 3})
	if s.FragBeforePercent != 40 || s.FragAfterPercent != 20 || s.FreedNodeCount != 3 {
		t.Errorf("summary=%+v", s)
	}
}

func TestRepackSummarySerializesZeroValues(t *testing.T) {
	data, err := json.Marshal(repackv1alpha1.RepackSummary{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"fragBeforePercent":0`,
		`"fragAfterPercent":0`,
		`"freedNodeCount":0`,
		`"movedCardCount":0`,
	} {
		if !strings.Contains(string(data), field) {
			t.Errorf("serialized summary %s does not contain %s", data, field)
		}
	}
}

func TestRepackResultSerializesZeroValues(t *testing.T) {
	data, err := json.Marshal(repackv1alpha1.RepackResult{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"fragAfterPercent":0`,
		`"freedNodeCount":0`,
		`"movedCardCount":0`,
		`"metricsVerified":false`,
	} {
		if !strings.Contains(string(data), field) {
			t.Errorf("serialized result %s does not contain %s", data, field)
		}
	}
}

func TestRepackResultSerializesVerifiedFreedNodes(t *testing.T) {
	data, err := json.Marshal(repackv1alpha1.RepackResult{
		FreedNodeCount:  2,
		FreedNodes:      []string{"node-a", "node-b"},
		MetricsVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"freedNodes":["node-a","node-b"]`) {
		t.Fatalf("serialized result %s does not contain the verified freed-node set", data)
	}
}

func TestBuildResolvedScope(t *testing.T) {
	resource := func(cards int64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpuResource: float64(cards * 1000)}}
	}
	nodes := []*schedapi.NodeInfo{
		{
			Name:        "n0",
			Node:        &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "selected"}}},
			Allocatable: resource(8),
			Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
				"a": {Job: "ns/a", Resreq: resource(2)},
				"b": {Job: "ns/b", Resreq: resource(2)},
			},
		},
		{
			Name:        "n1",
			Node:        &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "excluded"}}},
			Allocatable: resource(8),
			Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
				"a2": {Job: "ns/a", Resreq: resource(2)},
			},
		},
		{Name: "cpu-only", Allocatable: schedapi.EmptyResource()},
	}
	scope, err := enginescope.NewMatcher(
		&repackv1alpha1.RepackScope{
			PodGroups: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{Names: []string{"ns/a"}},
			},
			Nodes: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "selected"}},
				},
			},
		},
		func(id schedapi.JobID) (string, labels.Labels, bool) {
			return string(id), labels.Set{}, id == "ns/a" || id == "ns/b"
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved := enginestatus.BuildResolvedScope(nodes, scope, gpuResource)
	if resolved.NodeCount != 1 || resolved.PodGroupCount != 1 {
		t.Fatalf("resolvedScope = %+v, want nodeCount=1 podGroupCount=1", resolved)
	}
}

func TestStampLifecycleIsIdempotent(t *testing.T) {
	start := time.Unix(100, 0)
	later := start.Add(time.Minute)
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning}}
	enginestatus.StampLifecycle(run, start)
	if run.Status.StartTime == nil || !run.Status.StartTime.Time.Equal(start) || run.Status.CompletionTime != nil {
		t.Fatalf("running lifecycle = %+v", run.Status)
	}
	run.Status.Phase = repackv1alpha1.RepackSucceeded
	enginestatus.StampLifecycle(run, later)
	if run.Status.CompletionTime == nil || !run.Status.CompletionTime.Time.Equal(later) {
		t.Fatalf("completionTime = %v, want %v", run.Status.CompletionTime, later)
	}
	enginestatus.StampLifecycle(run, later.Add(time.Minute))
	if !run.Status.StartTime.Time.Equal(start) || !run.Status.CompletionTime.Time.Equal(later) {
		t.Fatalf("lifecycle timestamps were overwritten: %+v", run.Status)
	}
}

func TestMarkExecuteNotPerformedPreservesPlanAndClearsExecutionState(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{
				FragBeforePercent: 40,
				FragAfterPercent:  20,
				FreedNodeCount:    2,
				MovedCardCount:    8,
			},
			Moves:      []repackv1alpha1.RepackMove{{PodGroupName: "pg"}},
			FreedNodes: []string{"n0"},
		},
		Result:      &repackv1alpha1.RepackResult{FragAfterPercent: 30, FreedNodeCount: 1, MovedCardCount: 4},
		Relocations: []repackv1alpha1.PodRelocationStatus{{VictimPodName: "pod"}},
	}}
	enginestatus.MarkExecuteNotPerformed(run)
	summary := run.Status.Plan.Summary
	if summary.FragAfterPercent != 20 || summary.FreedNodeCount != 2 || summary.MovedCardCount != 8 {
		t.Fatalf("plan summary was modified: %+v", summary)
	}
	if len(run.Status.Plan.Moves) != 1 || len(run.Status.Plan.FreedNodes) != 1 {
		t.Fatalf("complete plan was not preserved: %+v", run.Status.Plan)
	}
	if run.Status.Result != nil || len(run.Status.Relocations) != 0 {
		t.Fatalf("execution state was not cleared: %+v", run.Status)
	}
}

func TestBuildPodRelocations(t *testing.T) {
	if relocations, err := enginestatus.BuildPodRelocations(nil, nil); err != nil || relocations != nil {
		t.Error("nil plan -> nil")
	}
	pod := &v1.Pod{Spec: v1.PodSpec{NodeSelector: map[string]string{"accelerator": "npu"}}}
	plan := &engineapi.RepackPlan{Moves: []*engineapi.Move{
		{
			Task: &schedapi.TaskInfo{
				Name: "w-0", Namespace: "ns", Job: "ns/g",
				Pod: pod,
			},
			From: "n0", To: "n2",
		},
	}}
	relocations, err := enginestatus.BuildPodRelocations(plan, testPodGroupPlacementPolicies{
		"ns/g": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(relocations) != 1 {
		t.Fatalf("got %d relocations, want 1", len(relocations))
	}
	relocation := relocations[0]
	if relocation.Namespace != "ns" || relocation.PodGroupName != "g" || relocation.VictimPodName != "w-0" || relocation.PlannedNodeName != "n2" {
		t.Errorf("relocation=%+v", relocation)
	}
	if relocation.Placement.Phase != repackv1alpha1.PodPlacementWaitingForReplacement {
		t.Errorf("phase=%q, want WaitingForReplacement", relocation.Placement.Phase)
	}
	if relocation.SchedulingRequirementsHash == "" {
		t.Error("SubGroup-enabled PodGroup must record schedulingRequirementsHash")
	}
	homogeneous, err := enginestatus.BuildPodRelocations(plan, testPodGroupPlacementPolicies{
		"ns/g": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if homogeneous[0].SchedulingRequirementsHash != "" {
		t.Errorf("homogeneous PodGroup hash = %q, want empty", homogeneous[0].SchedulingRequirementsHash)
	}

	missingVictimPod := &engineapi.RepackPlan{Moves: []*engineapi.Move{{
		Task: &schedapi.TaskInfo{Name: "missing", Namespace: "ns", Job: "ns/g"},
		From: "n0", To: "n2",
	}}}
	if _, err := enginestatus.BuildPodRelocations(missingVictimPod, testPodGroupPlacementPolicies{
		"ns/g": true,
	}); err == nil {
		t.Fatal("SubGroup placement without a victim Pod must fail before eviction")
	}

	invalidPodGroups := []struct {
		name string
		task *schedapi.TaskInfo
	}{
		{name: "missing PodGroup", task: &schedapi.TaskInfo{Name: "victim", Namespace: "ns"}},
		{name: "malformed PodGroup", task: &schedapi.TaskInfo{Name: "victim", Namespace: "ns", Job: "g"}},
		{name: "namespace mismatch", task: &schedapi.TaskInfo{Name: "victim", Namespace: "other", Job: "ns/g"}},
	}
	for _, testCase := range invalidPodGroups {
		t.Run(testCase.name, func(t *testing.T) {
			invalidPlan := &engineapi.RepackPlan{Moves: []*engineapi.Move{{
				Task: testCase.task, From: "n0", To: "n2",
			}}}
			if _, err := enginestatus.BuildPodRelocations(
				invalidPlan, testPodGroupPlacementPolicies{},
			); err == nil {
				t.Fatal("invalid PodGroup identity must fail execution preparation before eviction")
			}
		})
	}
}

func TestApplyPlan(t *testing.T) {
	plan := &engineapi.RepackPlan{
		Moves:      []*engineapi.Move{mkMove("a", "ns/g", 3, "n0", "n1")},
		FreedNodes: []string{"n0"},
	}
	report := engineapi.Report{FragmentationRateBefore: 0.5, FragmentationRateAfter: 0.25, NodesFreed: 1}

	// DryRun: plan populated, no relocation execution records.
	dry := &repackv1alpha1.RepackRun{}
	resolved := &repackv1alpha1.ResolvedScope{NodeCount: 3, PodGroupCount: 1}
	enginestatus.ApplyPlan(dry, report, plan, gpuResource, nil, resolved)
	if dry.Status.Plan == nil || dry.Status.Plan.Summary == nil {
		t.Fatal("plan/summary not set")
	}
	if dry.Status.Plan.Summary.MovedCardCount != 3 {
		t.Errorf("movedCardCount=%d, want 3", dry.Status.Plan.Summary.MovedCardCount)
	}
	if len(dry.Status.Plan.FreedNodes) != 1 || dry.Status.Relocations != nil {
		t.Errorf("DryRun should have freedNodes and no relocations")
	}
	if dry.Status.Plan.Summary.ResolvedScope == nil ||
		dry.Status.Plan.Summary.ResolvedScope.NodeCount != 3 ||
		dry.Status.Plan.Summary.ResolvedScope.PodGroupCount != 1 {
		t.Errorf("resolvedScope=%+v, want 3 nodes/1 PodGroup", dry.Status.Plan.Summary.ResolvedScope)
	}

	// Execute preparation is explicit and does not alter the plan.
	exec := &repackv1alpha1.RepackRun{}
	enginestatus.ApplyPlan(exec, report, plan, gpuResource, nil, resolved)
	if err := enginestatus.PrepareExecuteRelocations(exec, plan, testPodGroupPlacementPolicies{}); err != nil {
		t.Fatal(err)
	}
	if len(exec.Status.Relocations) != 1 {
		t.Errorf("Execute should populate relocations, got %d", len(exec.Status.Relocations))
	}
}

func TestRealizedFreedNodeNamesRequiresEveryPlannedVictim(t *testing.T) {
	run := &repackv1alpha1.RepackRun{Status: repackv1alpha1.RepackRunStatus{
		Plan: &repackv1alpha1.RepackPlan{
			FreedNodes: []string{"n0", "n1"},
			Moves: []repackv1alpha1.RepackMove{
				{Namespace: "ns", PodGroupName: "a", Pods: []repackv1alpha1.PodMove{
					{Name: "a-0", FromNode: "n0", ToNode: "n2"},
					{Name: "a-1", FromNode: "n0", ToNode: "n2"},
				}},
				{Namespace: "ns", PodGroupName: "b", Pods: []repackv1alpha1.PodMove{
					{Name: "b-0", FromNode: "n1", ToNode: "n2"},
				}},
			},
		},
		Relocations: []repackv1alpha1.PodRelocationStatus{
			{Namespace: "ns", PodGroupName: "a", VictimPodName: "a-0", PlannedNodeName: "n2"},
			{Namespace: "ns", PodGroupName: "b", VictimPodName: "b-0", PlannedNodeName: "n2"},
		},
	}}
	got := enginestatus.RealizedFreedNodeNames(run)
	if len(got) != 1 || got[0] != "n1" {
		t.Fatalf("accepted freed nodes = %v, want [n1]", got)
	}
}

func TestTerminalOutcome(t *testing.T) {
	mk := func(condType, reason string) *repackv1alpha1.RepackRun {
		r := &repackv1alpha1.RepackRun{}
		r.Status.Conditions = []metav1.Condition{{Type: condType, Status: metav1.ConditionTrue, Reason: reason}}
		return r
	}
	if got := enginestatus.TerminalOutcome(mk(state.CondComplete, state.ReasonExecutionCompleted)); got != state.ReasonExecutionCompleted {
		t.Errorf("complete outcome=%q, want Executed", got)
	}
	if got := enginestatus.TerminalOutcome(mk(state.CondFailed, "ExecuteFailed")); got != "ExecuteFailed" {
		t.Errorf("failed outcome=%q, want ExecuteFailed", got)
	}
	if got := enginestatus.TerminalOutcome(&repackv1alpha1.RepackRun{}); got != "Unknown" {
		t.Errorf("no condition outcome=%q, want Unknown", got)
	}
}
