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

package engine

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	"volcano.sh/volcano/pkg/repackengine/adapter"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

// planRun plans and acts on a cleared run (the gate already passed). Execute is
// deliberately two-phase: persist the complete plan and relocation journal
// first, then issue evictions. This closes the replacement-pod race and ensures
// a crash never performs an eviction whose intent was not durably recorded.
func (e *Engine) planRun(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	processingStartTime := time.Now()
	defer func() { metrics.ObserveCycle(string(run.Spec.Mode), time.Since(processingStartTime).Seconds()) }()
	releaseExecuteSlot := run.Spec.Mode == repackv1alpha1.RepackModeExecute
	if run.Spec.Mode == repackv1alpha1.RepackModeExecute {
		defer func() {
			if !releaseExecuteSlot {
				return
			}
			e.markExecuteDone(run.Name)
			e.requeueGatedRuns()
		}()
	}
	schedulerTiers, schedulerConfigurations := e.tiers, e.configurations

	schedulerSession := e.clusterCache.OpenSession(schedulerTiers, schedulerConfigurations)
	// Read-only close: the engine plans against the session but must NOT write back
	// PodGroup/Queue status (gang Unschedulable conditions, JobUpdater, queue
	// allocated) — that is the scheduler's job and a second writer would race it.
	defer schedframework.CloseSessionReadOnly(schedulerSession)

	klog.V(4).InfoS("repack: processing run", "run", run.Name, "mode", run.Spec.Mode)
	generation := run.Generation
	targetResource := engineconf.ResolveResource(run, e.config.DefaultResource)
	if targetResource == "" {
		// No target accelerator resolvable: spec.goals is empty AND the engine's
		// --repack-default-resource is unset. Measuring fragmentation on the empty
		// resource would count every node as empty and silently report
		// NoFragmentation, so fail fast with an actionable reason instead.
		return e.fail(ctx, run, generation, state.ReasonInvalidConfiguration,
			fmt.Errorf("no target accelerator resource: set spec.goals[0].resource or the engine flag --repack-default-resource"))
	}
	if !engineconf.SupportedTarget(targetResource) {
		// Only fully-qualified extended resources (nvidia.com/gpu, huawei.com/Ascend910)
		// are supported — they live in Resource.ScalarResources. Core compute resources
		// (cpu, memory, ephemeral-storage, ...) are stored in dedicated fields, so
		// Scalar() reads 0 for them and the run would be a silent no-op reporting
		// NoFragmentation. CEL rejects this on spec.goals, but the --repack-default-
		// resource fallback bypasses CEL, so guard it here too.
		return e.fail(ctx, run, generation, state.ReasonInvalidConfiguration,
			fmt.Errorf("target resource %q is not supported; only fully-qualified extended resources (e.g. nvidia.com/gpu) can be defragmented, not core resources like cpu/memory", targetResource))
	}
	actions := e.config.Actions
	if len(actions) == 0 {
		actions = engineframework.DefaultActions()
	}
	if err := engineconf.ValidatePipeline(actions, e.config.Plugins); err != nil {
		return e.fail(ctx, run, generation, state.ReasonInvalidConfiguration, err)
	}
	plugins := configuredPluginNames(e.config.Plugins)
	klog.V(3).InfoS("repack: run execution started",
		"run", run.Name, "mode", run.Spec.Mode, "resource", targetResource,
		"actions", actions, "plugins", plugins)

	progressMessage := fmt.Sprintf(
		"Planning cluster-wide fragmentation for %s in %s mode.",
		enginestatus.DisplayResource(targetResource), run.Spec.Mode)
	state.MarkRunning(run, state.ReasonPlanning, progressMessage)
	if err := e.updateStatus(ctx, run); err != nil {
		return fmt.Errorf("persist Running status: %w", err)
	}
	klog.V(4).InfoS("repack: run status persisted as Running", "run", run.Name, "reason", state.ReasonPlanning)

	scope, err := enginescope.NewMatcher(run.Spec.Scope, adapter.SessionGangScopeLookup(schedulerSession))
	if err != nil {
		return e.fail(ctx, run, generation, state.ReasonScopeResolutionFailed, err)
	}

	snapshot := adapter.NewSessionSnapshot(schedulerSession, targetResource, scope)
	resolvedScope := enginestatus.BuildResolvedScope(snapshot.Nodes(), scope, targetResource)
	maxPodGroups, maxResource, hasPodGroupLimit, hasResourceLimit := engineconf.MaxPerRun(run, targetResource)
	klog.V(5).InfoS("repack: engine session opened", "run", run.Name, "resource", targetResource,
		"nodes", len(snapshot.Nodes()), "resolvedNodeCount", resolvedScope.NodeCount,
		"resolvedPodGroupCount", resolvedScope.PodGroupCount,
		"maxPodGroups", maxPodGroups, "maxResource", maxResource)
	engineSession := engineframework.OpenSession(engineframework.SessionConfig{
		Context:                   ctx,
		Snapshot:                  snapshot,
		Run:                       run,
		Scope:                     scope,
		Resource:                  targetResource,
		Mode:                      run.Spec.Mode,
		MinNodesFreed:             e.config.MinNodesFreed,
		MinFragImprovementPercent: engineconf.MinFragImprovement(run),
		MaxPodGroups:              maxPodGroups,
		MaxResource:               maxResource,
		LimitPodGroups:            hasPodGroupLimit,
		LimitResource:             hasResourceLimit,
		Free:                      adapter.NodeFreeCapacity,
	}, e.config.Plugins)
	defer engineframework.CloseSession(engineSession)
	if err := engineconf.ValidateSession(actions, e.config.Plugins, engineSession); err != nil {
		return e.fail(ctx, run, generation, state.ReasonInvalidConfiguration, err)
	}

	// Actions are planning-only. Execute is committed below, after the plan has
	// been persisted successfully.
	engineframework.RunActions(e.config.Actions, engineSession)
	if err := ctx.Err(); err != nil {
		return err
	}

	report, plan := engineSession.Report(), engineSession.Plan()
	klog.V(3).InfoS("plan computed", "run", run.Name, "mode", run.Spec.Mode, "resource", targetResource,
		"freedNodes", report.NodesFreed, "movedResource", report.MovedResource,
		"affectedPodGroups", report.AffectedPodGroups,
		"fragBeforePct", enginestatus.PercentagePoints(report.FragmentationRateBefore), "fragAfterPct", enginestatus.PercentagePoints(report.FragmentationRateAfter))
	// The Complete reason doubles as the "worth repacking?" verdict (§5.2.2);
	// there is no summary.verdict. worthwhile = the plan freed nodes; an empty
	// plan splits by whether fragmentation existed: none (clean) vs below the
	// benefit gate (fragmented but not worth acting on).
	worthwhile := report.NodesFreed > 0
	execute := run.Spec.Mode == repackv1alpha1.RepackModeExecute
	nominationTTL := time.Duration(0)
	if execute {
		nominationTTL = e.config.NominationTTL
	}
	moveOwners := e.resolveMoveOwners(ctx, plan)
	enginestatus.ApplyPlan(run, report, plan, targetResource, moveOwners, resolvedScope)
	e.recordRunEvent(run, v1.EventTypeNormal, eventReasonPlanComputed, plannedBenefitEventMessage(run))
	if execute && worthwhile {
		if err := enginestatus.PrepareExecuteRelocations(run, plan, nominationTTL, e.now(), snapshot); err != nil {
			enginestatus.MarkExecuteNotPerformed(run)
			return e.fail(ctx, run, generation, state.ReasonExecutionPreparationFailed,
				fmt.Errorf("prepare per-Pod relocation records: %w", err))
		}
		// This is the prepare barrier. In particular, relocations must be visible
		// before an eviction can cause a replacement pod to appear. PodGroup
		// placement leases are the second half of that barrier: the admission
		// webhook can then gate every replacement before the scheduler observes it.
		executionMessage := fmt.Sprintf(
			"Executing repack for %s: evicting %d Pods from %d PodGroups and moving %d cards to free %d nodes.",
			enginestatus.DisplayResource(targetResource), len(run.Status.Relocations), len(run.Status.Plan.Moves),
			run.Status.Plan.Summary.MovedCardCount, run.Status.Plan.Summary.FreedNodeCount)
		state.MarkRunning(run, state.ReasonEvicting, executionMessage)
		if err := e.updateStatus(ctx, run); err != nil {
			return fmt.Errorf("persist prepared Execute plan: %w", err)
		}
		preparedPlacementGroups := placementPodGroups(run)
		if err := e.preparePlacementLeases(ctx, run); err != nil {
			e.releasePlacementLeases(ctx, run, preparedPlacementGroups)
			enginestatus.MarkExecuteNotPerformed(run)
			return e.fail(ctx, run, generation, state.ReasonExecutionPreparationFailed, fmt.Errorf("prepare placement leases: %w", err))
		}
		// Publish the admission lookup index only after the complete relocation
		// journal and every original PodGroup lease are durable. From this point a
		// workload-level recreation may safely be recognized by the PodGroup
		// webhook before its first replacement Pod is created.
		if err := e.setPlacementActive(ctx, run, true); err != nil {
			e.releasePlacementLeases(ctx, run, preparedPlacementGroups)
			enginestatus.MarkExecuteNotPerformed(run)
			return e.fail(ctx, run, generation, state.ReasonExecutionPreparationFailed,
				fmt.Errorf("publish placement discovery: %w", err))
		}
		e.recordRunEvent(run, v1.EventTypeNormal, eventReasonExecutePrepared,
			fmt.Sprintf("Prepared %d replacement placement intents across %d PodGroups before eviction.",
				len(run.Status.Relocations), len(preparedPlacementGroups)))
		klog.V(3).InfoS("repack: Execute plan prepared before eviction",
			"run", run.Name, "moves", len(plan.Moves), "freedNodeCount", len(plan.FreedNodes),
			"relocationCount", len(run.Status.Relocations), "nominationTTL", nominationTTL)

		// Once eviction can begin, keep the K=1 slot across transient failures. A
		// retry resumes the durable per-Pod journal instead of replanning or letting
		// another Execute overlap a partially committed run.
		releaseExecuteSlot = false
		return e.executePreparedEvictions(ctx, run, generation, targetResource)
	}
	if execute && !worthwhile {
		enginestatus.InitializeNoopExecuteResult(run)
	}

	evictedCount, rejectedCount := 0, 0

	var done string
	switch {
	case !worthwhile && report.FragmentationRateBefore > 0:
		done = state.ReasonInsufficientImprovement
	case !worthwhile:
		done = state.ReasonNoFragmentation
	case execute:
		done = state.ReasonExecutionCompleted
	default:
		done = state.ReasonRepackRecommended
	}
	msg := enginestatus.CompletionMessage(run, targetResource, done)
	state.MarkSucceeded(run, done, msg)
	if err := e.updateStatusTerminal(ctx, run); err != nil {
		return err
	}
	klog.V(3).InfoS("repack: run completed and terminal status persisted",
		"run", run.Name, "mode", run.Spec.Mode, "phase", run.Status.Phase, "outcome", done,
		"freedNodeCount", report.NodesFreed, "movedResource", report.MovedResource,
		"affectedPodGroupCount", report.AffectedPodGroups, "evictedCount", evictedCount, "rejectedCount", rejectedCount,
		"duration", time.Since(processingStartTime))
	return nil
}

func configuredPluginNames(options []engineframework.PluginOption) []string {
	names := make([]string, 0, len(options))
	for _, option := range options {
		names = append(names, option.Name)
	}
	return names
}
