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
	"fmt"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
)

// The helpers in this file render the compact, operator-facing status surface.
// Stable machine decisions remain in Condition.Reason; these messages explain
// the scope, result, and next diagnostic action without duplicating move lists.

func CompletionMessage(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName, reason string) string {
	resource := DisplayResource(targetResource)
	summary := Summary(run)
	switch reason {
	case state.ReasonRepackRecommended:
		return fmt.Sprintf(
			"Repack recommended for %s: move %d PodGroups and %d cards to free %d nodes; cluster fragmentation is expected to improve from %d%% to %d%%.",
			resource, summary.PodGroups, summary.MovedCards, summary.FreedNodes, summary.FragBefore, summary.FragAfter)
	case state.ReasonExecutionCompleted:
		result := Result(run)
		return fmt.Sprintf(
			"Repack completed for %s: moved %d PodGroups and %d cards, actually freed %d nodes; cluster fragmentation changed from %d%% to %d%%.",
			resource, acceptedPodGroupCount(run), result.MovedCards, result.FreedNodes, summary.FragBefore, result.FragAfter)
	case state.ReasonExecutionCompletedWithAlternativePlacement:
		result := Result(run)
		_, alternativeNodePlacements, _ := PlacementOutcomeCounts(run)
		return fmt.Sprintf(
			"Repack completed for %s with %d replacement %s scheduled on alternative nodes: all planned nodes were verified free, %d cards were moved, and cluster fragmentation changed from %d%% to %d%%.",
			resource, alternativeNodePlacements, pluralNoun(alternativeNodePlacements, "Pod", "Pods"),
			result.MovedCards, summary.FragBefore, result.FragAfter)
	case state.ReasonNoFragmentation:
		return fmt.Sprintf(
			"No repack needed for %s: cluster fragmentation is %d%%; the resolved scope contains %d resource nodes and %d target-resource PodGroups.",
			resource, summary.FragBefore, summary.ScopeNodes, summary.ScopePodGroups)
	case state.ReasonInsufficientImprovement:
		return fmt.Sprintf(
			"No repack performed for %s: cluster fragmentation is %d%%, but no feasible plan within the resolved scope met the repack run's benefit requirement (whole-node freed or fragmentation-rate threshold), so no migration was planned.",
			resource, summary.FragBefore)
	case state.ReasonRequiredNodeBlocksNotMet:
		required, size, tier := blockRequirement(run)
		return fmt.Sprintf(
			"No repack performed for %s: the resolved scope cannot form the required %d HyperNode node-block(s) of size %d in tier %s; cluster fragmentation is %d%%, so no migration was planned.",
			resource, required, size, tier, summary.FragBefore)
	default:
		return fmt.Sprintf("Repack for %s completed with outcome %s.", resource, reason)
	}
}

// blockRequirement reads the node-block admission target (required count, block
// size, tier) from the run's networkTopology.
func blockRequirement(run *repackv1alpha1.RepackRun) (required, size int, tier string) {
	size = 1
	tier = "<unset>"
	if run == nil || run.Spec.NetworkTopology == nil {
		return
	}
	topo := run.Spec.NetworkTopology
	required = topo.RequiredNodeBlocks
	if topo.NodeBlockSize != nil {
		size = *topo.NodeBlockSize
	}
	switch {
	case topo.HyperNodeTier != nil:
		tier = fmt.Sprintf("%d", *topo.HyperNodeTier)
	case topo.HyperNodeTierName != nil:
		tier = *topo.HyperNodeTierName
	}
	return
}

