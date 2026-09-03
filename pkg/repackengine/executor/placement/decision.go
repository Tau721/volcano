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

// Package placement owns replacement-Pod placement decisions and terminal
// result evaluation. Kubernetes writes and workqueue scheduling remain Engine
// orchestration concerns.
package placement

import (
	"sort"
	"time"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

func Candidates(run *repackv1alpha1.RepackRun) []*repackv1alpha1.PodRelocationStatus {
	if run == nil {
		return nil
	}
	result := make([]*repackv1alpha1.PodRelocationStatus, 0)
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if relocation.Placement.ReplacementPodName == "" || relocation.Placement.ReplacementPodUID == "" || relocation.Placement.SelectedNodeName != "" {
			continue
		}
		if relocation.Placement.Phase == repackv1alpha1.PodPlacementWaitingForNodeSelection {
			result = append(result, relocation)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return IdentityForRelocation(result[left]).Less(IdentityForRelocation(result[right]))
	})
	return result
}

func Receivers(nodes []*schedapi.NodeInfo, freedNodes []string, plannedNode string, task *schedapi.TaskInfo) []*schedapi.NodeInfo {
	if task == nil {
		return nil
	}
	freed := make(map[string]struct{}, len(freedNodes))
	for _, node := range freedNodes {
		freed[node] = struct{}{}
	}
	byName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, node := range nodes {
		if node != nil {
			byName[node.Name] = node
		}
	}
	receivers := make([]*schedapi.NodeInfo, 0, len(nodes))
	appendIfImmediatelyIdle := func(node *schedapi.NodeInfo) {
		if node == nil {
			return
		}
		if _, excluded := freed[node.Name]; excluded || !task.InitResreq.LessEqual(engineapi.NodeFreeCapacity(node), schedapi.Zero) {
			return
		}
		receivers = append(receivers, node)
	}
	appendIfImmediatelyIdle(byName[plannedNode])
	names := make([]string, 0, len(byName))
	for name := range byName {
		if name != plannedNode {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		appendIfImmediatelyIdle(byName[name])
	}
	return receivers
}

func Complete(run *repackv1alpha1.RepackRun) bool {
	if run == nil || len(run.Status.Relocations) == 0 {
		return false
	}
	accepted := 0
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if !evictionAllowsPlacement(relocation.Eviction.Phase) {
			continue
		}
		accepted++
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementPlaced, repackv1alpha1.PodPlacementTimedOut:
		default:
			return false
		}
	}
	return accepted > 0
}

func evictionAllowsPlacement(phase repackv1alpha1.PodEvictionPhase) bool {
	return phase == repackv1alpha1.PodEvictionAccepted ||
		phase == repackv1alpha1.PodEvictionIndirectlyRemoved
}

type FreedNodeComparison struct {
	Planned    []string
	Actual     []string
	Missing    []string
	Unexpected []string
	Equal      bool
}

type TerminalDecision struct {
	Succeeded bool
	Reason    string
	Nodes     FreedNodeComparison
}

func EvaluateTerminal(run *repackv1alpha1.RepackRun, resultSnapshotUnavailable bool) TerminalDecision {
	_, alternativeNodePlacements, timedOut := outcomeCounts(run)
	nodes := CompareFreedNodeSets(run)
	switch {
	case timedOut > 0:
		return TerminalDecision{Reason: state.ReasonPlacementTimedOut, Nodes: nodes}
	case resultSnapshotUnavailable || run == nil || run.Status.Result == nil || !run.Status.Result.MetricsVerified:
		return TerminalDecision{Reason: state.ReasonResultVerificationFailed, Nodes: nodes}
	case !nodes.Equal:
		return TerminalDecision{Reason: state.ReasonBenefitNotRealized, Nodes: nodes}
	case alternativeNodePlacements > 0:
		return TerminalDecision{Succeeded: true, Reason: state.ReasonExecutionCompletedWithAlternativePlacement, Nodes: nodes}
	default:
		return TerminalDecision{Succeeded: true, Reason: state.ReasonExecutionCompleted, Nodes: nodes}
	}
}

