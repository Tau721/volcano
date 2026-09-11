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

// Package placement contains the shared protocol logic used to hold and steer
// replacement Pods during a RepackRun.
package placement

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
)

// PodGroupName returns the PodGroup a Pod can be associated with before the
// podgroup controller has patched scheduling.k8s.io/group-name onto it.
// Explicit association wins. Automatic association is available only for a
// controller-owned Pod because ownerless Pods have no pre-existing PodGroup
// that the admission-time placement protocol can safely inspect.
func PodGroupName(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	if podGroupName := pod.Annotations[schedulingv1beta1.KubeGroupNameAnnotationKey]; podGroupName != "" {
		return podGroupName
	}
	controller := metav1.GetControllerOf(pod)
	if controller == nil || controller.UID == "" {
		return ""
	}
	return batchv1alpha1.PodgroupNamePrefix + string(controller.UID)
}

// OwnerValue returns the stable value shared by a PodGroup placement lease and
// every Pod gate created from that lease.
func OwnerValue(runName string, runUID types.UID) string {
	if runName == "" || runUID == "" {
		return ""
	}
	return runName + "/" + string(runUID)
}

// ParseOwner validates and splits a placement owner value.
func ParseOwner(value string) (runName string, runUID types.UID, ok bool) {
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], types.UID(parts[1]), true
}

// ActiveForPodGroup reports whether a Run still owns an unfinished placement
// in one PodGroup.
func ActiveForPodGroup(run *repackv1alpha1.RepackRun, namespace, podGroupName string) bool {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute || run.Status.Phase != repackv1alpha1.RepackRunning {
		return false
	}
	for i := range run.Status.Relocations {
		relocation := &run.Status.Relocations[i]
		if relocation.Namespace != namespace || !RelocationUsesPodGroup(relocation, podGroupName) {
			continue
		}
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementPlaced,
			repackv1alpha1.PodPlacementTimedOut:
			continue
		default:
			return true
		}
	}
	return false
}

// RelocationUsesPodGroup reports whether podGroupName is either the immutable
// plan-time group or the group that recreated the replacement Pod.
func RelocationUsesPodGroup(relocation *repackv1alpha1.PodRelocationStatus, podGroupName string) bool {
	if relocation == nil || podGroupName == "" {
		return false
	}
	return relocation.PodGroupName == podGroupName ||
		relocation.ReplacementPodGroupName == podGroupName
}

// EvictionAllowsPlacement reports whether a durable relocation is ready to
// claim a replacement Pod. Pending/InProgress/Rejected records continue to
// keep the admission lease active, but cannot steer a Pod.
func EvictionAllowsPlacement(relocation *repackv1alpha1.PodRelocationStatus) bool {
	if relocation == nil {
		return false
	}
	switch relocation.Eviction.Phase {
	case repackv1alpha1.PodEvictionAccepted,
		repackv1alpha1.PodEvictionIndirectlyRemoved:
		return true
	default:
		return false
	}
}

// WorkloadKey is the workload identity used by the current replacement
// protocol. UID is intentionally excluded in P0; deleting and recreating a
// workload under the same name during Execute remains an unsupported boundary.
type WorkloadKey struct {
	Namespace  string
	APIVersion string
	Kind       string
	Name       string
}

func (key WorkloadKey) Empty() bool {
	return key.Namespace == "" || key.APIVersion == "" || key.Kind == "" || key.Name == ""
}

func workloadKeyForMove(move *repackv1alpha1.RepackMove) WorkloadKey {
	if move == nil || move.Owner == nil {
		return WorkloadKey{}
	}
	return WorkloadKey{
		Namespace:  move.Namespace,
		APIVersion: move.Owner.APIVersion,
		Kind:       move.Owner.Kind,
		Name:       move.Owner.Name,
	}
}

