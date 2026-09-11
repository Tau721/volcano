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
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	repackstate "volcano.sh/volcano/pkg/controllers/repack/state"
)

// runNameFormat renders derived run names {policy}-{now-UTC-seconds}: second-unique
// and free of trigger semantics.
const runNameFormat = "20060102150405"

// reconcile reconciles one RepackPolicy (its steps are annotated inline).
func (c *Controller) reconcile(ctx context.Context, policyName string) error {
	// Step 1: fetch from the lister. A deleted policy (NotFound) is a silent exit.
	sharedPolicy, err := c.policyLister.Get(policyName)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	pol := sharedPolicy.DeepCopy() // never mutate the shared cache object
	status := &pol.Status

	changed := false
	setCondition := func(condStatus metav1.ConditionStatus, reason, message string) {
		if repackstate.SetCondition(&status.Conditions, repackv1alpha1.CondHealthy, condStatus, reason, message, pol.Generation) {
			changed = true
		}
	}
	healthy := func(reason, message string) { setCondition(metav1.ConditionTrue, reason, message) }
	unhealthy := func(reason, message string) { setCondition(metav1.ConditionFalse, reason, message) }
	updateLastSuccessful := func(run *repackv1alpha1.RepackRun) {
		if run.Status.Phase != repackv1alpha1.RepackSucceeded || run.Status.CompletionTime == nil {
			return
		}
		if status.LastSuccessfulTime == nil || run.Status.CompletionTime.After(status.LastSuccessfulTime.Time) {
			status.LastSuccessfulTime = run.Status.CompletionTime.DeepCopy()
			changed = true
		}
	}
	persist := func() error {
		if !changed {
			return nil
		}
		if _, err := c.vcClient.RepackV1alpha1().RepackPolicies().UpdateStatus(ctx, pol, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // policy deleted meanwhile; kube GC owns its runs
			}
			return err
		}
		return nil
	}
	// Common exit for active paths: re-arm the self-sustaining wakeup before
	// persist — losing the timer would stop the clock, an extra stale one is
	// harmless.
	finish := func() error {
		c.scheduleWakeup(pol)
		return persist()
	}

	// Step 2: history maintenance runs first so the concurrency gate and the
	// recycling below see the freshest state within the same reconcile.
	//
	// 2a. Converge inProgress[]: snapshot terminal runs, drop vanished ones
	// (without a snapshot), keep the rest.
	kept := make([]corev1.ObjectReference, 0, len(status.InProgress))
	var newestTerminal *repackv1alpha1.RepackRun
	for _, ref := range status.InProgress {
		run, err := c.runLister.Get(ref.Name)
		if apierrors.IsNotFound(err) {
			// Deleted before its terminal state was observed: nothing to mirror.
			changed = true
			continue
		}
		if err != nil {
			return err
		}
		if !repackstate.IsTerminal(run.Status.Phase) {
			kept = append(kept, ref)
			continue
		}
		changed = true
		updateLastSuccessful(run)
		if newestTerminal == nil || newerTerminal(run, newestTerminal) {
			newestTerminal = run
		}
	}
	if newestTerminal != nil {
		status.LastRunStatus = snapshotRun(newestTerminal)
	}
	if len(kept) != len(status.InProgress) {
		status.InProgress = kept
	}

	// 2b. List every derived run (reserved label), mirror a terminal orphan that
	// finished while the controller was down, then recycle the oldest beyond the
	// per-phase history limits.
	runs, err := c.runLister.List(labels.SelectorFromSet(labels.Set{repackv1alpha1.RepackPolicyLabel: policyName}))
	if err != nil {
		return err
	}
	var orphanTerminal *repackv1alpha1.RepackRun
	for _, run := range runs {
		if run.Status.CompletionTime == nil || !repackstate.IsTerminal(run.Status.Phase) || !ownedByPolicy(run, pol.UID) {
			continue
		}
		if recorded := status.LastRunStatus; recorded != nil && recorded.CompletionTime != nil &&
			!run.Status.CompletionTime.After(recorded.CompletionTime.Time) {
			continue // already reflected in the snapshot
		}
		if orphanTerminal == nil || newerTerminal(run, orphanTerminal) {
			orphanTerminal = run
		}
	}
	if orphanTerminal != nil {
		// Deterministic max-completion orphan, mirrored once; advancing
		// lastTriggerTime stops a restart from re-firing the consumed fire.
		status.LastRunStatus = snapshotRun(orphanTerminal)
		updateLastSuccessful(orphanTerminal)
		status.LastTriggerTime = &metav1.Time{Time: c.now()}
		changed = true
	}
	limits := map[repackv1alpha1.RepackPhase]*int32{
		repackv1alpha1.RepackSucceeded: pol.Spec.SuccessfulRunsHistoryLimit,
		repackv1alpha1.RepackFailed:    pol.Spec.FailedRunsHistoryLimit,
	}
	for phase, group := range groupByPhase(runs) {
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].CreationTimestamp.Before(&group[j].CreationTimestamp)
		})
		limit := len(group)
		if limits[phase] != nil {
			limit = int(*limits[phase])
		}
		for i := 0; i < len(group)-limit; i++ {
			if err := c.vcClient.RepackV1alpha1().RepackRuns().Delete(ctx, group[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	// Step 3: suspend stops everything — no evaluation, no creation, no wakeup.
	// Step 2 still ran above, so terminal runs keep converging while suspended.
	if pol.Spec.Suspend != nil && *pol.Spec.Suspend {
		healthy(repackv1alpha1.ReasonReconcileSucceeded, "Suspended")
		return persist()
	}

	// Step 4: trigger evaluation. Cron is evaluated first and short-circuits; the
	// frag source is only evaluated when cron does not create.
	now := c.now()
	spec := pol.Spec
	triggered := false
	triggerSource := ""
	var cronParseErr error

	if spec.Trigger.CronSchedule != nil && *spec.Trigger.CronSchedule != "" {
		anchor := pol.CreationTimestamp.Time
		if status.LastTriggerTime != nil && status.LastTriggerTime.Time.After(anchor) {
			anchor = status.LastTriggerTime.Time
		}
		nextFire, err := nextCronFire(*spec.Trigger.CronSchedule, anchor)
		if err != nil {
			cronParseErr = err
		} else if !now.Before(nextFire) && (status.LastTriggerTime == nil || nextFire.After(status.LastTriggerTime.Time)) {
			triggered = true
			triggerSource = TriggerCronSchedule
		}
	}

	// onFrag: cluster-wide rate strictly above the threshold, throttled to at
	// most one create per eval cycle (anchor = lastTriggerTime).
	resource := policyResource(spec)
	fragConfigured := spec.Trigger.OnFragAbovePercent != nil
	threshold := 0.0
	rate := FragResult{}
	var fragErr error
	if !triggered && fragConfigured {
		if resource != "" {
			rate, fragErr = c.measureFrag(resource)
			threshold = float64(*spec.Trigger.OnFragAbovePercent) / 100
			throttled := status.LastTriggerTime != nil && now.Sub(status.LastTriggerTime.Time) < c.opts.FragEvalCycle
			if rate.Rate > threshold && !throttled {
				triggered = true
				triggerSource = TriggerOnFrag
			}
		}
		// No measurable goal: rate stays 0 so onFrag never fires — surfaced on
		// the condition below.
	}

	// Evaluation tail: record when the trigger sources were evaluated (covers
	// both sources; pure-cron writes it too).
	if status.LastEvaluationTime == nil || !status.LastEvaluationTime.Time.Equal(now) {
		status.LastEvaluationTime = &metav1.Time{Time: now}
		changed = true
	}

	if !triggered {
		if cronParseErr != nil {
			// A configured cron that cannot parse will never fire. Admission only
			// rejects TZ in the schedule, so full cron validation lands here:
			// surface it as a failure instead of a silent, never-firing policy.
			unhealthy(repackv1alpha1.ReasonReconcileFailed,
				fmt.Sprintf("invalid cronSchedule %q: %v", *spec.Trigger.CronSchedule, cronParseErr))
			return finish()
		}
		if fragErr != nil {
			// A lister failure is transient: report it as a degraded state (never a
			// silent 0%) and let the eval-cycle wakeup retry once the cache recovers.
			unhealthy(repackv1alpha1.ReasonReconcileFailed,
				fmt.Sprintf("fragmentation measurement failed for %s: %v", resource, fragErr))
			return finish()
		}
		healthy(repackv1alpha1.ReasonReconcileSucceeded,
			fmt.Sprintf("no trigger fired; next cron %s; fragmentation %s",
				cronNextText(spec, now, nil),
				fragText(fragConfigured, resource, rate, threshold)))
		return finish()
	}

	// Step 5: per-policy concurrency gate — at most one non-terminal derived run.
	if len(status.InProgress) > 0 {
		healthy(repackv1alpha1.ReasonReconcileSucceeded,
			fmt.Sprintf("%s fired but RepackRun %s is still in progress", triggerSource, status.InProgress[len(status.InProgress)-1].Name))
		return finish()
	}

	// Step 6: adopt a crash-left orphan first; only then create a fresh run.
	adopt := func(run *repackv1alpha1.RepackRun) {
		status.InProgress = append(status.InProgress, runRef(run.Name))
		status.LastTriggerTime = &metav1.Time{Time: c.now()}
		changed = true
		healthy(repackv1alpha1.ReasonReconcileSucceeded,
			fmt.Sprintf("triggered by %s; adopted RepackRun %s", triggerSource, run.Name))
	}
	for _, run := range runs {
		if repackstate.IsTerminal(run.Status.Phase) || !ownedByPolicy(run, pol.UID) || containsRef(status.InProgress, run.Name) {
			continue
		}
		adopt(run)
		return finish()
	}

	runName := fmt.Sprintf("%s-%s", policyName, c.now().UTC().Format(runNameFormat))
	newRun := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:        runName,
			Labels:      runLabels(policyName, triggerSource, spec.RunTemplate.ObjectMeta.Labels),
			Annotations: cloneMap(spec.RunTemplate.ObjectMeta.Annotations),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         repackv1alpha1.SchemeGroupVersion.String(),
				Kind:               "RepackPolicy",
				Name:               policyName,
				UID:                pol.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(false),
			}},
		},
		Spec: *spec.RunTemplate.Spec.DeepCopy(),
	}
	created, err := c.vcClient.RepackV1alpha1().RepackRuns().Create(ctx, newRun, metav1.CreateOptions{})
	switch {
	case err == nil:
		status.InProgress = append(status.InProgress, runRef(created.Name))
		status.LastTriggerTime = &metav1.Time{Time: c.now()}
		changed = true
		healthy(repackv1alpha1.ReasonReconcileSucceeded,
			fmt.Sprintf("triggered by %s; created RepackRun %s", triggerSource, created.Name))
	case apierrors.IsAlreadyExists(err):
		// Secondary guard against an external same-name run; our own orphan was
		// already absorbed by the scan above. A same-second collision cannot be
		// retried under a fresh name within the same second, so inspect the one
		// that owns the name.
		existing, getErr := c.vcClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if getErr != nil {
			unhealthy(repackv1alpha1.ReasonReconcileFailed, fmt.Sprintf("cannot resolve existing RepackRun %s: %v", runName, getErr))
		} else if ownedByPolicy(existing, pol.UID) {
			adopt(existing) // safe net for an orphan the scan missed
		} else {
			unhealthy(repackv1alpha1.ReasonReconcileFailed,
				fmt.Sprintf("RepackRun %s already exists but is not owned by this policy", runName))
		}
	default:
		unhealthy(repackv1alpha1.ReasonReconcileFailed, fmt.Sprintf("failed to create RepackRun %s: %v", runName, err))
	}
	return finish()
}

