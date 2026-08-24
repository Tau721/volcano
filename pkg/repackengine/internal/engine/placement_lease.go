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
	"encoding/json"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	"volcano.sh/repack-controller/pkg/placement"
)

// placementRepairLimiter independently rate-limits the fallback scan that
// repairs placement leases on recreated PodGroups.
type placementRepairLimiter struct {
	runIdentity string
	lastRepair  time.Time
}

// allow records an accepted repair scan. When rejected, next is the earliest
// time the same RepackRun may scan again.
func (limiter *placementRepairLimiter) allow(run *repackv1alpha1.RepackRun, now time.Time, interval time.Duration) (allowed bool, next time.Time) {
	if run == nil {
		return false, time.Time{}
	}
	runIdentity := run.Name + "/" + string(run.UID)
	next = limiter.lastRepair.Add(interval)
	if limiter.runIdentity == runIdentity && now.Before(next) {
		return false, next
	}
	limiter.runIdentity = runIdentity
	limiter.lastRepair = now
	return true, now.Add(interval)
}

// preparePlacementLeases marks every affected PodGroup before any victim is
// evicted. The Pod mutating webhook reads this lease and injects a scheduling
// gate into a subsequently-created replacement Pod. The lease value contains
// both name and UID so a stale PodGroup annotation cannot accidentally attach a
// new RepackRun with the same name.
func (e *Engine) preparePlacementLeases(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	if run == nil || run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return nil
	}
	lease := placement.OwnerValue(run.Name, run.UID)
	groups := placementPodGroups(run)
	klog.V(4).InfoS("repack: preparing PodGroup placement leases",
		"run", run.Name, "podGroupCount", len(groups), "lease", lease)
	for podGroupKey := range groups {
		namespace, podGroupName := podGroupKey.Namespace, podGroupKey.Name
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			podGroup, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, podGroupName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			current := podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation]
			if current != "" && current != lease {
				active, err := e.placementLeaseActive(ctx, current, namespace, podGroupName)
				if err != nil {
					return err
				}
				if active {
					return fmt.Errorf("PodGroup %s/%s already has active placement lease %q", namespace, podGroupName, current)
				}
				klog.InfoS("repack: replacing stale placement lease", "podGroup", namespace+"/"+podGroupName, "staleLease", current, "newLease", lease)
			}
			if current == lease {
				return nil
			}
			podGroup = podGroup.DeepCopy()
			if podGroup.Annotations == nil {
				podGroup.Annotations = map[string]string{}
			}
			podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] = lease
			_, err = e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Update(ctx, podGroup, metav1.UpdateOptions{})
			return err
		}); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("prepare placement lease: PodGroup %s/%s no longer exists", namespace, podGroupName)
			}
			return err
		}
		klog.V(4).InfoS("repack: PodGroup placement lease prepared",
			"run", run.Name, "podGroup", namespace+"/"+podGroupName)
	}
	return nil
}

// placementLeaseActive distinguishes a live Execute from an annotation left
// behind by a deleted Run, a terminal Run, or an interrupted cleanup. Lease
// ownership includes the UID, so reusing a Run name is never treated as live.
func (e *Engine) placementLeaseActive(ctx context.Context, lease, namespace, podGroupName string) (bool, error) {
	runName, runUID, ok := placement.ParseOwner(lease)
	if !ok {
		return false, nil
	}
	run, err := e.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil || run.UID != runUID || run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return false, err
	}
	return placement.ActiveForPodGroup(run, namespace, podGroupName), nil
}

func placementPodGroups(run *repackv1alpha1.RepackRun) map[types.NamespacedName]struct{} {
	groups := make(map[types.NamespacedName]struct{})
	if run == nil {
		return groups
	}
	for i := range run.Status.Relocations {
		nomination := &run.Status.Relocations[i]
		if nomination.Namespace != "" && nomination.PodGroupName != "" {
			groups[types.NamespacedName{Namespace: nomination.Namespace, Name: nomination.PodGroupName}] = struct{}{}
		}
		if nomination.Namespace != "" && nomination.ReplacementPodGroupName != "" {
			groups[types.NamespacedName{Namespace: nomination.Namespace, Name: nomination.ReplacementPodGroupName}] = struct{}{}
		}
	}
	return groups
}

func placementGroupsDifference(
	all, retain map[types.NamespacedName]struct{},
) map[types.NamespacedName]struct{} {
	result := make(map[types.NamespacedName]struct{})
	for key := range all {
		if _, stillNeeded := retain[key]; !stillNeeded {
			result[key] = struct{}{}
		}
	}
	return result
}

type placementLeaseReleaseOutcome int

const (
	placementLeaseReleased placementLeaseReleaseOutcome = iota
	placementLeaseAlreadyAbsent
	placementLeasePodGroupNotFound
	placementLeaseNotOwned
)

