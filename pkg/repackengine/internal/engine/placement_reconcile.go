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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"

	"volcano.sh/volcano/pkg/repackengine/adapter"
	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

func (e *Engine) reconcilePlacement(ctx context.Context, run *repackv1alpha1.RepackRun) engineframework.RuntimeResult {
	if run == nil {
		return engineframework.RuntimeResult{}
	}
	selectedNodePlacements, alternativeNodePlacements, timedOutPlacements := enginestatus.PlacementOutcomeCounts(run)
	klog.V(4).InfoS("repack: reconciling replacement placement",
		"run", run.Name, "relocationCount", len(run.Status.Relocations),
		"selectedNodePlacementCount", selectedNodePlacements,
		"alternativeNodePlacementCount", alternativeNodePlacements,
		"timedOutPlacementCount", timedOutPlacements)
	if err := e.repairRecreatedPodGroupLeasesIfDue(ctx, run); err != nil {
		return runtimeError(fmt.Errorf("reconcile recreated PodGroup leases: %w", err))
	}
	if expired, err := e.expirePlacements(ctx, run); err != nil {
		return runtimeError(err)
	} else if expired {
		return engineframework.RuntimeResult{Requeue: true}
	}
	if placementexecutor.Complete(run) {
		return e.finishPlacement(ctx, run)
	}
	pending := placementexecutor.Candidates(run)
	if len(pending) == 0 {
		// A replacement controller may need time to create the Pod. Keep polling
		// until the durable deadline so an absent replacement cannot bypass the
		// expiration escape hatch.
		klog.V(4).InfoS("repack: no selectable replacement Pod observed yet; placement requeued",
			"run", run.Name, "retryAfter", placementRetryInterval)
		return engineframework.RuntimeResult{RequeueAfter: placementRetryInterval}
	}

	targetResource := engineconf.ResolveResource(run, e.config.DefaultResource)
	schedulerSession := e.clusterCache.OpenSession(e.tiers, e.configurations)
	defer schedframework.CloseSessionReadOnly(schedulerSession)
	scope, err := enginescope.NewMatcher(run.Spec.Scope, adapter.SessionGangScopeLookup(schedulerSession))
	if err != nil {
		return runtimeError(err)
	}
	snapshot := adapter.NewSessionSnapshot(schedulerSession, targetResource, scope)
	nodes := snapshot.Nodes()
	nodeNames := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if n != nil {
			nodeNames[n.Name] = struct{}{}
		}
	}
	excludedFreedNodes := enginestatus.RealizedFreedNodeNames(run)
	klog.V(4).InfoS("repack: evaluating live placement receivers",
		"run", run.Name, "candidateCount", len(pending), "snapshotNodeCount", len(nodes),
		"excludedFreedNodes", excludedFreedNodes)
	committed := make([]*engineapi.Move, 0, len(pending))
	selected := make(map[placementexecutor.Identity]string, len(pending))
	for _, relocation := range pending {
		pod, err := e.clusterCache.Client().CoreV1().Pods(relocation.Namespace).Get(ctx, relocation.Placement.ReplacementPodName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return runtimeError(err)
		}
		if pod.UID != relocation.Placement.ReplacementPodUID || pod.Spec.NodeName != "" {
			continue
		}
		// A restarted Engine can reconcile an in-flight placement before its
		// scheduler node cache has finished draining the initial node list, so
		// the planned node may be transiently absent. Committing now would
		// silently pick a different receiver; requeue until it is visible. The
		// placement's ExpirationTime still bounds the wait.
		if relocation.PlannedNodeName != "" {
			if _, plannedVisible := nodeNames[relocation.PlannedNodeName]; !plannedVisible {
				klog.V(3).InfoS("repack: planned receiver node not yet visible in snapshot; placement requeued",
					"run", run.Name, "pod", relocation.Namespace+"/"+relocation.Placement.ReplacementPodName,
					"plannedNode", relocation.PlannedNodeName, "snapshotNodeCount", len(nodes))
				if err := e.markWaitingForNodeSelection(ctx, run.Name, pending); err != nil {
					return runtimeError(err)
				}
				return engineframework.RuntimeResult{RequeueAfter: placementRetryInterval}
			}
		}
		// The replacement is a live Pod and has not been bound yet; constructing a
		// scheduler TaskInfo from it preserves its current resource requests and
		// scheduling constraints for the full predicate simulation below.
		task := schedapi.NewTaskInfo(pod)
		receivers := placementexecutor.Receivers(nodes, excludedFreedNodes, relocation.PlannedNodeName, task)
		klog.V(4).InfoS("repack: replacement receiver candidates evaluated",
			"run", run.Name, "pod", relocation.Namespace+"/"+relocation.Placement.ReplacementPodName,
			"plannedNode", relocation.PlannedNodeName, "receiverCount", len(receivers))
		placements, fit := snapshot.FeasibleRelocation(ctx, committed, []*schedapi.TaskInfo{task}, receivers)
		if !fit || len(placements) != 1 {
			klog.V(3).InfoS("repack: replacement is waiting for a feasible receiver node",
				"run", run.Name, "pod", relocation.Namespace+"/"+relocation.Placement.ReplacementPodName,
				"plannedNode", relocation.PlannedNodeName, "receiverCount", len(receivers))
			if err := e.markWaitingForNodeSelection(ctx, run.Name, pending); err != nil {
				return runtimeError(err)
			}
			return engineframework.RuntimeResult{RequeueAfter: placementRetryInterval}
		}
		committed = append(committed, placements[0])
		selected[placementexecutor.IdentityForRelocation(relocation)] = placements[0].To
		klog.V(4).InfoS("repack: replacement receiver selected in scheduler simulation",
			"run", run.Name, "pod", relocation.Namespace+"/"+relocation.Placement.ReplacementPodName,
			"plannedNode", relocation.PlannedNodeName, "selectedNode", placements[0].To)
	}
	if len(selected) == 0 {
		return engineframework.RuntimeResult{RequeueAfter: placementRetryInterval}
	}
	return runtimeError(e.writePlacementSelection(ctx, run.Name, selected))
}

