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

package drain

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/gangdisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/repackbudget"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloaddisruption"
)

const gpu = v1.ResourceName("nvidia.com/gpu")

// fakeSnap is a minimal framework.Snapshot for solver tests (every node fits).
type fakeSnap struct {
	resource          v1.ResourceName // default nvidia.com/gpu when empty
	nodes             []*schedapi.NodeInfo
	views             map[schedapi.JobID]api.PodGroupView
	notInScope        map[string]bool // node name → excluded from draining (receiver only)
	infeasibleSources map[string]bool
	feasibilityCalls  int
}

func (f *fakeSnap) res() v1.ResourceName {
	if f.resource != "" {
		return f.resource
	}
	return gpu
}

func (f *fakeSnap) Nodes() []*schedapi.NodeInfo { return f.nodes }
func (f *fakeSnap) NodeInScope(n *schedapi.NodeInfo) bool {
	if f.notInScope == nil {
		return true
	}
	return !f.notInScope[n.Name]
}
func (f *fakeSnap) PodGroupView(id schedapi.JobID) api.PodGroupView {
	if view, found := f.views[id]; found {
		return view
	}
	// Most historical fixtures key views by the short PodGroup name. Tasks use
	// the production namespace/name JobID form so the mandatory Repack ownership
	// boundary is exercised by every planner test.
	if separator := strings.IndexByte(string(id), '/'); separator >= 0 {
		return f.views[schedapi.JobID(string(id)[separator+1:])]
	}
	return api.PodGroupView{}
}

// FeasibleRelocation is a capacity-only stand-in for the scheduler feasibility check: every
// node "fits" (no predicate constraints in tests), so feasibility is pure target-resource
// capacity (Allocatable − Used − pods already placed this pass).
func (f *fakeSnap) FeasibleRelocation(_ context.Context, committed []*api.Move, victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo) ([]*api.Move, bool) {
	f.feasibilityCalls++
	if len(victims) > 0 && f.infeasibleSources[victims[0].NodeName] {
		return nil, false
	}
	res := f.res()
	placed := map[string]int64{}
	for _, m := range committed {
		if m != nil && m.Task != nil {
			placed[m.To] += api.Scalar(m.Task.InitResreq, res)
		}
	}
	return placeByCapacity(victims, receivers, res, placed)
}

// placeByCapacity is a small test double for the production scheduler-faithful
// Snapshot implementation. It performs exhaustive target-resource placement
// without creating a second exported feasibility engine.
func placeByCapacity(victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo, resource v1.ResourceName, placed map[string]int64) ([]*api.Move, bool) {
	ordered := append([]*schedapi.TaskInfo(nil), victims...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return api.Scalar(ordered[i].InitResreq, resource) > api.Scalar(ordered[j].InitResreq, resource)
	})
	remaining := make(map[string]int64, len(receivers))
	for _, receiver := range receivers {
		remaining[receiver.Name] = api.Scalar(receiver.Allocatable, resource) -
			api.Scalar(receiver.Used, resource) - placed[receiver.Name]
	}
	moves := make([]*api.Move, 0, len(ordered))
	var assign func(int) bool
	assign = func(index int) bool {
		if index == len(ordered) {
			return true
		}
		victim := ordered[index]
		requested := api.Scalar(victim.InitResreq, resource)
		for _, receiver := range receivers {
			if remaining[receiver.Name] < requested {
				continue
			}
			remaining[receiver.Name] -= requested
			moves = append(moves, &api.Move{Task: victim, From: victim.NodeName, To: receiver.Name})
			if assign(index + 1) {
				return true
			}
			moves = moves[:len(moves)-1]
			remaining[receiver.Name] += requested
		}
		return false
	}
	if !assign(0) {
		return nil, false
	}
	return moves, true
}

func gpuRes(n int64) *schedapi.Resource { return scalarRes(gpu, n) }

func scalarRes(name v1.ResourceName, n int64) *schedapi.Resource {
	return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{name: float64(n)}}
}

func gpuTask(name, gang string, g int64) *schedapi.TaskInfo {
	jobID := gang
	if !strings.ContainsRune(jobID, '/') {
		jobID = "ns/" + jobID
	}
	namespace := jobID[:strings.IndexByte(jobID, '/')]
	return &schedapi.TaskInfo{Name: name, Namespace: namespace, Job: schedapi.JobID(jobID), InitResreq: gpuRes(g)}
}

// sysTask is a non-accelerator pod (e.g. a system DaemonSet): CPU-only request,
// no gang. It must not count toward node-freeability of the accelerator.
func sysTask(name string) *schedapi.TaskInfo {
	return &schedapi.TaskInfo{Name: name, InitResreq: &schedapi.Resource{MilliCPU: 100}}
}

func capNode(name string, capGPU int64, tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
	m := map[schedapi.TaskID]*schedapi.TaskInfo{}
	var used int64
	for i, t := range tasks {
		t.NodeName = name
		m[schedapi.TaskID(fmt.Sprintf("%s-%d", name, i))] = t
		used += int64(t.InitResreq.ScalarResources[gpu] + 0.5)
	}
	return &schedapi.NodeInfo{Name: name, Tasks: m, Allocatable: gpuRes(capGPU), Used: gpuRes(used)}
}

