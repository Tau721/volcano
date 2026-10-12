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
	"slices"
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	schedulingapi "volcano.sh/apis/pkg/apis/scheduling"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"
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
	emptyGradientStubName    = "empty-gradient-stub"
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

// registerEmptyJobGradient installs a Job-entry stub that constrains every
// candidate away: an applied empty gradient is an unschedulable result, not an
// abstention.
func registerEmptyJobGradient(ssn *schedframework.Session) {
	ssn.AddHyperNodeGradientForJobFn(emptyGradientStubName,
		func(*schedapi.JobInfo, *schedapi.HyperNodeInfo, schedapi.SearchPurpose) schedapi.HyperNodeGradientResult {
			return schedapi.HyperNodeGradientConstrain(nil)
		})
	ssn.Tiers = []schedconf.Tier{{Plugins: []schedconf.PluginOption{
		{Name: emptyGradientStubName, EnabledHyperNodeGradient: ptr.To(true)},
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
// rewrites, so a serial gang commit re-anchors the peer and pins the follower.
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
// the allowed subtree) -> no feasible domain; gang rejected with no residue.
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
		t.Errorf("infeasible gang must leave the anchor at %q, want top", got)
	}
	if uA.NodeName != "n0" || uB.NodeName != "n1" {
		t.Errorf("infeasible gang must not rewrite tasks, uA=%q uB=%q", uA.NodeName, uB.NodeName)
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
	layers, ok := snap.allowedDomains(&jobGangs{job: ji}, &gang{victims: []*schedapi.TaskInfo{uA}})
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
		t.Fatalf("only-excluded-domain gang must be infeasible, got fit=%t moves=%v", fit2, moves2)
	}
}

// twoRoleSubGroupJob builds a SubGroupPolicy job over roleA/roleB, so the
// victims split into one gang per role.
func twoRoleSubGroupJob(uA, uB *schedapi.TaskInfo) *schedapi.JobInfo {
	sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
	sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
	ji := planTestJob("ns/job", sjA, sjB)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
	}}}
	return ji
}

// Required PodGroup anti-affinity whose term sits at the finest tier leaves no
// coarser candidate: excluding the peer's domain also excludes its ancestors
// (top, <cluster-top-hypernode>), so the Job's allowed roots are siblings and no
// single one of them holds both roles. allocate answers this class with its
// candidate forest — one gradient layer is one set of allowed roots and a SubJob
// settles in any of them — and the container mirrors it. A real Job-level hard
// network topology keeps allocate's legacy single-root path, so there the
// container still demands one domain.
func TestDomainRelocation_RequiredAntiAffinitySpreadsGangsAcrossAllowedRoots(t *testing.T) {
	// Both roles are drained from n0 (hA); n3 (hA) and n2 (hD) are the two empty
	// receivers, one per allowed tier-1 root. phaseBSession always builds its own
	// reference tree, so the session is assembled by hand to add n3 to it.
	build := func(t *testing.T, ji *schedapi.JobInfo, uA, uB *schedapi.TaskInfo, register func(*schedframework.Session)) (*SessionSnapshot, []*schedapi.TaskInfo, []*schedapi.NodeInfo) {
		t.Helper()
		hyperNodes, rns := planTestHyperNodes()
		rns["hA"].Insert("n3")
		rns["top"].Insert("n3")
		rns[schedframework.ClusterTopHyperNode].Insert("n3")
		nodes := map[string]*schedapi.NodeInfo{
			"n0": phaseBNode("n0", 8, uA, uB), // hA: the drained source
			"n2": phaseBNode("n2", 4),         // hD
			"n3": phaseBNode("n3", 4),         // hA
		}
		schedapi.SyncJobAllocatedHyperNode(ji, hyperNodes, rns)
		ssn := newPhaseBSSN(t)
		ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}
		ssn.Nodes = nodes
		ssn.HyperNodes = hyperNodes
		ssn.RealNodesSet = rns
		register(ssn)
		return NewSessionSnapshot(ssn, gpu, nil), []*schedapi.TaskInfo{uA, uB},
			[]*schedapi.NodeInfo{nodes["n3"], nodes["n2"]}
	}
	t.Run("required PodGroup anti-affinity spreads the roles across the allowed roots", func(t *testing.T) {
		uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
		uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
		ji := twoRoleSubGroupJob(uA, uB)
		ji.PodGroup.Spec.TopologyAffinity = &schedulingapi.TopologyAffinitySpec{
			PodGroupAntiAffinity: &schedulingapi.PodGroupAntiAffinity{
				Required: []schedulingapi.PodGroupAffinityTerm{{TopologyTier: ptr.To(int32(1))}},
			},
		}
		snap, victims, receivers := build(t, ji, uA, uB,
			func(s *schedframework.Session) { registerHardTierAndAntiAffinityGradient(s, sets.New("hB")) })

		// hB's exclusion drops top and <cluster-top-hypernode> with it, so hA and hD
		// are the only allowed roots — sibling roots their LCA ("top") may not stand
		// in for, since its subtree also holds the excluded hB.
		moves, fit := snap.FeasibleRelocation(context.Background(), nil, victims, receivers)
		if !fit {
			t.Fatalf("each role fits one allowed root, got fit=false")
		}
		if got := moveSummary(moves); !slices.Equal(got, []string{"uA: from n0 to n3", "uB: from n0 to n2"}) {
			t.Errorf("roles must spread across the allowed roots, got %v", got)
		}
		if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "top" {
			t.Errorf("committed job anchor=%q, want top (the LCA of hA and hD)", got)
		}
	})

	t.Run("a hard Job-level topology still demands one domain", func(t *testing.T) {
		uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
		uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
		ji := twoRoleSubGroupJob(uA, uB)
		ji.NetworkTopology = hardTopology(1)
		snap, victims, receivers := build(t, ji, uA, uB, registerHardTierGradient)

		// Same geometry, but the hard constraint allows only tier-1 domains and this
		// class keeps the single-root path: neither hA nor hD holds both roles.
		moves, fit := snap.FeasibleRelocation(context.Background(), nil, victims, receivers)
		if fit || moves != nil {
			t.Fatalf("a hard topology Job must stay in one domain, got fit=%t moves=%v", fit, moves)
		}
	})
}

