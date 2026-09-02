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

// Phase B acceptance tests (US-02 §5.2): the dual-mode FeasibleRelocation.
//
// The gradient functions here are faithful stubs of the scheduler's HyperNode
// constraint plugins: hard mode narrows to the gang's allowed ancestor subtree
// via the real scheduler GetSearchRoot — reading the plan-state
// AllocatedHyperNode the carrier rewrites, so the H1 anchor clear drives the
// no-anchor branch — and soft mode abstains. Resource pre-filtering and the
// anti-affinity term resolution stay in the plugin packages; what is under
// test here is the adapter's constraint consumption: whole-gang domain trial,
// two-entry intersection, plan-state commit and symmetric rollback.

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

// phaseBTask builds an allocated task with both Resreq and InitResreq set (the
// scheduler node AddTask/Clone paths deref Resreq) and a real Pod (node task
// maps are keyed by PodKey). g is in whole GPUs; the request is stored in the
// milli-unit ledger NewResource uses for scalar resources (1 device = 1000), so
// task requests and the node capacity that phaseBNode derives from
// Node.Status.Allocatable are on the same scale.
func phaseBTask(uid, jobID, name, nodeName string, g int64) *schedapi.TaskInfo {
	rr := gpuRes(g * 1000)
	return &schedapi.TaskInfo{
		UID: schedapi.TaskID(uid), Job: schedapi.JobID(jobID), Name: name,
		Pod:                &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}},
		TransactionContext: schedapi.TransactionContext{NodeName: nodeName, Status: schedapi.Running},
		Resreq:             rr, InitResreq: rr.Clone(),
	}
}

// phaseBNode builds a node whose Node object carries the GPU capacity, so
// NodeInfo.Clone (which rebuilds via NewNodeInfo from Node.Status.Allocatable)
// keeps the same capacity — the SessionSnapshot fit checks run on node clones.
func phaseBNode(name string, capGPU int64, tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{
			Capacity:    v1.ResourceList{gpu: *resource.NewQuantity(capGPU, resource.DecimalSI)},
			Allocatable: v1.ResourceList{gpu: *resource.NewQuantity(capGPU, resource.DecimalSI)},
		},
	}
	ni := schedapi.NewNodeInfo(node)
	for _, tk := range tasks {
		if err := ni.AddTask(tk); err != nil {
			panic(err)
		}
	}
	return ni
}

// phaseBJob builds a job (no SubGroupPolicy) with the given tasks. topology nil
// means the job has no HyperNode constraint (RequiresHyperNodeAllocate == false
// when it also has no SubGroupPolicy).
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

// phaseBSubJob builds a SubJob of a SubGroupPolicy job. GID follows the policy
// GID format getSubJobGID produces — jobID + "/" + policy — so
// SubJobPolicyName returns exactly the policy name (NOT the legacy uid+"/g"
// pattern, which would resolve to a bogus policy string).
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

// phaseBSubGroupAffinityJob builds a SubGroupPolicy job carrying the given
// Required SubGroup affinity terms — each term is a policy list, all resolved at
// the same topology tier — over the given subJobs. Policies are derived from the
// subJobs' GIDs so the PodGroup's SubGroupPolicy list matches the link edges.
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

// phaseBSession assembles a scheduler Session over the reference tree with the
// given jobs and nodes, syncs gang anchors, then runs register to install the
// gradient stubs under test.
func phaseBSession(t *testing.T, nodes map[string]*schedapi.NodeInfo, jobs map[schedapi.JobID]*schedapi.JobInfo, register func(*schedframework.Session)) *schedframework.Session {
	hn, rns := planTestHyperNodes()
	for _, ji := range jobs {
		schedapi.SyncJobAllocatedHyperNode(ji, hn, rns)
	}
	ssn := newPhaseBSSN(t)
	ssn.Jobs = jobs
	ssn.Nodes = nodes
	ssn.HyperNodes = hn
	ssn.RealNodesSet = rns
	if register != nil {
		register(ssn)
	}
	return ssn
}

// newPhaseBSSN opens a real scheduler Session over an empty cluster view. The
// bare cache (no informers, no clients) is enough for openSession to initialize
// every fn map — AddHyperNodeGradientForJobFn/ForSubJobFn panic on the nil maps
// of a hand-built Session — and to add the virtual ClusterTopHyperNode root
// (later overwritten by the caller's own tree). CloseSessionReadOnly skips the
// cluster writes the real close performs.
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