// nodeUnits is the node-domain contribution (one single-node unit per in-scope
// node — mirrors the real node plugin, which gates drain targets by NodeInScope).
func nodeUnits(snap framework.Snapshot) []api.FreeableUnit {
	out := make([]api.FreeableUnit, 0, len(snap.Nodes()))
	for _, n := range snap.Nodes() {
		if !snap.NodeInScope(n) {
			continue
		}
		out = append(out, api.FreeableUnit{Level: "node", Nodes: []string{n.Name}, Weight: 1})
	}
	return out
}

func drainSession(snap framework.Snapshot, movable framework.MovableFn, minFreed int, maxPG int, maxRes int64) *framework.Session {
	return drainSessionWithPlugins(snap, movable, minFreed, maxPG, maxRes, nil)
}

func drainSessionWithPlugins(snap framework.Snapshot, movable framework.MovableFn, minFreed int, maxPG int, maxRes int64, plugins []string) *framework.Session {
	plugins = append([]string{"repackbudget", "binpack"}, plugins...)
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot:      snap,
		Resource:      gpu,
		Mode:          repackv1alpha1.RepackModeDryRun,
		MinNodesFreed: minFreed,
		MaxPodGroups:  maxPG,
		MaxResource:   maxRes,
	}, framework.PluginOptions(plugins...))
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(movable)
	return ssn
}

// finalizedPlan keeps planner tests focused on the historical public outcome
// while production lifecycle ownership remains in the repack Action.
func finalizedPlan(ssn *framework.Session) (*api.RepackPlan, bool) {
	before := api.MeasureResourceFragmentation(ssn.Nodes(), ssn.Resource())
	plan := BuildPlan(ssn)
	if plan == nil {
		return nil, false
	}
	plan.Before = before
	if !ssn.PlanAdmissible(plan) {
		return nil, false
	}
	plan.Cost = api.CalculateDisruptionCost(plan.Moves, ssn.Resource())
	return plan, true
}

func allMovable(*schedapi.TaskInfo) bool { return true }

func realMoves(plan *api.RepackPlan) []*api.Move {
	var out []*api.Move
	for _, m := range plan.Moves {
		if m != nil && m.To != m.From {
			out = append(out, m)
		}
	}
	return out
}

// n0 holds a small gang (2 TargetResource) that fits into n1's slack; draining n0 frees a
// whole node with a single move. n1 (6 TargetResource used) cannot be freed.
func TestDrain_FreesOneNode(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	plan, ok := finalizedPlan(drainSession(snap, allMovable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan")
	}
	if len(plan.FreedNodes) != 1 || plan.FreedNodes[0] != "n0" {
		t.Fatalf("freed=%v, want [n0]", plan.FreedNodes)
	}
	mv := realMoves(plan)
	if len(mv) != 1 || mv[0].Task.Name != "a" || mv[0].From != "n0" || mv[0].To != "n1" {
		t.Fatalf("moves=%+v, want a:n0->n1", mv)
	}
	if plan.Benefit() != 1 {
		t.Errorf("benefit=%v, want 1", plan.Benefit())
	}
}

func TestDrainStopsBeforeSimulationWhenContextIsCancelled(t *testing.T) {
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("n0", 8, gpuTask("a", "g-a", 2)),
		capNode("n1", 8, gpuTask("b", "g-b", 6)),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ssn := framework.OpenSession(framework.SessionConfig{
		Context:       ctx,
		Snapshot:      snap,
		Resource:      gpu,
		Mode:          repackv1alpha1.RepackModeDryRun,
		MinNodesFreed: 1,
	}, framework.PluginOptions("repackbudget", "binpack"))
	defer framework.CloseSession(ssn)
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(allMovable)

	if plan := BuildPlan(ssn); plan != nil {
		t.Fatalf("cancelled planning returned plan %+v, want nil", plan)
	}
	if snap.feasibilityCalls != 0 {
		t.Fatalf("scheduler simulations = %d, want 0 after cancellation", snap.feasibilityCalls)
	}
}

// When both nodes are feasible drain targets, the production plan scores
// must choose the smaller blast radius. This mirrors three independent two-pod
// Deployments (one 2-card pod from each on node-a) and a one-pod Deployment on
// node-b. Every PodGroup has minAvailable=1: moving b-0 breaches its one-pod
// gang, whereas moving any a pod leaves its two-pod gang at minAvailable. Even
// with that gang cost, moving b-0 (one PodGroup / two cards) must beat the
// reverse direction (three PodGroups / six cards).
func TestDrain_PrefersLowerDisruptionCandidate(t *testing.T) {
	a0 := gpuTask("a-0", "pg-a0", 2)
	a1 := gpuTask("a-1", "pg-a1", 2)
	a2 := gpuTask("a-2", "pg-a2", 2)
	b0 := gpuTask("b-0", "pg-b0", 2)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("node-a", 8, a0, a1, a2),
		capNode("node-b", 8, b0),
	}, views: map[schedapi.JobID]api.PodGroupView{
		"pg-a0": {Running: 2, MinAvailable: 1, Footprint: 4},
		"pg-a1": {Running: 2, MinAvailable: 1, Footprint: 4},
		"pg-a2": {Running: 2, MinAvailable: 1, Footprint: 4},
		"pg-b0": {Running: 1, MinAvailable: 1, Footprint: 2},
	}}

	plan, ok := finalizedPlan(drainSessionWithPlugins(snap, allMovable, 1, 0, 0, []string{"workloaddisruption", "gangdisruption"}))
	if !ok || plan == nil {
		t.Fatal("expected both drain directions to be feasible")
	}
	if len(plan.FreedNodes) != 1 || plan.FreedNodes[0] != "node-b" {
		t.Fatalf("freed=%v, want [node-b]: lower-disruption B->A plan must win", plan.FreedNodes)
	}
	moves := realMoves(plan)
	if len(moves) != 1 || moves[0].Task.Name != "b-0" || moves[0].From != "node-b" || moves[0].To != "node-a" {
		t.Fatalf("moves=%+v, want b-0:node-b->node-a", moves)
	}
	if snap.feasibilityCalls != 1 {
		t.Fatalf("lazy selection must stop after the first feasible candidate, calls=%d want=1", snap.feasibilityCalls)
	}
}

