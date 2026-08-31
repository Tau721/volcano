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
	"k8s.io/apimachinery/pkg/types"
)

const (
	// PlacementGateName is injected only into replacement Pods belonging to an
	// active RepackRun placement lease.
	PlacementGateName = "repack.volcano.sh/placement"
	// PlacementLeaseAnnotation is written onto an affected PodGroup before its
	// victim Pods are evicted. Its value is the owning RepackRun name and UID.
	PlacementLeaseAnnotation = "repack.volcano.sh/placement-lease"
	// PlacementGateOwnerAnnotation is written beside a placement scheduling gate.
	// It records the exact Run name and UID that owns the gate, allowing terminal
	// cleanup to release unrelated scale-out Pods without touching another Run.
	PlacementGateOwnerAnnotation = "repack.volcano.sh/placement-gate-owner"
	// PlacementActiveLabel indexes the single Execute RepackRun whose replacement
	// placement protocol is active. Admission webhooks use the label only to find
	// a candidate Run efficiently and always validate its phase and relocations.
	PlacementActiveLabel = "repack.volcano.sh/placement-active"
)

// RepackMode selects whether a RepackRun only simulates or actually evicts.
// +kubebuilder:validation:Enum=DryRun;Execute
type RepackMode string

const (
	// RepackModeDryRun simulates and reports a plan without evicting anything.
	RepackModeDryRun RepackMode = "DryRun"
	// RepackModeExecute evicts/relocates per the recomputed plan.
	RepackModeExecute RepackMode = "Execute"
)

// RepackPhase is the coarse lifecycle phase. Conditions are authoritative;
// phase is a derived projection for kubectl wait / list views.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type RepackPhase string

const (
	RepackPending   RepackPhase = "Pending"
	RepackRunning   RepackPhase = "Running"
	RepackSucceeded RepackPhase = "Succeeded"
	RepackFailed    RepackPhase = "Failed"
)

// PodPlacementPhase reports the lifecycle of one replacement Pod placement.
// It deliberately describes observable progress rather than implementation
// mechanisms such as scheduling gates.
// +kubebuilder:validation:Enum=WaitingForReplacement;WaitingForNodeSelection;Nominated;Placed;TimedOut
type PodPlacementPhase string

const (
	// PodPlacementWaitingForReplacement is persisted before eviction and remains
	// active until a concrete replacement Pod is identified.
	PodPlacementWaitingForReplacement PodPlacementPhase = "WaitingForReplacement"
	// PodPlacementWaitingForNodeSelection identifies a concrete replacement Pod
	// while Repack is selecting a live receiver node.
	PodPlacementWaitingForNodeSelection PodPlacementPhase = "WaitingForNodeSelection"
	// PodPlacementNominated has had its selected node written to nominatedNodeName.
	PodPlacementNominated PodPlacementPhase = "Nominated"
	// PodPlacementPlaced means the replacement was bound. SelectedNodeName and
	// ActualNodeName show whether the scheduler honored the selected receiver.
	PodPlacementPlaced PodPlacementPhase = "Placed"
	// PodPlacementTimedOut means placement did not complete before its deadline.
	PodPlacementTimedOut PodPlacementPhase = "TimedOut"
)

// PodEvictionPhase reports the durable execution state of one planned victim.
// It is independent from PodPlacementPhase: eviction is engine-owned, while
// replacement placement is controller-owned.
// +kubebuilder:validation:Enum=Pending;InProgress;Accepted;IndirectlyRemoved;Rejected
type PodEvictionPhase string

const (
	// PodEvictionPending is persisted before the Eviction API is called.
	PodEvictionPending PodEvictionPhase = "Pending"
	// PodEvictionInProgress means the intent was persisted and the API call may
	// have been issued. Recovery observes the original Pod UID before retrying.
	PodEvictionInProgress PodEvictionPhase = "InProgress"
	// PodEvictionAccepted means the Eviction API accepted this victim.
	PodEvictionAccepted PodEvictionPhase = "Accepted"
	// PodEvictionIndirectlyRemoved means the original victim disappeared after
	// another eviction in the same PodGroup was accepted. Repack did not receive
	// an accepted Eviction API response for this individual Pod.
	PodEvictionIndirectlyRemoved PodEvictionPhase = "IndirectlyRemoved"
	// PodEvictionRejected means the planned victim was not evicted by this run.
	PodEvictionRejected PodEvictionPhase = "Rejected"
)

