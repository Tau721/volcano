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

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Labels and annotations set by the RepackPolicy controller on derived RepackRuns.
const (
	// RepackPolicyLabel identifies the owning Policy (by name); history GC and the
	// in-progress gate list runs by this label.
	RepackPolicyLabel = "repack.volcano.sh/repack-policy"
	// RepackTriggerLabel records the trigger source (cronSchedule|onFragAbovePercent).
	RepackTriggerLabel = "repack.volcano.sh/repack-trigger"
)

// RepackPolicyStatus condition type and reasons.
const (
	// CondHealthy expresses whether the last reconcile succeeded (see reasons).
	CondHealthy = "Healthy"
	// ReasonReconcileSucceeded covers every expected outcome: suspend, no trigger
	// hit, or trigger + Run creation.
	ReasonReconcileSucceeded = "ReconcileSucceeded"
	// ReasonReconcileFailed covers an error needing operator attention, e.g. the
	// Run creation API call failed.
	ReasonReconcileFailed = "ReconcileFailed"
)

// RepackPolicy is a template-based RepackRun generator (CronJob→Job pattern).
// It is cluster-scoped and user-mutable.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=repackpolicies,scope=Cluster,shortName=rpp;repackpolicy
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="SUSPEND",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="STATUS",type=string,JSONPath=`.status.conditions[?(@.type=="Healthy")].reason`,description="Healthy condition reason"
// +kubebuilder:printcolumn:name="LAST-TRIGGER",type=date,JSONPath=`.status.lastTriggerTime`
// +kubebuilder:printcolumn:name="LAST-EVAL",type=date,JSONPath=`.status.lastEvaluationTime`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type RepackPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RepackPolicySpec   `json:"spec"`
	Status            RepackPolicyStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type RepackPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepackPolicy `json:"items"`
}

// RepackPolicySpec declares when to derive a RepackRun and the template to use.
// The RepackRunSpec inside RunTemplate is the single source of truth for run
// shape (zero schema drift); whether a derived run is DryRun or Execute is fully
// decided by runTemplate.spec.mode.
type RepackPolicySpec struct {
	// Trigger is when a new RepackRun is derived (either source firing suffices).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="(has(self.cronSchedule) && self.cronSchedule != '') || has(self.onFragAbovePercent)",message="trigger must set at least one of cronSchedule or onFragAbovePercent"
	Trigger RepackRunTrigger `json:"trigger"`

	// RunTemplate is the template for derived RepackRuns.
	// +kubebuilder:validation:Required
	RunTemplate RepackRunTemplateSpec `json:"runTemplate"`

	// Suspend pauses triggering (running runs are unaffected). Default false.
	// +optional
	// +kubebuilder:default=false
	Suspend *bool `json:"suspend,omitempty"`

	// SuccessfulRunsHistoryLimit keeps the most recent Succeeded and
	// PartiallySucceeded derived runs as one combined successful history (flat,
	// CronJob successfulJobsHistoryLimit-style). Default 3.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`

	// FailedRunsHistoryLimit keeps the most recent failed derived runs. Default 3
	// (CronJob's failed side defaults to 1; we take 3 to stay symmetric).
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`
}

// RepackRunTrigger is the trigger source set: each configured source is enabled,
// and either firing derives a run. Reactive-condition evaluation cadence is a
// controller-level flag, not part of this schema.
type RepackRunTrigger struct {
	// CronSchedule fires on a standard 5-field cron expression; empty disables.
	// +optional
	// +kubebuilder:validation:XValidation:rule="!self.contains('TZ')",message="cronSchedule cannot contain TZ or CRON_TZ (RepackPolicy has no timeZone field); a policy always runs in the controller's local time"
	CronSchedule *string `json:"cronSchedule,omitempty"`

	// OnFragAbovePercent fires when the cluster-wide fragmentation rate for the
	// template's resource exceeds this 0-100 threshold (strictly greater than;
	// equal does not fire). 0 fires on any fragmentation (FragRate > 0). Unset disables.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	OnFragAbovePercent *int32 `json:"onFragAbovePercent,omitempty"`
}

// RepackRunTemplateSpec is the template for a derived RepackRun.
type RepackRunTemplateSpec struct {
	// ObjectMeta carries labels/annotations merged onto derived runs.
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the run spec itself (reused RepackRunSpec). Field-level validation
	// on RepackRunSpec carries in (template is pre-validated against run rules);
	// RepackRun's root spec-immutability transition rule does not, so this template
	// stays mutable and can evolve with the Policy.
	// +kubebuilder:validation:Required
	Spec RepackRunSpec `json:"spec"`
}

// RepackPolicyStatus mirrors CronJob's status, plus conditions for the Policy's
// own health, lastEvaluationTime for the reactive trigger, and lastRunStatus
// snapshotting the most recent terminal derived run so an operator can see the
// last outcome without querying RepackRuns.
type RepackPolicyStatus struct {
	// InProgress lists derived runs not yet terminal (Pending or Running).
	// +optional
	InProgress []v1.ObjectReference `json:"inProgress,omitempty"`

	// LastTriggerTime is when a run was last derived (unified across trigger
	// sources, so not named lastScheduleTime).
	// +optional
	LastTriggerTime *metav1.Time `json:"lastTriggerTime,omitempty"`

	// LastSuccessfulTime is when the most recent Succeeded or PartiallySucceeded
	// derived run completed.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// LastRunStatus is the most recent terminal (Succeeded/PartiallySucceeded/Failed)
	// derived run's context + full status snapshot (see LastRunStatus). Written
	// once when the run turns terminal, then overwritten by the next terminal run.
	// +optional
	LastRunStatus *LastRunStatus `json:"lastRunStatus,omitempty"`

	// LastEvaluationTime is when the trigger sources were last evaluated
	// (covers cronSchedule and onFragAbovePercent).
	// +optional
	LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`

	// Conditions are standard Kubernetes conditions. RepackPolicy uses a single
	// type "Healthy": Healthy=True/ReconcileSucceeded means the last reconcile
	// completed as expected; Healthy=False/ReconcileFailed means it errored.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// LastRunStatus is the context of the most recent terminal derived run plus a
// snapshot of its RepackRunStatus (flattened via inline embed), so the run's
// name/mode/trigger/resource sit next to phase/plan/result under lastRunStatus.
type LastRunStatus struct {
	// Name of the run that produced this snapshot (named {policy}-{YYYYMMDDHHmmss}),
	// retained for tracing after the run is TTL/GC-deleted.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Mode is the run's repack mode (DryRun/Execute, from runTemplate.spec.mode).
	// +kubebuilder:validation:Required
	Mode RepackMode `json:"mode"`

	// Trigger is the source that fired (cronSchedule|onFragAbovePercent), matching
	// the derived run's RepackTriggerLabel. Always set: a run is only created by a
	// firing source.
	// +kubebuilder:validation:Required
	Trigger string `json:"trigger"`

	// Resource is the target accelerator resource (RepackRun.spec.goals[0].resource);
	// empty when the template leaves goals unset.
	// +optional
	Resource v1.ResourceName `json:"resource,omitempty"`

	RepackRunStatus `json:",inline"`
}