func TestDrain_LazySelectionFallsBackToNextSchedulerFeasibleCandidate(t *testing.T) {
	snap := &fakeSnap{
		nodes: []*schedapi.NodeInfo{
			capNode("node-a", 8, gpuTask("a", "pg-a", 1)),
			capNode("node-b", 8, gpuTask("b", "pg-b", 2)),
		},
		infeasibleSources: map[string]bool{"node-a": true},
	}

	plan, ok := finalizedPlan(drainSessionWithPlugins(snap, allMovable, 1, 1, 0, []string{"workloaddisruption", "gangdisruption"}))
	if !ok || plan == nil {
		t.Fatal("expected the second candidate in preference order to be feasible")
	}
	if got := fmt.Sprint(plan.FreedNodes); got != "[node-b]" {
		t.Fatalf("freed=%s, want [node-b] after node-a fails full scheduler validation", got)
	}
	if snap.feasibilityCalls != 2 {
		t.Fatalf("feasibility calls=%d, want 2 (first fails, second succeeds)", snap.feasibilityCalls)
	}
}

func TestDrain_LazySelectionAt4000NodesRunsOneFullSimulation(t *testing.T) {
	const nodeCount = 4000
	nodes := make([]*schedapi.NodeInfo, 0, nodeCount)
	views := make(map[schedapi.JobID]api.PodGroupView, nodeCount)
	for index := 0; index < nodeCount; index++ {
		name := fmt.Sprintf("n%04d", index)
		podGroup := schedapi.JobID(fmt.Sprintf("g%04d", index))
		nodes = append(nodes, capNode(name, 8, gpuTask("pod-"+name, string(podGroup), 2)))
		views[podGroup] = api.PodGroupView{Running: 1, MinAvailable: 1, Footprint: 2}
	}
	snap := &fakeSnap{nodes: nodes, views: views}

	plan, ok := finalizedPlan(drainSessionWithPlugins(snap, allMovable, 1, 1, 0, []string{"workloaddisruption", "gangdisruption"}))
	if !ok || plan == nil || len(plan.FreedNodes) != 1 {
		t.Fatalf("plan=%+v ok=%v, want one freed node", plan, ok)
	}
	if snap.feasibilityCalls != 1 {
		t.Fatalf("4000-node lazy selection made %d full simulations, want 1", snap.feasibilityCalls)
	}
}

// Once draining a node has already disrupted a gang, receivers must be chosen
// without consuming the remaining nodes of that same gang. Otherwise those
// nodes become ineligible as later drain targets and the next step is forced to
// disrupt an unrelated gang.
//
// Job A spans node-1/node-2 and Job B spans node-3/node-4. The first step
// deterministically chooses node-1. Its victim should land on node-3 or node-4,
// preserving node-2 as the cheap continuation target. The resulting plan must
// empty node-1/node-2 and leave Job B intact.
func TestDrain_PreservesAlreadyAffectedGangNodesForLaterDrain(t *testing.T) {
	snap := &fakeSnap{
		nodes: []*schedapi.NodeInfo{
			capNode("node-1", 8, gpuTask("job-a-1", "job-a", 4)),
			capNode("node-2", 8, gpuTask("job-a-2", "job-a", 4)),
			capNode("node-3", 8, gpuTask("job-b-1", "job-b", 4)),
			capNode("node-4", 8, gpuTask("job-b-2", "job-b", 4)),
		},
		views: map[schedapi.JobID]api.PodGroupView{
			"job-a": {Running: 2, MinAvailable: 2, Footprint: 8},
			"job-b": {Running: 2, MinAvailable: 2, Footprint: 8},
		},
	}

	plan, ok := finalizedPlan(drainSessionWithPlugins(snap, allMovable, 2, 0, 0, []string{"workloaddisruption", "gangdisruption"}))
	if !ok || plan == nil {
		t.Fatal("expected a feasible two-node drain plan")
	}
	freed := append([]string(nil), plan.FreedNodes...)
	sort.Strings(freed)
	if got, want := fmt.Sprint(freed), "[node-1 node-2]"; got != want {
		t.Fatalf("freed=%v, want %s: continue draining the already affected job", freed, want)
	}
	for _, move := range realMoves(plan) {
		if move.Task.Job != "ns/job-a" {
			t.Fatalf("move=%s/%s:%s->%s affects a second job; all moves must remain within job-a",
				move.Task.Job, move.Task.Name, move.From, move.To)
		}
	}
}