// ---------- spec ----------

// RepackRunSpec is the user-facing, self-contained spec of one repack job.
//
// Admission is enforced entirely at the apiserver via CEL/markers (no controller
// admission step): mode is an enum, goals is capped at one entry, and the spec is
// immutable. Scope is optional in both modes: an omitted scope means whole-cluster
// (all PodGroups, all nodes); how much a run actually relocates is bounded by the
// engine's internal plan (maxPerRun, cooldown, K=1, PDB), not by requiring a scope.
type RepackRunSpec struct {
	// Mode selects DryRun (simulate + report) or Execute (act).
	// +kubebuilder:validation:Required
	Mode RepackMode `json:"mode"`

	// Scope bounds which PodGroups may move and which nodes participate.
	// +optional
	Scope *RepackScope `json:"scope,omitempty"`

	// Goals is the per-resource fragmentation target: exactly one entry
	// (a run defragments a single accelerator resource); multi-resource is reserved.
	// +optional
	// +kubebuilder:validation:MaxItems=1
	Goals []RepackGoal `json:"goals,omitempty"`

	// NetworkTopology optionally enables HyperNode-level fragmentation
	// optimization. When set, the run frees at least RequiredNodeBlocks blocks of
	// NodeBlockSize nodes within the target HyperNode tier, so topology-constrained
	// AI workloads can schedule. When omitted, no HyperNode-level optimization is
	// performed and the run keeps the existing Node-level semantics.
	// +optional
	NetworkTopology *NetworkTopology `json:"networkTopology,omitempty"`

	// MaxPerRun caps the blast radius of a single run.
	// +optional
	MaxPerRun *MaxPerRun `json:"maxPerRun,omitempty"`

	// Eviction configures how Execute issues Kubernetes Eviction requests. It is
	// ignored by DryRun because no Pod is evicted in that mode.
	// +optional
	Eviction *EvictionPolicy `json:"eviction,omitempty"`

	// TTLSecondsAfterFinished: auto-DELETE this Run that long after it finishes
	// (like Job). Unset = not auto-deleted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`
}

// EvictionPolicy configures the execution behavior of Kubernetes Eviction API
// requests. Selection and scoring remain internal to the engine; this type owns
// only the mechanics of submitting an accepted move.
type EvictionPolicy struct {
	// GracePeriodSeconds overrides the graceful-termination period requested for
	// every Pod eviction in this Run. Unset preserves each Pod's
	// spec.terminationGracePeriodSeconds; 0 requests immediate termination.
	// +optional
	// +kubebuilder:validation:Minimum=0
	GracePeriodSeconds *int64 `json:"gracePeriodSeconds,omitempty"`
}

// RepackScope bounds the run on two independent axes: which PodGroups may move
// and which nodes participate. Each axis has symmetric include/exclude
// (label selector and/or explicit name list); exclude wins.
//
// Everything is a PodGroup: selection is expressed uniformly against PodGroups
// (the engine's action/cost unit is the gang), so both `selector` (PG labels)
// and `names` (PG "namespace/name") address the SAME object. This works for
// K8s-native workloads (Deployment / StatefulSet / ...) and custom workloads
// that use the pg-controller inherit stable pod-template labels onto their
// auto-created PodGroup; system/controller labels such as pod-template-hash are
// filtered out. Workloads that create their own PodGroup must copy the labels
// they want to expose. Note `names` is only practical for deterministically
// named PodGroups (e.g. vcjob); auto-created PGs have UID-derived names, so those
// are selected via `selector`.
type RepackScope struct {
	// +optional
	PodGroups *RepackSelectorTerm `json:"podGroups,omitempty"`
	// +optional
	Nodes *RepackSelectorTerm `json:"nodes,omitempty"`
}

// RepackSelectorTerm is an include/exclude pair (exclude wins).
type RepackSelectorTerm struct {
	// +optional
	Include *RepackSelector `json:"include,omitempty"`
	// +optional
	Exclude *RepackSelector `json:"exclude,omitempty"`
}

