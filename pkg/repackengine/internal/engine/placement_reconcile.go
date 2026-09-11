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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/volcano/pkg/controllers/repack/state"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

// reconcilePlacement nominates every pending replacement Pod straight to its
// planned receiver: the plan is the selection, so the engine reads no live Pod
// and runs no feasibility recompute. The scheduler validates the target when it
// binds, and any drift is recorded on the relocation.
func (e *Engine) reconcilePlacement(ctx context.Context, run *repackv1alpha1.RepackRun) engineframework.RuntimeResult {
	if run == nil {
		return engineframework.RuntimeResult{}
	}
	if executionDeadlinePassed(run, e.now()) {
		return runtimeError(e.timeoutExecution(ctx, run, run.Generation, e.clusterCache.Client()))
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
	if placementexecutor.Complete(run) {
		if hasRetryableEvictions(run) && !hasTimedOutPlacement(run) {
			changed := state.MarkRunning(run, state.ReasonEvicting,
				"Accepted replacements are restored; resuming remaining eviction retries.")
			if changed {
				if err := e.updateStatus(ctx, run); err != nil {
					return runtimeError(fmt.Errorf("persist eviction retry resume: %w", err))
				}
			}
			if wait := e.evictionRetryWait(run); wait > 0 {
				return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), wait)}
			}
			return engineframework.RuntimeResult{Requeue: true}
		}
		return e.finishPlacement(ctx, run)
	}
	pending := placementexecutor.Candidates(run)
	if len(pending) == 0 {
		// A replacement controller may need time to create the Pod. Keep polling
		// until the durable deadline so an absent replacement cannot bypass the
		// expiration escape hatch.
		klog.V(4).InfoS("repack: no selectable replacement Pod observed yet; placement requeued",
			"run", run.Name, "retryAfter", placementRetryInterval)
		return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
	}

	selected := make(map[placementexecutor.Identity]string, len(pending))
	for _, relocation := range pending {
		selected[placementexecutor.IdentityForRelocation(relocation)] = relocation.PlannedNodeName
	}
	klog.V(4).InfoS("repack: nominating replacements to their planned receivers",
		"run", run.Name, "candidateCount", len(pending))
	changed, err := e.writePlacementSelection(ctx, run.Name, selected)
	if err != nil {
		return runtimeError(err)
	}
	if changed {
		// The status write re-enqueues this run and the nominator opens the gate
		// once it observes the selection; nothing further to do this pass.
		return engineframework.RuntimeResult{}
	}
	// A concurrent writer already nominated every candidate. Keep polling until
	// the durable deadline so a stuck placement cannot bypass expiration.
	return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
}

func hasTimedOutPlacement(run *repackv1alpha1.RepackRun) bool {
	if run == nil {
		return false
	}
	for index := range run.Status.Relocations {
		if run.Status.Relocations[index].Placement.Phase == repackv1alpha1.PodPlacementTimedOut {
			return true
		}
	}
	return false
}

// writePlacementSelection persists the planned receiver of every candidate as
// its SelectedNodeName and reports whether the durable journal changed.
func (e *Engine) writePlacementSelection(
	ctx context.Context,
	runName string,
	selected map[placementexecutor.Identity]string,
) (bool, error) {
	var updatedRun *repackv1alpha1.RepackRun
	written := 0
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		written = 0
		run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for index := range run.Status.Relocations {
			relocation := &run.Status.Relocations[index]
			if node, found := selected[placementexecutor.IdentityForRelocation(relocation)]; found && relocation.Placement.SelectedNodeName == "" {
				relocation.Placement.SelectedNodeName = node
				written++
			}
		}
		if written == 0 {
			return nil
		}
		updatedRun, err = e.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		return err
	})
	if err != nil || updatedRun == nil {
		return false, err
	}
	klog.V(3).InfoS("repack: planned receivers persisted",
		"run", runName, "selectionCount", written)
	e.recordRunEvent(updatedRun, v1.EventTypeNormal, eventReasonPlacementSelected,
		fmt.Sprintf("Selected planned receiver nodes for %d replacement Pods.", written))
	return true, nil
}