// Preserving a node from an already-affected gang is a preference, not a hard
// placement constraint. If the preferred unrelated-gang receiver lacks
// capacity, feasibility must continue to the preserved node so a valid drain
// plan is not rejected.
func TestReceiverPreferenceFallsBackWhenPreferredReceiverLacksCapacity(t *testing.T) {
	victim := gpuTask("job-a-1", "job-a", 4)
	sameGangReceiver := capNode("same-gang", 8, gpuTask("job-a-2", "job-a", 2))
	unrelatedReceiver := capNode("unrelated-gang", 8, gpuTask("job-b-1", "job-b", 7))
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("drain-target", 8, victim),
		sameGangReceiver,
		unrelatedReceiver,
	}}
	session := drainSession(snap, allMovable, 1, 0, 0)
	defer framework.CloseSession(session)

	nodesByName := make(map[string]*schedapi.NodeInfo, len(snap.nodes))
	for _, node := range snap.nodes {
		nodesByName[node.Name] = node
	}
	state := newDrainState(snap.nodes, nodesByName, session, allMovable, gpu)
	receivers := state.receiversInPreferenceOrder(
		map[string]bool{"drain-target": true},
		[]*schedapi.TaskInfo{victim},
	)
	if len(receivers) != 1 || receivers[0].Name != "same-gang" {
		t.Fatalf("receivers=%v, want [same-gang]: receiver unable to fit even the smallest victim must be pruned", nodeNames(receivers))
	}

	moves, feasible := snap.FeasibleRelocation(context.Background(), nil, []*schedapi.TaskInfo{victim}, receivers)
	if !feasible || len(moves) != 1 {
		t.Fatalf("feasible=%v moves=%+v, want fallback placement", feasible, moves)
	}
	if moves[0].To != "same-gang" {
		t.Fatalf("move target=%q, want same-gang after unrelated-gang capacity fallback", moves[0].To)
	}
}

// Two receivers can each introduce one new PodGroup while having very different
// gang consequences. The node whose future drain would breach minAvailable is
// more valuable as a receiver; preserving it as a target would make a later
// drain step unnecessarily disruptive.
func TestReceiverPreferenceAccountsForFutureMinAvailableBreach(t *testing.T) {
	victim := gpuTask("job-a-1", "job-a", 1)
	safeToDrain := capNode("safe-to-drain", 8, gpuTask("job-b-1", "job-b", 4))
	costlyToDrain := capNode("costly-to-drain", 8, gpuTask("job-c-1", "job-c", 4))
	snap := &fakeSnap{
		nodes: []*schedapi.NodeInfo{
			capNode("drain-target", 8, victim),
			safeToDrain,
			costlyToDrain,
		},
		views: map[schedapi.JobID]api.PodGroupView{
			"job-a": {Running: 4, MinAvailable: 2, Footprint: 4},
			"job-b": {Running: 4, MinAvailable: 2, Footprint: 24},
			"job-c": {Running: 1, MinAvailable: 1, Footprint: 8},
		},
	}
	session := drainSessionWithPlugins(snap, allMovable, 1, 0, 0, []string{"workloaddisruption", "gangdisruption"})
	defer framework.CloseSession(session)

	nodesByName := make(map[string]*schedapi.NodeInfo, len(snap.nodes))
	for _, node := range snap.nodes {
		nodesByName[node.Name] = node
	}
	state := newDrainState(snap.nodes, nodesByName, session, allMovable, gpu)
	receivers := state.receiversInPreferenceOrder(
		map[string]bool{"drain-target": true},
		[]*schedapi.TaskInfo{victim},
	)
	if len(receivers) != 2 || receivers[0].Name != "costly-to-drain" {
		t.Fatalf("receivers=%v, want costly-to-drain first because draining it would breach job-c minAvailable",
			nodeNames(receivers))
	}
}

