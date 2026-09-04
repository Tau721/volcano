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

package repack

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/gangdisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/nodeconsolidation"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/pdbconstraint"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/repackbudget"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloaddisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloadscope"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

type fakeActionRuntime struct {
	cycle            *framework.PlanningCycle
	openErr          error
	statusUpdates    int
	terminalUpdates  int
	prepared         int
	evictions        int
	resumedEvictions int
	placements       int
	cleanups         int
	failReason       string
	calls            []string
	panicPrepare     bool
	prepareErr       error
	evictionResult   framework.RuntimeResult
}

func (f *fakeActionRuntime) OpenPlanningCycle(context.Context, *repackv1alpha1.RepackRun) (*framework.PlanningCycle, error) {
	return f.cycle, f.openErr
}
func (f *fakeActionRuntime) UpdateStatus(context.Context, *repackv1alpha1.RepackRun) error {
	f.statusUpdates++
	f.calls = append(f.calls, "status")
	return nil
}
func (f *fakeActionRuntime) UpdateTerminalStatus(context.Context, *repackv1alpha1.RepackRun) error {
	f.terminalUpdates++
	return nil
}
func (f *fakeActionRuntime) Fail(_ context.Context, _ *repackv1alpha1.RepackRun, reason string, _ error) error {
	f.failReason = reason
	return nil
}
func (*fakeActionRuntime) ResolveMoveOwners(context.Context, *api.RepackPlan) map[string]*repackv1alpha1.WorkloadRef {
	return nil
}
func (f *fakeActionRuntime) PrepareExecution(context.Context, *repackv1alpha1.RepackRun, *api.RepackPlan, framework.Snapshot) error {
	f.prepared++
	f.calls = append(f.calls, "prepare")
	if f.panicPrepare {
		panic("prepare panic")
	}
	return f.prepareErr
}
func (f *fakeActionRuntime) ExecutePreparedEvictions(context.Context, *repackv1alpha1.RepackRun, v1.ResourceName) framework.RuntimeResult {
	f.evictions++
	f.calls = append(f.calls, "evict")
	if f.evictionResult.Requeue || f.evictionResult.RequeueAfter > 0 || f.evictionResult.Err != nil {
		return f.evictionResult
	}
	return framework.RuntimeResult{RequeueAfter: time.Second}
}
func (f *fakeActionRuntime) ResumePreparedEvictions(context.Context, *repackv1alpha1.RepackRun) framework.RuntimeResult {
	f.resumedEvictions++
	return framework.RuntimeResult{}
}
func (f *fakeActionRuntime) ReconcilePlacement(context.Context, *repackv1alpha1.RepackRun) framework.RuntimeResult {
	f.placements++
	return framework.RuntimeResult{}
}
func (f *fakeActionRuntime) CleanupPlacement(context.Context, *repackv1alpha1.RepackRun) error {
	f.cleanups++
	return nil
}
func (*fakeActionRuntime) RecordPlanComputed(*repackv1alpha1.RepackRun) {}

const testResource = v1.ResourceName("nvidia.com/gpu")

type actionSnapshot struct {
	nodes            []*schedapi.NodeInfo
	pdbs             []*policyv1.PodDisruptionBudget
	feasibilityCalls int
}

func (s *actionSnapshot) Nodes() []*schedapi.NodeInfo       { return s.nodes }
func (*actionSnapshot) NodeInScope(*schedapi.NodeInfo) bool { return true }
func (*actionSnapshot) PodGroupView(schedapi.JobID) api.PodGroupView {
	return api.PodGroupView{MinAvailable: 1, Running: 1, Footprint: 1}
}
func (s *actionSnapshot) ListPodDisruptionBudgets() ([]*policyv1.PodDisruptionBudget, error) {
	return s.pdbs, nil
}
func (s *actionSnapshot) FeasibleRelocation(_ context.Context, _ []*api.Move, victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo) ([]*api.Move, bool) {
	s.feasibilityCalls++
	if len(receivers) == 0 {
		return nil, false
	}
	moves := make([]*api.Move, 0, len(victims))
	for _, victim := range victims {
		moves = append(moves, &api.Move{Task: victim, From: victim.NodeName, To: receivers[0].Name})
	}
	return moves, true
}

