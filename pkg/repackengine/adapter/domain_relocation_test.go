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

// Phase B acceptance tests: the dual-mode FeasibleRelocation.
//
// The gradient functions are faithful stubs of the scheduler's HyperNode
// constraint plugins: hard mode narrows to the gang's allowed ancestor subtree
// via the real GetSearchRoot over the plan-state AllocatedHyperNode the carrier
// rewrites (clearing the anchor drives the no-anchor branch); soft mode abstains.
// Under test: whole-gang domain trial, two-entry intersection, plan-state commit
// and symmetric rollback.

import (
	"context"
	"math"
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	schedulingapi "volcano.sh/apis/pkg/apis/scheduling"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedcache "volcano.sh/volcano/pkg/scheduler/cache"
	schedconf "volcano.sh/volcano/pkg/scheduler/conf"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/util"
)

const (
	gradientStubName         = "hard-tier-stub"
	antiAffinityStubName     = "anti-affinity-stub"
	subgroupAffinityStubName = "subgroup-affinity-stub"
)

// phaseBTask builds an allocated task with Resreq and InitResreq set (AddTask/
// Clone deref Resreq) and a real Pod (node maps keyed by PodKey). g is whole
// GPUs; capacity lives in the milli-unit ledger (1 device = 1000).
func phaseBTask(uid, jobID, name, nodeName string, g int64) *schedapi.TaskInfo {
	rr := gpuRes(g * 1000)
	return &schedapi.TaskInfo{
		UID: schedapi.TaskID(uid), Job: schedapi.JobID(jobID), Name: name,
		Pod:                &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}},
		TransactionContext: schedapi.TransactionContext{NodeName: nodeName, Status: schedapi.Running},
		Resreq:             rr, InitResreq: rr.Clone(),
	}
}

// phaseBNode puts GPU capacity on the Node object so NodeInfo.Clone (fit checks
// run on clones) keeps the same capacity.
func phaseBNode(name string, capGPU int64, tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{
			Capacity:    v1.ResourceList{gpu: *resource.NewQuantity(capGPU, resource.DecimalSI)},
			Allocatable: v1.ResourceList{gpu: *resource.NewQuantity(capGPU, resource.DecimalSI)},
		},
	}
	nodeInfo := schedapi.NewNodeInfo(node)
	for _, tk := range tasks {
		if err := nodeInfo.AddTask(tk); err != nil {
			panic(err)
		}
	}
	return nodeInfo
}

// phaseBJob builds a plain job (no SubGroupPolicy). topology nil means no
// HyperNode constraint (RequiresHyperNodeAllocate == false).
func phaseBJob(jobID string, topology *schedulingapi.NetworkTopologySpec, tasks ...*schedapi.TaskInfo) *schedapi.JobInfo {
	ji := &schedapi.JobInfo{
		UID: schedapi.JobID(jobID), Name: jobID,
		NetworkTopology: topology,
		Tasks:           schedapi.TasksMap{},
		TaskStatusIndex: map[schedapi.TaskStatus]schedapi.TasksMap{},
		TaskToSubJob:    map[schedapi.TaskID]schedapi.SubJobID{},
		SubJobs:         map[schedapi.SubJobID]*schedapi.SubJobInfo{},
	}
	for _, tk := range tasks {
		ji.Tasks[tk.UID] = tk
		if ji.TaskStatusIndex[tk.Status] == nil {
			ji.TaskStatusIndex[tk.Status] = schedapi.TasksMap{}
		}
		ji.TaskStatusIndex[tk.Status][tk.UID] = tk
	}
	return ji
}

// phaseBSubJob builds a SubJob with the policy GID format getSubJobGID produces
// (jobID+"/"+policy), so SubJobPolicyName returns the policy name.
func phaseBSubJob(uid, jobID, policy string, tasks ...*schedapi.TaskInfo) *schedapi.SubJobInfo {
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
		GID: schedapi.SubJobGID(jobID + "/" + policy), UID: schedapi.SubJobID(jobID + "/" + policy),
		Job: schedapi.JobID(jobID), Tasks: m, TaskStatusIndex: index,
	}
}

// phaseBSubGroupAffinityJob builds a SubGroupPolicy job with the given Required
// SubGroup affinity terms (policy lists resolved at one tier) over the subJobs.
func phaseBSubGroupAffinityJob(jobID string, tier int, terms [][]string, subJobs ...*schedapi.SubJobInfo) *schedapi.JobInfo {
	ji := planTestJob(jobID, subJobs...)
	var policies []schedulingapi.SubGroupPolicySpec
	for _, sj := range subJobs {
		if p := schedapi.SubJobPolicyName(sj); p != "" {
			policies = append(policies, schedulingapi.SubGroupPolicySpec{Name: p})
		}
	}
	required := make([]schedulingapi.SubGroupAffinityTerm, 0, len(terms))
	for _, sg := range terms {
		required = append(required, schedulingapi.SubGroupAffinityTerm{
			SubGroups: sg, TopologyTier: ptr.To(int32(tier)),
		})
	}
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: policies,
		TopologyAffinity: &schedulingapi.TopologyAffinitySpec{
			SubGroupAffinity: &schedulingapi.SubGroupAffinity{Required: required},
		},
	}}}
	return ji
}

