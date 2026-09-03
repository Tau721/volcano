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

// Package status owns RepackRun status projection and persistence, including
// merge rules for independently updated eviction and placement progress.
package status

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"

	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
)

type Store struct {
	client vcclientset.Interface
}

func NewStore(client vcclientset.Interface) *Store {
	return &Store{client: client}
}

// Write persists desired status against the freshest RepackRun and preserves
// concurrent placement progress written from another informer observation.
func (s *Store) Write(ctx context.Context, name string, desired *repackv1alpha1.RepackRunStatus) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest, err := s.client.RepackV1alpha1().RepackRuns().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		merged := desired.DeepCopy()
		MergeRelocationProgress(merged.Relocations, latest.Status.Relocations)
		merged.DeepCopyInto(&latest.Status)
		_, err = s.client.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

// MergeRelocationProgress preserves two independently owned journals when an
// Engine status retry races with placement progress updates.
func MergeRelocationProgress(desired, latest []repackv1alpha1.PodRelocationStatus) {
	placements := make(map[placementexecutor.Identity]repackv1alpha1.PodRelocationStatus, len(latest))
	evictions := make(map[placementexecutor.Identity]repackv1alpha1.PodRelocationStatus, len(latest))
	replacements := make(map[placementexecutor.Identity]string, len(latest))
	for index := range latest {
		relocation := &latest[index]
		identity := placementexecutor.IdentityForRelocation(relocation)
		evictions[identity] = *relocation
		if relocation.ReplacementPodGroupName != "" {
			replacements[identity] = relocation.ReplacementPodGroupName
		}
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementWaitingForNodeSelection,
			repackv1alpha1.PodPlacementNominated,
			repackv1alpha1.PodPlacementPlaced,
			repackv1alpha1.PodPlacementTimedOut:
			placements[identity] = *relocation
		}
	}
	for index := range desired {
		identity := placementexecutor.IdentityForRelocation(&desired[index])
		if persisted, found := evictions[identity]; found {
			persistedPhase := persisted.Eviction.Phase
			desiredPhase := desired[index].Eviction.Phase
			if persistedPhase == desiredPhase {
				// Eviction is engine-owned. Preserve the durable UID while allowing
				// same-phase detail (notably a repeated InProgress PDB error) to be
				// refreshed by the current batch outcome.
				desired[index].VictimPodUID = persisted.VictimPodUID
			} else if persistedPhase != "" && EvictionPhaseAdvances(desiredPhase, persistedPhase) {
				desired[index].VictimPodUID = persisted.VictimPodUID
				desired[index].Eviction.Phase = persistedPhase
				desired[index].Eviction.Message = persisted.Eviction.Message
			}
		}
		if replacementPodGroupName := replacements[identity]; replacementPodGroupName != "" {
			desired[index].ReplacementPodGroupName = replacementPodGroupName
		}
		if placement, found := placements[identity]; found {
			latestPhase := placement.Placement.Phase
			desiredPhase := desired[index].Placement.Phase
			if latestPhase == desiredPhase || PlacementPhaseAdvances(desiredPhase, latestPhase) ||
				(placementTerminal(latestPhase) && placementTerminal(desiredPhase)) {
				desired[index].Placement = placement.Placement
			}
		}
	}
}

func placementTerminal(phase repackv1alpha1.PodPlacementPhase) bool {
	return phase == repackv1alpha1.PodPlacementPlaced || phase == repackv1alpha1.PodPlacementTimedOut
}

func PlacementPhaseAdvances(current, observed repackv1alpha1.PodPlacementPhase) bool {
	switch observed {
	case repackv1alpha1.PodPlacementWaitingForReplacement:
		return current == ""
	case repackv1alpha1.PodPlacementWaitingForNodeSelection:
		return current == "" || current == repackv1alpha1.PodPlacementWaitingForReplacement
	case repackv1alpha1.PodPlacementNominated:
		return current == "" || current == repackv1alpha1.PodPlacementWaitingForReplacement || current == repackv1alpha1.PodPlacementWaitingForNodeSelection
	case repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementTimedOut:
		return current == "" || current == repackv1alpha1.PodPlacementWaitingForReplacement ||
			current == repackv1alpha1.PodPlacementWaitingForNodeSelection || current == repackv1alpha1.PodPlacementNominated
	}
	return false
}

func EvictionPhaseAdvances(current, observed repackv1alpha1.PodEvictionPhase) bool {
	switch observed {
	case repackv1alpha1.PodEvictionPending:
		return current == ""
	case repackv1alpha1.PodEvictionInProgress:
		return current == "" || current == repackv1alpha1.PodEvictionPending
	case repackv1alpha1.PodEvictionAccepted, repackv1alpha1.PodEvictionIndirectlyRemoved, repackv1alpha1.PodEvictionRejected:
		return current == "" || current == repackv1alpha1.PodEvictionPending || current == repackv1alpha1.PodEvictionInProgress
	}
	return false
}