// RepackSelector matches by label selector and/or explicit names (union).
type RepackSelector struct {
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Names are "namespace/name" for PodGroups, or node names for nodes.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	Names []string `json:"names,omitempty"`
}

// RepackGoal is one resource's fragmentation target.
type RepackGoal struct {
	// Resource is the accelerator to defragment (e.g. nvidia.com/gpu). Only
	// fully-qualified extended resources (a name containing "/") are supported;
	// core compute resources such as cpu, memory, ephemeral-storage and pods are
	// rejected, because repack only consolidates scalar accelerator capacity.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self.contains('/')",message="goals.resource must be a fully-qualified accelerator/extended resource (e.g. nvidia.com/gpu); core resources like cpu, memory and ephemeral-storage are not supported"
	Resource v1.ResourceName `json:"resource"`
	// MinFragImprovementPercent is the required minimum drop in this resource's
	// fragmentation, in percentage points (0-100). 0 = any benefit counts.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MinFragImprovementPercent int32 `json:"minFragImprovementPercent,omitempty"`
}

// NetworkTopology specifies the HyperNode-tier fragmentation optimization target
// of one RepackRun: free at least RequiredNodeBlocks blocks of NodeBlockSize
// nodes within exactly one HyperNode tier.
//
// The tier identifier is mutually exclusive by construction: configure exactly
// one of HyperNodeTier (numeric, matching HyperNode.Spec.Tier) or
// HyperNodeTierName (matching HyperNode.Spec.TierName). The exclusivity is
// enforced at the apiserver with a CEL XValidation on this struct (there is no
// repo precedent for struct-level XValidation; verify via make manifests + e2e).
//
// +kubebuilder:validation:XValidation:rule="(has(self.hyperNodeTier) && !has(self.hyperNodeTierName)) || (!has(self.hyperNodeTier) && has(self.hyperNodeTierName))",message="hyperNodeTier and hyperNodeTierName are mutually exclusive; configure exactly one"
type NetworkTopology struct {
	// HyperNodeTier is the target HyperNode tier (numeric), matching
	// HyperNode.Spec.Tier. Mutually exclusive with HyperNodeTierName; exactly one
	// must be configured (enforced by CEL).
	// +optional
	HyperNodeTier *int `json:"hyperNodeTier,omitempty"`
	// HyperNodeTierName is the target HyperNode tier name, matching
	// HyperNode.Spec.TierName. Mutually exclusive with HyperNodeTier; exactly one
	// must be configured (enforced by CEL).
	// +optional
	HyperNodeTierName *string `json:"hyperNodeTierName,omitempty"`
	// NodeBlockSize is the number of nodes in one block. A block must lie entirely
	// inside a single HyperNode of the target tier: nodes from different HyperNodes
	// cannot be combined into one block. It is a pointer so that an explicit 0 is
	// distinguishable from "omitted": 0 is rejected by the minimum:1 rule (a block
	// must hold at least one node), while omitting the field applies the default 1
	// (each node is a block).
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	NodeBlockSize *int `json:"nodeBlockSize,omitempty"`
	// RequiredNodeBlocks is the minimum number of blocks the defragmentation must
	// free. The target is met only when at least RequiredNodeBlocks blocks are
	// freed; if it cannot be met, the plan is infeasible and no defragmentation is
	// performed. 0 (default) relaxes the gate entirely: the block semantics degrade
	// to pure soft guidance. Must be non-negative.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	RequiredNodeBlocks int `json:"requiredNodeBlocks,omitempty"`
	// Mode expresses how blocks should be distributed across HyperNodes: binpack
	// concentrates blocks onto as few HyperNodes as possible; spread disperses
	// them. Omitted = no distribution preference.
	// +optional
	// +kubebuilder:validation:Enum=binpack;spread
	Mode RepackBlockMode `json:"mode,omitempty"`
}

// RepackBlockMode expresses the preferred distribution of blocks across
// HyperNodes at the target tier.
type RepackBlockMode string

const (
	// RepackBlockModeBinpack concentrates blocks onto as few HyperNodes as
	// possible: new freed nodes favor HyperNodes that already hold complete blocks.
	RepackBlockModeBinpack RepackBlockMode = "binpack"
	// RepackBlockModeSpread disperses blocks across different HyperNodes: new
	// freed nodes favor HyperNodes that hold few complete blocks.
	RepackBlockModeSpread RepackBlockMode = "spread"
)