// releasePlacementLeases removes only annotations owned by this Run. It is safe
// to call on every terminal/error path: a newer Run's lease is never touched.
func (e *Engine) releasePlacementLeases(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	groups map[types.NamespacedName]struct{},
) error {
	if run == nil || len(groups) == 0 {
		return nil
	}
	lease := placement.OwnerValue(run.Name, run.UID)
	releasedCount, alreadyAbsentCount, notFoundCount, notOwnedCount := 0, 0, 0, 0
	for podGroupKey := range groups {
		namespace, podGroupName := podGroupKey.Namespace, podGroupKey.Name
		outcome := placementLeaseAlreadyAbsent
		observedLease := ""
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			outcome = placementLeaseAlreadyAbsent
			observedLease = ""
			podGroup, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, podGroupName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				outcome = placementLeasePodGroupNotFound
				return nil
			}
			if err != nil {
				return err
			}
			observedLease = podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation]
			if observedLease == "" {
				outcome = placementLeaseAlreadyAbsent
				return nil
			}
			if observedLease != lease {
				outcome = placementLeaseNotOwned
				return nil
			}
			podGroup = podGroup.DeepCopy()
			delete(podGroup.Annotations, repackv1alpha1.PlacementLeaseAnnotation)
			_, err = e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Update(ctx, podGroup, metav1.UpdateOptions{})
			if err == nil {
				outcome = placementLeaseReleased
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("release placement lease for PodGroup %s/%s: %w", namespace, podGroupName, err)
		}
		switch outcome {
		case placementLeaseReleased:
			releasedCount++
			klog.V(4).InfoS("repack: PodGroup placement lease released",
				"run", run.Name, "podGroup", namespace+"/"+podGroupName)
		case placementLeaseAlreadyAbsent:
			alreadyAbsentCount++
			klog.V(4).InfoS("repack: PodGroup placement lease was already absent",
				"run", run.Name, "podGroup", namespace+"/"+podGroupName)
		case placementLeasePodGroupNotFound:
			notFoundCount++
			klog.V(4).InfoS("repack: PodGroup already deleted during placement lease cleanup",
				"run", run.Name, "podGroup", namespace+"/"+podGroupName)
		case placementLeaseNotOwned:
			notOwnedCount++
			klog.V(3).InfoS("repack: PodGroup placement lease belongs to another owner; cleanup skipped",
				"run", run.Name, "podGroup", namespace+"/"+podGroupName, "observedLease", observedLease)
		}
	}
	klog.V(3).InfoS("repack: placement lease cleanup completed",
		"run", run.Name, "requestedPodGroupCount", len(groups),
		"releasedCount", releasedCount, "alreadyAbsentCount", alreadyAbsentCount,
		"notFoundCount", notFoundCount, "notOwnedCount", notOwnedCount)
	return nil
}

// cleanupPlacement removes engine-owned PodGroup leases. Pod gate lifecycle is
// owned exclusively by the nomination controller, which watches terminal and
// deleted Runs through the gate-owner Pod index.
func (e *Engine) cleanupPlacement(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	groups, err := e.ownedPlacementLeaseGroups(ctx, run)
	if err != nil {
		return err
	}
	if err := e.releasePlacementLeases(ctx, run, groups); err != nil {
		return err
	}
	return e.setPlacementActive(ctx, run, false)
}

// ownedPlacementLeaseGroups includes both status-recorded groups and admission-
// time candidate groups that were never claimed by a nomination (for example a
// concurrent scale-out). Terminal cleanup must not leave those Pods gated.
func (e *Engine) ownedPlacementLeaseGroups(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
) (map[types.NamespacedName]struct{}, error) {
	groups := placementPodGroups(run)
	if run == nil || e.volcanoClient == nil {
		return groups, nil
	}
	lease := placement.OwnerValue(run.Name, run.UID)
	namespaces := map[string]struct{}{}
	if run.Status.Plan != nil {
		for index := range run.Status.Plan.Moves {
			if namespace := run.Status.Plan.Moves[index].Namespace; namespace != "" {
				namespaces[namespace] = struct{}{}
			}
		}
	}
	for namespace := range namespaces {
		podGroups, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list PodGroups in namespace %s for placement cleanup: %w", namespace, err)
		}
		for index := range podGroups.Items {
			podGroup := &podGroups.Items[index]
			if podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] == lease {
				groups[types.NamespacedName{Namespace: namespace, Name: podGroup.Name}] = struct{}{}
			}
		}
	}
	return groups, nil
}