func TestPreliminaryCandidateOrderUsesPlanScoreThenStableTieBreakers(t *testing.T) {
	session := drainSessionWithPlugins(&fakeSnap{}, allMovable, 1, 0, 0, []string{"workloaddisruption"})
	defer framework.CloseSession(session)
	state := &drainState{ssn: session}

	moreDisruptive := candidate{
		unit: api.FreeableUnit{Level: "node", Nodes: []string{"node-a"}, Weight: 1},
		key:  "node-a",
		placed: []*api.Move{
			{Task: gpuTask("a-0", "pg-a0", 2), From: "node-a", To: "receiver"},
			{Task: gpuTask("a-1", "pg-a1", 2), From: "node-a", To: "receiver"},
		},
	}
	lessDisruptive := candidate{
		unit: api.FreeableUnit{Level: "node", Nodes: []string{"node-b"}, Weight: 1},
		key:  "node-b",
		placed: []*api.Move{
			{Task: gpuTask("b-0", "pg-b0", 1), From: "node-b", To: "receiver"},
		},
	}
	scored := state.scoreCandidates([]candidate{moreDisruptive, lessDisruptive})
	ordered := orderScoredCandidates(scored)
	if len(ordered) != 2 || ordered[0].candidate.key != "node-b" ||
		ordered[0].score.Total <= ordered[1].score.Total {
		t.Fatalf("ordered=%+v, want node-b first with the higher preference score", ordered)
	}

	// All scoring dimensions tie, so the larger freeable-unit benefit wins even
	// though it appears later in the input.
	higherBenefit := candidate{
		unit: api.FreeableUnit{Level: "hypernode", Nodes: []string{"node-c", "node-d"}, Weight: 2},
		key:  "node-c,node-d",
	}
	lowerBenefit := candidate{
		unit: api.FreeableUnit{Level: "node", Nodes: []string{"node-a"}, Weight: 1},
		key:  "node-a",
	}
	ordered = orderScoredCandidates(state.scoreCandidates([]candidate{lowerBenefit, higherBenefit}))
	if ordered[0].candidate.key != higherBenefit.key ||
		ordered[0].score.Total != ordered[1].score.Total {
		t.Fatalf("tie order=%+v, want higher-benefit unit first", ordered)
	}
}

// four-small-into-one mirrors the e2e defrag scenario: four nodes each holding 2
// GPUs should consolidate onto one node and free three.
func TestDrain_FourSmallIntoOne(t *testing.T) {
	nodes := make([]*schedapi.NodeInfo, 4)
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("n%d", i)
		nodes[i] = capNode(name, 8, gpuTask(fmt.Sprintf("w%d", i), fmt.Sprintf("g%d", i), 2))
	}
	snap := &fakeSnap{nodes: nodes}
	plan, ok := finalizedPlan(drainSession(snap, allMovable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan")
	}
	if len(plan.FreedNodes) < 3 {
		t.Fatalf("freed=%v, want >= 3", plan.FreedNodes)
	}
}

// MinNodesFreed=2 is unreachable here (only one node can be vacated) → NoRepack.
func TestDrain_RejectsBelowMinFreed(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	if plan, ok := finalizedPlan(drainSession(snap, allMovable, 2, 0, 0)); ok {
		t.Fatalf("expected NoRepack (min 2 nodes), got %+v", plan)
	}
}

// A maxResource budget below the victim's footprint blocks the only move.
func TestDrain_BudgetBlocks(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	if _, ok := finalizedPlan(drainSession(snap, allMovable, 1, 0, 1)); ok {
		t.Fatal("expected NoRepack: maxResource=1 < victim 2")
	}
	if snap.feasibilityCalls != 0 {
		t.Fatalf("budget-rejected candidates must not enter expensive feasibility simulation, calls=%d", snap.feasibilityCalls)
	}
}

func TestDrain_ResourceCapacityPreflightSkipsFeasibilitySimulation(t *testing.T) {
	// Neither node has enough slack to receive the other's accelerator task. The
	// scalar capacity lower bound can reject both candidates before the scheduler
	// predicate simulation is invoked.
	a := gpuTask("a", "g-a", 5)
	b := gpuTask("b", "g-b", 4)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	if _, ok := finalizedPlan(drainSession(snap, allMovable, 1, 0, 0)); ok {
		t.Fatal("expected no plan when neither receiver has enough resource capacity")
	}
	if snap.feasibilityCalls != 0 {
		t.Fatalf("capacity-rejected candidates must not enter feasibility simulation, calls=%d", snap.feasibilityCalls)
	}
}

func TestPreliminaryCandidatesExcludeAggregateCapacityFailuresBeforeScoring(t *testing.T) {
	// The 16-card node cannot be drained because the two 8-card nodes provide only
	// six cards of receiver slack. The smaller partial candidate remains eligible,
	// while the full node is excluded as a source before scoring. This proves both
	// prefilters run before the scoring candidate set is built.
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("large", 16, gpuTask("large-task", "large-group", 12)),
		capNode("small", 8, gpuTask("small-task", "small-group", 2)),
		capNode("full", 8, gpuTask("full-task", "full-group", 8)),
	}}
	session := drainSession(snap, allMovable, 1, 0, 0)
	defer framework.CloseSession(session)

	nodesByName := make(map[string]*schedapi.NodeInfo, len(snap.nodes))
	for _, node := range snap.nodes {
		nodesByName[node.Name] = node
	}
	state := newDrainState(snap.nodes, nodesByName, session, allMovable, gpu)
	state.prepareUnits(nodeUnits(snap))
	preliminary := state.preliminaryCandidates()

	keys := make([]string, 0, len(preliminary))
	for _, candidate := range preliminary {
		keys = append(keys, candidate.key)
	}
	sort.Strings(keys)
	if got, want := fmt.Sprint(keys), fmt.Sprint([]string{"small"}); got != want {
		t.Fatalf("preliminary candidates=%v, want %v", keys, []string{"small"})
	}
	if !state.stuckUnits["large"] {
		t.Fatal("aggregate-capacity failure must deactivate the large candidate")
	}
	if state.prunedByReason["insufficient_receiver_resource"] != 1 {
		t.Fatalf("capacity prune count=%d, want 1", state.prunedByReason["insufficient_receiver_resource"])
	}
}