// registerHardTierGradient installs the hard-tier gradient stub, enabled for
// the Session's HyperNodeGradient aggregation.
func registerHardTierGradient(ssn *schedframework.Session) {
	jobFn, subJobFn := hardTierGradientStub(ssn.HyperNodes)
	ssn.AddHyperNodeGradientForJobFn(gradientStubName, jobFn)
	ssn.AddHyperNodeGradientForSubJobFn(gradientStubName, subJobFn)
	ssn.Tiers = []schedconf.Tier{{Plugins: []schedconf.PluginOption{
		{Name: gradientStubName, EnabledHyperNodeGradient: ptr.To(true)},
	}}}
}

// registerHardTierAndAntiAffinityGradient installs the hard-tier stub plus an
// anti-affinity stub that drops excluded domains (and their ancestors) from the
// candidate forest, emulating group-topology-affinity's Required PodGroup
// anti-affinity. Both are enabled so the Session intersects them.
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

// hardTierGradientStub mimics the network-topology-aware plugin's hard-mode
// gradient: soft mode abstains; hard mode narrows to the gang's highest-allowed
// ancestor subtree via the scheduler's real GetSearchRoot, reading the
// plan-state AllocatedHyperNode (the carrier rewrites it; H1 clears it).
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

// subtreeLayers mirrors the plugin's hyperNodeGradientFn layer construction:
// BFS from the search root, keep HyperNodes at or below the highest allowed
// tier, group into tier-ascending layers.
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
			if hni, ok := hyperNodes[child]; ok {
				queue = append(queue, hni)
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
			for _, hn := range layer {
				if excludedAll.Has(hn.Name) {
					continue
				}
				row = append(row, hn)
			}
			if len(row) > 0 {
				kept = append(kept, row)
			}
		}
		return schedapi.HyperNodeGradientConstrain(kept)
	}
}

// registerSubGroupAffinityGradient installs the Required SubGroup affinity
// gradient stub, enabled for the Session's HyperNodeGradient aggregation. It is
// the test-double counterpart of group-topology-affinity's hard SubGroup
// filtering: the Job entry abstains (a pure SubGroup-affinity job has no hard
// PodGroup terms) and the SubJob entry narrows candidates to the
// Required-affinity peer domains.
func registerSubGroupAffinityGradient(ssn *schedframework.Session) {
	jobFn, subJobFn := subGroupAffinityGradientStub(ssn)
	ssn.AddHyperNodeGradientForJobFn(subgroupAffinityStubName, jobFn)
	ssn.AddHyperNodeGradientForSubJobFn(subgroupAffinityStubName, subJobFn)
	ssn.Tiers = []schedconf.Tier{{Plugins: []schedconf.PluginOption{
		{Name: subgroupAffinityStubName, EnabledHyperNodeGradient: ptr.To(true)},
	}}}
}