// scheduleWakeup re-arms the self-sustaining wakeup at the earliest of the next
// strictly-future cron slot and (onFrag) lastEvaluationTime+evalCycle; a source
// with no such future candidate is ignored.
func (c *Controller) scheduleWakeup(pol *repackv1alpha1.RepackPolicy) {
	now := c.now()
	spec := &pol.Spec
	var wake time.Time
	if spec.Trigger.CronSchedule != nil && *spec.Trigger.CronSchedule != "" {
		if nextFire, err := nextCronFire(*spec.Trigger.CronSchedule, now); err == nil && (wake.IsZero() || nextFire.Before(wake)) {
			wake = nextFire
		}
	}
	if spec.Trigger.OnFragAbovePercent != nil && pol.Status.LastEvaluationTime != nil {
		evalAt := pol.Status.LastEvaluationTime.Time.Add(c.opts.FragEvalCycle)
		if evalAt.After(now) && (wake.IsZero() || evalAt.Before(wake)) {
			wake = evalAt
		}
	}
	if !wake.IsZero() {
		c.scheduleNext(pol.Name, wake.Sub(now))
	}
}

// snapshotRun captures the terminal run's context (name/mode/trigger/resource)
// plus a full status snapshot into LastRunStatus.
func snapshotRun(run *repackv1alpha1.RepackRun) *repackv1alpha1.LastRunStatus {
	snapshot := &repackv1alpha1.LastRunStatus{
		Name:    run.Name,
		Mode:    run.Spec.Mode,
		Trigger: run.Labels[repackv1alpha1.RepackTriggerLabel],
	}
	if len(run.Spec.Goals) > 0 {
		snapshot.Resource = run.Spec.Goals[0].Resource
	}
	snapshot.RepackRunStatus = *run.Status.DeepCopy()
	return snapshot
}