// MaxPerRun caps a single run's blast radius (distinct from K8s resource limits).
type MaxPerRun struct {
	// PodGroups caps distinct PodGroups moved (cross-resource count).
	// +optional
	// +kubebuilder:validation:Minimum=0
	PodGroups *int32 `json:"podGroups,omitempty"`
	// Resources caps accelerator cards moved per resource.
	// +optional
	Resources v1.ResourceList `json:"resources,omitempty"`
}

// ---------- status ----------

// RepackRunStatus reports lifecycle and business output. Conditions are
// authoritative; phase is derived. "Worth repacking?" is folded into the
// terminal Complete condition's reason (RepackRecommended /
// ExecutionCompleted / NoFragmentation / InsufficientImprovement), not a
// summary field.
type RepackRunStatus struct {
	// Phase is a derived projection of conditions.
	// +optional
	Phase RepackPhase `json:"phase,omitempty"`

	// Conditions are the authoritative facts (Job-style:
	// Progressing/Complete/Failed). Admission is CEL-only, so there is no
	// Admitted condition. Progressing=False explains why a Pending run is
	// waiting. The Complete condition's reason also encodes whether repacking
	// was worthwhile.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Message is the current one-line operator summary.
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is when the run first entered Running.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run first reached a terminal phase (TTL anchor).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Plan is the immutable plan-time decision in both modes. DryRun reports what
	// would be done; Execute preserves the complete pre-eviction plan so rejected
	// or alternatively placed actions remain auditable. Actual Execute metrics
	// live in Result.
	// +optional
	Plan *RepackPlan `json:"plan,omitempty"`

	// Result is the Execute-only observed outcome. It is absent for DryRun and
	// when Execute failed before any eviction was attempted.
	// +optional
	Result *RepackResult `json:"result,omitempty"`

	// Relocations are the durable per-Pod execution records produced by Execute:
	// one entry per relocated Pod, consumed by the placement reconciler per the
	// replacement matching contract (victimPodName exact match ->
	// schedulingRequirementsHash -> homogeneous PodGroup fallback). See the
	// design proposal §5.2.2.
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Relocations []PodRelocationStatus `json:"relocations,omitempty"`
}

// PodRelocationStatus records the immutable plan identity and the independently
// reconciled eviction and replacement placement state for one relocated Pod.
type PodRelocationStatus struct {
	// Namespace of the target pod (PodGroup shares this namespace).
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// PodGroupName the pod belongs to.
	// +optional
	PodGroupName string `json:"podGroupName,omitempty"`
	// ReplacementPodGroupName is the latest PodGroup generation that recreates
	// this relocation's replacement Pod. It remains empty when the workload reuses
	// the original PodGroup name. The placement controller owns this runtime field
	// and may advance it after another full-group recreation; PodGroupName remains
	// the immutable plan-time identity for audit.
	// +optional
	ReplacementPodGroupName string `json:"replacementPodGroupName,omitempty"`
	// VictimPodName is the evicted pod's name: audit + exact fast-path when the
	// controller recreates the replacement with the same name.
	// +optional
	VictimPodName string `json:"victimPodName,omitempty"`
	// VictimPodUID identifies the exact Pod instance selected by the plan. The
	// engine uses it as an eviction precondition and during crash recovery so a
	// same-name replacement is never evicted by a replayed request.
	// +optional
	VictimPodUID types.UID `json:"victimPodUID,omitempty"`
	// SchedulingRequirementsHash is an opaque hash of normalized scheduling
	// requirements from the victim Pod. It is populated only when the PodGroup
	// defines SubGroup policies and is compared only for equality when matching a
	// renamed replacement Pod. Empty means the PodGroup is treated as homogeneous.
	// +optional
	SchedulingRequirementsHash string `json:"schedulingRequirementsHash,omitempty"`
	// PlannedNodeName is the immutable plan-time target node. It is retained for
	// audit even if a later placement reconciliation selects another receiver.
	// +kubebuilder:validation:Required
	PlannedNodeName string `json:"plannedNodeName"`
	// Eviction is the engine-owned durable victim eviction state.
	Eviction PodEvictionStatus `json:"eviction"`
	// Placement is the controller-owned replacement placement state.
	Placement PodPlacementStatus `json:"placement"`
}