func PlacementMessage(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName, decision placementexecutor.TerminalDecision) string {
	plan := Summary(run)
	result := Result(run)
	selectedNodePlacements, alternativeNodePlacements, timedOutPlacements := PlacementOutcomeCounts(run)
	resource := DisplayResource(targetResource)
	plannedNodes := FormatNodeNames(decision.Nodes.Planned)
	actualNodes := FormatNodeNames(decision.Nodes.Actual)
	switch decision.Reason {
	case state.ReasonExecutionCompleted:
		return fmt.Sprintf(
			"Repack succeeded for %s: all %d replacement %s were scheduled and all %d planned %s were verified free [%s]; cluster fragmentation changed from %d%% to %d%%.",
			resource, selectedNodePlacements, pluralNoun(selectedNodePlacements, "Pod", "Pods"),
			len(decision.Nodes.Planned), pluralNoun(len(decision.Nodes.Planned), "node", "nodes"),
			plannedNodes, plan.FragBefore, result.FragAfter) + podGroupReplacementStatusSuffix(run)
	case state.ReasonExecutionCompletedWithAlternativePlacement:
		return fmt.Sprintf(
			"Repack succeeded for %s with alternative placement: all %d replacement %s were scheduled (%d reached selected nodes and %d used alternative nodes), and all %d planned %s were verified free [%s]; cluster fragmentation changed from %d%% to %d%%.",
			resource, selectedNodePlacements+alternativeNodePlacements,
			pluralNoun(selectedNodePlacements+alternativeNodePlacements, "Pod", "Pods"),
			selectedNodePlacements, alternativeNodePlacements,
			len(decision.Nodes.Planned), pluralNoun(len(decision.Nodes.Planned), "node", "nodes"),
			plannedNodes, plan.FragBefore, result.FragAfter) + podGroupReplacementStatusSuffix(run)
	case state.ReasonPlacementTimedOut:
		return fmt.Sprintf(
			"Repack failed for %s because %d replacement %s did not bind before the placement deadline; %d reached selected nodes and %d were placed elsewhere. Planned nodes [%s] were not accepted as a verified complete result; inspect Pod scheduling events and available receiver capacity.",
			resource, timedOutPlacements, pluralNoun(timedOutPlacements, "Pod", "Pods"),
			selectedNodePlacements, alternativeNodePlacements, plannedNodes) +
			podGroupReplacementStatusSuffix(run)
	case state.ReasonResultVerificationFailed:
		return fmt.Sprintf(
			"Repack failed verification for %s: replacement bindings were reported, but the scheduler cache did not expose one coherent terminal snapshot before the deadline. Planned nodes [%s] cannot be confirmed free; inspect scheduler cache and Pod informer synchronization.",
			resource, plannedNodes) + podGroupReplacementStatusSuffix(run)
	case state.ReasonBenefitNotRealized:
		return fmt.Sprintf(
			"Repack did not realize the planned benefit for %s: planned to free %d %s [%s], but verified %d %s free [%s]; nodes still occupied or unavailable: [%s]. All %d replacement %s were scheduled (%d %s); inspect target-resource usage on the missing nodes.",
			resource, len(decision.Nodes.Planned), pluralNoun(len(decision.Nodes.Planned), "node", "nodes"), plannedNodes,
			len(decision.Nodes.Actual), pluralNoun(len(decision.Nodes.Actual), "node", "nodes"), actualNodes,
			FormatNodeNames(decision.Nodes.Missing),
			selectedNodePlacements+alternativeNodePlacements,
			pluralNoun(selectedNodePlacements+alternativeNodePlacements, "Pod", "Pods"),
			alternativeNodePlacements, pluralNoun(alternativeNodePlacements, "alternative placement", "alternative placements")) +
			podGroupReplacementStatusSuffix(run)
	default:
		return fmt.Sprintf(
			"Repack for %s reached terminal placement outcome %s: planned nodes [%s], actually free nodes [%s].",
			resource, decision.Reason, plannedNodes, actualNodes)
	}
}

func FormatNodeNames(nodeNames []string) string {
	const maxDisplayedNodes = 8
	if len(nodeNames) == 0 {
		return "none"
	}
	if len(nodeNames) <= maxDisplayedNodes {
		return strings.Join(nodeNames, ", ")
	}
	return fmt.Sprintf("%s, ... (%d more)", strings.Join(nodeNames[:maxDisplayedNodes], ", "), len(nodeNames)-maxDisplayedNodes)
}

func pluralNoun(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func FailureMessage(targetResource v1.ResourceName, reason string, err error) string {
	detail := "unknown error"
	if err != nil {
		detail = strings.TrimSpace(err.Error())
	}
	message := fmt.Sprintf("Repack failed for %s during %s: %s", DisplayResource(targetResource), failureStage(reason), detail)
	if reason == state.ReasonEvictionFailed &&
		(strings.Contains(strings.ToLower(detail), "evict") || strings.Contains(strings.ToLower(detail), "reject")) {
		message += "; check PodDisruptionBudgets and Kubernetes eviction events"
	}
	return strings.TrimRight(message, ".") + "."
}

func PlacementProgressMessage(run *repackv1alpha1.RepackRun, targetResource v1.ResourceName) string {
	total := len(run.Status.Relocations)
	selectedNodePlacements, alternativeNodePlacements, timedOutPlacements := PlacementOutcomeCounts(run)
	remaining := total - selectedNodePlacements - alternativeNodePlacements - timedOutPlacements
	return fmt.Sprintf(
		"Reconciling replacement placement for %s: %d of %d Pods reached selected nodes, %d used alternative nodes, %d timed out, and %d are still pending.",
		DisplayResource(targetResource), selectedNodePlacements, total,
		alternativeNodePlacements, timedOutPlacements, remaining) +
		podGroupReplacementStatusSuffix(run)
}

func podGroupReplacementStatusSuffix(run *repackv1alpha1.RepackRun) string {
	if run == nil {
		return ""
	}
	replacements := map[string]struct{}{}
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if nomination.Namespace == "" || nomination.PodGroupName == "" ||
			nomination.ReplacementPodGroupName == "" ||
			nomination.ReplacementPodGroupName == nomination.PodGroupName {
			continue
		}
		replacements[fmt.Sprintf("%s/%s -> %s/%s",
			nomination.Namespace, nomination.PodGroupName,
			nomination.Namespace, nomination.ReplacementPodGroupName)] = struct{}{}
	}
	if len(replacements) == 0 {
		return ""
	}
	values := make([]string, 0, len(replacements))
	for replacement := range replacements {
		values = append(values, replacement)
	}
	sort.Strings(values)
	const maxDisplayedReplacements = 4
	if len(values) > maxDisplayedReplacements {
		return fmt.Sprintf(" PodGroup replacements: %s, ... (%d more).",
			strings.Join(values[:maxDisplayedReplacements], ", "),
			len(values)-maxDisplayedReplacements)
	}
	return " PodGroup replacements: " + strings.Join(values, ", ") + "."
}