func hardTopology(highestTier int) *schedulingapi.NetworkTopologySpec {
	return &schedulingapi.NetworkTopologySpec{Mode: schedulingapi.HardNetworkTopologyMode, HighestTierAllowed: ptr.To(highestTier)}
}

func softTopology() *schedulingapi.NetworkTopologySpec {
	return &schedulingapi.NetworkTopologySpec{Mode: schedulingapi.SoftNetworkTopologyMode, HighestTierAllowed: ptr.To(1)}
}

// phaseBSession builds a Session over the reference tree with the given jobs and
// nodes, syncs gang anchors, then installs the gradient stubs under test.
func phaseBSession(t *testing.T, nodes map[string]*schedapi.NodeInfo, jobs map[schedapi.JobID]*schedapi.JobInfo, register func(*schedframework.Session)) *schedframework.Session {
	hyperNodes, rns := planTestHyperNodes()
	for _, ji := range jobs {
		schedapi.SyncJobAllocatedHyperNode(ji, hyperNodes, rns)
	}
	ssn := newPhaseBSSN(t)
	ssn.Jobs = jobs
	ssn.Nodes = nodes
	ssn.HyperNodes = hyperNodes
	ssn.RealNodesSet = rns
	if register != nil {
		register(ssn)
	}
	return ssn
}

// newPhaseBSSN opens a real Session over an empty cache: openSession inits every
// fn map (gradients panic on nil maps) and adds the virtual ClusterTopHyperNode
// root (later overwritten); CloseSessionReadOnly skips the close's cluster
// writes.
func newPhaseBSSN(t *testing.T) *schedframework.Session {
	sc := &schedcache.SchedulerCache{
		Nodes:             map[string]*schedapi.NodeInfo{},
		Jobs:              map[schedapi.JobID]*schedapi.JobInfo{},
		Queues:            map[schedapi.QueueID]*schedapi.QueueInfo{},
		HyperNodesInfo:    schedapi.NewHyperNodesInfo(nil),
		InUseNodesInShard: sets.Set[string]{},
		StatusUpdater:     &util.FakeStatusUpdater{},
		Recorder:          record.NewFakeRecorder(100),
	}
	ssn := schedframework.OpenSession(sc, nil, nil)
	t.Cleanup(func() { schedframework.CloseSessionReadOnly(ssn) })
	return ssn
}

// registerHardTierGradient installs the hard-tier stub and enables it in the
// Session's HyperNodeGradient tiers.
func registerHardTierGradient(ssn *schedframework.Session) {
	jobFn, subJobFn := hardTierGradientStub(ssn.HyperNodes)
	ssn.AddHyperNodeGradientForJobFn(gradientStubName, jobFn)
	ssn.AddHyperNodeGradientForSubJobFn(gradientStubName, subJobFn)
	ssn.Tiers = []schedconf.Tier{{Plugins: []schedconf.PluginOption{
		{Name: gradientStubName, EnabledHyperNodeGradient: ptr.To(true)},
	}}}
}

// registerHardTierAndAntiAffinityGradient also installs an anti-affinity stub
// dropping excluded domains and their ancestors, emulating Required PodGroup
// anti-affinity.
func registerHardTierAndAntiAffinityGradient(ssn *schedframework.Session, excluded sets.Set[string]) {
	jobFn, subJobFn := hardTierGradientStub(ssn.HyperNodes)
	ssn.AddHyperNodeGradientForJobFn(gradientStubName, jobFn)
	ssn.AddHyperNodeGradientForSubJobFn(gradientStubName, subJobFn)
	ssn.AddHyperNodeGradientForJobFn(antiAffinityStubName, antiAffinityGradientStub(ssn.HyperNodes, excluded))
	ssn.Tiers = []schedconf.Tier{{Plugins: []schedconf.PluginOption{
		{Name: gradientStubName, EnabledHyperNodeGradient: ptr.To(true)},
		{Name: antiAffinityStubName, EnabledHyperNodeGradient: ptr.To(true)},
	}}}
}

// hardTierGradientStub mimics the hard-mode gradient: soft abstains; hard narrows
// to the gang's highest-allowed ancestor subtree via GetSearchRoot, reading the
// plan-state AllocatedHyperNode the carrier rewrites (a fully-vacated gang clears
// it).
func hardTierGradientStub(hyperNodes schedapi.HyperNodeInfoMap) (schedapi.HyperNodeGradientForJobFn, schedapi.HyperNodeGradientForSubJobFn) {
	build := func(root *schedapi.HyperNodeInfo, highestAllowedTier int, allocated string) schedapi.HyperNodeGradientResult {
		searchRoot, err := schedapi.GetSearchRoot(hyperNodes, root, highestAllowedTier, allocated)
		if err != nil {
			return schedapi.HyperNodeGradientConstrain(nil)
		}
		return schedapi.HyperNodeGradientConstrain(subtreeLayers(hyperNodes, searchRoot, highestAllowedTier))
	}
	jobFn := func(job *schedapi.JobInfo, root *schedapi.HyperNodeInfo, purpose schedapi.SearchPurpose) schedapi.HyperNodeGradientResult {
		hard, highest := job.IsHardTopologyMode()
		if !hard {
			return schedapi.HyperNodeGradientAbstain()
		}
		return build(root, highest, job.AllocatedHyperNode)
	}
	subJobFn := func(subJob *schedapi.SubJobInfo, root *schedapi.HyperNodeInfo, purpose schedapi.SearchPurpose) schedapi.HyperNodeGradientResult {
		hard, highest := subJob.IsHardTopologyMode()
		if !hard {
			return schedapi.HyperNodeGradientAbstain()
		}
		return build(root, highest, subJob.AllocatedHyperNode)
	}
	return jobFn, subJobFn
}

