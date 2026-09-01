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

// Package repack implements the complete RepackRun business workflow. The
// Engine drives this Action for both a new run and durable recovery; planners,
// plugins and executors provide the detailed policy and mechanics.
package repack

import (
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	"volcano.sh/volcano/pkg/repackengine/planner/drain"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

func init() {
	framework.RegisterAction(framework.ActionRepack, framework.ActionRegistration{
		Factory:  func() framework.Action { return &repackAction{} },
		Requires: []framework.PluginCapability{framework.CapabilityDomain},
	})
}

type repackAction struct{}

func (*repackAction) Name() string { return framework.ActionRepack }

// Execute is the single Repack business entry. Status-derived stage dispatch
// makes normal execution and restart recovery follow the same workflow.
func (a *repackAction) Execute(actionCtx *framework.ActionContext) framework.ActionResult {
	if actionCtx == nil || actionCtx.Run == nil || actionCtx.Runtime == nil {
		return framework.ActionResult{Stop: true, Err: fmt.Errorf("repack action context is incomplete")}
	}
	run := actionCtx.Run
	switch enginestatus.ResolveStage(run) {
	case enginestatus.StagePlanning:
		return a.plan(actionCtx)
	case enginestatus.StageEvicting:
		actionCtx.HoldExecuteSlot()
		return executionResult(actionCtx.Runtime.ResumePreparedEvictions(actionCtx.Context, run))
	case enginestatus.StagePlacing:
		actionCtx.HoldExecuteSlot()
		return executionResult(actionCtx.Runtime.ReconcilePlacement(actionCtx.Context, run))
	case enginestatus.StageCleanup:
		return framework.ActionResult{Stop: true, Err: actionCtx.Runtime.CleanupPlacement(actionCtx.Context, run)}
	default:
		return framework.ActionResult{Stop: true}
	}
}

func (a *repackAction) plan(actionCtx *framework.ActionContext) framework.ActionResult {
	run, runtime := actionCtx.Run, actionCtx.Runtime
	processingStartTime := time.Now()
	defer func() {
		metrics.ObserveCycle(string(run.Spec.Mode), time.Since(processingStartTime).Seconds())
	}()

	cycle, err := runtime.OpenPlanningCycle(actionCtx.Context, run)
	if err != nil {
		if actionCtx.Context.Err() != nil {
			return framework.ActionResult{Stop: true, Err: actionCtx.Context.Err()}
		}
		return fail(actionCtx, framework.ActionErrorReason(err), err)
	}
	defer cycle.Close()

	resource := cycle.Resource
	progressMessage := fmt.Sprintf("Planning cluster-wide fragmentation for %s in %s mode.",
		enginestatus.DisplayResource(resource), run.Spec.Mode)
	state.MarkRunning(run, state.ReasonPlanning, progressMessage)
	if err := runtime.UpdateStatus(actionCtx.Context, run); err != nil {
		return framework.ActionResult{Stop: true, Err: fmt.Errorf("persist Running status: %w", err)}
	}

	klog.V(3).InfoS("repack: Action started", "run", run.Name, "mode", run.Spec.Mode, "resource", resource)
	buildPlan(cycle.Session)
	if err := actionCtx.Context.Err(); err != nil {
		return framework.ActionResult{Stop: true, Err: err}
	}

	report, plan := cycle.Session.Report(), cycle.Session.Plan()
	klog.V(3).InfoS("repack: plan computed", "run", run.Name, "mode", run.Spec.Mode, "resource", resource,
		"freedNodes", report.NodesFreed, "movedResource", report.MovedResource,
		"affectedPodGroups", report.AffectedPodGroups,
		"fragBeforePct", enginestatus.PercentagePoints(report.FragmentationRateBefore),
		"fragAfterPct", enginestatus.PercentagePoints(report.FragmentationRateAfter))

	owners := runtime.ResolveMoveOwners(actionCtx.Context, plan)
	enginestatus.ApplyPlan(run, report, plan, resource, owners, cycle.ResolvedScope)
	runtime.RecordPlanComputed(run)
	worthwhile := report.NodesFreed > 0

	if run.Spec.Mode == repackv1alpha1.RepackModeExecute && worthwhile {
		// Hold before the prepare barrier: once any part of the durable journal is
		// visible, even a panic must recover this Run rather than start cooldown.
		actionCtx.HoldExecuteSlot()
		if err := runtime.PrepareExecution(actionCtx.Context, run, plan, cycle.Session.Snapshot()); err != nil {
			if reason := framework.ActionErrorReason(err); reason != "" {
				return fail(actionCtx, reason, err)
			}
			// The relocation journal may already be durable and some PodGroup
			// leases may already belong to this Run. Keep the Run recoverable and
			// retry preparation instead of prematurely terminalizing it.
			return framework.ActionResult{Stop: true, Err: err}
		}
		return executionResult(runtime.ExecutePreparedEvictions(actionCtx.Context, run, resource))
	}

	if run.Spec.Mode == repackv1alpha1.RepackModeExecute {
		enginestatus.InitializeNoopExecuteResult(run)
	}
	return complete(actionCtx, report, worthwhile, resource, processingStartTime, cycle.Session.ConstraintRejection())
}

func executionResult(result framework.RuntimeResult) framework.ActionResult {
	return framework.ActionResult{
		Stop:         true,
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
		Err:          result.Err,
	}
}

func complete(actionCtx *framework.ActionContext, report api.Report, worthwhile bool, resource v1.ResourceName, started time.Time, constraintRejection string) framework.ActionResult {
	run := actionCtx.Run
	var reason string
	switch {
	case constraintRejection != "":
		reason = constraintRejection // a hard constraint rejected the plan
	case !worthwhile && report.FragmentationRateBefore > 0:
		reason = state.ReasonInsufficientImprovement
	case !worthwhile:
		reason = state.ReasonNoFragmentation
	case run.Spec.Mode == repackv1alpha1.RepackModeExecute:
		reason = state.ReasonExecutionCompleted
	default:
		reason = state.ReasonRepackRecommended
	}
	state.MarkSucceeded(run, reason, enginestatus.CompletionMessage(run, resource, reason))
	if err := actionCtx.Runtime.UpdateTerminalStatus(actionCtx.Context, run); err != nil {
		return framework.ActionResult{Stop: true, Err: err}
	}
	klog.V(3).InfoS("repack: Action completed", "run", run.Name, "mode", run.Spec.Mode,
		"phase", run.Status.Phase, "outcome", reason, "freedNodeCount", report.NodesFreed,
		"movedResource", report.MovedResource, "affectedPodGroupCount", report.AffectedPodGroups,
		"duration", time.Since(started))
	return framework.ActionResult{Stop: true}
}

func fail(actionCtx *framework.ActionContext, reason string, cause error) framework.ActionResult {
	if reason == "" {
		reason = state.ReasonReconcileFailed
	}
	return framework.ActionResult{Stop: true, Err: actionCtx.Runtime.Fail(actionCtx.Context, actionCtx.Run, reason, cause)}
}

// buildPlan is the mode-independent planning body. It is deliberately separate
// from Execute orchestration so DryRun and Execute produce the same proposal.
func buildPlan(ssn *framework.Session) {
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	resource := ssn.Resource()
	nodes := ssn.Nodes()
	before := api.MeasureResourceFragmentation(nodes, resource)
	klog.V(3).InfoS("repack: planning pass started", "run", runName, "resource", resource,
		"nodes", len(nodes), "occupiedNodes", before.OccupiedNodeCount,
		"optimalNodes", before.OptimalOccupiedNodeCount, "providingNodes", before.ProvidingNodeCount)

	plan := drain.BuildPlan(ssn)
	if plan != nil {
		plan.Before = before
		if !ssn.PlanAdmissible(plan) {
			klog.V(3).InfoS("repack: plan rejected by benefit constraints", "run", runName, "resource", resource,
				"freedNodeCount", len(plan.FreedNodes), "moveCount", len(plan.Moves),
				"fragmentationBefore", before.FragmentationRate(), "fragmentationDelta", plan.FragmentationRateDelta())
			plan = nil
		} else {
			plan.Cost = api.CalculateDisruptionCost(plan.Moves, resource)
			klog.V(3).InfoS("repack: plan accepted", "run", runName, "resource", resource,
				"freedNodeCount", len(plan.FreedNodes), "moveCount", len(plan.Moves),
				"movedResource", plan.Cost.MovedResource, "affectedPodGroupCount", plan.Cost.AffectedPodGroups,
				"fragmentationBefore", before.FragmentationRate(), "fragmentationDelta", plan.FragmentationRateDelta())
		}
	}
	ssn.SetPlan(plan)
	report := api.RenderReport(plan)
	if plan == nil {
		current := before.FragmentationRate()
		report.FragmentationRateBefore, report.FragmentationRateAfter = current, current
	}
	ssn.SetReport(report)
}