// HyperNodesSetByTier / RealNodesSet are stubs required by the extended
// Snapshot interface: the action tests plan without HyperNode topology.
func (*actionSnapshot) HyperNodesSetByTier() map[int]sets.Set[string] {
	return map[int]sets.Set[string]{}
}
func (*actionSnapshot) RealNodesSet() map[string]sets.Set[string] {
	return map[string]sets.Set[string]{}
}
func (*actionSnapshot) HyperNodeTierNameMap() map[string]int {
	return map[string]int{}
}

func actionResource(value int64) *schedapi.Resource {
	return &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{testResource: float64(value)}}
}

func actionNode(name string, capacity int64, task *schedapi.TaskInfo) *schedapi.NodeInfo {
	tasks := map[schedapi.TaskID]*schedapi.TaskInfo{}
	used := int64(0)
	if task != nil {
		task.NodeName = name
		tasks[schedapi.TaskID(task.Name)] = task
		used = api.Scalar(task.InitResreq, testResource)
	}
	return &schedapi.NodeInfo{Name: name, Tasks: tasks, Allocatable: actionResource(capacity), Used: actionResource(used)}
}

func actionSession(minNodesFreed int) *framework.Session {
	return actionSessionWithPlugins(minNodesFreed, []string{
		"workloadscope", "pdbconstraint", "repackbudget", "nodeconsolidation",
		"workloaddisruption", "gangdisruption", "binpack",
	})
}

func actionSessionWithPlugins(minNodesFreed int, plugins []string) *framework.Session {
	victimResource := actionResource(2)
	receiverResource := actionResource(4)
	fullResource := actionResource(8)
	snapshot := &actionSnapshot{nodes: []*schedapi.NodeInfo{
		actionNode("victim", 8, &schedapi.TaskInfo{Name: "victim-pod", Job: "ns/victim", InitResreq: victimResource, Resreq: victimResource}),
		actionNode("receiver", 8, &schedapi.TaskInfo{Name: "receiver-pod", Job: "ns/receiver", InitResreq: receiverResource, Resreq: receiverResource}),
		actionNode("empty", 8, nil),
		actionNode("full", 8, &schedapi.TaskInfo{Name: "full-pod", Job: "ns/full", InitResreq: fullResource, Resreq: fullResource}),
	}}
	return framework.OpenSession(framework.SessionConfig{
		Snapshot:      snapshot,
		Resource:      testResource,
		Mode:          repackv1alpha1.RepackModeDryRun,
		MinNodesFreed: minNodesFreed,
	}, framework.PluginOptions(plugins...))
}

func TestRepackActionOwnsPlanAdmissionAndReport(t *testing.T) {
	ssn := actionSession(1)
	defer framework.CloseSession(ssn)

	buildPlan(ssn)

	if ssn.Plan() == nil || ssn.Report().NodesFreed != 1 {
		t.Fatalf("plan=%v report=%+v, want one admitted freed node", ssn.Plan(), ssn.Report())
	}
	if ssn.Plan().Cost.MovedResource != 2 || ssn.Report().MovedResource != 2 {
		t.Fatalf("cost=%+v report=%+v, want action-computed moved resource 2", ssn.Plan().Cost, ssn.Report())
	}
}

func TestRepackActionRejectsBelowBenefitButPreservesCurrentMetric(t *testing.T) {
	ssn := actionSession(2)
	defer framework.CloseSession(ssn)

	buildPlan(ssn)

	if ssn.Plan() != nil {
		t.Fatalf("plan=%v, want benefit constraint rejection", ssn.Plan())
	}
	if ssn.Report().FragmentationRateBefore <= 0 || ssn.Report().FragmentationRateAfter != ssn.Report().FragmentationRateBefore {
		t.Fatalf("report=%+v, want current fragmentation retained for rejected plan", ssn.Report())
	}
}