// policyResource is the run template's first goal resource — the resource a
// fragmentation trigger measures.
func policyResource(spec repackv1alpha1.RepackPolicySpec) corev1.ResourceName {
	if len(spec.RunTemplate.Spec.Goals) > 0 {
		return spec.RunTemplate.Spec.Goals[0].Resource
	}
	return ""
}

// runLabels merges the template labels, then overrides the reserved keys last,
// so template values can never shadow the policy/trigger accounting.
func runLabels(policyName, trigger string, templateLabels map[string]string) map[string]string {
	merged := cloneMap(templateLabels)
	if merged == nil {
		merged = map[string]string{}
	}
	merged[repackv1alpha1.RepackPolicyLabel] = policyName
	merged[repackv1alpha1.RepackTriggerLabel] = trigger
	return merged
}

// groupByPhase buckets terminal runs by phase (Succeeded/Failed).
func groupByPhase(runs []*repackv1alpha1.RepackRun) map[repackv1alpha1.RepackPhase][]*repackv1alpha1.RepackRun {
	group := map[repackv1alpha1.RepackPhase][]*repackv1alpha1.RepackRun{}
	for _, run := range runs {
		if repackstate.IsTerminal(run.Status.Phase) {
			group[run.Status.Phase] = append(group[run.Status.Phase], run)
		}
	}
	return group
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func runRef(name string) corev1.ObjectReference {
	return corev1.ObjectReference{
		Kind:       "RepackRun",
		APIVersion: repackv1alpha1.SchemeGroupVersion.String(),
		Name:       name,
	}
}

// ownedByPolicy reports whether run carries a controller owner reference to the
// given policy UID — the discriminator for crash orphans (the label alone also
// matches stale incarnations after a delete+recreate).
func ownedByPolicy(run *repackv1alpha1.RepackRun, uid types.UID) bool {
	for _, owner := range run.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.UID == uid {
			return true
		}
	}
	return false
}

