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
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// hasInProgressRun reports whether any entry in inProgress represents a still-active
// Run (one that has not yet reached a terminal phase). The caller is expected to have
// cleaned terminal runs from the list before calling this.
func hasInProgressRun(inProgress []v1.ObjectReference) bool {
	return len(inProgress) > 0
}

// nextCronFire returns the next scheduled fire time STRICTLY AFTER now,
// for the given cron expression. Returns zero time when the expression is
// empty or unparseable.
func nextCronFire(cronSchedule string, lastTriggerTime, creationTimestamp, now time.Time) time.Time {
	if cronSchedule == "" {
		return time.Time{}
	}

	sched, err := cron.ParseStandard(cronSchedule)
	if err != nil {
		klog.V(4).InfoS("Failed to parse cron schedule", "schedule", cronSchedule, "error", err)
		return time.Time{}
	}

	// Return the next fire after `now` for scheduling / display purposes.
	return sched.Next(now)
}

// isCronDue reports whether the cron schedule has a fire time at or before now,
// based on the reference (the later of lastTriggerTime and creationTimestamp).
// This is used for trigger evaluation in the reconcile loop.
func isCronDue(cronSchedule string, lastTriggerTime, creationTimestamp, now time.Time) bool {
	if cronSchedule == "" {
		return false
	}

	sched, err := cron.ParseStandard(cronSchedule)
	if err != nil {
		return false
	}

	ref := lastTriggerTime
	if creationTimestamp.After(ref) {
		ref = creationTimestamp
	}

	next := sched.Next(ref)
	return !next.IsZero() && !next.After(now)
}

// getHistoryLimit resolves a nullable int32 to a concrete value, using defaultVal
// when the pointer is nil.
func getHistoryLimit(limit *int32, defaultVal int32) int32 {
	if limit == nil {
		return defaultVal
	}
	return *limit
}

// runName generates a deterministic, second-granularity name for a derived Run.
// The format is "{policyName}-{YYYYMMDDHHmmss}" (UTC). Same policy + same triggerTime
// always produces the same name, enabling dedup via AlreadyExists handling.
func runName(policyName string, triggerTime time.Time) string {
	return fmt.Sprintf("%s-%s", policyName, triggerTime.UTC().Format("20060102150405"))
}

// triggerLabelValue returns the trigger source label value for the first matching
// trigger. Priority: cronSchedule > onFragAbovePercent. Returns empty string if
// neither is set (should not happen with CEL validation).
func triggerLabelValue(trigger repackv1alpha1.RepackRunTrigger) string {
	if trigger.CronSchedule != nil && *trigger.CronSchedule != "" {
		return "cronSchedule"
	}
	if trigger.OnFragAbovePercent != nil && *trigger.OnFragAbovePercent > 0 {
		return "onFragAbovePercent"
	}
	return ""
}