// subGroupAffinityGradientStub mirrors the group-topology-affinity plugin's hard
// SubGroup filter (isEligibleForSubGroupHardTerms): the Job entry abstains; the
// SubJob entry keeps only candidates whose ancestor at each Required affinity
// term tier equals the single occupied domain of the term's peers (empty peers
// impose no constraint) and whose ancestor is outside the Required anti-affinity
// peers' domains. Peer occupancy reads the plan-state task bindings the carrier
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
			for _, hn := range layer {
				if subJobEligibleForHardTerms(ssn, job, subJob, hn) {
					row = append(row, hn)
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

// subJobEligibleForHardTerms is the test-double equivalent of the plugin's
// isEligibleForSubGroupHardTerms (affinity and anti-affinity branches), using the
// adapter's own term-inclusion predicate.
func subJobEligibleForHardTerms(ssn *schedframework.Session, job *schedapi.JobInfo, subJob *schedapi.SubJobInfo, hn *schedapi.HyperNodeInfo) bool {
	for _, term := range job.RequiredSubGroupAffinityTerms() {
		if !subGroupTermIncludesLocal(term, schedapi.SubJobPolicyName(subJob)) {
			continue
		}
		tier, err := schedapi.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return false
		}
		ancestor := ssn.HyperNodes.GetAncestorHyperNode(hn.Name, tier)
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
		ancestor := ssn.HyperNodes.GetAncestorHyperNode(hn.Name, tier)
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

// subGroupTermPeers is the test-double equivalent of the plugin's
// peerSubJobOccupiedHyperNodesAtTier: the term-tier domains occupied by the
// term's peer subJobs, read from plan-state task bindings. antiAffinity mirrors
// the plugin's subGroupPeerMatchesTerm semantics for single-policy terms.
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
		for hn := range schedapi.CollectSubJobOccupiedHyperNodesAtTier(peer, ssn.HyperNodes, tier, ssn.RealNodesSet) {
			occupied.Insert(hn)
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

// subGroupTermIncludesLocal is the test-local copy of the adapter's term
// membership predicate, kept for the affinity gradient stub since the adapter
// no longer exports it.
func subGroupTermIncludesLocal(term schedulingapi.SubGroupAffinityTerm, policy string) bool {
	for _, sg := range term.SubGroups {
		if sg == policy {
			return true
		}
	}
	return false
}

// R19: a gang with no hard HyperNode requirement keeps the legacy per-victim
// greedy cross-domain placement — first fit in receiver order — and the plan
// state is never touched.
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
	// greedy first fit in receiver order: n2 holds both (4+4 on 8 GPUs).
	if len(moves) != 2 || moves[0].To != "n2" || moves[1].To != "n2" {
		t.Errorf("greedy cross-domain first-fit expected both on n2, got %+v", moves)
	}
	// plan state untouched (R19: ==false never enters the plan-state carrier).
	// The setup-time SyncJobAllocatedHyperNode anchored the job at top (LCA of
	// hA/hB); the greedy path must not move it.
	if got := ji.AllocatedHyperNode; got != "top" {
		t.Errorf("==false job AllocatedHyperNode=%q, want top (unchanged by greedy)", got)
	}
	if u0.NodeName != "n0" || u1.NodeName != "n1" {
		t.Errorf("==false must not rewrite job-side tasks, u0=%q u1=%q", u0.NodeName, u1.NodeName)
	}
}

// R19: a hard-requirement gang with NO HyperNode topology in the session falls
// back to the legacy greedy path — the constraint stack is inert without a tree.
func TestDomainRelocation_GreedyFallbackWithoutTree(t *testing.T) {
	u0 := phaseBTask("u0", "ns/job", "u0", "n0", 4)
	ji := phaseBJob("ns/job", hardTopology(1), u0) // hard, but no tree present
	n0 := phaseBNode("n0", 8, u0)
	n1 := phaseBNode("n1", 8)
	ssn := newPhaseBSSN(t)
	ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}
	ssn.Nodes = map[string]*schedapi.NodeInfo{"n0": n0, "n1": n1}
	// A session whose HyperNode tree is not ready keeps the constraint stack
	// inert (hasHyperNodeTopology() == false), so the hard gang falls back to
	// greedy. Ready defaults to true in NewHyperNodesInfo, hence the explicit off.
	ssn.HyperNodesReadyToSchedule = false
	snap := NewSessionSnapshot(ssn, gpu, nil)

	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{u0},
		[]*schedapi.NodeInfo{n1})
	if !fit || len(moves) != 1 || moves[0].To != "n1" {
		t.Fatalf("no-tree hard gang must fall back to greedy, got fit=%t moves=%v", fit, moves)
	}
}

// R20 + R18 + H1: a fully-vacated hard-tier-1 gang (both tasks are victims)
// clears its gang anchor for the no-anchor evaluation, and the whole gang is
// placed by domain trial within a single allowed tier-1 domain. With n2 the
// only receiver the winning domain is hD; the plan-state commit recomputes the
// gang anchor to hD while the node side stays untouched.
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

	// The gang's anchor is top (tier 2 > allowed 1); a successful cross-domain
	// move is only possible because H1 cleared the anchor for the gradient
	// evaluation of the fully-vacated gang (otherwise GetSearchRoot errors and
	// the unit is infeasible).
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

// R20 negative: a hard-tier-1 gang that is NOT fully vacated keeps its anchor
// (top, tier 2, beyond the allowed subtree), so no domain is feasible and the
// unit is rejected with no residue.
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

	// only uA is a victim; uB stays, so the gang is NOT fully vacated and the
	// anchor stays "top" -> the hard-tier-1 gradient has no feasible search root.
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

// R21: soft topology abstains, so the allowed domains are the whole candidate
// universe — tier is not restricted — and a soft gang relocates freely.
func TestDomainRelocation_SoftModeUnrestricted(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	ji := phaseBJob("ns/job", softTopology(), uA)
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 8),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerHardTierGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	// white-box: the soft gang's allowed domains are the full universe, tier-2
	// root included — no tier restriction (abstain == no constraint).
	layers, ok := snap.allowedDomains(&gangUnit{job: ji, victims: []*schedapi.TaskInfo{uA}})
	if !ok {
		t.Fatal("soft-mode gang must have allowed domains (the universe)")
	}
	names := sets.New[string]()
	for _, layer := range layers {
		for _, hn := range layer {
			names.Insert(hn.Name)
		}
	}
	for _, want := range []string{"hA", "hB", "hD", "top"} {
		if !names.Has(want) {
			t.Errorf("soft-mode universe missing %q, got %v", want, names.UnsortedList())
		}
	}

	// end-to-end: relocation succeeds across domains (hA -> hB), which hard mode
	// would have restricted.
	moves, fit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA},
		[]*schedapi.NodeInfo{nodes["n1"]})
	if !fit || len(moves) != 1 || moves[0].To != "n1" {
		t.Fatalf("soft gang must relocate into hB freely, got fit=%t moves=%v", fit, moves)
	}
}

