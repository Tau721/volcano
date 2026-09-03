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

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"

	"volcano.sh/volcano/pkg/repackengine/adapter"
	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

func (e *Engine) finishPlacement(ctx context.Context, run *repackv1alpha1.RepackRun) engineframework.RuntimeResult {
	placementTimedOut := false
	resultSnapshotUnavailable := false
	for index := range run.Status.Relocations {
		if run.Status.Relocations[index].Placement.Phase == repackv1alpha1.PodPlacementTimedOut {
			placementTimedOut = true
		}
	}
	targetResource := engineconf.ResolveResource(run, e.config.DefaultResource)
	if placementTimedOut {
		// A timed-out replacement has been released to normal scheduling but has
		// not produced a trustworthy terminal binding. Do not claim the optimistic
		// plan benefit while workload demand may be temporarily absent.
		placementexecutor.MarkBenefitUnverified(run)
	} else {
		schedulerSession := e.clusterCache.OpenSession(e.tiers, e.configurations)
		nodes := adapter.NewSessionSnapshot(schedulerSession, targetResource, nil).Nodes()
		visible := placementexecutor.BindingsVisible(nodes, run.Status.Relocations)
		if !visible {
			schedframework.CloseSessionReadOnly(schedulerSession)
			// The nomination controller may observe Pod binding just before the
			// scheduler cache applies the same Pod update. Wait for one coherent
			// snapshot before publishing cluster-wide actual metrics.
			if !placementexecutor.ObservationDeadlinePassed(run, e.now()) {
				return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
			}
			resultSnapshotUnavailable = true
			placementexecutor.MarkBenefitUnverified(run)
		} else {
			updateActualExecuteResult(run, nodes, targetResource)
			comparison, verificationPending := placementexecutor.FreedNodeVerificationPending(run, e.now())
			schedframework.CloseSessionReadOnly(schedulerSession)
			// Replacement binding and source-node resource release are observed
			// through independent informer streams. A coherent scheduler snapshot
			// can therefore contain the replacement while still retaining a
			// terminating victim (or another stale source-node task) briefly.
			// Do not turn that convergence window into a permanent failed Run.
			//
			// A genuinely occupied planned node is still a failed outcome: it
			// remains unequal through the placement deadline and is evaluated
			// below as BenefitNotRealized.
			if verificationPending {
				klog.V(4).InfoS("repack: waiting for planned node-freeing observation to converge",
					"run", run.Name,
					"plannedFreedNodes", comparison.Planned,
					"actualFreedNodes", comparison.Actual,
					"missingFreedNodes", comparison.Missing,
					"unexpectedFreedNodes", comparison.Unexpected,
					"retryAfter", placementRetryInterval)
				return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
			}
		}
	}

	decision := placementexecutor.EvaluateTerminal(run, resultSnapshotUnavailable)
	message := enginestatus.PlacementMessage(run, targetResource, decision)
	result := run.Status.Result
	resultMetrics := enginestatus.Result(run)
	selectedNodePlacementCount, alternativeNodePlacementCount, timedOutPlacementCount := enginestatus.PlacementOutcomeCounts(run)
	klog.V(3).InfoS("repack: replacement placement terminal result evaluated",
		"run", run.Name, "succeeded", decision.Succeeded, "reason", decision.Reason,
		"resultSnapshotUnavailable", resultSnapshotUnavailable,
		"selectedNodePlacementCount", selectedNodePlacementCount,
		"alternativeNodePlacementCount", alternativeNodePlacementCount,
		"timedOutPlacementCount", timedOutPlacementCount,
		"plannedFreedNodeCount", len(decision.Nodes.Planned), "actualFreedNodeCount", len(decision.Nodes.Actual),
		"missingFreedNodeCount", len(decision.Nodes.Missing), "missingFreedNodes", enginestatus.FormatNodeNames(decision.Nodes.Missing),
		"unexpectedFreedNodeCount", len(decision.Nodes.Unexpected),
		"fragAfterPercent", resultMetrics.FragAfter, "movedCardCount", resultMetrics.MovedCards)
	klog.V(4).InfoS("repack: terminal node-freeing set comparison",
		"run", run.Name, "plannedFreedNodes", decision.Nodes.Planned,
		"actualFreedNodes", decision.Nodes.Actual, "missingFreedNodes", decision.Nodes.Missing,
		"unexpectedFreedNodes", decision.Nodes.Unexpected, "setsEqual", decision.Nodes.Equal,
		"result", result)
	if decision.Succeeded {
		state.MarkSucceeded(run, decision.Reason, message)
	} else {
		state.MarkFailed(run, decision.Reason, message)
	}
	if err := e.updateStatusTerminal(ctx, run); err != nil {
		return runtimeError(err)
	}
	// Placement is terminal even if API cleanup needs a retry. Do not hold the
	// global Execute slot while only removing our own metadata and gates.
	if e.markExecuteDone(run.Name) {
		e.requeueGatedRuns()
	}
	// The terminal result is durable before cleanup. Returning an error makes the
	// workqueue retry the idempotent cleanup without ever repeating eviction.
	if err := e.cleanupPlacement(ctx, run); err != nil {
		return runtimeError(fmt.Errorf("cleanup placement after terminal result: %w", err))
	}
	return engineframework.RuntimeResult{}
}

