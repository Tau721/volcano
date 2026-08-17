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

// RepackPolicy is a template-based RepackRun generator (P1, CronJob→Job pattern).
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

	Spec   RepackPolicySpec   `json:"spec"`
	Status RepackPolicyStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type RepackPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepackPolicy `json:"items"`
}

// RepackPolicySpec defines the desired state of a RepackPolicy.
type RepackPolicySpec struct {
	// Trigger specifies when to generate a RepackRun (at least one source required).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="has(self.cronSchedule) || has(self.onFragAbovePercent)",message="trigger must set at least one of cronSchedule/onFragAbovePercent"
	Trigger RepackRunTrigger `json:"trigger"`

	// RunTemplate is the template from which RepackRuns are created.
	// +kubebuilder:validation:Required
	RunTemplate RepackRunTemplateSpec `json:"runTemplate"`

	// Suspend suspends trigger evaluation (existing Runs unaffected). Defaults to false.
	// +optional
	// +kubebuilder:default=false
	Suspend *bool `json:"suspend,omitempty"`

	// SuccessfulRunsHistoryLimit is how many successful Runs to keep (0 = none). Defaults to 3.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3
	SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`

	// FailedRunsHistoryLimit is how many failed Runs to keep (0 = none). Defaults to 3.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3
	FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`
}

// RepackRunTrigger specifies the trigger sources. At least one must be set.
type RepackRunTrigger struct {
	// CronSchedule is a standard 5-field cron expression (e.g. "0 */6 * * *").
	// Omitted or empty string means cron triggering is disabled.
	// +optional
	CronSchedule *string `json:"cronSchedule,omitempty"`

	// OnFragAbovePercent triggers when cluster fragmentation exceeds this
	// percentage (0–100). 0 or nil means reactive triggering is disabled.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	OnFragAbovePercent *int32 `json:"onFragAbovePercent,omitempty"`
}

// RepackRunTemplateSpec wraps a RepackRun spec along with metadata labels/annotations.
type RepackRunTemplateSpec struct {
	// ObjectMeta carries labels and annotations to propagate to generated Runs.
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the RepackRunSpec to deep-copy into each generated Run.
	// +kubebuilder:validation:Required
	Spec RepackRunSpec `json:"spec"`
}

// RepackPolicyStatus communicates the observed state of a RepackPolicy.
// Inspired by CronJobStatus but uses conditions instead of Active list solely,
// and adds lastEvaluationTime for reactive-trigger tracking.
type RepackPolicyStatus struct {
	// InProgress lists generated Runs that have not yet reached a terminal phase.
	// Terminal (Succeeded/Failed) runs are removed.
	// +optional
	InProgress []v1.ObjectReference `json:"inProgress,omitempty"`

	// LastTriggerTime is the most recent time a Run was generated (any trigger source).
	// +optional
	LastTriggerTime *metav1.Time `json:"lastTriggerTime,omitempty"`

	// LastSuccessfulTime is the completion time of the most recent successful Run.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// LastEvaluationTime is the most recent reactive-trigger evaluation time.
	// +optional
	LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`

	// Conditions expresses the policy's health. The only condition type is
	// "Healthy": True=ReconcileSucceeded, False=ReconcileFailed.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Labels applied to RepackRuns generated by a RepackPolicy.
const (
	// RepackPolicyLabel identifies the policy that created the Run.
	// Value: the RepackPolicy name. Used for history GC and concurrency gating.
	RepackPolicyLabel = "repack.volcano.sh/repack-policy"

	// RepackTriggerLabel records which trigger source caused the Run to be created.
	// Value: "cronSchedule" or "onFragAbovePercent".
	RepackTriggerLabel = "repack.volcano.sh/repack-trigger"
)

// Condition type constants for RepackPolicy status.
const (
	// CondHealthy is the condition type expressing reconcile health.
	CondHealthy = "Healthy"
)

// Condition reason constants for RepackPolicy status.
const (
	// ReasonReconcileSucceeded means the last reconcile completed as expected
	// (suspend, no trigger, or trigger+Run creation all count as success).
	ReasonReconcileSucceeded = "ReconcileSucceeded"

	// ReasonReconcileFailed means the last reconcile encountered an error
	// that requires operator attention (e.g. Run creation API failure).
	ReasonReconcileFailed = "ReconcileFailed"
)