type SummaryValues struct {
	PodGroups      int
	MovedCards     int64
	FreedNodes     int32
	FragBefore     int32
	FragAfter      int32
	ScopeNodes     int32
	ScopePodGroups int32
}

func Summary(run *repackv1alpha1.RepackRun) SummaryValues {
	var result SummaryValues
	if run == nil || run.Status.Plan == nil {
		return result
	}
	result.PodGroups = len(run.Status.Plan.Moves)
	if run.Status.Plan.Summary == nil {
		return result
	}
	summary := run.Status.Plan.Summary
	result.MovedCards = summary.MovedCardCount
	result.FreedNodes = summary.FreedNodeCount
	result.FragBefore = summary.FragBeforePercent
	result.FragAfter = summary.FragAfterPercent
	if summary.ResolvedScope != nil {
		result.ScopeNodes = summary.ResolvedScope.NodeCount
		result.ScopePodGroups = summary.ResolvedScope.PodGroupCount
	}
	return result
}

func Result(run *repackv1alpha1.RepackRun) SummaryValues {
	var result SummaryValues
	if run == nil || run.Status.Result == nil {
		return result
	}
	result.MovedCards = run.Status.Result.MovedCardCount
	result.FreedNodes = run.Status.Result.FreedNodeCount
	result.FragAfter = run.Status.Result.FragAfterPercent
	return result
}

func PlacementOutcomeCounts(run *repackv1alpha1.RepackRun) (selectedNodePlacements, alternativeNodePlacements, timedOut int) {
	if run == nil {
		return 0, 0, 0
	}
	for index := range run.Status.Relocations {
		placementStatus := &run.Status.Relocations[index].Placement
		switch placementStatus.Phase {
		case repackv1alpha1.PodPlacementPlaced:
			if placementStatus.SelectedNodeName != "" &&
				placementStatus.SelectedNodeName == placementStatus.ActualNodeName {
				selectedNodePlacements++
			} else {
				alternativeNodePlacements++
			}
		case repackv1alpha1.PodPlacementTimedOut:
			timedOut++
		}
	}
	return selectedNodePlacements, alternativeNodePlacements, timedOut
}

func acceptedPodGroupCount(run *repackv1alpha1.RepackRun) int {
	if run == nil {
		return 0
	}
	podGroups := make(map[types.NamespacedName]struct{})
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		podGroups[types.NamespacedName{
			Namespace: nomination.Namespace,
			Name:      nomination.PodGroupName,
		}] = struct{}{}
	}
	return len(podGroups)
}

func DisplayResource(targetResource v1.ResourceName) string {
	if targetResource == "" {
		return "the target resource"
	}
	return string(targetResource)
}

func failureStage(reason string) string {
	switch reason {
	case state.ReasonInvalidConfiguration:
		return "configuration validation"
	case state.ReasonScopeResolutionFailed:
		return "scope resolution"
	case state.ReasonExecutionPreparationFailed:
		return "execution preparation"
	case state.ReasonEvictionFailed:
		return "Pod eviction"
	case state.ReasonExecutionTimedOut:
		return "execution deadline"
	case state.ReasonBenefitNotRealized:
		return "benefit verification"
	case state.ReasonPlacementTimedOut:
		return "replacement placement"
	case state.ReasonResultVerificationFailed:
		return "terminal metric verification"
	case state.ReasonExecutionInterrupted:
		return "engine recovery"
	case state.ReasonReconcileFailed:
		return "reconciliation"
	default:
		return reason
	}
}