// A container trial that settles one gang and then fails must leave the task
// placements as it found them: trialGangInDomains only discards its own attempt,
// so the settled gang's residue is removed by the container-level restore. The
// map is call-local to FeasibleRelocation, so the container is called directly to
// see it.
func TestTrialWholeJob_FailedContainerRestoresTaskPlacements(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
	// A hard Job-level topology keeps the container on the single-domain path: its
	// candidate domains are the three tier-1 roots, tried hA, hB, hD. No tier-1 root
	// holds both roles, so every set fails — but hA fails only after roleA has
	// settled on n3, so the container is left holding a partial placement.
	ji := twoRoleSubGroupJob(uA, uB)
	ji.NetworkTopology = hardTopology(1)
	// An earlier container of this call moved its own victim to n2; that placement
	// must survive the failing container's rollback.
	prior := phaseBTask("prior", "ns/prior", "prior", "n0", 1)

	hyperNodes, rns := planTestHyperNodes()
	rns["hA"].Insert("n3")
	rns["top"].Insert("n3")
	rns[schedframework.ClusterTopHyperNode].Insert("n3")
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 12, uA, uB, prior), // hA: the drained source
		"n2": phaseBNode("n2", 4),                 // hD
		"n3": phaseBNode("n3", 4),                 // hA
	}
	schedapi.SyncJobAllocatedHyperNode(ji, hyperNodes, rns)
	ssn := newPhaseBSSN(t)
	ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji, "ns/prior": phaseBJob("ns/prior", nil, prior)}
	ssn.Nodes = nodes
	ssn.HyperNodes = hyperNodes
	ssn.RealNodesSet = rns
	registerHardTierGradient(ssn)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	victims := []*schedapi.TaskInfo{uA, uB}
	jgs := snap.groupVictimsByJobGangs(victims)
	if len(jgs) != 1 || len(jgs[0].gangs) != 2 {
		t.Fatalf("the victims must form one two-gang container, got %d containers", len(jgs))
	}
	tasksPlacedByNode := map[string][]*schedapi.TaskInfo{"n2": {prior}}
	moves, fit := snap.trialWholeJob(context.Background(), jgs[0], newTrialScope(victims), append(victims, prior),
		[]*schedapi.NodeInfo{nodes["n3"], nodes["n2"]}, tasksPlacedByNode)
	if fit || moves != nil {
		t.Fatalf("no tier-1 root holds both roles, got fit=%t moves=%v", fit, moves)
	}
	if len(tasksPlacedByNode) != 1 || !slices.Equal(tasksPlacedByNode["n2"], []*schedapi.TaskInfo{prior}) {
		t.Errorf("a failed container trial must restore the task placements to the prior container's move, got %v", tasksPlacedByNode)
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

// SubJob gang: the two-entry gradient narrowing (Job-entry ∩ SubJob-entry,
// root-or-ancestor) keeps the whole subJob inside one allowed tier-1 domain;
// commits the subJob and job anchors together.
func TestDomainRelocation_SubJobGangCommonDomain(t *testing.T) {
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

// The job's single role is fully drained: the job anchor must clear alongside the
// subJob anchor, else the Job entry pins the trial to n0/hA — the drained source only.
func TestDomainRelocation_FullyVacatedJobClearsJobAnchor(t *testing.T) {
	uA := phaseBTask("uA", "ns/jobA", "uA", "n0", 1)
	uB := phaseBTask("uB", "ns/jobA", "uB", "n0", 1)
	sj := phaseBSubJob("ns/jobA/role", "ns/jobA", "role", uA, uB)
	sj.NetworkTopology = hardTopology(1)
	ji := planTestJob("ns/jobA", sj)
	ji.NetworkTopology = hardTopology(1)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "role"}},
	}}}

	uC := phaseBTask("uC", "ns/jobB", "uC", "n1", 1)
	uD := phaseBTask("uD", "ns/jobB", "uD", "n1", 1)
	jb := phaseBJob("ns/jobB", hardTopology(1), uC, uD)

	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 16, uA, uB),
		"n1": phaseBNode("n1", 16, uC, uD),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/jobA": ji, "ns/jobB": jb}, registerHardTierGradient)
	if got := ji.AllocatedHyperNode; got != "hA" {
		t.Fatalf("initial job anchor=%q, want hA", got)
	}
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil,
		[]*schedapi.TaskInfo{uA, uB}, []*schedapi.NodeInfo{nodes["n1"]})
	if !fit {
		t.Fatalf("whole job vacated: its role must be free to move to another HyperNode, got fit=false")
	}
	if len(moves) != 2 || moves[0].To != "n1" || moves[1].To != "n1" {
		t.Errorf("role victims must land in hB (n1), got %+v", moves)
	}
}