// PodEvictionStatus contains the durable eviction journal for one victim Pod.
type PodEvictionStatus struct {
	// Phase is the current eviction lifecycle phase.
	Phase PodEvictionPhase `json:"phase"`
	// Message contains operator-readable rejection or recovery detail.
	// +optional
	Message string `json:"message,omitempty"`
}

// PodPlacementStatus contains the replacement Pod identity, selected receiver,
// observed binding, and placement deadline.
type PodPlacementStatus struct {
	// Phase is the current replacement placement lifecycle phase.
	Phase PodPlacementPhase `json:"phase"`
	// SelectedNodeName is the receiver selected from the live scheduler snapshot.
	// It is written before the placement gate is removed.
	// +optional
	SelectedNodeName string `json:"selectedNodeName,omitempty"`
	// ReplacementPodName and ReplacementPodUID identify the concrete replacement
	// Pod held by the placement gate.
	// +optional
	ReplacementPodName string `json:"replacementPodName,omitempty"`
	// +optional
	ReplacementPodUID types.UID `json:"replacementPodUID,omitempty"`
	// ActualNodeName is populated after the replacement is bound. Comparing it
	// with SelectedNodeName reveals an alternative scheduler placement.
	// +optional
	ActualNodeName string `json:"actualNodeName,omitempty"`
	// ExpirationTime bounds placement reconciliation.
	// +optional
	ExpirationTime *metav1.Time `json:"expirationTime,omitempty"`
}

// RepackPlan is the immutable plan-time output in both modes. Three progressive
// layers (summary + moves + freedNodes), capped by maxItems. Execute never
// rewrites it with the accepted subset or actual placement result.
type RepackPlan struct {
	// Summary is the flat, at-a-glance layer (UI lists / alerts / printers read this).
	// +optional
	Summary *RepackSummary `json:"summary,omitempty"`
	// Moves is the per-PodGroup relocation detail; fromNode/toNode live per-pod
	// inside pods[]. moves is a pure plan (identical in DryRun and Execute).
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Moves []RepackMove `json:"moves,omitempty"`
	// FreedNodes are the names of nodes the plan empties.
	// +optional
	// +kubebuilder:validation:MaxItems=2048
	FreedNodes []string `json:"freedNodes,omitempty"`
}

// RepackSummary is the flat second layer of the plan. Single-resource per run
// (goals maxItems=1): the fragmentation figures are for that one accelerator
// resource (goals[0].resource) — no per-resource breakdown. Multi-resource is
// Reserved for multi-resource (would add a per-resource layer then).
type RepackSummary struct {
	// Cluster-wide fragmentation before the run and predicted after the complete
	// plan, in percentage points (0-100). Only nodes providing the target resource
	// participate; scope limits actions, not this cluster health metric.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FragBeforePercent int32 `json:"fragBeforePercent"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FragAfterPercent int32 `json:"fragAfterPercent"`
	// FreedNodeCount is the number of nodes the complete plan predicts it will free.
	FreedNodeCount int32 `json:"freedNodeCount"`
	// MovedCardCount is the total accelerator cards the complete plan would move.
	MovedCardCount int64 `json:"movedCardCount"`
	// +optional
	ResolvedScope *ResolvedScope `json:"resolvedScope,omitempty"`
}

// RepackResult is the Execute-only observed outcome. Plan remains the immutable
// audit record; Result reports what was accepted and what the cluster looked like
// after replacement placement completed.
type RepackResult struct {
	// FragAfterPercent is the observed cluster-wide fragmentation after Execute.
	// When MetricsVerified is false, it conservatively equals plan.fragBeforePercent.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	FragAfterPercent int32 `json:"fragAfterPercent"`
	// FreedNodeCount is the number of plan.freedNodes actually free of the target
	// resource in the verified terminal snapshot.
	FreedNodeCount int32 `json:"freedNodeCount"`
	// FreedNodes are the plan.freedNodes verified free of the target resource in
	// the terminal scheduler snapshot. The list is sorted for deterministic
	// status and lets operators compare the realized node set with plan.freedNodes.
	// +optional
	// +kubebuilder:validation:MaxItems=2048
	FreedNodes []string `json:"freedNodes,omitempty"`
	// MovedCardCount is the accelerator-card total for accepted Pod evictions.
	MovedCardCount int64 `json:"movedCardCount"`
	// MetricsVerified reports whether FragAfterPercent, FreedNodeCount, and
	// FreedNodes came from a coherent scheduler snapshot after replacement binding.
	MetricsVerified bool `json:"metricsVerified"`
}