func containsRef(refs []corev1.ObjectReference, name string) bool {
	for _, ref := range refs {
		if ref.Name == name {
			return true
		}
	}
	return false
}

// newerTerminal orders terminal runs by CompletionTime; one without a completion
// time sorts last.
func newerTerminal(a, b *repackv1alpha1.RepackRun) bool {
	if a.Status.CompletionTime == nil {
		return false
	}
	if b.Status.CompletionTime == nil {
		return true
	}
	return a.Status.CompletionTime.Time.After(b.Status.CompletionTime.Time)
}

func cronNextText(spec repackv1alpha1.RepackPolicySpec, now time.Time, parseErr error) string {
	if spec.Trigger.CronSchedule == nil || *spec.Trigger.CronSchedule == "" {
		return "not configured"
	}
	if parseErr != nil {
		return "invalid schedule"
	}
	next, err := nextCronFire(*spec.Trigger.CronSchedule, now)
	if err != nil {
		return "invalid schedule"
	}
	return next.UTC().Format(time.RFC3339)
}

func fragText(configured bool, resource corev1.ResourceName, rate FragResult, threshold float64) string {
	if !configured {
		return "not configured"
	}
	if resource == "" {
		return "unmeasurable (template goal resource empty)"
	}
	return fmt.Sprintf("%.1f%% vs threshold %.1f%%", rate.Rate*100, threshold*100)
}
