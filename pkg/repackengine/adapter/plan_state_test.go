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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// planTestHyperNodes builds the reference tree for plan-state tests, mirroring
// the scheduler session's shape (addClusterTopHyperNode): a virtual
// <cluster-top-hypernode> root at the top tier wraps the real root "top".
//
//	<cluster-top-hypernode> (tier 3)
//	└── top (tier 2)
//	    ├── hA (tier 1)  — contains n0
//	    ├── hB (tier 1)  — contains n1
//	    └── hD (tier 1)  — contains n2
//
// The virtual root is what lets hasHyperNodeTopology()/allowedDomains() find a tree.
func planTestHyperNodes() (schedapi.HyperNodeInfoMap, map[string]sets.Set[string]) {
	top := schedframework.ClusterTopHyperNode
	hyperNodes := schedapi.HyperNodeInfoMap{
		top:   schedapi.NewHyperNodeInfo(&topologyv1alpha1.HyperNode{ObjectMeta: metav1.ObjectMeta{Name: top}, Spec: topologyv1alpha1.HyperNodeSpec{Tier: 3}}, schedapi.TierOpt(3)),
		"top": schedapi.NewHyperNodeInfo(&topologyv1alpha1.HyperNode{ObjectMeta: metav1.ObjectMeta{Name: "top"}, Spec: topologyv1alpha1.HyperNodeSpec{Tier: 2}}, schedapi.TierOpt(2), schedapi.ParentOpt(top)),
		"hA":  schedapi.NewHyperNodeInfo(&topologyv1alpha1.HyperNode{ObjectMeta: metav1.ObjectMeta{Name: "hA"}, Spec: topologyv1alpha1.HyperNodeSpec{Tier: 1}}, schedapi.TierOpt(1), schedapi.ParentOpt("top")),
		"hB":  schedapi.NewHyperNodeInfo(&topologyv1alpha1.HyperNode{ObjectMeta: metav1.ObjectMeta{Name: "hB"}, Spec: topologyv1alpha1.HyperNodeSpec{Tier: 1}}, schedapi.TierOpt(1), schedapi.ParentOpt("top")),
		"hD":  schedapi.NewHyperNodeInfo(&topologyv1alpha1.HyperNode{ObjectMeta: metav1.ObjectMeta{Name: "hD"}, Spec: topologyv1alpha1.HyperNodeSpec{Tier: 1}}, schedapi.TierOpt(1), schedapi.ParentOpt("top")),
	}
	// ParentOpt only records the parent pointer; gradient BFS walks Children.
	for _, hyperNode := range hyperNodes {
		if hyperNode.Parent != "" {
			if parent, ok := hyperNodes[hyperNode.Parent]; ok {
				parent.Children.Insert(hyperNode.Name)
			}
		}
	}
	rns := map[string]sets.Set[string]{
		top:   sets.New("n0", "n1", "n2"),
		"top": sets.New("n0", "n1", "n2"),
		"hA":  sets.New("n0"),
		"hB":  sets.New("n1"),
		"hD":  sets.New("n2"),
	}
	return hyperNodes, rns
}

// planTestTask builds an allocated task with both Resreq and InitResreq set:
// TaskInfo.Clone() clones both unconditionally, and Resource.Clone() is not nil-safe.
func planTestTask(uid, jobID, name, nodeName string) *schedapi.TaskInfo {
	rr := gpuRes(4)
	return &schedapi.TaskInfo{
		UID: schedapi.TaskID(uid), Job: schedapi.JobID(jobID), Name: name,
		TransactionContext: schedapi.TransactionContext{NodeName: nodeName, Status: schedapi.Running},
		Resreq:             rr, InitResreq: rr.Clone(),
	}
}

func planTestSubJob(uid, jobID string, tasks ...*schedapi.TaskInfo) *schedapi.SubJobInfo {
	m := schedapi.TasksMap{}
	index := map[schedapi.TaskStatus]schedapi.TasksMap{}
	for _, tk := range tasks {
		m[tk.UID] = tk
		if index[tk.Status] == nil {
			index[tk.Status] = schedapi.TasksMap{}
		}
		index[tk.Status][tk.UID] = tk
	}
	return &schedapi.SubJobInfo{
		GID: schedapi.SubJobGID(uid + "/g"), UID: schedapi.SubJobID(uid), Job: schedapi.JobID(jobID),
		Tasks: m, TaskStatusIndex: index,
	}
}

