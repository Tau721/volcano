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

package pdbconstraint

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const testResource = v1.ResourceName("example.com/accelerator")

type snapshotView struct {
	nodes []*schedapi.NodeInfo
}

func (s *snapshotView) Nodes() []*schedapi.NodeInfo       { return s.nodes }
func (*snapshotView) NodeInScope(*schedapi.NodeInfo) bool { return true }
func (*snapshotView) PodGroupView(schedapi.JobID) api.PodGroupView {
	return api.PodGroupView{}
}
func (*snapshotView) FeasibleRelocation(context.Context, []*api.Move, []*schedapi.TaskInfo, []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}
func (*snapshotView) HyperNodesSetByTier() map[int]sets.Set[string] {
	return map[int]sets.Set[string]{}
}
func (*snapshotView) RealNodesSet() map[string]sets.Set[string] { return map[string]sets.Set[string]{} }
func (*snapshotView) HyperNodeTierNameMap() map[string]int      { return map[string]int{} }

type pdbSnapshot struct {
	*snapshotView
	pdbs  []*policyv1.PodDisruptionBudget
	err   error
	calls int
}

func (s *pdbSnapshot) ListPodDisruptionBudgets() ([]*policyv1.PodDisruptionBudget, error) {
	s.calls++
	return s.pdbs, s.err
}

func TestIsZeroDisruptionPDB(t *testing.T) {
	tests := []struct {
		name string
		pdb  *policyv1.PodDisruptionBudget
		want bool
	}{
		{name: "nil", pdb: nil},
		{name: "maxUnavailable zero", pdb: pdbWithStatus(1, 1, 4), want: true},
		{name: "maxUnavailable zero percent", pdb: pdbWithStatus(4, 4, 4), want: true},
		{name: "minAvailable one hundred percent", pdb: pdbWithStatus(8, 8, 4), want: true},
		{name: "desired exceeds expected", pdb: pdbWithStatus(3, 4, 4), want: true},
		{name: "nonzero static allowance despite current zero disruptions", pdb: pdbWithStatus(3, 2, 4)},
		{name: "no expected pods", pdb: pdbWithStatus(0, 0, 4)},
		{name: "stale status", pdb: pdbWithStatus(3, 3, 2)},
		{name: "controller sync failed", pdb: pdbWithSyncFailure(3, 3, 4)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isZeroDisruptionPDB(test.pdb); got != test.want {
				t.Fatalf("isZeroDisruptionPDB()=%t, want %t", got, test.want)
			}
		})
	}
}

func TestPDBSelectorSemantics(t *testing.T) {
	matching := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "train"}}
	expression := &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"batch"},
	}}}
	invalid := &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
		Key: "tier", Operator: "InvalidOperator", Values: []string{"batch"},
	}}}
	tests := []struct {
		name      string
		pdb       *policyv1.PodDisruptionBudget
		task      *schedapi.TaskInfo
		wantBlock bool
	}{
		{name: "same namespace and labels", pdb: strictPDB("ns", "matching", matching), task: testTask("a", "ns", map[string]string{"app": "train"}, true, true), wantBlock: true},
		{name: "different namespace", pdb: strictPDB("other", "different", matching), task: testTask("a", "ns", map[string]string{"app": "train"}, true, true)},
		{name: "labels do not match", pdb: strictPDB("ns", "mismatch", matching), task: testTask("a", "ns", map[string]string{"app": "serve"}, true, true)},
		{name: "nil selector matches none", pdb: strictPDB("ns", "nil", nil), task: testTask("a", "ns", nil, true, true)},
		{name: "empty selector matches unlabeled pod", pdb: strictPDB("ns", "empty", &metav1.LabelSelector{}), task: testTask("a", "ns", nil, true, true), wantBlock: true},
		{name: "match expressions", pdb: strictPDB("ns", "expression", expression), task: testTask("a", "ns", map[string]string{"tier": "batch"}, true, true), wantBlock: true},
		{name: "invalid selector fails open", pdb: strictPDB("ns", "invalid", invalid), task: testTask("a", "ns", map[string]string{"tier": "batch"}, true, true)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiled, _ := compilePDBConstraints([]*policyv1.PodDisruptionBudget{test.pdb})
			_, blocked := blockingPDB(test.task, compiled)
			if blocked != test.wantBlock {
				t.Fatalf("blockingPDB()=%t, want %t", blocked, test.wantBlock)
			}
		})
	}
}

