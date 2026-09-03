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

package status

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
)

// Stage is the durable Repack workflow stage derived from RepackRun status.
// It is intentionally not another persisted state field: the existing plan,
// relocation journal and conditions remain the source of truth.
type Stage string

const (
	StageNone     Stage = ""
	StagePlanning Stage = "Planning"
	StageEvicting Stage = "Evicting"
	StagePlacing  Stage = "Placing"
	StageCleanup  Stage = "Cleanup"
)

// ResolveStage is the single workflow classification entry used by the Engine
// queue and the Repack Action. Keeping classification here prevents normal and
// recovery paths from drifting apart.
func ResolveStage(run *repackv1alpha1.RepackRun) Stage {
	if run == nil {
		return StageNone
	}
	if cleanupRequired(run) {
		return StageCleanup
	}
	// Accepted evictions are restored before the next disruption retry. Once all
	// currently accepted replacements are terminal, unfinished evictions resume.
	if acceptedPlacementPending(run) {
		return StagePlacing
	}
	if evictionPending(run) {
		return StageEvicting
	}
	if placementPending(run) {
		return StagePlacing
	}
	phase := run.Status.Phase
	if phase == "" || phase == repackv1alpha1.RepackPending ||
		(phase == repackv1alpha1.RepackRunning && run.Status.Plan == nil) {
		return StagePlanning
	}
	return StageNone
}

func acceptedPlacementPending(run *repackv1alpha1.RepackRun) bool {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute ||
		run.Status.Phase != repackv1alpha1.RepackRunning {
		return false
	}
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if relocation.Eviction.Phase != repackv1alpha1.PodEvictionAccepted &&
			relocation.Eviction.Phase != repackv1alpha1.PodEvictionIndirectlyRemoved {
			continue
		}
		if relocation.Placement.Phase != repackv1alpha1.PodPlacementPlaced &&
			relocation.Placement.Phase != repackv1alpha1.PodPlacementTimedOut {
			return true
		}
	}
	return false
}

func ShouldReconcile(run *repackv1alpha1.RepackRun) bool {
	return ResolveStage(run) != StageNone
}

func evictionPending(run *repackv1alpha1.RepackRun) bool {
	if run.Spec.Mode != repackv1alpha1.RepackModeExecute ||
		run.Status.Phase != repackv1alpha1.RepackRunning || run.Status.Plan == nil {
		return false
	}
	evictionJournalPresent := false
	for index := range run.Status.Relocations {
		phase := run.Status.Relocations[index].Eviction.Phase
		if phase != "" {
			evictionJournalPresent = true
		}
		if phase == repackv1alpha1.PodEvictionPending || phase == repackv1alpha1.PodEvictionInProgress {
			return true
		}
	}
	if !evictionJournalPresent {
		return false
	}
	for index := range run.Status.Conditions {
		condition := &run.Status.Conditions[index]
		if condition.Type == state.CondProgressing && condition.Status == metav1.ConditionTrue &&
			condition.Reason == state.ReasonReconcilingPlacements {
			return false
		}
	}
	// Every outcome may be final while the accepted subset and placement barrier
	// are not yet durable. Resume eviction finalization in that window.
	return true
}

func placementPending(run *repackv1alpha1.RepackRun) bool {
	return run.Spec.Mode == repackv1alpha1.RepackModeExecute &&
		run.Status.Phase == repackv1alpha1.RepackRunning && run.Status.Plan != nil &&
		len(run.Status.Relocations) > 0 && !evictionPending(run)
}

func cleanupRequired(run *repackv1alpha1.RepackRun) bool {
	if run.Spec.Mode != repackv1alpha1.RepackModeExecute ||
		(run.Status.Phase != repackv1alpha1.RepackSucceeded &&
			run.Status.Phase != repackv1alpha1.RepackFailed) {
		return false
	}
	if run.Labels[repackv1alpha1.PlacementActiveLabel] == "true" {
		return true
	}
	return ExecutePreparationCleanupPending(run)
}

// ExecutePreparationCleanupPending identifies a terminal Execute whose durable
// relocation journal was prepared but whose Eviction API calls never started.
// The journal is the existing recovery evidence for a lease preparation that
// may have partially succeeded before placement-active was published. Cleanup
// clears this unperformed journal only after every engine-owned lease is gone,
// so no additional persisted cleanup marker is required.
func ExecutePreparationCleanupPending(run *repackv1alpha1.RepackRun) bool {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute ||
		(run.Status.Phase != repackv1alpha1.RepackSucceeded &&
			run.Status.Phase != repackv1alpha1.RepackFailed) ||
		len(run.Status.Relocations) == 0 {
		return false
	}
	for index := range run.Status.Relocations {
		if run.Status.Relocations[index].Eviction.Phase != repackv1alpha1.PodEvictionPending {
			return false
		}
	}
	return true
}