func TestDrain_BaseEligibilityExcludesEmptyAndFullNodesWithoutBinpack(t *testing.T) {
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("partial-small", 8, gpuTask("small", "small-group", 2)),
		capNode("partial-large", 8, gpuTask("large", "large-group", 6)),
		capNode("empty", 8),
		capNode("full", 8, gpuTask("full", "full-group", 8)),
	}}
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: snap, Resource: gpu, MinNodesFreed: 1,
	}, nil)
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(allMovable)
	ssn.AddReceiverPoolFn(func(_ *api.PlanContext, nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo {
		// A faulty/custom plugin must not be able to bypass the planner's base
		// receiver invariant by adding empty or full nodes back into the pool.
		return append(nodes, snap.nodes[2], snap.nodes[3])
	})
	defer framework.CloseSession(ssn)

	nodesByName := make(map[string]*schedapi.NodeInfo, len(snap.nodes))
	for _, node := range snap.nodes {
		nodesByName[node.Name] = node
	}
	state := newDrainState(snap.nodes, nodesByName, ssn, allMovable, gpu)
	if got, want := fmt.Sprint(nodeNames(state.receiverNodes)), "[partial-small partial-large]"; got != want {
		t.Fatalf("base receivers=%s, want %s", got, want)
	}

	plan, ok := finalizedPlan(ssn)
	if !ok || plan == nil {
		t.Fatal("expected partially occupied nodes to consolidate without binpack")
	}
	for _, move := range realMoves(plan) {
		if move.From == "empty" || move.From == "full" || move.To == "empty" || move.To == "full" {
			t.Fatalf("ineligible empty/full node participated in move: %+v", move)
		}
	}
}

func TestDrain_OnlyEmptyAndFullNodesSkipsSimulation(t *testing.T) {
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("empty", 8),
		capNode("full", 8, gpuTask("full", "full-group", 8)),
	}}
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: snap, Resource: gpu, MinNodesFreed: 1,
	}, nil)
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(allMovable)
	scoreCalls := 0
	ssn.AddPlanScoreFn("mustNotRun", 1, func(*api.PlanContext, *api.CandidatePlan) int64 {
		scoreCalls++
		return 0
	})
	defer framework.CloseSession(ssn)

	if plan := BuildPlan(ssn); plan != nil {
		t.Fatalf("plan=%+v, want none when no partially occupied node exists", plan)
	}
	if snap.feasibilityCalls != 0 {
		t.Fatalf("feasibility calls=%d, want no simulation for empty/full-only cluster", snap.feasibilityCalls)
	}
	if scoreCalls != 0 {
		t.Fatalf("score calls=%d, want source eligibility pruning before candidate scoring", scoreCalls)
	}
}

func TestDrain_ExplicitZeroBudgetBlocks(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot:       snap,
		Resource:       gpu,
		Mode:           repackv1alpha1.RepackModeDryRun,
		MinNodesFreed:  1,
		MaxPodGroups:   0,
		LimitPodGroups: true,
	}, framework.PluginOptions("repackbudget", "binpack"))
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(allMovable)
	if plan, ok := finalizedPlan(ssn); ok {
		t.Fatalf("explicit podGroups=0 must block every move, got %+v", plan)
	}
}

// A frozen task makes its node un-vacatable as a drain TARGET, but the node is
// still a valid RECEIVER: here g-a (on n0) is frozen, so drain frees n1 instead
// by moving b onto n0's slack.
func TestDrain_FrozenNodeSkippedAsTarget(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	frozen := func(t *schedapi.TaskInfo) bool { return t.Job != "ns/g-a" } // g-a frozen
	plan, ok := finalizedPlan(drainSession(snap, frozen, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected n1 to be freed (n0 frozen but usable as receiver)")
	}
	if len(plan.FreedNodes) != 1 || plan.FreedNodes[0] != "n1" {
		t.Fatalf("freed=%v, want [n1]", plan.FreedNodes)
	}
	mv := realMoves(plan)
	if len(mv) != 1 || mv[0].Task.Name != "b" || mv[0].From != "n1" || mv[0].To != "n0" {
		t.Fatalf("moves=%+v, want b:n1->n0", mv)
	}
}

// Nothing movable anywhere → no node can be vacated → NoRepack.
func TestDrain_AllFrozenNoRepack(t *testing.T) {
	a := gpuTask("a", "g-a", 2)
	b := gpuTask("b", "g-b", 6)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a), capNode("n1", 8, b)}}

	none := func(*schedapi.TaskInfo) bool { return false }
	if _, ok := finalizedPlan(drainSession(snap, none, 1, 0, 0)); ok {
		t.Fatal("expected NoRepack: nothing movable")
	}
}