func TestUnhealthyPodEvictionPolicyAndDisruptedPods(t *testing.T) {
	alwaysAllow := policyv1.AlwaysAllow
	ifHealthyBudget := policyv1.IfHealthyBudget
	unknown := policyv1.UnhealthyPodEvictionPolicyType("FutureConservativePolicy")
	tests := []struct {
		name           string
		ready          bool
		policy         *policyv1.UnhealthyPodEvictionPolicyType
		currentHealthy int32
		disrupted      bool
		wantBlock      bool
	}{
		{name: "ready default", ready: true, wantBlock: true},
		{name: "ready AlwaysAllow", ready: true, policy: &alwaysAllow, wantBlock: true},
		{name: "not ready default while disrupted", wantBlock: true},
		{name: "not ready default while workload healthy", currentHealthy: 1},
		{name: "not ready IfHealthyBudget while disrupted", policy: &ifHealthyBudget, wantBlock: true},
		{name: "not ready IfHealthyBudget while workload healthy", policy: &ifHealthyBudget, currentHealthy: 1},
		{name: "not ready AlwaysAllow", policy: &alwaysAllow},
		{name: "unknown policy blocks conservatively", policy: &unknown, wantBlock: true},
		{name: "already disrupted", ready: true, disrupted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pdb := strictPDB("ns", "strict", &metav1.LabelSelector{})
			pdb.Spec.UnhealthyPodEvictionPolicy = test.policy
			pdb.Status.CurrentHealthy = test.currentHealthy
			if test.disrupted {
				pdb.Status.DisruptedPods = map[string]metav1.Time{"pod-a": {}}
			}
			compiled, _ := compilePDBConstraints([]*policyv1.PodDisruptionBudget{pdb})
			_, blocked := blockingPDB(testTask("a", "ns", nil, test.ready, true), compiled)
			if blocked != test.wantBlock {
				t.Fatalf("blockingPDB()=%t, want %t", blocked, test.wantBlock)
			}
		})
	}
}

func TestPodStatesThatBypassEvictionPDBChecks(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name        string
		phase       v1.PodPhase
		terminating bool
		wantBlock   bool
	}{
		{name: "running pod", phase: v1.PodRunning, wantBlock: true},
		{name: "pending pod", phase: v1.PodPending},
		{name: "succeeded pod", phase: v1.PodSucceeded},
		{name: "failed pod", phase: v1.PodFailed},
		{name: "terminating pod", phase: v1.PodRunning, terminating: true},
	}
	compiled, _ := compilePDBConstraints([]*policyv1.PodDisruptionBudget{
		strictPDB("ns", "strict", &metav1.LabelSelector{}),
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := testTask("a", "ns", nil, true, true)
			task.Pod.Status.Phase = test.phase
			if test.terminating {
				task.Pod.DeletionTimestamp = &now
			}
			_, blocked := blockingPDB(task, compiled)
			if blocked != test.wantBlock {
				t.Fatalf("blockingPDB()=%t, want %t", blocked, test.wantBlock)
			}
		})
	}
}

func TestPluginPrecomputesBlockedTargetTasksOnce(t *testing.T) {
	blocked := testTask("blocked", "ns", map[string]string{"app": "train"}, true, true)
	blocked.UID = ""
	blocked.Namespace = ""
	blocked.Name = ""
	cpuOnly := testTask("cpu-only", "ns", map[string]string{"app": "train"}, true, false)
	unmatched := testTask("unmatched", "ns", map[string]string{"app": "serve"}, true, true)
	snapshot := &pdbSnapshot{
		snapshotView: &snapshotView{nodes: []*schedapi.NodeInfo{{Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
			"blocked": blocked, "cpu-only": cpuOnly, "unmatched": unmatched,
		}}}},
		pdbs: []*policyv1.PodDisruptionBudget{strictPDB("ns", "strict", &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": "train"},
		})},
	}
	ssn := framework.OpenSession(framework.SessionConfig{Snapshot: snapshot, Resource: testResource}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	movable := ssn.Movable()
	if movable(blocked) {
		t.Fatal("strict PDB target task should be immovable")
	}
	if !movable(cpuOnly) {
		t.Fatal("CPU-only task must not be blocked by pdbconstraint")
	}
	if !movable(unmatched) {
		t.Fatal("unmatched target task should remain movable")
	}
	_ = movable(blocked)
	if snapshot.calls != 1 {
		t.Fatalf("ListPodDisruptionBudgets calls=%d, want exactly one per Session", snapshot.calls)
	}
}

