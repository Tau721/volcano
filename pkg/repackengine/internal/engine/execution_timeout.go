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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
)

func executionDeadlinePassed(run *repackv1alpha1.RepackRun, now time.Time) bool {
	return run != nil && run.Status.ExecutionDeadline != nil &&
		!now.Before(run.Status.ExecutionDeadline.Time)
}

// timeoutExecution performs one final live victim observation before closing
// the Run. This narrows the race where an accepted eviction becomes visible at
// the deadline, while still enforcing one wall-clock budget for all execution
// stages.
func (e *Engine) timeoutExecution(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	generation int64,
	kubernetesClient kubernetes.Interface,
) error {
	if run == nil {
		return nil
	}
	if kubernetesClient != nil {
		for _, victim := range plannedVictims(run) {
			relocation := &run.Status.Relocations[victim.relocationIndex]
			if relocation.Eviction.Phase != repackv1alpha1.PodEvictionInProgress {
				continue
			}
			pod, err := kubernetesClient.CoreV1().Pods(victim.namespace).Get(
				ctx, victim.podName, metav1.GetOptions{})
			originalGone := apierrors.IsNotFound(err)
			if err == nil {
				originalGone = (relocation.VictimPodUID != "" && pod.UID != relocation.VictimPodUID) ||
					(pod.DeletionTimestamp != nil &&
						(relocation.VictimPodUID == "" || pod.UID == relocation.VictimPodUID))
			}
			if originalGone {
				setEvictionOutcome(relocation, repackv1alpha1.PodEvictionAccepted,
					"The original victim was terminating or gone at the execution deadline; the durable eviction intent is treated as accepted.")
			}
		}
	}

	unfinishedEvictions, unfinishedPlacements := 0, 0
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		switch relocation.Eviction.Phase {
		case repackv1alpha1.PodEvictionPending, repackv1alpha1.PodEvictionInProgress:
			unfinishedEvictions++
			setEvictionOutcome(relocation, repackv1alpha1.PodEvictionRejected,
				"Execution deadline reached before this eviction completed.")
		case repackv1alpha1.PodEvictionAccepted, repackv1alpha1.PodEvictionIndirectlyRemoved:
			if relocation.Placement.Phase != repackv1alpha1.PodPlacementPlaced &&
				relocation.Placement.Phase != repackv1alpha1.PodPlacementTimedOut {
				if kubernetesClient != nil && relocation.Placement.ReplacementPodName != "" {
					replacement, err := kubernetesClient.CoreV1().Pods(relocation.Namespace).Get(
						ctx, relocation.Placement.ReplacementPodName, metav1.GetOptions{})
					if err == nil && replacement.DeletionTimestamp == nil && replacement.Spec.NodeName != "" &&
						(relocation.Placement.ReplacementPodUID == "" ||
							replacement.UID == relocation.Placement.ReplacementPodUID) {
						relocation.Placement.ReplacementPodUID = replacement.UID
						relocation.Placement.ActualNodeName = replacement.Spec.NodeName
						relocation.Placement.Phase = repackv1alpha1.PodPlacementPlaced
						continue
					}
				}
				unfinishedPlacements++
				relocation.Placement.Phase = repackv1alpha1.PodPlacementTimedOut
			}
		}
	}
	initializeExecuteResultFromStatus(run)
	placementexecutor.MarkBenefitUnverified(run)
	e.clearEvictionRetry(run)
	unfinishedVerification := 0
	if unfinishedEvictions == 0 && unfinishedPlacements == 0 {
		unfinishedVerification = 1
	}
	return e.fail(ctx, run, generation, state.ReasonExecutionTimedOut,
		fmt.Errorf("execution exceeded its deadline with %d unfinished evictions, %d unfinished replacement placements, and %d unfinished result verifications",
			unfinishedEvictions, unfinishedPlacements, unfinishedVerification))
}