// A role drained whole while its sibling role keeps running: only the drained
// role's anchor clears, so it may still join the surviving sibling's domain
// (both are inside the job's own envelope).
func TestDomainRelocation_FullyVacatedSubJobJoinsSurvivingSiblingDomain(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 1)
	sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
	sjA.NetworkTopology = hardTopology(2)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 1)
	sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
	sjB.NetworkTopology = hardTopology(2)
	ji := planTestJob("ns/job", sjA, sjB)
	ji.NetworkTopology = hardTopology(2)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
	}}}

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

	// n0 is the drained source, so roleB's n1 is the only in-envelope receiver.
	moves, fit := snap.FeasibleRelocation(context.Background(), nil,
		[]*schedapi.TaskInfo{uA}, []*schedapi.NodeInfo{nodes["n1"]})
	if !fit {
		t.Fatalf("drained role must move inside the job envelope, got fit=false")
	}
	if len(moves) != 1 || moves[0].To != "n1" {
		t.Errorf("role A must land on n1 (hB), got %+v", moves)
	}
}

// Only a whole-gang evacuation widens the envelope: with a residual pod the
// pre-eviction anchor and the survivor-only anchor resolve up to the declared
// tier and yield the same allowed domains, sibling domains included — so the
// trial cannot land in a domain the scheduler forbids, nor reject a legal one.
func TestDomainRelocation_PartialVacateKeepsSiblingDomainAllowed(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 1)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 1)
	sj := phaseBSubJob("ns/job/role", "ns/job", "role", uA, uB)
	sj.NetworkTopology = hardTopology(2)
	ji := planTestJob("ns/job", sj)
	ji.NetworkTopology = hardTopology(2)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "role"}},
	}}}

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

	stale := allowedDomainNames(t, snap, ji, sj, uA)
	ji.AllocatedHyperNode = "hB" // the anchor a post-eviction recompute would leave
	survivor := allowedDomainNames(t, snap, ji, sj, uA)

	if !slices.Equal(stale, survivor) {
		t.Errorf("a residual pod must keep the envelope: pre-eviction %v vs survivor-only %v", stale, survivor)
	}
	if !slices.Contains(stale, "hD") {
		t.Errorf("allowed domains %v must keep the sibling HyperNode hD", stale)
	}
}

// Two roles of one job drained together: the job's envelope must follow the whole
// call, not one gang. Both roles leave n0/hA, and the Job entry must not keep the
// pre-eviction anchor hA — its only node is the drained source, so the run would
// report no plan (the multi-SubJob bug). Only hB's n1 and hD's n2 can receive.
func TestDomainRelocation_MultipleSubJobsVacatedTogetherLeaveSourceDomain(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
	sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
	sjA.NetworkTopology = hardTopology(1)
	sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
	sjB.NetworkTopology = hardTopology(1)
	ji := planTestJob("ns/job", sjA, sjB)
	ji.NetworkTopology = hardTopology(1)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
	}}}

	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA, uB), // hA: the drained source
		"n2": phaseBNode("n2", 8),         // hD: the only receiver
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	if got := ji.AllocatedHyperNode; got != "hA" {
		t.Fatalf("initial job anchor=%q, want hA (both roles on n0)", got)
	}
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB},
		[]*schedapi.NodeInfo{nodes["n2"]})
	if !fit {
		t.Fatalf("a job whose every role is drained must be free to leave hA, got fit=false")
	}
	if len(moves) != 2 || moves[0].To != "n2" || moves[1].To != "n2" {
		t.Errorf("both roles must land in the single receiver domain hD, got %+v", moves)
	}
	if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hD" {
		t.Errorf("committed job anchor=%q, want hD", got)
	}
	if ssn.Nodes["n0"].Tasks[schedapi.PodKey(uA.Pod)].NodeName != "n0" {
		t.Error("node-side uA must keep its original binding")
	}
}

// The roles of one job must share their tier-1 domain, so a role that fits only in
// another domain is refused rather than planned apart: roleA takes hB's single
// free receiver, roleB — pinned to roleA's fresh anchor hB — has nowhere to go and
// fails the whole call. Releasing roleB would split the job across hB and hD.
func TestDomainRelocation_MultiSubJobCoLocationRejectsSplitPlan(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
	sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
	sjA.NetworkTopology = hardTopology(1)
	sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
	sjB.NetworkTopology = hardTopology(1)
	ji := planTestJob("ns/job", sjA, sjB)
	ji.NetworkTopology = hardTopology(1)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
	}}}

	// One free receiver per domain: hB holds one pod, hD one pod.
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA, uB),
		"n1": phaseBNode("n1", 4), // hB
		"n2": phaseBNode("n2", 4), // hD
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB},
		[]*schedapi.NodeInfo{nodes["n1"], nodes["n2"]})
	if fit || moves != nil {
		t.Fatalf("no single domain holds both roles, so the call must be refused, got fit=%t moves=%v", fit, moves)
	}
	// Symmetric rollback: the committed leader leaves no residue.
	if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hA" {
		t.Errorf("refused call must leave job anchor=%q, want hA", got)
	}
	if got := snap.planState().SubJobAllocatedHyperNode(ji.UID, sjA.UID); got != "hA" {
		t.Errorf("refused call must leave subJob A anchor=%q, want hA", got)
	}
	if uA.NodeName != "n0" || uB.NodeName != "n0" {
		t.Errorf("refused call must not rewrite tasks, uA=%q uB=%q", uA.NodeName, uB.NodeName)
	}
}