// The dynamic single pass must be deterministic: identical input → identical
// plan (guards against map-iteration-order leaking into the result).
func TestDrain_Deterministic(t *testing.T) {
	mk := func() *fakeSnap {
		return &fakeSnap{nodes: []*schedapi.NodeInfo{
			capNode("n0", 8, gpuTask("a", "g-a", 2)),
			capNode("n1", 8, gpuTask("b", "g-b", 2)),
			capNode("n2", 8, gpuTask("c", "g-c", 6)),
		}}
	}
	p1, ok1 := finalizedPlan(drainSession(mk(), allMovable, 1, 0, 0))
	p2, ok2 := finalizedPlan(drainSession(mk(), allMovable, 1, 0, 0))
	if !ok1 || !ok2 {
		t.Fatalf("expected feasible plans (ok1=%v ok2=%v)", ok1, ok2)
	}
	if s1, s2 := planKey(p1), planKey(p2); s1 != s2 {
		t.Fatalf("nondeterministic plan:\n  %s\n  %s", s1, s2)
	}
}

// planKey is a stable string fingerprint of a plan (freed nodes + moves), for
// deterministic comparison across runs.
func planKey(p *api.RepackPlan) string {
	freed := append([]string(nil), p.FreedNodes...)
	sort.Strings(freed)
	mv := make([]string, 0, len(p.Moves))
	for _, m := range p.Moves {
		if m != nil && m.To != m.From {
			mv = append(mv, fmt.Sprintf("%s:%s->%s", m.Task.Name, m.From, m.To))
		}
	}
	sort.Strings(mv)
	return fmt.Sprintf("freed=%v moves=%v", freed, mv)
}

// A staying node (holds a frozen pod → can never be vacated) should be the
// preferred receiver, so filling it does not waste a drainable node. Here n2
// (frozen f) and n3 (movable m) both have free 6, but the node list puts the
// drainable n3 first — plain best-fit (a tie) would fill n3 and lose it, freeing
// only 1 node; the staying-receiver preference sends the victim to n2, keeping n3
// drainable → 2 nodes freed.
func TestDrain_PrefersStayingReceiver(t *testing.T) {
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("n0", 8, gpuTask("a", "g-a", 2)),
		capNode("n3", 8, gpuTask("m", "g-m", 2)),
		capNode("n2", 8, gpuTask("f", "g-f", 2)),
	}}
	movable := func(tk *schedapi.TaskInfo) bool { return tk.Job != "ns/g-f" } // g-f frozen
	plan, ok := finalizedPlan(drainSession(snap, movable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan")
	}
	if len(plan.FreedNodes) != 2 {
		t.Fatalf("freed=%v, want 2 (n0,n3 kept drainable via staying-receiver preference)", plan.FreedNodes)
	}
	// The frozen node n2 must never be a drain target.
	for _, n := range plan.FreedNodes {
		if n == "n2" {
			t.Errorf("n2 (frozen) must not be vacated; freed=%v", plan.FreedNodes)
		}
	}
}

// scope.nodes.exclude = "don't drain but may receive": n2 is excluded from
// draining, so it is never a target but is the preferred receiver — a,b land on
// it and the two in-scope nodes free.
func TestDrain_ExcludedNodeIsReceiverNotTarget(t *testing.T) {
	snap := &fakeSnap{
		nodes: []*schedapi.NodeInfo{
			capNode("n0", 8, gpuTask("a", "g-a", 2)),
			capNode("n1", 8, gpuTask("b", "g-b", 2)),
			capNode("n2", 8, gpuTask("c", "g-c", 2)),
		},
		notInScope: map[string]bool{"n2": true}, // excluded from draining
	}
	plan, ok := finalizedPlan(drainSession(snap, allMovable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan")
	}
	for _, n := range plan.FreedNodes {
		if n == "n2" {
			t.Fatalf("n2 (excluded from draining) must not be a target; freed=%v", plan.FreedNodes)
		}
	}
	if len(plan.FreedNodes) != 2 {
		t.Fatalf("freed=%v, want 2 (n0,n1; n2 absorbs as preferred receiver)", plan.FreedNodes)
	}
}

// A node running a non-movable, non-accelerator pod (a system DaemonSet) is still
// freeable: only its accelerator pods must be movable, and only they are evicted.
// Regression for the bug where any pinned system pod made every real accelerator
// node unfreeable (repack no-op in production).
func TestDrain_SystemPodDoesNotBlockFreeing(t *testing.T) {
	a := gpuTask("a", "g-a", 2)  // movable TargetResource pod on n0
	sys := sysTask("kube-proxy") // pinned, non-accelerator, out-of-scope pod on n0
	b := gpuTask("b", "g-b", 6)  // n1 stays occupied
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{capNode("n0", 8, a, sys), capNode("n1", 8, b)}}

	// Everything movable except the system pod (no gang → out of scope in reality).
	movable := func(t *schedapi.TaskInfo) bool { return t.Name != "kube-proxy" }

	plan, ok := finalizedPlan(drainSession(snap, movable, 1, 0, 0))
	if !ok || plan == nil {
		t.Fatal("expected a feasible plan: n0's TargetResource pod can move; the system pod stays")
	}
	if len(plan.FreedNodes) != 1 || plan.FreedNodes[0] != "n0" {
		t.Fatalf("freed=%v, want [n0]", plan.FreedNodes)
	}
	if mv := realMoves(plan); len(mv) != 1 || mv[0].Task.Name != "a" {
		t.Fatalf("moves=%+v, want only the TargetResource pod a (system pod must not move)", mv)
	}
}