// R25: a Required PodGroup anti-affinity that excludes the matched gang's
// occupied domain (hB) keeps the domain-trial off hB's nodes; when the only
// viable placement is excluded, the unit is infeasible.
func TestDomainRelocation_RequiredAntiAffinityExcludesDomain(t *testing.T) {
	// Peer gang occupies hB -> exclude hB (and ancestors) from the candidate
	// forest. n0 is the drained source; only n1 (hB, excluded) and n2 (hD) remain.
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

	// Now the peer also occupies hD: every viable receiver domain is excluded,
	// so the unit is infeasible and no move is produced.
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

// Mixed-gang atomicity: a ==true gang that commits into hD is followed by a
// ==false victim that cannot fit; the whole call fails and the ==true gang's
// plan-state commit is rolled back to the pre-call baseline.
func TestDomainRelocation_MixedGangAtomicity(t *testing.T) {
	uA := phaseBTask("uA", "ns/true", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/true", "uB", "n1", 4)
	jt := phaseBJob("ns/true", hardTopology(1), uA, uB) // ==true
	uC := phaseBTask("uC", "ns/false", "uC", "n3", 8)
	jf := phaseBJob("ns/false", nil, uC) // ==false

	hn, rns := planTestHyperNodes()
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
		schedapi.SyncJobAllocatedHyperNode(ji, hn, rns)
	}
	ssn := newPhaseBSSN(t)
	ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/true": jt, "ns/false": jf}
	ssn.Nodes = nodes
	ssn.HyperNodes = hn
	ssn.RealNodesSet = rns
	registerHardTierGradient(ssn)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	// uC needs 8 GPUs but the only receiver n2 is already filled by the ==true
	// gang's two 4-GPU victims -> greedy fails after the ==true commit.
	moves, fit := snap.FeasibleRelocation(context.Background(), nil,
		[]*schedapi.TaskInfo{uA, uB, uC}, []*schedapi.NodeInfo{nodes["n2"]})
	if fit || moves != nil {
		t.Fatalf("greedy failure must fail the whole call, got fit=%t moves=%v", fit, moves)
	}
	// ==true gang's plan-state commit rolled back to the baseline.
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

// SubJob unit: the two-entry gradient narrowing — Job-entry gradient
// intersected with SubJob-entry gradient (root-or-ancestor) — keeps the whole
// subJob's relocation inside one allowed tier-1 domain, committing the subJob
// and job anchors together.
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
// under the outer (Job-entry) forest — root-or-ancestor — and is empty (no
// common domain) when the forests do not overlap.
func TestIntersectGradientForest(t *testing.T) {
	hn, _ := planTestHyperNodes()
	inner := [][]*schedapi.HyperNodeInfo{{hn["hB"], hn["hD"], hn["top"]}}

	// roots {hA,hB}: hB kept (self), hD and top dropped (not under hA/hB).
	got, ok := intersectGradientForest([][]*schedapi.HyperNodeInfo{{hn["hA"], hn["hB"]}}, inner, hn)
	if !ok || len(got) != 1 || len(got[0]) != 1 || got[0][0].Name != "hB" {
		t.Errorf("intersection=%v ok=%t, want [[hB]] true", got, ok)
	}

	// empty outer forest: infeasible, no common domain.
	if got, ok := intersectGradientForest(nil, inner, hn); ok || got != nil {
		t.Errorf("empty outer must be infeasible, got %v ok=%t", got, ok)
	}

	// outer spanning all tier-1 domains keeps hB and hD, drops top.
	got2, ok := intersectGradientForest([][]*schedapi.HyperNodeInfo{{hn["hA"], hn["hB"], hn["hD"]}}, inner, hn)
	if !ok || len(got2) != 1 || len(got2[0]) != 2 {
		t.Fatalf("full outer must keep two domains, got %v ok=%t", got2, ok)
	}
	names := sets.New[string]()
	for _, hni := range got2[0] {
		names.Insert(hni.Name)
	}
	if !names.HasAll("hB", "hD") {
		t.Errorf("kept domains=%v, want {hB,hD}", names.UnsortedList())
	}
}

// Serial unit pinning co-locates Required-affinity-linked subJobs: each member
// is a separate gang unit, tried in victim order. A is pinned to the
// not-yet-moved peer B's current domain hB and lands on hB's empty receiver n3;
// the commit re-anchors A to hB. B is then pinned to A's settled domain and
// follows onto n3 — both land within one HyperNode domain.
func TestDomainRelocation_SubJobAffinitySerialCoLocation(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 4)
	sjA := phaseBSubJob("ns/job/policyA", "ns/job", "policyA", uA)
	sjB := phaseBSubJob("ns/job/policyB", "ns/job", "policyB", uB)
	ji := phaseBSubGroupAffinityJob("ns/job", 1, [][]string{{"policyA", "policyB"}}, sjA, sjB)

	// n3 is an empty cap-8 receiver under hB (the peer's current domain). It must
	// not be n1, or the first unit could land beside the peer and pin it in place.
	hn, rns := planTestHyperNodes()
	rns["hB"].Insert("n3")
	rns["top"].Insert("n3")
	rns[schedframework.ClusterTopHyperNode].Insert("n3")
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA), // hA
		"n1": phaseBNode("n1", 8, uB), // hB
		"n3": phaseBNode("n3", 8),     // hB
	}
	schedapi.SyncJobAllocatedHyperNode(ji, hn, rns)
	ssn := newPhaseBSSN(t)
	ssn.Jobs = map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}
	ssn.Nodes = nodes
	ssn.HyperNodes = hn
	ssn.RealNodesSet = rns
	registerSubGroupAffinityGradient(ssn)
	snap := NewSessionSnapshot(ssn, gpu, nil)
	if got := sjA.AllocatedHyperNode; got != "hA" {
		t.Fatalf("initial subJob A anchor=%q, want hA", got)
	}
	if got := sjB.AllocatedHyperNode; got != "hB" {
		t.Fatalf("initial subJob B anchor=%q, want hB", got)
	}

	// A lands on hB's n3 (pinned by B's current hB); the commit re-anchors A to
	// hB, so B is pinned to that same settled domain and follows onto n3.
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