// subtreeLayers mirrors the plugin's gradient layer build: BFS from the search
// root, keep HyperNodes at or below the allowed tier, group tier-ascending.
func subtreeLayers(hyperNodes schedapi.HyperNodeInfoMap, root *schedapi.HyperNodeInfo, highestAllowedTier int) [][]*schedapi.HyperNodeInfo {
	byTier := map[int][]*schedapi.HyperNodeInfo{}
	visited := sets.New[string](root.Name)
	queue := []*schedapi.HyperNodeInfo{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.Tier() <= highestAllowedTier {
			byTier[cur.Tier()] = append(byTier[cur.Tier()], cur)
		}
		children := cur.Children.UnsortedList()
		sort.Strings(children)
		for _, child := range children {
			if visited.Has(child) {
				continue
			}
			visited.Insert(child)
			if hyperNodeInfo, ok := hyperNodes[child]; ok {
				queue = append(queue, hyperNodeInfo)
			}
		}
	}
	tiers := make([]int, 0, len(byTier))
	for t := range byTier {
		tiers = append(tiers, t)
	}
	sort.Ints(tiers)
	result := make([][]*schedapi.HyperNodeInfo, 0, len(tiers))
	for _, t := range tiers {
		result = append(result, byTier[t])
	}
	return result
}

// antiAffinityGradientStub drops the excluded domains and every ancestor that
// contains them from the candidate forest.
func antiAffinityGradientStub(hyperNodes schedapi.HyperNodeInfoMap, excluded sets.Set[string]) schedapi.HyperNodeGradientForJobFn {
	excludedAll := sets.New[string](excluded.UnsortedList()...)
	for name := range excluded {
		for _, anc := range hyperNodes.GetAncestors(name) {
			excludedAll.Insert(anc)
		}
	}
	return func(job *schedapi.JobInfo, root *schedapi.HyperNodeInfo, purpose schedapi.SearchPurpose) schedapi.HyperNodeGradientResult {
		var kept [][]*schedapi.HyperNodeInfo
		for _, layer := range subtreeLayers(hyperNodes, root, math.MaxInt) {
			var row []*schedapi.HyperNodeInfo
			for _, hyperNode := range layer {
				if excludedAll.Has(hyperNode.Name) {
					continue
				}
				row = append(row, hyperNode)
			}
			if len(row) > 0 {
				kept = append(kept, row)
			}
		}
		return schedapi.HyperNodeGradientConstrain(kept)
	}
}

// registerSubGroupAffinityGradient installs the Required SubGroup affinity stub:
// the Job entry abstains (no hard PodGroup terms); the SubJob entry narrows
// candidates to the affinity peers' domains.
func registerSubGroupAffinityGradient(ssn *schedframework.Session) {
	jobFn, subJobFn := subGroupAffinityGradientStub(ssn)
	ssn.AddHyperNodeGradientForJobFn(subgroupAffinityStubName, jobFn)
	ssn.AddHyperNodeGradientForSubJobFn(subgroupAffinityStubName, subJobFn)
	ssn.Tiers = []schedconf.Tier{{Plugins: []schedconf.PluginOption{
		{Name: subgroupAffinityStubName, EnabledHyperNodeGradient: ptr.To(true)},
	}}}
}

// subGroupAffinityGradientStub mirrors the plugin's hard SubGroup filter: the Job
// entry abstains; the SubJob entry keeps candidates whose ancestor at each
// term tier is the single occupied peer domain (empty peers impose nothing) and
// outside anti-affinity peers. Occupancy reads plan-state bindings the carrier
// rewrites, so a serial unit commit re-anchors the peer and pins the follower.
func subGroupAffinityGradientStub(ssn *schedframework.Session) (schedapi.HyperNodeGradientForJobFn, schedapi.HyperNodeGradientForSubJobFn) {
	jobFn := func(job *schedapi.JobInfo, root *schedapi.HyperNodeInfo, purpose schedapi.SearchPurpose) schedapi.HyperNodeGradientResult {
		return schedapi.HyperNodeGradientAbstain()
	}
	subJobFn := func(subJob *schedapi.SubJobInfo, root *schedapi.HyperNodeInfo, purpose schedapi.SearchPurpose) schedapi.HyperNodeGradientResult {
		job := ssn.Jobs[subJob.Job]
		if job == nil {
			return schedapi.HyperNodeGradientConstrain(nil)
		}
		var kept [][]*schedapi.HyperNodeInfo
		for _, layer := range subtreeLayers(ssn.HyperNodes, root, math.MaxInt) {
			var row []*schedapi.HyperNodeInfo
			for _, hyperNode := range layer {
				if subJobEligibleForHardTerms(ssn, job, subJob, hyperNode) {
					row = append(row, hyperNode)
				}
			}
			if len(row) > 0 {
				kept = append(kept, row)
			}
		}
		return schedapi.HyperNodeGradientConstrain(kept)
	}
	return jobFn, subJobFn
}