// ResolvedScope reports the effective action scope after selector resolution.
// It does not change the cluster-wide fragmentation metric's denominator.
type ResolvedScope struct {
	// PodGroupCount is the number of PodGroups selected by podGroup scope that
	// currently consume the target resource. Later feasibility and disruption
	// checks may still prevent them from moving.
	PodGroupCount int32 `json:"podGroupCount"`
	// NodeCount is the number of in-scope nodes providing the target resource and
	// eligible to be selected as drain targets.
	NodeCount int32 `json:"nodeCount"`
}

// RepackMove is one PodGroup's relocation. fromNode/toNode are per-pod (a gang's
// pods may be spread across nodes / move to multiple targets), so they live in
// pods[]; the PodGroup level keeps identity + aggregate.
type RepackMove struct {
	// Namespace of this PodGroup (owner and pods share it).
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// PodGroupName is the precise scheduling-dimension identity (matches scope).
	// +kubebuilder:validation:Required
	PodGroupName string `json:"podGroupName"`
	// Owner is the user-facing workload owning this PodGroup (PodGroup is an
	// internal object). Taken from the PG controller ownerReference (not walked
	// up); empty for ownerless bare pods.
	// +optional
	Owner *WorkloadRef `json:"owner,omitempty"`
	// Cards is this PodGroup's total accelerator cards moved (= sum of pods[].cards).
	// +optional
	Cards int64 `json:"cards,omitempty"`
	// Pods is the per-pod relocation detail (only relocated pods appear).
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Pods []PodMove `json:"pods,omitempty"`
}

// PodMove is one pod's planned relocation (pure plan; DryRun/Execute identical).
type PodMove struct {
	// Name is the pod name (a plan-time snapshot for random-named controllers).
	// +optional
	Name string `json:"name,omitempty"`
	// FromNode is where the pod currently runs.
	// +optional
	FromNode string `json:"fromNode,omitempty"`
	// ToNode is the pod's PLANNED target node (soft nomination, no reservation).
	// +optional
	ToNode string `json:"toNode,omitempty"`
	// Cards is the accelerator cards (GPU/NPU) this pod occupies.
	// +optional
	Cards int64 `json:"cards,omitempty"`
}

// WorkloadRef is the user-facing workload owning a PodGroup (Deployment/
// StatefulSet/Job/vcjob…). Passed through from the PG controller ownerReference
// (not walked up: a Deployment's pod shows ReplicaSet). Namespace matches the move.
type WorkloadRef struct {
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
}

// ---------- root objects ----------

// RepackRun is a one-shot runtime-defragmentation job. It is cluster-scoped and
// user-immutable (CREATE/READ/DELETE only): spec is frozen after admission via a
// CEL transition rule — to change anything, delete and create a new Run.
//
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=repackruns,scope=Cluster,shortName=rpr;repackrun
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MODE",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="RESOURCE",type=string,JSONPath=`.spec.goals[0].resource`,description="Target accelerator resource being defragmented"
// +kubebuilder:printcolumn:name="PHASE",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="PLAN-FREED",type=integer,JSONPath=`.status.plan.summary.freedNodeCount`
// +kubebuilder:printcolumn:name="ACTUAL-FREED",type=integer,JSONPath=`.status.result.freedNodeCount`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type RepackRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is immutable once set (one-shot job; recompute by creating a new Run).
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="RepackRun.spec is immutable; create a new RepackRun to change it"
	Spec RepackRunSpec `json:"spec"`
	// +optional
	Status RepackRunStatus `json:"status,omitempty"`
}

// RepackRunList is a list of RepackRun.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
type RepackRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepackRun `json:"items"`
}