func planTestJob(jobID string, subJobs ...*schedapi.SubJobInfo) *schedapi.JobInfo {
	ji := &schedapi.JobInfo{
		UID: schedapi.JobID(jobID), Name: jobID,
		Tasks:           schedapi.TasksMap{},
		TaskStatusIndex: map[schedapi.TaskStatus]schedapi.TasksMap{},
		TaskToSubJob:    map[schedapi.TaskID]schedapi.SubJobID{},
		SubJobs:         map[schedapi.SubJobID]*schedapi.SubJobInfo{},
	}
	for _, sj := range subJobs {
		ji.SubJobs[sj.UID] = sj
		for uid, tk := range sj.Tasks {
			ji.Tasks[uid] = tk
			if ji.TaskStatusIndex[tk.Status] == nil {
				ji.TaskStatusIndex[tk.Status] = schedapi.TasksMap{}
			}
			ji.TaskStatusIndex[tk.Status][uid] = tk
			ji.TaskToSubJob[uid] = sj.UID
		}
	}
	return ji
}

func planTestNode(name string, tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
	m := map[schedapi.TaskID]*schedapi.TaskInfo{}
	var used int64
	for _, tk := range tasks {
		m[tk.UID] = tk
		used += int64(tk.Resreq.ScalarResources[gpu])
	}
	return &schedapi.NodeInfo{
		Name: name, Tasks: m,
		Allocatable: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: 8}},
		Used:        &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{gpu: float64(used)}},
	}
}

// planTestBothMovingSession builds a two-subJob session (A on n0 anchored hA,
// B on n1 anchored hB, job anchor = LCA top). Anchors use the real
// SyncJobAllocatedHyperNode so tests assert the true initial state.
func planTestBothMovingSession(t *testing.T) (*schedframework.Session, *schedapi.JobInfo, *schedapi.SubJobInfo, *schedapi.SubJobInfo) {
	hyperNodes, rns := planTestHyperNodes()

	uA := planTestTask("uA", "ns/job", "ta", "n0")
	uB := planTestTask("uB", "ns/job", "tb", "n1")
	sjA := planTestSubJob("ns/job/grp/valA", "ns/job", uA)
	sjB := planTestSubJob("ns/job/grp/valB", "ns/job", uB)
	ji := planTestJob("ns/job", sjA, sjB)

	ssn := &schedframework.Session{
		Jobs: map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji},
		Nodes: map[string]*schedapi.NodeInfo{
			"n0": planTestNode("n0", uA),
			"n1": planTestNode("n1", uB),
			"n2": planTestNode("n2"),
		},
		HyperNodes: hyperNodes, RealNodesSet: rns,
	}
	schedapi.SyncJobAllocatedHyperNode(ji, hyperNodes, rns)
	if sjA.AllocatedHyperNode != "hA" || sjB.AllocatedHyperNode != "hB" || ji.AllocatedHyperNode != "top" {
		t.Fatalf("bad initial anchors: sjA=%q sjB=%q job=%q, want hA/hB/top", sjA.AllocatedHyperNode, sjB.AllocatedHyperNode, ji.AllocatedHyperNode)
	}
	return ssn, ji, sjA, sjB
}