// WorkloadKeyForPodGroup returns the direct controller owner recorded on a
// PodGroup. Repack stays workload-kind agnostic and never traverses owner chains.
func WorkloadKeyForPodGroup(podGroup *schedulingv1beta1.PodGroup) WorkloadKey {
	if podGroup == nil {
		return WorkloadKey{}
	}
	owner := metav1.GetControllerOf(podGroup)
	if owner == nil {
		return WorkloadKey{}
	}
	return WorkloadKey{
		Namespace:  podGroup.Namespace,
		APIVersion: owner.APIVersion,
		Kind:       owner.Kind,
		Name:       owner.Name,
	}
}

// SourcePodGroupsByWorkload groups the original PodGroups by workload owner.
// status.plan.moves is the authoritative affected set; no duplicate status list
// is needed for replacement discovery.
func SourcePodGroupsByWorkload(run *repackv1alpha1.RepackRun) map[WorkloadKey][]string {
	result := map[WorkloadKey][]string{}
	if run == nil || run.Status.Plan == nil {
		return result
	}
	for index := range run.Status.Plan.Moves {
		move := &run.Status.Plan.Moves[index]
		key := workloadKeyForMove(move)
		if key.Empty() || move.PodGroupName == "" {
			continue
		}
		result[key] = appendUnique(result[key], move.PodGroupName)
	}
	return result
}

// HasPendingPlacementsForWorkload reports whether a workload still owns an unfinished
// placement. Concrete Pod-to-nomination claiming remains the nominator's job.
func HasPendingPlacementsForWorkload(run *repackv1alpha1.RepackRun, workload WorkloadKey) bool {
	if run == nil || workload.Empty() {
		return false
	}
	sources := SourcePodGroupsByWorkload(run)[workload]
	sourceSet := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		sourceSet[source] = struct{}{}
	}
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if nomination.Namespace != workload.Namespace || PlacementReachedTerminalPhase(nomination) {
			continue
		}
		if _, found := sourceSet[nomination.PodGroupName]; found {
			return true
		}
	}
	return false
}

// PlacementAppliesToPodGroup reports whether the active Run covers a PodGroup.
// Callers validate or inject the lease separately. A newly
// recreated PodGroup is accepted by workload owner while unfinished relocations
// remain, closing the admission window before ReplacementPodGroupName is durable.
func PlacementAppliesToPodGroup(run *repackv1alpha1.RepackRun, podGroup *schedulingv1beta1.PodGroup) bool {
	if run == nil || podGroup == nil ||
		run.Spec.Mode != repackv1alpha1.RepackModeExecute ||
		run.Status.Phase != repackv1alpha1.RepackRunning {
		return false
	}
	if ActiveForPodGroup(run, podGroup.Namespace, podGroup.Name) {
		return true
	}
	// An exact lease owner is stronger evidence than the creation-time guard.
	// This is important immediately after PodGroup admission: the CREATE webhook
	// may have already attached the current Run's lease while the durable
	// ReplacementPodGroupName mapping is still pending. Rejecting that same
	// PodGroup during Pod admission would open a window where its first Pod is
	// created without the placement gate.
	expectedLease := OwnerValue(run.Name, run.UID)
	ownedLease := expectedLease != "" &&
		podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] == expectedLease
	if !ownedLease && run.Status.StartTime != nil && !podGroup.CreationTimestamp.IsZero() &&
		podGroup.CreationTimestamp.Time.Before(run.Status.StartTime.Time) {
		return false
	}
	return HasPendingPlacementsForWorkload(run, WorkloadKeyForPodGroup(podGroup))
}

// PlacementReachedTerminalPhase is intentionally different from the
// nominator's nominationUnavailableForClaim predicate: Nominated has claimed a
// concrete Pod but is not terminal until the scheduler's binding is observed.
func PlacementReachedTerminalPhase(nomination *repackv1alpha1.PodRelocationStatus) bool {
	if nomination == nil {
		return true
	}
	switch nomination.Placement.Phase {
	case repackv1alpha1.PodPlacementPlaced,
		repackv1alpha1.PodPlacementTimedOut:
		return true
	default:
		return false
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
