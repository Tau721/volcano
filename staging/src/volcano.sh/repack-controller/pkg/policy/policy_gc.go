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

package policy

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

// gcHistory deletes oldest Succeeded and Failed Runs belonging to the given policy,
// keeping only the most recent up to the history limits (or zero=None).
// Runs are identified by label repack.volcano.sh/repack-policy={policyName}.
func gcHistory(ctx context.Context, volcanoClient vcclientset.Interface,
	runLister repacklisters.RepackRunLister,
	policy *repackv1alpha1.RepackPolicy,
	defaultSuccessLimit, defaultFailedLimit int32) error {

	successLimit := getHistoryLimit(policy.Spec.SuccessfulRunsHistoryLimit, defaultSuccessLimit)
	failedLimit := getHistoryLimit(policy.Spec.FailedRunsHistoryLimit, defaultFailedLimit)

	// List all Runs belonging to this policy.
	req, err := labels.NewRequirement(repackv1alpha1.RepackPolicyLabel, selection.Equals, []string{policy.Name})
	if err != nil {
		return err
	}
	selector := labels.NewSelector().Add(*req)
	runs, err := runLister.List(selector)
	if err != nil {
		return err
	}

	// Partition by phase.
	var succeeded, failed []*repackv1alpha1.RepackRun
	for _, r := range runs {
		switch r.Status.Phase {
		case repackv1alpha1.RepackSucceeded:
			succeeded = append(succeeded, r)
		case repackv1alpha1.RepackFailed:
			failed = append(failed, r)
		}
	}

	// Delete oldest beyond success limit.
	if err := trimRuns(ctx, volcanoClient, succeeded, int(successLimit)); err != nil {
		return err
	}

	// Delete oldest beyond failed limit.
	return trimRuns(ctx, volcanoClient, failed, int(failedLimit))
}

// trimRuns sorts runs by creationTimestamp descending, then deletes the oldest
// beyond the keep count. keep=0 means delete all. Runs not created by this policy
// are ignored (but the caller already filters by label).
func trimRuns(ctx context.Context, volcanoClient vcclientset.Interface,
	runs []*repackv1alpha1.RepackRun, keep int) error {

	if len(runs) <= keep {
		return nil
	}

	// Sort newest-first.
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].CreationTimestamp.After(runs[j].CreationTimestamp.Time)
	})

	// Delete the tail beyond keep.
	for _, r := range runs[keep:] {
		klog.V(4).InfoS("GC: deleting RepackRun", "name", r.Name, "phase", r.Status.Phase)
		if err := volcanoClient.RepackV1alpha1().RepackRuns().Delete(ctx, r.Name, metav1.DeleteOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
	}
	return nil
}