// Serial-pinning infeasibility: each affinity-linked subJob is a gang unit
// pinned to the other's domain. A (tried first) lands on its peer's domain hB
// (n1); B, pinned to A's settled hB, finds no free receiver there (n1 is full,
// hD is out of the pinned domain), so the whole call fails with no residue. The
// contrast call shows the first subJob ALONE relocates fine, so the rejection is
// the shared-domain requirement, not the capacity of a single subJob.
func TestDomainRelocation_SubJobAffinityNoInDomainReceiverInfeasible(t *testing.T) {
	uA := phaseBTask("uA", "ns/job", "uA", "n0", 4)
	uB := phaseBTask("uB", "ns/job", "uB", "n1", 4)
	sjA := phaseBSubJob("ns/job/policyA", "ns/job", "policyA", uA)
	sjB := phaseBSubJob("ns/job/policyB", "ns/job", "policyB", uB)
	ji := phaseBSubGroupAffinityJob("ns/job", 1, [][]string{{"policyA", "policyB"}}, sjA, sjB)

	// n1 (hB) is a single 4-GPU unit of capacity; hD (n2) is out of the pinned
	// domain for both subJobs.
	nodes := map[string]*schedapi.NodeInfo{
		"n0": phaseBNode("n0", 8, uA),
		"n1": phaseBNode("n1", 4),
		"n2": phaseBNode("n2", 4),
	}
	ssn := phaseBSession(t, nodes, map[schedapi.JobID]*schedapi.JobInfo{"ns/job": ji}, registerSubGroupAffinityGradient)
	snap := NewSessionSnapshot(ssn, gpu, nil)

	// A is pinned to hB and fills n1; B, pinned to A's settled hB, finds no free
	// receiver there (n1 full, hD out of the pinned domain), so the whole call is
	// infeasible and nothing is committed.
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

	// Contrast: A alone (B static on hB) relocates onto its peer domain hB (n1)
	// fine — the rejection above is the shared-domain requirement, not A's own
	// feasibility.
	alone, aFit := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{uA},
		[]*schedapi.NodeInfo{nodes["n1"]})
	if !aFit || len(alone) != 1 || alone[0].To != "n1" {
		t.Fatalf("single subJob must relocate onto peer domain hB (n1), got fit=%t moves=%v", aFit, alone)
	}
}