// subJobEligibleForHardTerms is the test-double of the plugin's
// isEligibleForSubGroupHardTerms (affinity and anti-affinity branches).
func subJobEligibleForHardTerms(ssn *schedframework.Session, job *schedapi.JobInfo, subJob *schedapi.SubJobInfo, hyperNode *schedapi.HyperNodeInfo) bool {
	for _, term := range job.RequiredSubGroupAffinityTerms() {
		if !subGroupTermIncludesLocal(term, schedapi.SubJobPolicyName(subJob)) {
			continue
		}
		tier, err := schedapi.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return false
		}
		ancestor := ssn.HyperNodes.GetAncestorHyperNode(hyperNode.Name, tier)
		if ancestor == "" {
			return false
		}
		peers := subGroupTermPeers(ssn, job, subJob, term, tier, false)
		if peers.Len() == 0 {
			continue
		}
		if peers.Len() != 1 || !peers.Has(ancestor) {
			return false
		}
	}
	for _, term := range job.RequiredSubGroupAntiAffinityTerms() {
		if !subGroupTermIncludesLocal(term, schedapi.SubJobPolicyName(subJob)) {
			continue
		}
		tier, err := schedapi.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return false
		}
		ancestor := ssn.HyperNodes.GetAncestorHyperNode(hyperNode.Name, tier)
		if ancestor == "" {
			return false
		}
		peers := subGroupTermPeers(ssn, job, subJob, term, tier, true)
		if peers.Has(ancestor) {
			return false
		}
	}
	return true
}

// subGroupTermPeers mirrors peerSubJobOccupiedHyperNodesAtTier: term-tier domains
// occupied by the term's peer subJobs, from plan-state bindings.
func subGroupTermPeers(ssn *schedframework.Session, job *schedapi.JobInfo, self *schedapi.SubJobInfo, term schedulingapi.SubGroupAffinityTerm, tier int, antiAffinity bool) sets.Set[string] {
	occupied := sets.New[string]()
	selfPolicy := schedapi.SubJobPolicyName(self)
	for _, peer := range job.SubJobs {
		if peer == nil || peer.UID == self.UID {
			continue
		}
		peerPolicy := schedapi.SubJobPolicyName(peer)
		if !subGroupPeerMatchesTermLocal(selfPolicy, peerPolicy, term, antiAffinity) {
			continue
		}
		for hyperNodeName := range schedapi.CollectSubJobOccupiedHyperNodesAtTier(peer, ssn.HyperNodes, tier, ssn.RealNodesSet) {
			occupied.Insert(hyperNodeName)
		}
	}
	return occupied
}

// subGroupPeerMatchesTermLocal mirrors the plugin's subGroupPeerMatchesTerm.
func subGroupPeerMatchesTermLocal(selfPolicy, peerPolicy string, term schedulingapi.SubGroupAffinityTerm, antiAffinity bool) bool {
	if selfPolicy == "" || peerPolicy == "" || !subGroupTermIncludesLocal(term, selfPolicy) || !subGroupTermIncludesLocal(term, peerPolicy) {
		return false
	}
	if !antiAffinity {
		return true
	}
	if len(term.SubGroups) == 1 {
		return peerPolicy == selfPolicy
	}
	return peerPolicy != selfPolicy
}

// subGroupTermIncludesLocal is a test-local copy of the adapter's term
// membership predicate.
func subGroupTermIncludesLocal(term schedulingapi.SubGroupAffinityTerm, policy string) bool {
	for _, sg := range term.SubGroups {
		if sg == policy {
			return true
		}
	}
	return false
}

// No hard HyperNode requirement: legacy per-victim greedy cross-domain first fit;
// plan state never touched.
func TestDomainRelocation_GreedyPreservedForNoRequirement(t *testing.T) {
	u0 := phaseBTask("u0", "ns/job", "u0", "n0", 4)
	u1 := phaseBTask("u1", "ns/job", "u1", "n1", 4)
	ji := phaseBJob("ns/job", nil, u0, u1) // no topology, no SubGroupPolicy -> ==false
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, u0),
		"n1": phaseBNode("n1", 8, u1),
		"n2": phaseBNode("n2", 8),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, nil)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{u0, u1},
		[]*schedapi.NodeInfo{nodes["n2"], nodes["n0"], nodes["n1"]})
	if !fit {
		t.Fatalf("==false gang must relocate greedily, got fit=false")
	}
	// Greedy first fit in receiver order: n2 holds both 4-GPU tasks.
	if len(moves) != 2 || moves[0].To != "n2" || moves[1].To != "n2" {
		t.Errorf("greedy cross-domain first-fit expected both on n2, got %+v", moves)
	}
	// Plan state untouched: ==false skips the plan-state carrier, so the setup
	// anchor top (LCA of hA/hB) survives.
	if got := ji.AllocatedHyperNode; got != "top" {
		t.Errorf("==false job AllocatedHyperNode=%q, want top (unchanged by greedy)", got)
	}
	if u0.NodeName != "n0" || u1.NodeName != "n1" {
		t.Errorf("==false must not rewrite job-side tasks, u0=%q u1=%q", u0.NodeName, u1.NodeName)
	}
}