// setPlacementActive maintains the metadata index used by the PodGroup webhook.
// The label is not authoritative: webhooks still validate phase, Run UID, owner,
// creation time, and unfinished relocations.
func (e *Engine) setPlacementActive(ctx context.Context, run *repackv1alpha1.RepackRun, active bool) error {
	if run == nil || e.volcanoClient == nil {
		return nil
	}
	value := interface{}(nil)
	if active {
		value = "true"
	}
	body, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				repackv1alpha1.PlacementActiveLabel: value,
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = e.volcanoClient.RepackV1alpha1().RepackRuns().Patch(
		ctx, run.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("set placement-active=%t on RepackRun %s: %w", active, run.Name, err)
	}
	klog.V(4).InfoS("repack: placement discovery label reconciled",
		"run", run.Name, "active", active)
	return nil
}

const placementLeaseRepairInterval = 30 * time.Second

// repairRecreatedPodGroupLeasesIfDue is the eventual-consistency fallback for a
// PodGroup CREATE that bypassed or raced admission mutation. The webhook remains
// the primary barrier; this independently rate-limited repair protects later
// Pods without placing namespace-wide LIST load on every placement retry.
func (e *Engine) repairRecreatedPodGroupLeasesIfDue(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	if run == nil || run.Status.Plan == nil {
		return nil
	}
	pendingWorkload := false
	for workload := range placement.SourcePodGroupsByWorkload(run) {
		if placement.HasPendingPlacementsForWorkload(run, workload) {
			pendingWorkload = true
			break
		}
	}
	if !pendingWorkload || !e.placementLeaseRepairDue(run) {
		return nil
	}

	lease := placement.OwnerValue(run.Name, run.UID)
	namespaces := map[string]struct{}{}
	for index := range run.Status.Plan.Moves {
		namespaces[run.Status.Plan.Moves[index].Namespace] = struct{}{}
	}
	scannedPodGroupCount, repairedLeaseCount, conflictingLeaseCount := 0, 0, 0
	klog.V(4).InfoS("repack: scanning for recreated PodGroups that missed admission-time placement lease",
		"run", run.Name, "namespaceCount", len(namespaces))
	for namespace := range namespaces {
		podGroups, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		scannedPodGroupCount += len(podGroups.Items)
		for index := range podGroups.Items {
			podGroup := &podGroups.Items[index]
			if !placement.PlacementAppliesToPodGroup(run, podGroup) ||
				podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] == lease {
				continue
			}
			if current := podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation]; current != "" {
				conflictingLeaseCount++
				klog.V(3).InfoS("repack: recreated PodGroup has a different placement lease; repair skipped",
					"run", run.Name, "podGroup", namespace+"/"+podGroup.Name, "lease", current)
				continue
			}
			repaired := false
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest, err := e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(ctx, podGroup.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if latest.Annotations[repackv1alpha1.PlacementLeaseAnnotation] != "" {
					return nil
				}
				latest = latest.DeepCopy()
				if latest.Annotations == nil {
					latest.Annotations = map[string]string{}
				}
				latest.Annotations[repackv1alpha1.PlacementLeaseAnnotation] = lease
				_, err = e.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Update(ctx, latest, metav1.UpdateOptions{})
				repaired = err == nil
				return err
			}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("repair placement lease for PodGroup %s/%s: %w", namespace, podGroup.Name, err)
			}
			if !repaired {
				continue
			}
			repairedLeaseCount++
			klog.V(3).InfoS("repack: repaired placement lease on recreated PodGroup",
				"run", run.Name, "podGroup", namespace+"/"+podGroup.Name, "lease", lease)
			e.recordRunEvent(run, v1.EventTypeNormal, eventReasonPlacementLeaseRepaired,
				fmt.Sprintf("Repaired placement lease on recreated PodGroup %s/%s.", namespace, podGroup.Name))
		}
	}
	klog.V(4).InfoS("repack: recreated PodGroup placement lease repair scan completed",
		"run", run.Name, "namespaceCount", len(namespaces),
		"scannedPodGroupCount", scannedPodGroupCount,
		"repairedLeaseCount", repairedLeaseCount,
		"conflictingLeaseCount", conflictingLeaseCount)
	return nil
}

func (e *Engine) placementLeaseRepairDue(run *repackv1alpha1.RepackRun) bool {
	if run == nil {
		return false
	}
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	allowed, nextRepairTime := e.placementRepairLimiter.allow(run, now, placementLeaseRepairInterval)
	if !allowed {
		klog.V(5).InfoS("repack: recreated PodGroup lease repair scan rate-limited",
			"run", run.Name, "nextRepairTime", nextRepairTime)
		return false
	}
	return true
}

const placementRetryInterval = 2 * time.Second

// reconcilePlacement is the engine-owned dynamic placement decision. The
// controller only reports gated replacement Pods; this method opens the same
// scheduler session used by planning and chooses a current, immediately-idle
// receiver while excluding the nodes this Run is trying to free.