func TestPluginConfigurationOrderDoesNotAffectPlan(t *testing.T) {
	forward := []string{
		"workloadscope", "pdbconstraint", "repackbudget", "nodeconsolidation",
		"workloaddisruption", "gangdisruption", "binpack",
	}
	reversed := []string{
		"binpack", "gangdisruption", "workloaddisruption",
		"nodeconsolidation", "repackbudget", "pdbconstraint", "workloadscope",
	}
	forwardSession := actionSessionWithPlugins(1, forward)
	defer framework.CloseSession(forwardSession)
	reversedSession := actionSessionWithPlugins(1, reversed)
	defer framework.CloseSession(reversedSession)

	buildPlan(forwardSession)
	buildPlan(reversedSession)

	if !reflect.DeepEqual(forwardSession.Plan(), reversedSession.Plan()) {
		t.Fatalf("plugin order changed plan:\nforward=%+v\nreversed=%+v", forwardSession.Plan(), reversedSession.Plan())
	}
	if !reflect.DeepEqual(forwardSession.Report(), reversedSession.Report()) {
		t.Fatalf("plugin order changed report:\nforward=%+v\nreversed=%+v", forwardSession.Report(), reversedSession.Report())
	}
}

func TestOptionalPluginCombinationsPreserveMainFlowAndReceiverInvariants(t *testing.T) {
	optional := []string{
		"workloadscope", "pdbconstraint", "repackbudget", "workloaddisruption", "gangdisruption", "binpack",
	}
	for mask := 0; mask < 1<<len(optional); mask++ {
		plugins := []string{"nodeconsolidation"}
		for index, name := range optional {
			if mask&(1<<index) != 0 {
				plugins = append(plugins, name)
			}
		}
		ssn := actionSessionWithPlugins(1, plugins)
		buildPlan(ssn)
		plan := ssn.Plan()
		framework.CloseSession(ssn)

		if plan == nil || len(plan.Moves) == 0 {
			t.Fatalf("plugins=%v produced no plan; optional plugins must not disable the main flow", plugins)
		}
		for _, move := range plan.Moves {
			if move == nil || move.Task == nil || move.From == "empty" || move.To == "empty" ||
				move.From == "full" || move.To == "full" {
				t.Fatalf("plugins=%v produced invalid move=%+v; empty/full nodes must never participate", plugins, move)
			}
		}
	}
}

func TestPDBConstraintFiltersDrainCandidatesBeforeSimulation(t *testing.T) {
	strict := actionPDB("strict", map[string]string{"protected": "true"}, 2, 2)
	ssn, snapshot, protected := actionPDBSession([]*policyv1.PodDisruptionBudget{strict}, true)
	defer framework.CloseSession(ssn)

	buildPlan(ssn)
	if ssn.Plan() == nil {
		t.Fatal("an unprotected drain candidate should still produce a plan")
	}
	for _, move := range ssn.Plan().Moves {
		if move != nil && move.Task != nil && move.Task.UID == protected.UID {
			t.Fatalf("strict PDB task unexpectedly entered plan: %+v", move)
		}
	}
	if snapshot.feasibilityCalls == 0 {
		t.Fatal("the remaining unprotected candidate should reach scheduler simulation")
	}
}

func TestPDBConstraintAllCandidatesBlockedSkipsSimulation(t *testing.T) {
	strict := actionPDB("strict-all", nil, 2, 2)
	ssn, snapshot, _ := actionPDBSession([]*policyv1.PodDisruptionBudget{strict}, true)
	defer framework.CloseSession(ssn)

	buildPlan(ssn)
	if ssn.Plan() != nil {
		t.Fatalf("plan=%+v, want all strict-PDB candidates filtered", ssn.Plan())
	}
	if snapshot.feasibilityCalls != 0 {
		t.Fatalf("feasibility calls=%d, want zero when all candidates are statically blocked", snapshot.feasibilityCalls)
	}
}