// Two roles of one job drained together, and only one of the two domains holds
// both: hB's single free slot fits roleA alone, hD fits both. The first gang's
// first fit must not marry the job to hB — the group is settled per domain, so
// the plan lands in hD instead of being refused.
func TestDomainRelocation_TwoRolesVacatedTogetherMoveToTheDomainHoldingBoth(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
	sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
	sjA.NetworkTopology = hardTopology(1)
	sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
	sjB.NetworkTopology = hardTopology(1)
	ji := planTestJob("ns/job", sjA, sjB)
	ji.NetworkTopology = hardTopology(1)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
	}}}

	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA, uB), // hA: the drained source
		"n1": phaseBNode("n1", 4),         // hB: room for one role
		"n2": phaseBNode("n2", 8),         // hD: room for both
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB},
		[]*schedapi.NodeInfo{nodes["n1"], nodes["n2"]})
	if !fit {
		t.Fatalf("hD holds both roles, so the whole-job trial must find it, got fit=false")
	}
	if len(moves) != 2 || moves[0].To != "n2" || moves[1].To != "n2" {
		t.Errorf("both roles must land in hD, got %+v", moves)
	}
	if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hD" {
		t.Errorf("committed job anchor=%q, want hD", got)
	}
}

// One role declared tighter than the job: the tighter role narrows only its own
// gang. The group is settled inside one Job-entry candidate domain (tier ascending,
// first fit), and inside it each gang uses its own Job∩SubJob gradient — the
// scheduler's allocate semantics, where the candidate comes from the job's
// gradient and each SubJob is allocated inside that candidate.
func TestDomainRelocation_TighterRoleDoesNotPinTheWholeJob(t *testing.T) {
	build := func(t *testing.T, n1cap, n2cap int64) (*SessionSnapshot, *schedapi.JobInfo, map[string]*schedapi.TaskInfo) {
		t.Helper()
		uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
		uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
		sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
		sjA.NetworkTopology = hardTopology(2)
		sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
		sjB.NetworkTopology = hardTopology(1) // tighter than the job's tier 2
		ji := planTestJob("ns/job", sjA, sjB)
		ji.NetworkTopology = hardTopology(2)
		ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
			SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
		}}}

		nodes := map[string]*schedapi.NodeInfo{
			"n0": phaseBNode("n0", 8, uA, uB), // hA: the drained source
			"n1": phaseBNode("n1", n1cap),     // hB
			"n2": phaseBNode("n2", n2cap),     // hD
		}
		ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
		return NewSessionSnapshot(ssn, gpu, nil), ji, map[string]*schedapi.TaskInfo{"uA": uA, "uB": uB}
	}

	t.Run("a tier-1 domain holds both roles", func(t *testing.T) {
		snap, ji, tasks := build(t, 4, 8)
		moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{tasks["uA"], tasks["uB"]},
			[]*schedapi.NodeInfo{snap.ssn.Nodes["n1"], snap.ssn.Nodes["n2"]})
		if !fit {
			t.Fatalf("hD holds both roles, got fit=false")
		}
		if len(moves) != 2 || moves[0].To != "n2" || moves[1].To != "n2" {
			t.Errorf("both roles must land in hD, got %+v", moves)
		}
		// Candidates are tried tier ascending, so the tier-1 domain that holds both
		// roles wins before "top" (tier 2) is ever reached.
		if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hD" {
			t.Errorf("committed job anchor=%q, want hD", got)
		}
	})

	t.Run("no tier-1 domain holds both roles so the job spreads inside its tier-2 domain", func(t *testing.T) {
		snap, ji, tasks := build(t, 4, 4)
		moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{tasks["uA"], tasks["uB"]},
			[]*schedapi.NodeInfo{snap.ssn.Nodes["n1"], snap.ssn.Nodes["n2"]})
		if !fit {
			t.Fatalf("neither tier-1 domain holds both roles, but the job's tier-2 domain does, got fit=false")
		}
		// Role A (tier 2) takes hB, role B (the tighter tier-1 role) takes hD: each
		// gang stays inside its own Job∩SubJob candidate, both inside the tier-2
		// candidate "top" the trial fell through to.
		if got := moveSummary(moves); !slices.Equal(got, []string{"uA: from n0 to n1", "uB: from n0 to n2"}) {
			t.Errorf("each role must land in its own tier-1 domain, got %v", got)
		}
		if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "top" {
			t.Errorf("committed job anchor=%q, want top (the LCA of hB and hD)", got)
		}
	})
}

// The same geometry with the Job-level constraint removed: no plugin applies at the
// Job entry, so the container yields the unconstrained candidate and each gang
// keeps its own first fit — role A takes hB instead of the two roles being gathered
// into hD. That is the unconstrained boundary's whole point: an unconstrained Job
// entry must not buy a tighter plan than today's per-gang path.
func TestDomainRelocation_UnconstrainedJobEntryKeepsPerGangChoice(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
	sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
	sjA.NetworkTopology = hardTopology(2)
	sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
	sjB.NetworkTopology = hardTopology(1)
	ji := planTestJob("ns/job", sjA, sjB)
	ji.NetworkTopology = softTopology() // the only difference from the tighter-role test
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
	}}}

	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA, uB), // hA: the drained source
		"n1": phaseBNode("n1", 4),         // hB
		"n2": phaseBNode("n2", 8),         // hD
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB},
		[]*schedapi.NodeInfo{nodes["n1"], nodes["n2"]})
	if !fit {
		t.Fatalf("both roles fit, got fit=false")
	}
	if got := moveSummary(moves); !slices.Equal(got, []string{"uA: from n0 to n1", "uB: from n0 to n2"}) {
		t.Errorf("an unconstrained Job entry must keep each gang's own first fit, got %v", got)
	}
}