// Hard gang with no HyperNode tree in the session falls back to greedy: the
// constraint stack is inert without a tree.
func TestDomainRelocation_GreedyFallbackWithoutTree(t *testing.T) {
	u0 := phaseBTask("u0", "ns/job", "u0", "n0", 4)
	ji := phaseBJob("ns/job", hardTopology(1), u0) // hard, but no tree present
	n0 := phaseBNode("n0", 8, u0)
	n1 := phaseBNode("n1", 8)
	ssn := newPhaseBSSN(t)
	ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}
	ssn.Nodes = map[string]*schedapi.NodeInfo{"n0": n0, "n1": n1}
	// Tree not ready -> constraint inert (hasHyperNodeTopology()==false), so greedy.
	// NewHyperNodesInfo defaults Ready true, hence the explicit off.
	ssn.HyperNodesReadyToSchedule = false
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{u0},
		[]*schedapi.NodeInfo{n1})
	if !fit || len(moves) != 1 || moves[0].To != "n1" {
		t.Fatalf("no-tree hard gang must fall back to greedy, got fit=%t moves=%v", fit, moves)
	}
}

// A fully-vacated hard-tier-1 gang (both tasks victims) clears its anchor and is
// placed by whole-gang domain trial within one allowed tier-1 domain: n2 is the
// only receiver, so the winning domain is hD; commit re-anchors the gang to hD.
func TestDomainRelocation_HardTierFullyVacatedSingleDomain(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 4)
	ji := phaseBJob("ns/job", hardTopology(1), uA, uB)
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 8, uB),
		"n2": phaseBNode("n2", 8),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	if got := ji.AllocatedHyperNode; got != "top" {
		t.Fatalf("initial job anchor=%q, want top (LCA of hA/hB)", got)
	}
	snap := NewSessionSnapshot(ssn, gpu, nil)

	// Anchor top is tier 2 > allowed 1; success requires the cleared anchor
	// (otherwise GetSearchRoot errors -> infeasible).
	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB},
		[]*schedapi.NodeInfo{nodes["n2"]})
	if !fit {
		t.Fatalf("fully-vacated hard-tier gang must relocate, got fit=false")
	}
	if len(moves) != 2 || moves[0].To != "n2" || moves[1].To != "n2" {
		t.Errorf("domain trial must land both victims in the single receiver domain hD, got %+v", moves)
	}
	// plan-state commit: anchors recomputed to the landed domain.
	if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hD" {
		t.Errorf("committed job anchor=%q, want hD", got)
	}
	// node side untouched (Jobs/Nodes 分叉).
	if ssn.Nodes["n0"].Tasks[schedapi.PodKey(uA.Pod)].NodeName != "n0" {
		t.Error("node-side uA must keep its original binding")
	}
}

// Negative: a hard-tier-1 gang NOT fully vacated keeps anchor top (tier 2, out of
// the allowed subtree) -> no feasible domain; unit rejected with no residue.
func TestDomainRelocation_HardTierPartiallyVacatedInfeasible(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 4)
	ji := phaseBJob("ns/job", hardTopology(1), uA, uB)
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 8, uB),
		"n2": phaseBNode("n2", 8),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	// Only uA is a victim; uB stays -> not fully vacated, anchor stays top -> the
	// hard-tier-1 gradient has no feasible search root.
	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA},
		[]*schedapi.NodeInfo{nodes["n2"]})
	if fit || moves != nil {
		t.Fatalf("hard-tier-1 gang anchored at tier-2 top must be infeasible, got fit=%t moves=%v", fit, moves)
	}
	if got := ji.AllocatedHyperNode; got != "top" {
		t.Errorf("infeasible unit must leave the anchor at %q, want top", got)
	}
	if uA.NodeName != "n0" || uB.NodeName != "n1" {
		t.Errorf("infeasible unit must not rewrite tasks, uA=%q uB=%q", uA.NodeName, uB.NodeName)
	}
}

// Soft mode abstains -> allowed domains are the whole candidate universe (no tier
// restriction); a soft gang relocates freely.
func TestDomainRelocation_SoftModeUnrestricted(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	ji := phaseBJob("ns/job", softTopology(), uA)
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 8),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	// White-box: soft gang's allowed domains are the full universe, tier-2 root
	// included (abstain == no constraint).
	layers, ok := snap.allowedDomains(&gangUnit{job: ji, victims: []*schedapi.TaskInfo{uA}})
	if !ok {
		t.Fatal("soft-mode gang must have allowed domains (the universe)")
	}
	names := sets.New[string]()
	for _, layer := range layers {
		for _, hyperNode := range layer {
			names.Insert(hyperNode.Name)
		}
	}
	for _, want := range []string{"hA", "hB", "hD", "top"} {
		if !names.Has(want) {
			t.Errorf("soft-mode universe missing %q, got %v", want, names.UnsortedList())
		}
	}

	// End-to-end: relocation succeeds across domains (hA -> hB), which hard mode
	// would restrict.
	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA},
		[]*schedapi.NodeInfo{nodes["n1"]})
	if !fit || len(moves) != 1 || moves[0].To != "n1" {
		t.Fatalf("soft gang must relocate into hB freely, got fit=%t moves=%v", fit, moves)
	}
}