func TestPDBConstraintIgnoresDynamicZeroAllowanceAndCanBeDisabled(t *testing.T) {
	// DesiredHealthy < ExpectedPods means the PDB has static disruption
	// capacity even when its current DisruptionsAllowed happens to be zero.
	dynamicZero := actionPDB("dynamic-zero", nil, 2, 1)
	dynamicZero.Status.DisruptionsAllowed = 0
	ssn, _, _ := actionPDBSession([]*policyv1.PodDisruptionBudget{dynamicZero}, true)
	buildPlan(ssn)
	if ssn.Plan() == nil {
		t.Fatal("dynamic zero allowance must remain plannable")
	}
	framework.CloseSession(ssn)

	strict := actionPDB("strict-disabled", nil, 2, 2)
	disabled, _, _ := actionPDBSession([]*policyv1.PodDisruptionBudget{strict}, false)
	defer framework.CloseSession(disabled)
	buildPlan(disabled)
	if disabled.Plan() == nil {
		t.Fatal("removing pdbconstraint should restore the previous planning behavior")
	}
}

func actionPDBSession(pdbs []*policyv1.PodDisruptionBudget, enableConstraint bool) (*framework.Session, *actionSnapshot, *schedapi.TaskInfo) {
	protected := actionPodTask("protected", "protected", "true", 2)
	unprotected := actionPodTask("unprotected", "protected", "false", 4)
	full := actionPodTask("full", "protected", "false", 8)
	snapshot := &actionSnapshot{nodes: []*schedapi.NodeInfo{
		actionNode("protected-node", 8, protected),
		actionNode("unprotected-node", 8, unprotected),
		actionNode("empty", 8, nil),
		actionNode("full", 8, full),
	}, pdbs: pdbs}
	plugins := []string{"nodeconsolidation", "binpack"}
	if enableConstraint {
		plugins = append(plugins, "pdbconstraint")
	}
	ssn := framework.OpenSession(framework.SessionConfig{
		Snapshot: snapshot, Resource: testResource, Mode: repackv1alpha1.RepackModeDryRun, MinNodesFreed: 1,
	}, framework.PluginOptions(plugins...))
	return ssn, snapshot, protected
}

func actionPodTask(name, labelKey, labelValue string, resourceValue int64) *schedapi.TaskInfo {
	resource := actionResource(resourceValue)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name), Labels: map[string]string{labelKey: labelValue}},
		Status: v1.PodStatus{Conditions: []v1.PodCondition{{
			Type: v1.PodReady, Status: v1.ConditionTrue,
		}}},
	}
	return &schedapi.TaskInfo{
		UID: schedapi.TaskID(name), Job: schedapi.JobID("ns/" + name), Name: name, Namespace: "ns",
		InitResreq: resource, Resreq: resource, Pod: pod,
	}
}

func actionPDB(name string, matchLabels map[string]string, expectedPods, desiredHealthy int32) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Generation: 1},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: matchLabels}},
		Status: policyv1.PodDisruptionBudgetStatus{
			ObservedGeneration: 1, ExpectedPods: expectedPods, DesiredHealthy: desiredHealthy,
		},
	}
}

func TestRepackActionOwnsDryRunWorkflow(t *testing.T) {
	ssn := actionSession(1)
	runtime := &fakeActionRuntime{cycle: &framework.PlanningCycle{
		Session: ssn, Resource: testResource, Close: func() { framework.CloseSession(ssn) },
	}}
	run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeDryRun}}
	result := (&repackAction{}).Execute(&framework.ActionContext{
		Context: context.Background(), Run: run, Runtime: runtime,
	})
	if result.Err != nil || !result.Stop {
		t.Fatalf("result=%+v, want successful terminal Action result", result)
	}
	if runtime.statusUpdates != 1 || runtime.terminalUpdates != 1 {
		t.Fatalf("status updates=%d terminal=%d, want 1/1", runtime.statusUpdates, runtime.terminalUpdates)
	}
	if run.Status.Plan == nil || run.Status.Phase != repackv1alpha1.RepackSucceeded {
		t.Fatalf("status=%+v, want persisted DryRun plan and succeeded phase", run.Status)
	}
}