func (e *Engine) writePlacementSelection(
	ctx context.Context,
	runName string,
	selected map[placementexecutor.Identity]string,
) error {
	var updatedRun *repackv1alpha1.RepackRun
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range run.Status.Relocations {
			relocation := &run.Status.Relocations[index]
			if node, found := selected[placementexecutor.IdentityForRelocation(relocation)]; found && relocation.Placement.SelectedNodeName == "" {
				relocation.Placement.SelectedNodeName = node
				changed = true
			}
		}
		if !changed {
			return nil
		}
		updatedRun, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
	if err != nil || updatedRun == nil {
		return err
	}
	klog.V(3).InfoS("repack: live replacement receivers persisted",
		"run", runName, "selectionCount", len(selected))
	e.recordRunEvent(updatedRun, v1.EventTypeNormal, eventReasonPlacementSelected,
		fmt.Sprintf("Selected live receiver nodes for %d replacement Pods.", len(selected)))
	return nil
}

func (e *Engine) markWaitingForNodeSelection(ctx context.Context, runName string, relocations []*repackv1alpha1.PodRelocationStatus) error {
	keys := make(map[placementexecutor.Identity]struct{}, len(relocations))
	for _, relocation := range relocations {
		keys[placementexecutor.IdentityForRelocation(relocation)] = struct{}{}
	}
	var updatedRun *repackv1alpha1.RepackRun
	placementStateChanged := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for index := range run.Status.Relocations {
			relocation := &run.Status.Relocations[index]
			if _, found := keys[placementexecutor.IdentityForRelocation(relocation)]; found && relocation.Placement.SelectedNodeName == "" && relocation.Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection {
				relocation.Placement.Phase = repackv1alpha1.PodPlacementWaitingForNodeSelection
				changed = true
				placementStateChanged = true
			}
		}
		if state.MarkRunning(run, state.ReasonReconcilingPlacements,
			enginestatus.PlacementProgressMessage(run, engineconf.ResolveResource(run, e.config.DefaultResource))) {
			changed = true
		}
		if !changed {
			return nil
		}
		updatedRun, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
	if err == nil && placementStateChanged && updatedRun != nil {
		message := enginestatus.PlacementProgressMessage(updatedRun, engineconf.ResolveResource(updatedRun, e.config.DefaultResource))
		klog.V(3).InfoS("repack: replacement placement waiting for node selection",
			"run", runName, "pendingCount", len(relocations), "retryAfter", placementRetryInterval)
		e.recordRunEvent(updatedRun, v1.EventTypeWarning, eventReasonWaitingForNodeSelection, message)
	}
	return err
}