// The container's candidate list is the Job-entry gradient on the anchors this pass
// intends, not the intersection of the gangs' own domains: a Job entry no plugin
// constrains yields the tree root (a domain that excludes nothing, so every gang
// falls back to its own domains — the per-gang path), and the anchors still bind (a
// fully drained job reads the cleared job anchor). A Job entry with no tree to
// search, or one that constrains every candidate away, yields no layer at all: the
// container is INFEASIBLE, not unconstrained.
func TestJobEntryDomains_JobGradientOrUnconstrained(t *testing.T) {
	subGroupJob := func(t *testing.T, topology *schedulingapi.NetworkTopologySpec) *schedapi.JobInfo {
		t.Helper()
		uA := phaseBTask("uA", "ns/job", "uA", "n0", 1)
		uB := phaseBTask("uB", "ns/job", "uB", "n0", 1)
		sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
		sjA.NetworkTopology = hardTopology(2)
		sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
		sjB.NetworkTopology = hardTopology(1)
		ji := planTestJob("ns/job", sjA, sjB)
		ji.NetworkTopology = topology
		ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
			SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
		}}}
		return ji
	}

	cases := []struct {
		name            string
		topology        *schedulingapi.NetworkTopologySpec
		register        func(*schedframework.Session)
		victimsAll      bool
		noHyperNodeTree bool
		wantInfeasible  bool
		want            []string
	}{
		{
			name:       "no gradient plugin registered",
			topology:   hardTopology(1),
			victimsAll: true,
			want:       []string{schedframework.ClusterTopHyperNode},
		},
		{
			name:       "Job entry abstains while the SubJob entry constrains",
			topology:   softTopology(),
			register:   registerSubGroupAffinityGradient,
			victimsAll: true,
			want:       []string{schedframework.ClusterTopHyperNode},
		},
		{
			name:       "hard Job-level topology, fully drained",
			topology:   hardTopology(1),
			register:   registerHardTierGradient,
			victimsAll: true,
			want:       []string{"hA", "hB", "hD"},
		},
		{
			name:     "partly drained job keeps its anchor",
			topology: hardTopology(1),
			register: registerHardTierGradient,
			want:     []string{"hA"},
		},
		{
			// A registered plugin reads the tree, so a nil tree root must be
			// rejected before the gradient is asked — not left to the plugin.
			name:            "no HyperNode tree to search",
			topology:        hardTopology(1),
			register:        registerHardTierGradient,
			victimsAll:      true,
			noHyperNodeTree: true,
			wantInfeasible:  true,
		},
		{
			name:           "the Job entry constrains every candidate away",
			topology:       softTopology(),
			register:       registerEmptyJobGradient,
			victimsAll:     true,
			wantInfeasible: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ji := subGroupJob(t, tc.topology)
			nodes := map[string]*schedapi.NodeInfo{
				"n0": phaseBNode("n0", 8, ji.Tasks["uA"], ji.Tasks["uB"]),
				"n1": phaseBNode("n1", 8),
				"n2": phaseBNode("n2", 8),
			}
			victims := []*schedapi.TaskInfo{ji.Tasks["uA"]}
			if tc.victimsAll {
				victims = append(victims, ji.Tasks["uB"])
			}
			ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, tc.register)
			if tc.noHyperNodeTree {
				ssn.HyperNodes = schedapi.HyperNodeInfoMap{}
			}
			snap := NewSessionSnapshot(ssn, gpu, nil)
			jg := &jobGangs{job: ji, gangs: []gang{{victims: victims}}}

			layers, ok := snap.jobEntryDomains(jg, newTrialScope(victims))
			if tc.wantInfeasible {
				if ok || layers != nil {
					t.Fatalf("no candidate layer must be INFEASIBLE, got ok=%t layers=%v", ok, layers)
				}
				return
			}
			if !ok {
				t.Fatal("the Job entry must yield candidates")
			}
			var names []string
			for _, layer := range layers {
				for _, hyperNode := range layer {
					names = append(names, hyperNode.Name)
				}
			}
			sort.Strings(names)
			if !slices.Equal(names, tc.want) {
				t.Errorf("Job-entry candidates=%v, want %v", names, tc.want)
			}
		})
	}
}

// jobFullyVacated asks a job-level question of the call's victim set: with more
// than one gang, each gang only sees its own pods, so the sibling gangs are what tell
// whether the job is leaving whole.
func TestJobFullyVacated_CallScopeMembership(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 1)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 1)
	sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
	sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
	ji := planTestJob("ns/job", sjA, sjB)

	// Execute-side reconcile moves live Pending replacements, so a job can hold
	// victims without holding a single allocated pod.
	pending := phaseBTask("uP", "ns/jobP", "uP", "n0", 1)
	pending.Status = schedapi.Pending
	sjP := phaseBSubJob("ns/jobP/role", "ns/jobP", "role", pending)
	jiPending := planTestJob("ns/jobP", sjP)

	snap := &SessionSnapshot{}
	tests := []struct {
		name    string
		job     *schedapi.JobInfo
		victims []*schedapi.TaskInfo
		want    bool
	}{
		{"every gang of the job is a victim", ji, []*schedapi.TaskInfo{uA, uB}, true},
		{"a sibling gang keeps a pod", ji, []*schedapi.TaskInfo{uA}, false},
		{"the job holds no allocated pod", jiPending, []*schedapi.TaskInfo{pending}, false},
		{"container without a Job", nil, []*schedapi.TaskInfo{uA}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := snap.jobFullyVacated(newTrialScope(tt.victims), tt.job); got != tt.want {
				t.Errorf("jobFullyVacated = %t, want %t", got, tt.want)
			}
		})
	}
}