func TestRepackActionOwnsExecuteModeBranch(t *testing.T) {
	ssn := actionSession(1)
	runtime := &fakeActionRuntime{cycle: &framework.PlanningCycle{
		Session: ssn, Resource: testResource, Close: func() { framework.CloseSession(ssn) },
	}}
	run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute}}
	result := (&repackAction{}).Execute(&framework.ActionContext{
		Context: context.Background(), Run: run, Runtime: runtime,
	})
	if result.Err != nil || !result.Stop {
		t.Fatalf("result=%+v, want in-flight Execute Action result", result)
	}
	if runtime.prepared != 1 || runtime.evictions != 1 || runtime.terminalUpdates != 0 {
		t.Fatalf("prepared=%d evictions=%d terminal=%d, want 1/1/0", runtime.prepared, runtime.evictions, runtime.terminalUpdates)
	}
	if run.Status.Plan == nil {
		t.Fatal("Execute Action must persist the same computed plan before execution")
	}
	if !reflect.DeepEqual(runtime.calls, []string{"status", "prepare", "evict"}) {
		t.Fatalf("calls=%v, want status -> prepare barrier -> eviction", runtime.calls)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("requeueAfter=%v, want Runtime result propagated", result.RequeueAfter)
	}
}

func TestRepackActionCompletesExecuteWithoutWorthwhilePlan(t *testing.T) {
	ssn := actionSession(2)
	runtime := &fakeActionRuntime{cycle: &framework.PlanningCycle{
		Session: ssn, Resource: testResource, Close: func() { framework.CloseSession(ssn) },
	}}
	run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute}}
	result := (&repackAction{}).Execute(&framework.ActionContext{Context: context.Background(), Run: run, Runtime: runtime})
	if result.Err != nil || runtime.prepared != 0 || runtime.terminalUpdates != 1 {
		t.Fatalf("result=%+v runtime=%+v, want terminal no-op Execute", result, runtime)
	}
	if got := enginestatus.TerminalOutcome(run); got != state.ReasonInsufficientImprovement {
		t.Fatalf("outcome=%q, want %q", got, state.ReasonInsufficientImprovement)
	}
	if run.Status.Result == nil || !run.Status.Result.MetricsVerified {
		t.Fatalf("result=%+v, want verified no-op Execute result", run.Status.Result)
	}
}

func TestRepackActionHoldsExecuteSlotBeforePreparePanic(t *testing.T) {
	ssn := actionSession(1)
	runtime := &fakeActionRuntime{panicPrepare: true, cycle: &framework.PlanningCycle{
		Session: ssn, Resource: testResource, Close: func() { framework.CloseSession(ssn) },
	}}
	actionCtx := &framework.ActionContext{
		Context: context.Background(),
		Run:     &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute}},
		Runtime: runtime,
	}
	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		(&repackAction{}).Execute(actionCtx)
	}()
	if recovered == nil {
		t.Fatal("prepare panic was not observed")
	}
	if !actionCtx.ExecuteSlotHeld() {
		t.Fatal("Execute slot must be held before a prepare-stage panic can unwind to Engine")
	}
}

func TestRepackActionPropagatesImmediateRuntimeRequeue(t *testing.T) {
	ssn := actionSession(1)
	runtime := &fakeActionRuntime{
		evictionResult: framework.RuntimeResult{Requeue: true},
		cycle: &framework.PlanningCycle{
			Session: ssn, Resource: testResource, Close: func() { framework.CloseSession(ssn) },
		},
	}
	run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute}}
	result := (&repackAction{}).Execute(&framework.ActionContext{Context: context.Background(), Run: run, Runtime: runtime})
	if result.Err != nil || !result.Stop || !result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result=%+v, want immediate Runtime requeue propagated through ActionResult", result)
	}
}

func TestRepackActionPreservesRuntimeFailureReason(t *testing.T) {
	runtime := &fakeActionRuntime{openErr: framework.NewActionError(state.ReasonInvalidConfiguration, errors.New("bad resource"))}
	run := &repackv1alpha1.RepackRun{}
	result := (&repackAction{}).Execute(&framework.ActionContext{Context: context.Background(), Run: run, Runtime: runtime})
	if result.Err != nil || runtime.failReason != state.ReasonInvalidConfiguration {
		t.Fatalf("result=%+v failReason=%q, want terminal invalid-configuration failure", result, runtime.failReason)
	}
}