func updateActualExecuteResult(run *repackv1alpha1.RepackRun, nodes []*schedapi.NodeInfo, targetResource v1.ResourceName) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil || run.Status.Result == nil {
		return
	}
	run.Status.Result.FragAfterPercent = enginestatus.PercentagePoints(engineapi.MeasureResourceFragmentation(nodes, targetResource).FragmentationRate())
	nodesByName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, node := range nodes {
		if node != nil {
			nodesByName[node.Name] = node
		}
	}
	realizedCandidates := placementexecutor.SortedUniqueNodeNames(enginestatus.RealizedFreedNodeNames(run))
	realizedCandidateSet := make(map[string]struct{}, len(realizedCandidates))
	for _, nodeName := range realizedCandidates {
		realizedCandidateSet[nodeName] = struct{}{}
	}
	actuallyFreedNodes := make([]string, 0, len(realizedCandidates))
	for _, nodeName := range placementexecutor.SortedUniqueNodeNames(run.Status.Plan.FreedNodes) {
		if _, realized := realizedCandidateSet[nodeName]; !realized {
			klog.V(4).InfoS("repack: planned node is not an actual-free candidate because its complete victim set was not removed",
				"run", run.Name, "node", nodeName, "resource", targetResource)
			continue
		}
		node := nodesByName[nodeName]
		if node == nil {
			klog.V(4).InfoS("repack: planned node not present in terminal scheduler snapshot",
				"run", run.Name, "node", nodeName, "resource", targetResource)
			continue
		}
		allocatable := engineapi.Scalar(node.Allocatable, targetResource)
		used := engineapi.Scalar(node.Used, targetResource)
		if allocatable <= 0 {
			klog.V(4).InfoS("repack: planned node no longer provides the target resource",
				"run", run.Name, "node", nodeName, "resource", targetResource,
				"allocatable", allocatable, "used", used)
			continue
		}
		if used == 0 {
			actuallyFreedNodes = append(actuallyFreedNodes, nodeName)
			klog.V(4).InfoS("repack: planned node verified free of the target resource",
				"run", run.Name, "node", nodeName, "resource", targetResource,
				"allocatable", allocatable, "used", used)
			continue
		}
		klog.V(4).InfoS("repack: planned node remains occupied by the target resource",
			"run", run.Name, "node", nodeName, "resource", targetResource,
			"allocatable", allocatable, "used", used)
	}
	run.Status.Result.FreedNodes = actuallyFreedNodes
	run.Status.Result.FreedNodeCount = int32(len(actuallyFreedNodes))
	run.Status.Result.MetricsVerified = true
	comparison := placementexecutor.CompareFreedNodeSets(run)
	klog.V(3).InfoS("repack: actual Execute benefit measured from scheduler snapshot",
		"run", run.Name, "resource", targetResource,
		"fragAfterPercent", run.Status.Result.FragAfterPercent,
		"freedNodeCount", run.Status.Result.FreedNodeCount,
		"movedCardCount", run.Status.Result.MovedCardCount,
		"plannedFreedNodeCount", len(comparison.Planned),
		"missingFreedNodeCount", len(comparison.Missing), "missingFreedNodes", enginestatus.FormatNodeNames(comparison.Missing),
		"unexpectedFreedNodeCount", len(comparison.Unexpected))
	klog.V(4).InfoS("repack: actual Execute benefit node sets",
		"run", run.Name, "resource", targetResource,
		"plannedFreedNodes", comparison.Planned, "actualFreedNodes", comparison.Actual,
		"missingFreedNodes", comparison.Missing, "unexpectedFreedNodes", comparison.Unexpected)
}