// The plugin builds each tier layer by walking the map-backed Children, so the
// layer order differs from call to call; the first-fit trial must not inherit it.
func TestAllowedDomains_SortsDomainsWithinTier(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 1)
	sj := phaseBSubJob("ns/job/role", "ns/job", "role", uA)
	ji := planTestJob("ns/job", sj)
	ji.NetworkTopology = hardTopology(1)
	sj.NetworkTopology = hardTopology(1)

	// One tier layer, deliberately reversed: hD, hB, hA.
	reversed := func(ssn *schedframework.Session) schedapi.HyperNodeGradientResult {
		return schedapi.HyperNodeGradientConstrain([][]*schedapi.HyperNodeInfo{
			{ssn.HyperNodes["hD"], ssn.HyperNodes["hB"], ssn.HyperNodes["hA"]},
		})
	}
	register := func(ssn *schedframework.Session) {
		ssn.AddHyperNodeGradientForJobFn("reversed-stub",
			func(*schedapi.JobInfo, *schedapi.HyperNodeInfo, schedapi.SearchPurpose) schedapi.HyperNodeGradientResult {
				return reversed(ssn)
			})
		ssn.AddHyperNodeGradientForSubJobFn("reversed-stub",
			func(*schedapi.SubJobInfo, *schedapi.HyperNodeInfo, schedapi.SearchPurpose) schedapi.HyperNodeGradientResult {
				return reversed(ssn)
			})
		ssn.Tiers = []schedconf.Tier{{Plugins: []schedconf.PluginOption{
			{Name: "reversed-stub", EnabledHyperNodeGradient: ptr.To(true)},
		}}}
	}

	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 8),
		"n2": phaseBNode("n2", 8),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, register)
	snap := NewSessionSnapshot(ssn, gpu, nil)
	jg := &jobGangs{job: ji}
	g := &gang{subJob: sj, victims: []*schedapi.TaskInfo{uA}}

	layers, ok := snap.allowedDomains(jg, g)
	if !ok {
		t.Fatalf("gang must have allowed domains")
	}
	var got []string
	for _, layer := range layers {
		for _, hyperNode := range layer {
			got = append(got, hyperNode.Name)
		}
	}
	if want := []string{"hA", "hB", "hD"}; !slices.Equal(got, want) {
		t.Errorf("allowed domains=%v, want %v (each tier name-sorted)", got, want)
	}
}

// allowedDomainNames returns the gang's allowed HyperNode names, sorted.
func allowedDomainNames(t *testing.T, snap *SessionSnapshot, job *schedapi.JobInfo, subJob *schedapi.SubJobInfo, victims ...*schedapi.TaskInfo) []string {
	t.Helper()
	layers, ok := snap.allowedDomainsForTrial(&jobGangs{job: job}, &gang{subJob: subJob, victims: victims}, newTrialScope(victims))
	if !ok {
		t.Fatalf("gang must have allowed domains")
	}
	names := sets.New[string]()
	for _, layer := range layers {
		for _, hyperNode := range layer {
			names.Insert(hyperNode.Name)
		}
	}
	values := names.UnsortedList()
	sort.Strings(values)
	return values
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

// layerCandidateDomainSets decides what one candidate layer yields: without a
// candidate forest each domain is its own attempt, in layer order; with one, the
// whole layer is a single attempt over its deduped roots, and a layer whose roots
// the tree does not join is skipped (allocate's guard).
func TestLayerCandidateDomainSets(t *testing.T) {
	hyperNodes, _ := planTestHyperNodes()
	render := func(attempts [][]*schedapi.HyperNodeInfo) []string {
		out := make([]string, 0, len(attempts))
		for _, attempt := range attempts {
			out = append(out, domainsByTier([][]*schedapi.HyperNodeInfo{attempt}))
		}
		return out
	}

	if got := layerCandidateDomainSets(nil, false, hyperNodes); len(got) != 0 {
		t.Errorf("an empty layer must yield no attempt, got %v", got)
	}
	if got := layerCandidateDomainSets(nil, true, hyperNodes); len(got) != 0 {
		t.Errorf("an empty forest layer must yield no attempt, got %v", got)
	}
	// Without a forest a nil domain is no attempt at all; with one it is dropped
	// by the name dedup.
	if got := render(layerCandidateDomainSets([]*schedapi.HyperNodeInfo{hyperNodes["hD"], nil, hyperNodes["hA"]}, false, hyperNodes)); !slices.Equal(got, []string{"tier-1: hD", "tier-1: hA"}) {
		t.Errorf("each domain must be its own attempt in layer order, got %v", got)
	}
	if got := render(layerCandidateDomainSets([]*schedapi.HyperNodeInfo{hyperNodes["hD"], hyperNodes["hA"], hyperNodes["hA"], nil}, true, hyperNodes)); !slices.Equal(got, []string{"tier-1: hA, hD"}) {
		t.Errorf("a forest must read the layer as one deduped set, got %v", got)
	}
	if got := render(layerCandidateDomainSets([]*schedapi.HyperNodeInfo{hyperNodes["hA"]}, true, hyperNodes)); !slices.Equal(got, []string{"tier-1: hA"}) {
		t.Errorf("a single-root forest is that one domain, got %v", got)
	}

	// Roots with no common ancestor have no envelope: allocate skips the layer and
	// so does the container.
	disjoint := schedapi.HyperNodeInfoMap{
		"x": schedapi.NewHyperNodeInfo(&topologyv1alpha1.HyperNode{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Spec: topologyv1alpha1.HyperNodeSpec{Tier: 1}}, schedapi.TierOpt(1)),
		"y": schedapi.NewHyperNodeInfo(&topologyv1alpha1.HyperNode{ObjectMeta: metav1.ObjectMeta{Name: "y"}, Spec: topologyv1alpha1.HyperNodeSpec{Tier: 1}}, schedapi.TierOpt(1)),
	}
	if got := layerCandidateDomainSets([]*schedapi.HyperNodeInfo{disjoint["x"], disjoint["y"]}, true, disjoint); got != nil {
		t.Errorf("roots with no common ancestor must skip the layer, got %v", render(got))
	}
}

// Serial gang pinning co-locates Required-affinity subJobs (each member its own
// gang, in victim order): A pins to unmoved peer B's hB and lands on hB's empty
// n3; the commit re-anchors A to hB, so B follows onto n3. Both stay in one
// HyperNode domain.
func TestDomainRelocation_SubJobAffinitySerialCoLocation(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 4)
	sjA := phaseBSubJob("ns/job/policyA", "ns/job", "policyA", uA)
	sjB := phaseBSubJob("ns/job/policyB", "ns/job", "policyB", uB)
	ji := phaseBSubGroupAffinityJob("ns/job", 1, [][]string{{"policyA", "policyB"}}, sjA, sjB)

	// n3 is an empty cap-8 receiver under hB (the peer's domain), not n1, or the
	// first gang could land beside the peer and pin it in place.
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
			// SubJob gang, partial evac: residual allocated peer is not a victim.
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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (&jobGangs{job: tc.job}).gangFullyVacated(&gang{subJob: tc.subJob, victims: tc.victims})
			if got != tc.want {
				t.Errorf("gangFullyVacated()=%t, want %t", got, tc.want)
			}
		})
	}
}