// Both-moving: committing both subJobs into the common domain hD rewrites the
// job side (n0,n1 → n2) and re-anchors subJob/job to hD; the node side stays
// untouched, and rollback returns to the pre-commit baseline.
func TestSessionPlanState_ApplyCommitBothMoving(t *testing.T) {
	ssn, ji, sjA, sjB := planTestBothMovingSession(t)
	ps := NewSessionPlanState(ssn)

	baseline := ps.Save()
	ps.ApplyCommit([]*api.Move{
		{Task: ji.Tasks["uA"], From: "n0", To: "n2"},
		{Task: ji.Tasks["uB"], From: "n1", To: "n2"},
	})

	// job-side rewrite: the job maps point at clones carrying the new NodeName.
	if ji.Tasks["uA"].NodeName != "n2" || ji.Tasks["uB"].NodeName != "n2" {
		t.Fatalf("job-side rewrite missing: uA=%q uB=%q, want n2/n2", ji.Tasks["uA"].NodeName, ji.Tasks["uB"].NodeName)
	}
	if ji.SubJobs[sjA.UID].Tasks["uA"].NodeName != "n2" {
		t.Errorf("subJob A task not rewritten, got %q", ji.SubJobs[sjA.UID].Tasks["uA"].NodeName)
	}
	if ji.TaskStatusIndex[schedapi.Running]["uA"].NodeName != "n2" {
		t.Error("job TaskStatusIndex not rewritten")
	}

	// anchors recomputed via SyncJobAllocatedHyperNode.
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hD" {
		t.Errorf("subJob A anchor=%q, want hD", got)
	}
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjB.UID); got != "hD" {
		t.Errorf("subJob B anchor=%q, want hD", got)
	}
	if got := ps.JobAllocatedHyperNode(ji.UID); got != "hD" {
		t.Errorf("job anchor=%q, want hD", got)
	}

	// node side untouched: original pointers and NodeName (job side and node side diverge).
	if n := ssn.Nodes["n0"].Tasks["uA"]; n.NodeName != "n0" || n == ji.Tasks["uA"] {
		t.Errorf("node-side uA must stay the original n0 task, got NodeName=%q", n.NodeName)
	}
	if n := ssn.Nodes["n1"].Tasks["uB"]; n.NodeName != "n1" || n == ji.Tasks["uB"] {
		t.Errorf("node-side uB must stay the original n1 task, got NodeName=%q", n.NodeName)
	}

	// rollback baseline advances after commit: re-Save then Restore is a no-op.
	committed := ps.Save()
	ps.Restore(committed)
	if got := ps.JobAllocatedHyperNode(ji.UID); got != "hD" {
		t.Errorf("restore to committed baseline changed job anchor to %q, want hD", got)
	}
	if ji.Tasks["uA"].NodeName != "n2" {
		t.Errorf("restore to committed baseline reverted task, got %q", ji.Tasks["uA"].NodeName)
	}

	// full rollback to the pre-commit baseline: anchors and job-side tasks revert.
	ps.Restore(baseline)
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hA" {
		t.Errorf("restored subJob A anchor=%q, want hA", got)
	}
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjB.UID); got != "hB" {
		t.Errorf("restored subJob B anchor=%q, want hB", got)
	}
	if got := ps.JobAllocatedHyperNode(ji.UID); got != "top" {
		t.Errorf("restored job anchor=%q, want top", got)
	}
	if ji.Tasks["uA"].NodeName != "n0" || ji.Tasks["uB"].NodeName != "n1" {
		t.Errorf("restored task NodeNames not reverted, uA=%q uB=%q", ji.Tasks["uA"].NodeName, ji.Tasks["uB"].NodeName)
	}
	if ji.Tasks["uA"] != ssn.Nodes["n0"].Tasks["uA"] || ji.Tasks["uB"] != ssn.Nodes["n1"].Tasks["uB"] {
		t.Error("job side must point back at the original tasks after restore")
	}
	if len(ps.rewritten) != 0 {
		t.Errorf("rewritten=%d, want 0 after full rollback", len(ps.rewritten))
	}
}

// ClearGangAnchor temporarily clears a job/subJob AllocatedHyperNode for
// no-anchor gradient evaluation; Save/Restore restores it.
func TestSessionPlanState_ClearGangAnchor(t *testing.T) {
	_, ji, sjA, _ := planTestBothMovingSession(t)
	ps := NewSessionPlanState(nilSessionJobs(ji))

	snap := ps.Save()
	ps.ClearGangAnchor(ji.UID, "")
	if got := ps.JobAllocatedHyperNode(ji.UID); got != "" {
		t.Errorf("cleared job anchor=%q, want empty", got)
	}
	ps.ClearGangAnchor(ji.UID, sjA.UID)
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "" {
		t.Errorf("cleared subJob A anchor=%q, want empty", got)
	}
	// the sibling subJob anchor is untouched.
	if got := ps.SubJobAllocatedHyperNode(ji.UID, "ns/job/grp/valB"); got != "hB" {
		t.Errorf("sibling subJob B anchor=%q, want hB untouched", got)
	}

	ps.Restore(snap)
	if got := ps.JobAllocatedHyperNode(ji.UID); got != "top" {
		t.Errorf("restored job anchor=%q, want top", got)
	}
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hA" {
		t.Errorf("restored subJob A anchor=%q, want hA", got)
	}
}