func TestPluginFailOpenAndCanBeDisabled(t *testing.T) {
	task := testTask("a", "ns", nil, true, true)
	nodes := []*schedapi.NodeInfo{{Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{"a": task}}}
	tests := []struct {
		name     string
		snapshot framework.Snapshot
		plugins  []framework.PluginOption
	}{
		{name: "reader absent", snapshot: &snapshotView{nodes: nodes}, plugins: framework.PluginOptions(Name)},
		{name: "reader failure", snapshot: &pdbSnapshot{snapshotView: &snapshotView{nodes: nodes}, err: errors.New("lister unavailable")}, plugins: framework.PluginOptions(Name)},
		{name: "plugin disabled", snapshot: &pdbSnapshot{snapshotView: &snapshotView{nodes: nodes}, pdbs: []*policyv1.PodDisruptionBudget{strictPDB("ns", "strict", &metav1.LabelSelector{})}}, plugins: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ssn := framework.OpenSession(framework.SessionConfig{Snapshot: test.snapshot, Resource: testResource}, test.plugins)
			defer framework.CloseSession(ssn)
			if !ssn.Movable()(task) {
				t.Fatal("pdbconstraint failure/disable path must fail open")
			}
		})
	}
}

func TestPluginRejectsArguments(t *testing.T) {
	if err := framework.ValidatePluginArguments(Name, nil); err != nil {
		t.Fatalf("argument-free configuration should be valid: %v", err)
	}
	err := framework.ValidatePluginArguments(Name, framework.Arguments{"unexpected": true})
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("unknown argument error=%v, want strict rejection", err)
	}
}

func TestMultiplePDBsBlockWhenAnyApplicableConstraintBlocks(t *testing.T) {
	alwaysAllow := policyv1.AlwaysAllow
	allowUnready := strictPDB("ns", "a-always-allow", &metav1.LabelSelector{})
	allowUnready.Spec.UnhealthyPodEvictionPolicy = &alwaysAllow
	blockUnready := strictPDB("ns", "b-default", &metav1.LabelSelector{})
	compiled, count := compilePDBConstraints([]*policyv1.PodDisruptionBudget{blockUnready, allowUnready})
	if count != 2 {
		t.Fatalf("zero-disruption PDB count=%d, want 2", count)
	}
	info, blocked := blockingPDB(testTask("a", "ns", nil, false, true), compiled)
	if !blocked || info.PDBName != "b-default" {
		t.Fatalf("blocked=%t info=%+v, want blocking default policy", blocked, info)
	}
}

func pdbWithStatus(expected, desired, observedGeneration int32) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
		Status: policyv1.PodDisruptionBudgetStatus{
			ExpectedPods:       expected,
			DesiredHealthy:     desired,
			ObservedGeneration: int64(observedGeneration),
		},
	}
}

func pdbWithSyncFailure(expected, desired, observedGeneration int32) *policyv1.PodDisruptionBudget {
	pdb := pdbWithStatus(expected, desired, observedGeneration)
	pdb.Status.Conditions = []metav1.Condition{{
		Type: policyv1.DisruptionAllowedCondition, Status: metav1.ConditionFalse, Reason: policyv1.SyncFailedReason,
	}}
	return pdb
}

func strictPDB(namespace, name string, selector *metav1.LabelSelector) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Generation: 1},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: selector},
		Status: policyv1.PodDisruptionBudgetStatus{
			ObservedGeneration: 1,
			ExpectedPods:       1,
			DesiredHealthy:     1,
		},
	}
}

func testTask(name, namespace string, labels map[string]string, ready, targetResource bool) *schedapi.TaskInfo {
	resource := &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{}}
	if targetResource {
		resource.ScalarResources[testResource] = 1000
	}
	conditions := []v1.PodCondition{}
	if ready {
		conditions = append(conditions, v1.PodCondition{Type: v1.PodReady, Status: v1.ConditionTrue})
	}
	return &schedapi.TaskInfo{
		UID:        schedapi.TaskID(name),
		Job:        schedapi.JobID(namespace + "/pg"),
		Name:       "pod-" + name,
		Namespace:  namespace,
		InitResreq: resource,
		Resreq:     resource,
		Pod:        &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + name, Namespace: namespace, Labels: labels}, Status: v1.PodStatus{Conditions: conditions}},
	}
}