// Required PodGroup anti-affinity excluding the gang's occupied domain (hB) keeps
// the domain trial off hB's nodes; infeasible when every viable placement is
// excluded.
func TestDomainRelocation_RequiredAntiAffinityExcludesDomain(t *testing.T) {
	// Peer gang occupies hB -> excluded. n0 is the drained source; n1 (hB) and n2
	// (hD) are the receivers.
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 8)
	ji := phaseBJob("ns/job", hardTopology(1), uA)
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 8),
		"n2": phaseBNode("n2", 8),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji},
		func(s *schedframework.Session) { registerHardTierAndAntiAffinityGradient(s, sets.New("hB")) })
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA},
		[]*schedapi.NodeInfo{nodes["n1"], nodes["n2"]})
	if !fit {
		t.Fatalf("hD is a viable non-excluded domain, got fit=false")
	}
	if len(moves) != 1 || moves[0].To != "n2" {
		t.Errorf("must land on hD (n2), never on the excluded hB (n1); got %+v", moves)
	}

	// Now the peer also occupies hD -> every receiver domain excluded -> infeasible.
	uB := phaseBTask("uB", "ns/job2", "uB", "n0", 8)
	ji2 := phaseBJob("ns/job2", hardTopology(1), uB)
	nodes2 := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uB),
		"n1": phaseBNode("n1", 8),
		"n2": phaseBNode("n2", 8),
	}
	ssn2 := phaseBSession(t, nodes2, map[schedapi.JobID]*schedapi.JobInfo{"ns/job2": ji2},
		func(s *schedframework.Session) { registerHardTierAndAntiAffinityGradient(s, sets.New("hB", "hD")) })
	snap2 := NewSessionSnapshot(ssn2, gpu, nil)
	moves2, fit2 := snap2.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uB},
		[]*schedapi.NodeInfo{nodes2["n1"], nodes2["n2"]})
	if fit2 || moves2 != nil {
		t.Fatalf("only-excluded-domain unit must be infeasible, got fit=%t moves=%v", fit2, moves2)
	}
}

// Mixed-gang atomicity: after a ==true gang commits into hD, a ==false victim
// that cannot fit fails the whole call; the ==true plan-state commit rolls back.
func TestDomainRelocation_MixedGangAtomicity(t *testing.T) {
	uA := phaseBTask("uA", "ns/true", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/true", "uB", "n1", 4)
	jt := phaseBJob("ns/true", hardTopology(1), uA, uB) // ==true
	uC := phaseBTask("uC", "ns/false", "uC", "n3", 8)
	jf := phaseBJob("ns/false", nil, uC) // ==false

	hyperNodes, rns := planTestHyperNodes()
	rns["top"].Insert("n3")
	rns["hD"].Insert("n3")
	rns[schedframework.ClusterTopHyperNode].Insert("n3")
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 8, uB),
		"n2": phaseBNode("n2", 8),
		"n3": phaseBNode("n3", 8, uC),
	}
	for _, ji := range []*schedapi.JobInfo{jt, jf} {
		schedapi.SyncJobAllocatedHyperNode(ji, hyperNodes, rns)
	}
	ssn := newPhaseBSSN(t)
	ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/true": jt, "ns/false": jf}
	ssn.Nodes = nodes
	ssn.HyperNodes = hyperNodes
	ssn.RealNodesSet = rns
	registerHardTierGradient(ssn)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	// uC needs 8 GPUs; n2 is already filled by the ==true gang's two 4-GPU victims
	// -> greedy fails after the ==true commit.
	moves, fit := snap.FeasibleRelocation(context.Background(), nil,
		[]*schedapi.TaskInfo{uA, uB, uC}, []*schedapi.NodeInfo{nodes["n2"]})
	if fit || moves != nil {
		t.Fatalf("greedy failure must fail the whole call, got fit=%t moves=%v", fit, moves)
	}
	// ==true plan-state commit rolled back to baseline.
	if got := snap.planState().JobAllocatedHyperNode(jt.UID); got != "top" {
		t.Errorf("==true job anchor after rollback=%q, want top", got)
	}
	if uA.NodeName != "n0" || uB.NodeName != "n1" {
		t.Errorf("==true tasks must revert, uA=%q uB=%q", uA.NodeName, uB.NodeName)
	}
	if ssn.Nodes["n0"].Tasks[schedapi.PodKey(uA.Pod)].NodeName != "n0" {
		t.Error("node side must be untouched")
	}
}

// SubJob unit: the two-entry gradient narrowing (Job-entry ∩ SubJob-entry,
// root-or-ancestor) keeps the whole subJob inside one allowed tier-1 domain;
// commits the subJob and job anchors together.
func TestDomainRelocation_SubJobUnitCommonDomain(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 4)
	sj := planTestSubJob("ns/job/grp", "ns/job", uA, uB)
	sj.NetworkTopology = hardTopology(1)
	ji := planTestJob("ns/job", sj)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "workers"}},
	}}}

	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 8, uB),
		"n2": phaseBNode("n2", 8),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	if got := sj.AllocatedHyperNode; got != "top" {
		t.Fatalf("initial subJob anchor=%q, want top (LCA of hA/hB)", got)
	}
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB},
		[]*schedapi.NodeInfo{nodes["n2"]})
	if !fit {
		t.Fatalf("fully-vacated subJob must relocate, got fit=false")
	}
	if len(moves) != 2 || moves[0].To != "n2" || moves[1].To != "n2" {
		t.Errorf("subJob victims must land in the single receiver domain hD, got %+v", moves)
	}
	if got := snap.planState().SubJobAllocatedHyperNode(ji.UID, sj.UID); got != "hD" {
		t.Errorf("committed subJob anchor=%q, want hD", got)
	}
	if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hD" {
		t.Errorf("committed job anchor=%q, want hD", got)
	}
}