// Trial rollback: a rejected trial must leave no residue — anchors and task
// bindings return to the committed baseline, keeping the next trial clean.
func TestSessionPlanState_TrialRollbackNoResidue(t *testing.T) {
	ssn, ji, sjA, _ := planTestBothMovingSession(t)
	ps := NewSessionPlanState(ssn)

	baseline := ps.Save()

	// trial: place uA on n2.
	ps.ApplyCommit([]*api.Move{{Task: ji.Tasks["uA"], From: "n0", To: "n2"}})
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hD" {
		t.Fatalf("trial should take effect, subJob A anchor=%q want hD", got)
	}
	if ji.Tasks["uA"].NodeName != "n2" {
		t.Fatalf("trial should rewrite task, got %q", ji.Tasks["uA"].NodeName)
	}

	// rejected: rollback to baseline.
	ps.Restore(baseline)
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hA" {
		t.Errorf("after rollback subJob A anchor=%q, want hA", got)
	}
	if got := ps.JobAllocatedHyperNode(ji.UID); got != "top" {
		t.Errorf("after rollback job anchor=%q, want top", got)
	}
	if ji.Tasks["uA"].NodeName != "n0" {
		t.Errorf("after rollback uA NodeName=%q, want n0", ji.Tasks["uA"].NodeName)
	}
	if len(ps.rewritten) != 0 {
		t.Errorf("rewritten=%d, want 0 after rollback", len(ps.rewritten))
	}
	if ji.Tasks["uA"] != ssn.Nodes["n0"].Tasks["uA"] {
		t.Error("job side must point back at the original task after trial rollback")
	}

	// A fresh trial from the same baseline behaves identically (no residue).
	ps.ApplyCommit([]*api.Move{{Task: ji.Tasks["uA"], From: "n0", To: "n2"}})
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hD" {
		t.Errorf("second trial subJob A anchor=%q, want hD (residue left from first rollback?)", got)
	}
}

// ApplyCommit must ignore malformed moves: nil, empty destination, no-op, and
// unknown jobs — none may touch the plan state or the rewritten bookkeeping.
func TestSessionPlanState_IgnoresInvalidMoves(t *testing.T) {
	ssn, ji, sjA, _ := planTestBothMovingSession(t)
	ps := NewSessionPlanState(ssn)

	baseline := ps.Save()
	ps.ApplyCommit([]*api.Move{
		nil,
		{Task: ji.Tasks["uA"], From: "n0", To: ""},
		{Task: ji.Tasks["uA"], From: "n0", To: "n0"},
		{Task: &schedapi.TaskInfo{UID: "uX", Job: "ns/nope"}, From: "n1", To: "n2"},
	})

	if len(ps.rewritten) != 0 {
		t.Fatalf("rewritten=%d, want 0 for invalid moves", len(ps.rewritten))
	}
	if got := ps.SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hA" {
		t.Errorf("subJob A anchor=%q, want hA (invalid moves must not recompute)", got)
	}
	if ji.Tasks["uA"] != ssn.Nodes["n0"].Tasks["uA"] {
		t.Error("job-side uA must remain the original task")
	}
	ps.Restore(baseline) // must be harmless
	if got := ps.JobAllocatedHyperNode(ji.UID); got != "top" {
		t.Errorf("after restore job anchor=%q, want top", got)
	}
}

// nilSessionJobs wraps a job into a bare session (jobs map only) so the plan
// state carrier has a non-nil Jobs map to iterate.
func nilSessionJobs(jobs ...*schedapi.JobInfo) *schedframework.Session {
	m := map[schedapi.JobID]*schedapi.JobInfo{}
	for _, ji := range jobs {
		m[ji.UID] = ji
	}
	return &schedframework.Session{Jobs: m}
}