func TestRepackActionDispatchesRecoveryStages(t *testing.T) {
	plan := &repackv1alpha1.RepackPlan{}
	tests := []struct {
		name         string
		run          *repackv1alpha1.RepackRun
		want         func(*fakeActionRuntime) int
		wantSlotHeld bool
	}{
		{name: "eviction", run: &repackv1alpha1.RepackRun{
			Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning, Plan: plan,
				Relocations: []repackv1alpha1.PodRelocationStatus{{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending}}}},
		}, want: func(f *fakeActionRuntime) int { return f.resumedEvictions }, wantSlotHeld: true},
		{name: "placement", run: &repackv1alpha1.RepackRun{
			Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning, Plan: plan,
				Relocations: []repackv1alpha1.PodRelocationStatus{{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}}},
				Conditions:  []metav1.Condition{{Type: state.CondProgressing, Status: metav1.ConditionTrue, Reason: state.ReasonReconcilingPlacements}}},
		}, want: func(f *fakeActionRuntime) int { return f.placements }, wantSlotHeld: true},
		{name: "cleanup", run: &repackv1alpha1.RepackRun{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{repackv1alpha1.PlacementActiveLabel: "true"}},
			Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
			Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackSucceeded,
				Relocations: []repackv1alpha1.PodRelocationStatus{{}}},
		}, want: func(f *fakeActionRuntime) int { return f.cleanups }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeActionRuntime{}
			actionCtx := &framework.ActionContext{Context: context.Background(), Run: test.run, Runtime: runtime}
			result := (&repackAction{}).Execute(actionCtx)
			if result.Err != nil || test.want(runtime) != 1 {
				t.Fatalf("result=%+v runtime=%+v, want stage handler called once", result, runtime)
			}
			if actionCtx.ExecuteSlotHeld() != test.wantSlotHeld {
				t.Fatalf("execute slot held=%t, want %t for %s stage", actionCtx.ExecuteSlotHeld(), test.wantSlotHeld, test.name)
			}
		})
	}
}

func TestRepackActionTerminalizesDeterministicExecutionPreparationFailure(t *testing.T) {
	ssn := actionSession(1)
	runtime := &fakeActionRuntime{
		prepareErr: framework.NewActionError(state.ReasonExecutionPreparationFailed, errors.New("invalid move")),
		cycle: &framework.PlanningCycle{
			Session: ssn, Resource: testResource, Close: func() { framework.CloseSession(ssn) },
		},
	}
	run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute}}
	actionCtx := &framework.ActionContext{Context: context.Background(), Run: run, Runtime: runtime}
	result := (&repackAction{}).Execute(actionCtx)

	if result.Err != nil || !result.Stop {
		t.Fatalf("result=%+v, want deterministic preparation failure handled as terminal", result)
	}
	if runtime.failReason != state.ReasonExecutionPreparationFailed {
		t.Fatalf("failReason=%q, want %q", runtime.failReason, state.ReasonExecutionPreparationFailed)
	}
	if !actionCtx.ExecuteSlotHeld() {
		t.Fatal("deterministic preparation failure must retain the Execute slot until terminal cleanup")
	}
}

func TestRepackActionRetriesRecoverableExecutionPreparationFailure(t *testing.T) {
	ssn := actionSession(1)
	prepareErr := errors.New("temporary PodGroup API outage")
	runtime := &fakeActionRuntime{prepareErr: prepareErr, cycle: &framework.PlanningCycle{
		Session: ssn, Resource: testResource, Close: func() { framework.CloseSession(ssn) },
	}}
	run := &repackv1alpha1.RepackRun{Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute}}
	actionCtx := &framework.ActionContext{Context: context.Background(), Run: run, Runtime: runtime}
	result := (&repackAction{}).Execute(actionCtx)

	if !errors.Is(result.Err, prepareErr) || !result.Stop {
		t.Fatalf("result=%+v, want recoverable preparation error returned to workqueue", result)
	}
	if runtime.failReason != "" || runtime.terminalUpdates != 0 {
		t.Fatalf("failReason=%q terminalUpdates=%d, preparation infrastructure failure must remain recoverable",
			runtime.failReason, runtime.terminalUpdates)
	}
	if !actionCtx.ExecuteSlotHeld() {
		t.Fatal("recoverable preparation must retain the Execute slot")
	}
}