// intersectGradientForest keeps the inner (SubJob-entry) forest's HyperNodes
// under the outer (Job-entry) forest (root-or-ancestor); empty when the forests
// do not overlap.
func TestIntersectGradientForest(t *testing.T) {
	hyperNodes, _ := planTestHyperNodes()
	inner := [][]*schedapi.HyperNodeInfo{{hyperNodes["hB"], hyperNodes["hD"], hyperNodes["top"]}}

	// outer {hA,hB}: hB kept, hD and top dropped (not under hA/hB).
	got, ok := intersectGradientForest([][]*schedapi.HyperNodeInfo{{hyperNodes["hA"], hyperNodes["hB"]}}, inner, hyperNodes)
	if !ok || len(got) != 1 || len(got[0]) != 1 || got[0][0].Name != "hB" {
		t.Errorf("intersection=%v ok=%t, want [[hB]] true", got, ok)
	}

	// empty outer forest: infeasible, no common domain.
	if got, ok := intersectGradientForest(nil, inner, hyperNodes); ok || got != nil {
		t.Errorf("empty outer must be infeasible, got %v ok=%t", got, ok)
	}

	// outer spanning all tier-1 domains keeps hB and hD, drops top.
	got2, ok := intersectGradientForest([][]*schedapi.HyperNodeInfo{{hyperNodes["hA"], hyperNodes["hB"], hyperNodes["hD"]}}, inner, hyperNodes)
	if !ok || len(got2) != 1 || len(got2[0]) != 2 {
		t.Fatalf("full outer must keep two domains, got %v ok=%t", got2, ok)
	}
	names := sets.New[string]()
	for _, hyperNodeInfo := range got2[0] {
		names.Insert(hyperNodeInfo.Name)
	}
	if !names.HasAll("hB", "hD") {
		t.Errorf("kept domains=%v, want {hB,hD}", names.UnsortedList())
	}
}

// Serial unit pinning co-locates Required-affinity subJobs (each member its own
// unit, in victim order): A pins to unmoved peer B's hB and lands on hB's empty
// n3; the commit re-anchors A to hB, so B follows onto n3. Both stay in one
// HyperNode domain.
func TestDomainRelocation_SubJobAffinitySerialCoLocation(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 4)
	sjA := phaseBSubJob("ns/job/policyA", "ns/job", "policyA", uA)
	sjB := phaseBSubJob("ns/job/policyB", "ns/job", "policyB", uB)
	ji := phaseBSubGroupAffinityJob("ns/job", 1, [][]string{{"policyA", "policyB"}}, sjA, sjB)

	// n3 is an empty cap-8 receiver under hB (the peer's domain), not n1, or the
	// first unit could land beside the peer and pin it in place.
	hyperNodes, rns := planTestHyperNodes()
	rns["hB"].Insert("n3")
	rns["top"].Insert("n3")
	rns[schedframework.ClusterTopHyperNode].Insert("n3")
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA), // hA
		"n1": phaseBNode("n1", 8, uB), // hB
		"n3": phaseBNode("n3", 8),     // hB
	}
	schedapi.SyncJobAllocatedHyperNode(ji, hyperNodes, rns)
	ssn := newPhaseBSSN(t)
	ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}
	ssn.Nodes = nodes
	ssn.HyperNodes = hyperNodes
	ssn.RealNodesSet = rns
	registerSubGroupAffinityGradient(ssn)
	snap := NewSessionSnapshot(ssn, gpu, nil)
	if got := sjA.AllocatedHyperNode; got != "hA" {
		t.Fatalf("initial subJob A anchor=%q, want hA", got)
	}
	if got := sjB.AllocatedHyperNode; got != "hB" {
		t.Fatalf("initial subJob B anchor=%q, want hB", got)
	}

	// A lands on hB's n3 (pinned by B); commit re-anchors A to hB, so B follows
	// onto n3.
	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB},
		[]*schedapi.NodeInfo{nodes["n3"]})
	if !fit {
		t.Fatalf("affinity-linked subJobs must co-locate by serial pinning, got fit=false")
	}
	if len(moves) != 2 || moves[0].To != "n3" || moves[1].To != "n3" {
		t.Errorf("serial pinning must land both subJobs on n3 (hB), got %+v", moves)
	}
	if got := snap.planState().SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hB" {
		t.Errorf("committed subJob A anchor=%q, want hB", got)
	}
	if got := snap.planState().SubJobAllocatedHyperNode(ji.UID, sjB.UID); got != "hB" {
		t.Errorf("committed subJob B anchor=%q, want hB", got)
	}
	if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hB" {
		t.Errorf("committed job anchor=%q, want hB", got)
	}
	// node side untouched (Jobs/Nodes 分叉).
	if ssn.Nodes["n0"].Tasks[schedapi.PodKey(uA.Pod)].NodeName != "n0" {
		t.Error("node-side uA must keep its original binding")
	}
}