// A target-resource Pod without a PodGroup is outside Repack's execution
// contract even when workloadscope is disabled. Its node must never become a
// drain target because no PodGroup lease or replacement tracking can protect
// the eviction.
func TestDrain_TargetResourcePodWithoutPodGroupBlocksFreeing(t *testing.T) {
	unmanaged := &schedapi.TaskInfo{Name: "kube-scheduled-npu", InitResreq: gpuRes(2)}
	receiverWorkload := gpuTask("receiver", "g-b", 8)
	snap := &fakeSnap{nodes: []*schedapi.NodeInfo{
		capNode("n0", 8, unmanaged),
		capNode("n1", 8, receiverWorkload),
	}}

	plan, ok := finalizedPlan(drainSession(snap, allMovable, 1, 0, 0))
	if ok || plan != nil {
		t.Fatalf("plan=%+v ok=%t, want no plan for a target-resource Pod without PodGroup ownership", plan, ok)
	}
}

// drainSessionFragGate mirrors drainSession but sets the per-run frag-improvement
// gate (spec.goals[0].minFragImprovementPercent).
func drainSessionFragGate(snap framework.Snapshot, minImprovePct int) *framework.Session {
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot:                  snap,
		Resource:                  gpu,
		Mode:                      repackv1alpha1.RepackModeDryRun,
		MinNodesFreed:             1,
		MinFragImprovementPercent: minImprovePct,
	}, framework.PluginOptions("repackbudget", "binpack"))
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(allMovable)
	return ssn
}

// Freeing n0 of two TargetResource nodes cuts fragmentation by 1/2 = 50 percentage points.
// A gate at 50 admits the plan (improvement meets the bar); a gate at 60 rejects
// it even though a node could be freed (below the run's benefit threshold).
func TestDrain_FragImprovementGate(t *testing.T) {
	newSnap := func() *fakeSnap {
		return &fakeSnap{nodes: []*schedapi.NodeInfo{
			capNode("n0", 8, gpuTask("a", "g-a", 2)),
			capNode("n1", 8, gpuTask("b", "g-b", 6)),
		}}
	}
	if plan, ok := finalizedPlan(drainSessionFragGate(newSnap(), 50)); !ok || plan == nil {
		t.Fatalf("gate=50: expected a feasible plan (50pp improvement meets the bar)")
	}
	if plan, ok := finalizedPlan(drainSessionFragGate(newSnap(), 60)); ok {
		t.Fatalf("gate=60: expected NoRepack (50pp improvement below the bar), got %+v", plan)
	}
}

const e2eNPU = v1.ResourceName("volcano.sh/e2e-npu")

// TestDrain_E2EMilliNPULayout mirrors the e2e two-halves-into-one scenario at the
// scheduler's milli scale (8→8000, 4→4000, 2→2000). Ledger math must work even
// when Scalar() returns thousands, not single-digit card counts.
func TestDrain_E2EMilliNPULayout(t *testing.T) {
	npuRes := func(n float64) *schedapi.Resource {
		return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{e2eNPU: n}}
	}
	npuTask := func(name, gang string, cards float64) *schedapi.TaskInfo {
		return &schedapi.TaskInfo{Name: name, Namespace: "ns", Job: schedapi.JobID("ns/" + gang), InitResreq: npuRes(cards)}
	}
	capNPUNode := func(name string, cap, used float64, tasks ...*schedapi.TaskInfo) *schedapi.NodeInfo {
		m := map[schedapi.TaskID]*schedapi.TaskInfo{}
		for i, t := range tasks {
			t.NodeName = name
			m[schedapi.TaskID(fmt.Sprintf("%s-%d", name, i))] = t
		}
		return &schedapi.NodeInfo{
			Name: name, Tasks: m,
			Allocatable: npuRes(cap),
			Used:        npuRes(used),
		}
	}
	a := npuTask("a", "g-a", 4000)
	b := npuTask("b", "g-b", 2000)
	snap := &fakeSnap{resource: e2eNPU, nodes: []*schedapi.NodeInfo{
		capNPUNode("n0", 8000, 4000, a),
		capNPUNode("n1", 8000, 2000, b),
		capNPUNode("n2", 8000, 0),
	}}
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: snap, Resource: e2eNPU,
		MinNodesFreed: 1,
	}, framework.PluginOptions("repackbudget", "binpack"))
	ssn.AddDomainFn(nodeUnits)
	ssn.AddMovableFn(allMovable)

	plan, ok := finalizedPlan(ssn)
	if !ok || plan == nil {
		t.Fatal("expected consolidation plan for milli-scale e2e layout")
	}
	if len(plan.FreedNodes) != 1 {
		t.Fatalf("freed=%v, want one node", plan.FreedNodes)
	}
	if mv := realMoves(plan); len(mv) != 1 || mv[0].To == mv[0].From {
		t.Fatalf("moves=%+v, want one real relocation", mv)
	}
}
