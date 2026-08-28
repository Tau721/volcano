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

package drain

import (
	"fmt"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

const candidateOrderEdgeCount = 3

// logCandidateOrder separates the operator-facing decision from its scoring
// mechanics. scored is already in preliminary score order;
// selectedPosition identifies the first candidate that passed full scheduling
// simulation (one-based).
func (s *drainState) logCandidateOrder(step int, ordered []scoredCandidate, selectedPosition int) {
	if !klog.V(3).Enabled() {
		return
	}
	if len(ordered) == 0 || selectedPosition < 1 || selectedPosition > len(ordered) {
		return
	}

	selected := ordered[selectedPosition-1]
	klog.V(3).InfoS("repack drain: selected drain target",
		"run", runName(s.ssn),
		"step", step,
		"resource", s.resource,
		"selectedTarget", selected.candidate.key,
		"targetLevel", selected.candidate.unit.Level,
		"nodesToFree", selected.candidate.unit.Nodes,
		"candidateCount", len(ordered),
		"selectedPosition", selectedPosition,
		"totalScore", selected.score.Total,
		"scorePreference", "higher-is-better",
		"additionalMoveCount", candidateMoveCount(selected.candidate),
		"additionalMovedResource", selected.candidate.additionalResource,
		"prospectivePlanImpact", formatPlanImpact(selected.score.Terms, s.resource),
		"selectionReason", candidateSelectionReason(ordered, selectedPosition),
		"orderBestToWorst", formatCandidateOrderSummary(ordered))

	if !klog.V(4).Enabled() {
		return
	}
	displayedIndexes, omittedCandidateCount := displayedCandidateIndexes(len(ordered), selectedPosition-1)
	for position, index := range displayedIndexes {
		if omittedCandidateCount > 0 && position == candidateOrderEdgeCount {
			klog.V(4).InfoS("repack drain: candidate order entries omitted",
				"run", runName(s.ssn),
				"step", step,
				"omittedCandidateCount", omittedCandidateCount)
		}
		entry := ordered[index]
		klog.V(4).InfoS("repack drain: drain target score",
			"run", runName(s.ssn),
			"step", step,
			"position", index+1,
			"candidateCount", len(ordered),
			"selected", index == selectedPosition-1,
			"drainTarget", entry.candidate.key,
			"targetLevel", entry.candidate.unit.Level,
			"nodesToFree", entry.candidate.unit.Nodes,
			"drainBenefitWeight", entry.candidate.unit.Weight,
			"totalScore", entry.score.Total,
			"additionalMoveCount", candidateMoveCount(entry.candidate),
			"additionalMovedResource", entry.candidate.additionalResource,
			"prospectivePlanImpact", formatPlanImpact(entry.score.Terms, s.resource),
			"scoreContributions", formatScoreContributions(entry.score.Terms, s.resource))
		s.logScoreFormula(step, index+1, entry)
	}
}

func candidateMoveCount(candidate candidate) int {
	if len(candidate.victims) > 0 {
		return len(candidate.victims)
	}
	return len(candidate.placed)
}

// displayedCandidateIndexes bounds candidate-order logs on large clusters and reports
// how many entries were omitted. requiredIndex is included when it identifies a
// valid middle entry; pass -1 when no additional entry is required.
func displayedCandidateIndexes(candidateCount, requiredIndex int) ([]int, int) {
	if candidateCount <= 0 {
		return nil, 0
	}
	if candidateCount <= candidateOrderEdgeCount*2 {
		indexes := make([]int, candidateCount)
		for index := range indexes {
			indexes[index] = index
		}
		return indexes, 0
	}
	indexes := make([]int, 0, candidateOrderEdgeCount*2)
	for index := 0; index < candidateOrderEdgeCount; index++ {
		indexes = append(indexes, index)
	}
	for index := candidateCount - candidateOrderEdgeCount; index < candidateCount; index++ {
		indexes = append(indexes, index)
	}
	if requiredIndex >= candidateOrderEdgeCount && requiredIndex < candidateCount-candidateOrderEdgeCount {
		indexes = append(indexes, requiredIndex)
		sort.Ints(indexes)
	}
	return indexes, candidateCount - len(indexes)
}

// formatCandidateOrderSummary presents the decision order without exposing scoring
// mechanics. V4/V5 logs carry the progressively deeper explanation.
func formatCandidateOrderSummary(ordered []scoredCandidate) []string {
	if len(ordered) == 0 {
		return nil
	}
	formatAt := func(index int) string {
		return fmt.Sprintf("#%d %s(score=%d)", index+1, ordered[index].candidate.key, ordered[index].score.Total)
	}
	displayedIndexes, omittedCandidateCount := displayedCandidateIndexes(len(ordered), -1)
	result := make([]string, 0, min(len(ordered), candidateOrderEdgeCount*2+1))
	for position, index := range displayedIndexes {
		if omittedCandidateCount > 0 && position == candidateOrderEdgeCount {
			result = append(result, fmt.Sprintf("... %d candidates omitted ...", omittedCandidateCount))
		}
		result = append(result, formatAt(index))
	}
	return result
}

func candidateSelectionReason(ordered []scoredCandidate, selectedPosition int) string {
	if len(ordered) == 0 {
		return ""
	}
	if selectedPosition > 1 {
		return "first scheduler-feasible target in score order"
	}
	if len(ordered) == 1 {
		return "only active drain target"
	}
	if ordered[0].score.Total > ordered[1].score.Total {
		return "highest weighted score"
	}
	if ordered[0].candidate.unit.Weight > ordered[1].candidate.unit.Weight {
		return "score tie; higher drain benefit"
	}
	return "score and drain benefit tie; lexical target name"
}

func formatPlanImpact(terms []framework.PlanScoreTerm, targetResource v1.ResourceName) string {
	impact := make([]string, 0, len(terms))
	for _, term := range terms {
		impact = append(impact, fmt.Sprintf("%s=%s", planScoreTermDisplayName(term.Name, targetResource), formatReadableScoreValue(term.Raw)))
	}
	return strings.Join(impact, " ")
}

func formatScoreContributions(terms []framework.PlanScoreTerm, targetResource v1.ResourceName) string {
	contributions := make([]string, 0, len(terms))
	for _, term := range terms {
		contributions = append(contributions, fmt.Sprintf(
			"%s=%d", planScoreTermDisplayName(term.Name, targetResource), term.Contribution))
	}
	return strings.Join(contributions, " ")
}

func planScoreTermDisplayName(termName string, targetResource v1.ResourceName) string {
	switch termName {
	case "affectedPodGroups":
		return "podGroups"
	case "movedPods":
		return "pods"
	case "movedResource":
		return string(targetResource)
	case "damagedResource":
		return "resourceInAffectedGangs"
	default:
		return termName
	}
}

func formatReadableScoreValue(value int64) string {
	return fmt.Sprintf("%d", value)
}

// logScoreFormula reserves normalization mechanics for V5. The values are only
// comparable within this planning step because each term is min-max normalized
// across that step's active preliminary candidates.
func (s *drainState) logScoreFormula(step, position int, entry scoredCandidate) {
	if !klog.V(5).Enabled() {
		return
	}
	for _, term := range entry.score.Terms {
		klog.V(5).InfoS("repack drain: drain target score formula",
			"run", runName(s.ssn),
			"step", step,
			"resource", s.resource,
			"position", position,
			"drainTarget", entry.candidate.key,
			"term", term.Name,
			"rawValue", term.Raw,
			"weight", term.Weight,
			"strategyScore", term.Score,
			"weightedContribution", term.Contribution,
			"normalizationScope", "current-step-candidates")
	}
}