func CompareFreedNodeSets(run *repackv1alpha1.RepackRun) FreedNodeComparison {
	var planned, actual []string
	if run != nil && run.Status.Plan != nil {
		planned = run.Status.Plan.FreedNodes
	}
	if run != nil && run.Status.Result != nil {
		actual = run.Status.Result.FreedNodes
	}
	result := FreedNodeComparison{Planned: SortedUniqueNodeNames(planned), Actual: SortedUniqueNodeNames(actual)}
	plannedSet := make(map[string]struct{}, len(result.Planned))
	actualSet := make(map[string]struct{}, len(result.Actual))
	for _, nodeName := range result.Planned {
		plannedSet[nodeName] = struct{}{}
	}
	for _, nodeName := range result.Actual {
		actualSet[nodeName] = struct{}{}
	}
	for _, nodeName := range result.Planned {
		if _, found := actualSet[nodeName]; !found {
			result.Missing = append(result.Missing, nodeName)
		}
	}
	for _, nodeName := range result.Actual {
		if _, found := plannedSet[nodeName]; !found {
			result.Unexpected = append(result.Unexpected, nodeName)
		}
	}
	result.Equal = len(result.Missing) == 0 && len(result.Unexpected) == 0
	return result
}

func SortedUniqueNodeNames(nodeNames []string) []string {
	unique := make(map[string]struct{}, len(nodeNames))
	for _, nodeName := range nodeNames {
		if nodeName != "" {
			unique[nodeName] = struct{}{}
		}
	}
	result := make([]string, 0, len(unique))
	for nodeName := range unique {
		result = append(result, nodeName)
	}
	sort.Strings(result)
	return result
}

func ObservationDeadlinePassed(run *repackv1alpha1.RepackRun, now time.Time) bool {
	return run != nil && run.Status.ExecutionDeadline != nil &&
		!now.Before(run.Status.ExecutionDeadline.Time)
}

func FreedNodeVerificationPending(run *repackv1alpha1.RepackRun, now time.Time) (FreedNodeComparison, bool) {
	comparison := CompareFreedNodeSets(run)
	return comparison, !comparison.Equal && !ObservationDeadlinePassed(run, now)
}

func BindingsVisible(nodes []*schedapi.NodeInfo, relocations []repackv1alpha1.PodRelocationStatus) bool {
	expected := make(map[string]string)
	for index := range relocations {
		relocation := &relocations[index]
		if relocation.Placement.Phase == repackv1alpha1.PodPlacementPlaced {
			if relocation.Placement.ReplacementPodUID == "" || relocation.Placement.ActualNodeName == "" {
				return false
			}
			expected[string(relocation.Placement.ReplacementPodUID)] = relocation.Placement.ActualNodeName
		}
	}
	if len(expected) == 0 {
		return true
	}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		for _, task := range node.Tasks {
			if task == nil {
				continue
			}
			expectedNode, found := expected[string(task.UID)]
			if found && expectedNode == node.Name {
				delete(expected, string(task.UID))
			}
		}
	}
	return len(expected) == 0
}

func MarkBenefitUnverified(run *repackv1alpha1.RepackRun) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return
	}
	if run.Status.Result == nil {
		run.Status.Result = &repackv1alpha1.RepackResult{}
	}
	run.Status.Result.FragAfterPercent = run.Status.Plan.Summary.FragBeforePercent
	run.Status.Result.FreedNodeCount = 0
	run.Status.Result.FreedNodes = nil
	run.Status.Result.MetricsVerified = false
}

func outcomeCounts(run *repackv1alpha1.RepackRun) (selectedNodePlacements, alternativeNodePlacements, timedOut int) {
	if run == nil {
		return 0, 0, 0
	}
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		switch relocation.Placement.Phase {
		case repackv1alpha1.PodPlacementPlaced:
			if relocation.Placement.SelectedNodeName != "" && relocation.Placement.ActualNodeName == relocation.Placement.SelectedNodeName {
				selectedNodePlacements++
			} else {
				alternativeNodePlacements++
			}
		case repackv1alpha1.PodPlacementTimedOut:
			timedOut++
		}
	}
	return
}