// Victims of Jobs the session does not carry share one container with job == nil
// and stay on the greedy path: the container trial reads the Job-entry gradient, so
// the grouping must not assume a Job is there.
func TestDomainRelocation_VictimsOfJobsOutsideTheSessionStayGreedy(t *testing.T) {
	uA := phaseBTask("uA", "ns/absent", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/absent2", "uB", "n0", 4)
	victims := []*schedapi.TaskInfo{uA, uB}

	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA, uB),
		"n1": phaseBNode("n1", 8),
	}
	ssn := phaseBSession(t, nodes, nil, registerHardTierGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	jgs := snap.groupVictimsByJobGangs(victims)
	if len(jgs) != 1 || jgs[0].job != nil || len(jgs[0].gangs) != 1 {
		t.Fatalf("victims without a session Job must share one job==nil gang, got %d containers", len(jgs))
	}
	if jgs[0].requiresHyperNodeAllocate(snap) {
		t.Error("a job==nil container cannot hit RequiresHyperNodeAllocate")
	}

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, victims, []*schedapi.NodeInfo{nodes["n1"]})
	if !fit {
		t.Fatalf("the greedy path must place both victims, got fit=false")
	}
	if got := moveSummary(moves); !slices.Equal(got, []string{"uA: from n0 to n1", "uB: from n0 to n1"}) {
		t.Errorf("greedy placements=%v", got)
	}
}

// Two calls of one pass share one snapshot and the second gets the first one's
// moves as `committed`, whose pods count as present: it must not reuse the receiver
// they occupy. The refusal is a planning matter, not residue — the failed call rolls
// back to its own baseline, which already holds the first call's commit.
func TestDomainRelocation_SecondCallDoesNotReuseTheCommittedPlacement(t *testing.T) {
	build := func(t *testing.T) (*SessionSnapshot, *schedapi.JobInfo, map[string]*schedapi.NodeInfo) {
		t.Helper()
		uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
		uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
		ji := phaseBJob("ns/job", hardTopology(1), uA, uB)

		hyperNodes, rns := planTestHyperNodes()
		rns["hA"].Insert("n3")
		rns["top"].Insert("n3")
		rns[schedframework.ClusterTopHyperNode].Insert("n3")
		nodes := map[string]*schedapi.NodeInfo{
			"n0": phaseBNode("n0", 8, uA, uB), // hA: the drained source
			"n3": phaseBNode("n3", 4),         // hA: room for one task
			"n2": phaseBNode("n2", 4),         // hD
		}
		schedapi.SyncJobAllocatedHyperNode(ji, hyperNodes, rns)
		ssn := newPhaseBSSN(t)
		ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}
		ssn.Nodes = nodes
		ssn.HyperNodes = hyperNodes
		ssn.RealNodesSet = rns
		registerHardTierGradient(ssn)
		return NewSessionSnapshot(ssn, gpu, nil), ji, nodes
	}

	snap, ji, nodes := build(t)
	receivers := []*schedapi.NodeInfo{nodes["n3"], nodes["n2"]}

	// Call 1 drains uA: the Job keeps uB, so the anchor stays hA and n3 is the only
	// in-domain receiver.
	first, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{ji.Tasks["uA"]}, receivers)
	if !fit || len(first) != 1 || first[0].To != "n3" {
		t.Fatalf("the first call must place uA on n3, got fit=%t moves=%v", fit, first)
	}

	// Call 2 drains uB with that move committed: hA's only node is taken, so uB has
	// no in-domain receiver and the call is refused.
	second, fit := snap.FeasibleRelocation(context.Background(), first, []*schedapi.TaskInfo{ji.Tasks["uB"]}, receivers)
	if fit || second != nil {
		t.Fatalf("the committed placement occupies n3, got fit=%t moves=%v", fit, second)
	}
	if got := snap.ssn.Jobs[ji.UID].Tasks["uA"].NodeName; got != "n3" {
		t.Errorf("the failed second call must keep the first call's commit, uA node=%q", got)
	}

	// Control: without the committed move the same call fits uB on n3 — the refusal
	// above is the committed pod's occupancy, not the geometry.
	control, controlJob, controlNodes := build(t)
	third, fit := control.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{controlJob.Tasks["uB"]},
		[]*schedapi.NodeInfo{controlNodes["n3"], controlNodes["n2"]})
	if !fit || len(third) != 1 || third[0].To != "n3" {
		t.Fatalf("without the committed move uB fits n3, got fit=%t moves=%v", fit, third)
	}
}