// Serial-pinning infeasibility: A (first) lands on peer domain hB (n1); B, pinned
// to A's settled hB, finds no free receiver there (n1 full, hD out of domain), so
// the call fails with no residue. Contrast: A alone relocates fine, so the
// rejection is the shared-domain requirement, not a single subJob's capacity.
func TestDomainRelocation_SubJobAffinityNoInDomainReceiverInfeasible(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 4)
	sjA := phaseBSubJob("ns/job/policyA", "ns/job", "policyA", uA)
	sjB := phaseBSubJob("ns/job/policyB", "ns/job", "policyB", uB)
	ji := phaseBSubGroupAffinityJob("ns/job", 1, [][]string{{"policyA", "policyB"}}, sjA, sjB)

	// n1 (hB) holds one 4-GPU task; hD (n2) is out of the pinned domain.
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 4),
		"n2": phaseBNode("n2", 4),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerSubGroupAffinityGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	// A fills hB's n1; B, pinned to A's settled hB, has no free receiver ->
	// infeasible, nothing committed.
	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB},
		[]*schedapi.NodeInfo{nodes["n1"], nodes["n2"]})
	if fit || moves != nil {
		t.Fatalf("serial pinning with no shared receiver capacity must be infeasible, got fit=%t moves=%v", fit, moves)
	}
	if got := snap.planState().SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hA" {
		t.Errorf("infeasible call must leave subJob A anchor=%q, want hA", got)
	}
	if uA.NodeName != "n0" || uB.NodeName != "n1" {
		t.Errorf("infeasible call must not rewrite tasks, uA=%q uB=%q", uA.NodeName, uB.NodeName)
	}

	// Contrast: A alone relocates onto peer domain hB (n1) — the rejection above
	// is the shared-domain requirement, not A's feasibility.
	alone, aFit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA},
		[]*schedapi.NodeInfo{nodes["n1"]})
	if !aFit || len(alone) != 1 || alone[0].To != "n1" {
		t.Fatalf("single subJob must relocate onto peer domain hB (n1), got fit=%t moves=%v", aFit, alone)
	}
}

// TestGangFullyVacated_SetMembership pins the predicate shape: a victim may be a
// Pending replacement that was never allocated, so count equality would misfire
// while a residual allocated pod still anchors it. gangFullyVacated = set
// membership: every plan-state allocated task of the gang is a victim.
func TestGangFullyVacated_SetMembership(t *testing.T) {
	mkJob := func(tasks ...*schedapi.TaskInfo) *schedapi.JobInfo {
		return phaseBJob("ns/job", hardTopology(1), tasks...)
	}
	mkPending := func(uid string) *schedapi.TaskInfo {
		tk := phaseBTask(uid, "ns/job", uid, "", 4)
		tk.Status = schedapi.Pending // replacement pod, never allocated
		return tk
	}
	cases := []struct {
		name    string
		job     *schedapi.JobInfo
		subJob  *schedapi.SubJobInfo
		victims []*schedapi.TaskInfo
		want    bool
	}{
		{
			// Partial evac: residual Running uB + Pending victim; count 1==1 misfires.
			name:    "execute partial-evac residual keeps anchor",
			job:     mkJob(phaseBTask("uB", "ns/job", "uB", "n1", 4), mkPending("uA")),
			victims: []*schedapi.TaskInfo{mkPending("uA")},
			want:    false,
		},
		{
			// Fully drained: only the Pending replacement remains -> no residual.
			name:    "execute fully-drained no residual",
			job:     mkJob(mkPending("uA")),
			victims: []*schedapi.TaskInfo{mkPending("uA")},
			want:    true,
		},
		{
			// Planning whole-gang drained: both allocated tasks are victims.
			name: "planning whole-gang drained",
			job: mkJob(
				phaseBTask("uA", "ns/job", "uA", "n0", 4),
				phaseBTask("uB", "ns/job", "uB", "n0", 4)),
			victims: []*schedapi.TaskInfo{
				phaseBTask("uA", "ns/job", "uA", "n0", 4),
				phaseBTask("uB", "ns/job", "uB", "n0", 4)},
			want: true,
		},
		{
			// Planning shape, partial: uB stays allocated and is not a victim.
			name: "planning partial-evac keeps anchor",
			job: mkJob(
				phaseBTask("uA", "ns/job", "uA", "n0", 4),
				phaseBTask("uB", "ns/job", "uB", "n1", 4)),
			victims: []*schedapi.TaskInfo{phaseBTask("uA", "ns/job", "uA", "n0", 4)},
			want:    false,
		},
		{
			// SubJob unit, partial evac: residual allocated peer is not a victim.
			name: "subjob execute residual keeps anchor",
			job:  mkJob(phaseBTask("uA", "ns/job", "uA", "n1", 4), mkPending("uB")),
			subJob: func() *schedapi.SubJobInfo {
				sj := phaseBSubJob("ns/job/p", "ns/job", "p",
					phaseBTask("uA", "ns/job", "uA", "n1", 4), mkPending("uB"))
				return sj
			}(),
			victims: []*schedapi.TaskInfo{mkPending("uB")},
			want:    false,
		},
	}
	snap := &SessionSnapshot{} // gangFullyVacated reads only the unit
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := snap.gangFullyVacated(&gangUnit{job: tc.job, subJob: tc.subJob, victims: tc.victims})
			if got != tc.want {
				t.Errorf("gangFullyVacated()=%t, want %t", got, tc.want)
			}
		})
	}
}