// A Job whose pods sit on two nodes is evacuated by two drain steps, and no single
// step sees the whole Job: the other gang's pod stays allocated, so jobFullyVacated
// is false and the Job anchor is never cleared — each step is bound to the
// pre-eviction domain hA, which holds only the two drain units, and hD (room for both
// roles) is never tried. Registered as a known gap in §7.2.
func TestDomainRelocation_MultiStepEvacuationStaysInAnchoredDomain(t *testing.T) {
	build := func(t *testing.T) (*SessionSnapshot, *schedapi.JobInfo, map[string]*schedapi.NodeInfo) {
		t.Helper()
		uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
		uB := phaseBTask("uB", "ns/job", "uB", "n3", 4)
		sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
		sjA.NetworkTopology = hardTopology(1)
		sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
		sjB.NetworkTopology = hardTopology(1)
		ji := planTestJob("ns/job", sjA, sjB)
		ji.NetworkTopology = hardTopology(1)
		ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
			SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}},
		}}}

		hyperNodes, rns := planTestHyperNodes()
		rns["hA"].Insert("n3")
		rns["top"].Insert("n3")
		rns[schedframework.ClusterTopHyperNode].Insert("n3")
		nodes := map[string]*schedapi.NodeInfo{
			"n0": phaseBNode("n0", 4, uA), // hA: the first step's drain unit
			"n3": phaseBNode("n3", 4, uB), // hA: the second step's drain unit
			"n1": phaseBNode("n1", 4),     // hB: room for one role
			"n2": phaseBNode("n2", 8),     // hD: room for both
		}
		schedapi.SyncJobAllocatedHyperNode(ji, hyperNodes, rns)
		ssn := newPhaseBSSN(t)
		ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}
		ssn.Nodes = nodes
		ssn.HyperNodes = hyperNodes
		ssn.RealNodesSet = rns
		registerHardTierGradient(ssn)
		return NewSessionSnapshot(ssn, gpu, nil), ji, nodes
	}

	snap, ji, nodes := build(t)
	if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hA" {
		t.Fatalf("initial job anchor=%q, want hA (both roles sit in hA)", got)
	}
	receivers := []*schedapi.NodeInfo{nodes["n1"], nodes["n2"]}

	for _, step := range []struct {
		name   string
		victim *schedapi.TaskInfo
	}{
		{"step 1 drains n0 (roleA)", ji.Tasks["uA"]},
		{"step 2 drains n3 (roleB)", ji.Tasks["uB"]},
	} {
		moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{step.victim}, receivers)
		if fit || moves != nil {
			t.Errorf("%s: the anchored domain has no receiver, want fit=false, got fit=%t moves=%v", step.name, fit, moves)
		}
	}
	if got := snap.ssn.Jobs[ji.UID].Tasks["uA"].NodeName; got != "n0" {
		t.Errorf("a refused step must leave no residue, uA node=%q", got)
	}

	// The boundary the container does cover: one call evacuating the whole Job clears
	// the anchor and lands both roles in hD. Not planner-reachable — §2.4 drains one
	// node unit per step, which is what splits the Job in the first place.
	oneCall, oneCallJob, oneCallNodes := build(t)
	moves, fit := oneCall.FeasibleRelocation(context.Background(), nil,
		[]*schedapi.TaskInfo{oneCallJob.Tasks["uA"], oneCallJob.Tasks["uB"]},
		[]*schedapi.NodeInfo{oneCallNodes["n1"], oneCallNodes["n2"]})
	if !fit {
		t.Fatalf("a whole-job evacuation fits hD, got fit=false")
	}
	if got := moveSummary(moves); !slices.Equal(got, []string{"uA: from n0 to n2", "uB: from n3 to n2"}) {
		t.Errorf("both roles must land in hD, got %v", got)
	}
}

// Three gangs in one container: only hD holds all three, and it is reached after the
// two earlier domain sets fail — those sets must leave no residue behind.
func TestDomainRelocation_ThreeRolesShareTheDomainHoldingAll(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n0", 4)
	uC := phaseBTask("uC", "ns/job", "uC", "n0", 4)
	sjA := phaseBSubJob("ns/job/roleA", "ns/job", "roleA", uA)
	sjA.NetworkTopology = hardTopology(1)
	sjB := phaseBSubJob("ns/job/roleB", "ns/job", "roleB", uB)
	sjB.NetworkTopology = hardTopology(1)
	sjC := phaseBSubJob("ns/job/roleC", "ns/job", "roleC", uC)
	sjC.NetworkTopology = hardTopology(1)
	ji := planTestJob("ns/job", sjA, sjB, sjC)
	ji.NetworkTopology = hardTopology(1)
	ji.PodGroup = &schedapi.PodGroup{PodGroup: schedulingapi.PodGroup{Spec: schedulingapi.PodGroupSpec{
		SubGroupPolicy: []schedulingapi.SubGroupPolicySpec{{Name: "roleA"}, {Name: "roleB"}, {Name: "roleC"}},
	}}}

	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 12, uA, uB, uC), // hA: the drained source
		"n1": phaseBNode("n1", 4),              // hB: room for one role
		"n2": phaseBNode("n2", 12),             // hD: room for all three
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA, uB, uC},
		[]*schedapi.NodeInfo{nodes["n1"], nodes["n2"]})
	if !fit {
		t.Fatalf("hD holds all three roles, got fit=false")
	}
	if got := moveSummary(moves); !slices.Equal(got, []string{"uA: from n0 to n2", "uB: from n0 to n2", "uC: from n0 to n2"}) {
		t.Errorf("all three roles must land in hD, got %v", got)
	}
	if got := snap.planState().JobAllocatedHyperNode(ji.UID); got != "hD" {
		t.Errorf("committed job anchor=%q, want hD", got)
	}
}
