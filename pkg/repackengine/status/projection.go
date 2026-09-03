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
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	"volcano.sh/repack-controller/pkg/placement"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

func TerminalOutcome(run *repackv1alpha1.RepackRun) string {
	for _, c := range run.Status.Conditions {
		if c.Status == metav1.ConditionTrue &&
			(c.Type == state.CondComplete || c.Type == state.CondFailed) {
			return c.Reason
		}
	}
	return "Unknown"
}

// StampLifecycle records StartTime on first Running and CompletionTime on first
// terminal — the anchors the controller's TTL-GC and Execute cooldown (K=1) key
// off. Both stamps are nil-guarded so the engine and the controller (which guards
// the same fields) never clobber each other: whoever reaches the state first wins.
func StampLifecycle(run *repackv1alpha1.RepackRun, now time.Time) {
	if run.Status.Phase == repackv1alpha1.RepackRunning && run.Status.StartTime == nil {
		t := metav1.NewTime(now)
		run.Status.StartTime = &t
	}
	if state.IsTerminal(run.Status.Phase) && run.Status.CompletionTime == nil {
		t := metav1.NewTime(now)
		run.Status.CompletionTime = &t
	}
}

// ApplyPlan maps the search outcome onto the immutable status.plan. DryRun and
// Execute expose the same complete pre-eviction decision, including predicted
// benefit. Execute acceptance and observed cluster metrics are deliberately
// reported through status.relocations and status.result instead of rewriting
// this audit record.
func ApplyPlan(
	run *repackv1alpha1.RepackRun,
	report engineapi.Report,
	plan *engineapi.RepackPlan,
	targetResource v1.ResourceName,
	owners map[string]*repackv1alpha1.WorkloadRef,
	resolvedScope *repackv1alpha1.ResolvedScope,
) {
	moves := BuildStatusMoves(plan, targetResource, owners)
	summary := BuildRepackSummary(report)
	if summary != nil {
		var cards int64
		for _, m := range moves {
			cards += m.Cards
		}
		summary.MovedCardCount = cards
		if resolvedScope != nil {
			summary.ResolvedScope = resolvedScope.DeepCopy()
		}
	}
	run.Status.Plan = &repackv1alpha1.RepackPlan{
		Summary:    summary,
		Moves:      moves,
		FreedNodes: SortedFreedNodeNames(plan),
	}
}

// BuildResolvedScope summarizes the two independent action-scope axes. The
// fragmentation report remains cluster-wide: node scope limits drain targets,
// while PodGroup scope limits which accelerator consumers may be moved.
func BuildResolvedScope(nodes []*schedapi.NodeInfo, scope *enginescope.Matcher, targetResource v1.ResourceName) *repackv1alpha1.ResolvedScope {
	resolved := &repackv1alpha1.ResolvedScope{}
	podGroups := make(map[schedapi.JobID]struct{})
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if engineapi.Scalar(node.Allocatable, targetResource) > 0 &&
			(scope == nil || scope.NodeInScope(node)) {
			resolved.NodeCount++
		}
		for _, task := range node.Tasks {
			if task == nil || task.Job == "" || engineapi.Scalar(task.Resreq, targetResource) <= 0 {
				continue
			}
			if scope == nil || scope.InScope(task.Job) {
				podGroups[task.Job] = struct{}{}
			}
		}
	}
	resolved.PodGroupCount = int32(len(podGroups))
	return resolved
}

func MarkExecuteNotPerformed(run *repackv1alpha1.RepackRun) {
	if run == nil {
		return
	}
	run.Status.Result = nil
	run.Status.Relocations = nil
}

// PrepareExecuteRelocations records the complete per-Pod execution journal
// before the eviction barrier. A later commit retains only Pods that require a
// replacement after a directly accepted or indirectly observed removal.
type PodGroupPlacementPolicyReader interface {
	PodGroupUsesSubGroupPolicy(schedapi.JobID) bool
}

func PrepareExecuteRelocations(
	run *repackv1alpha1.RepackRun,
	plan *engineapi.RepackPlan,
	policyReader PodGroupPlacementPolicyReader,
) error {
	if run == nil {
		return nil
	}
	relocations, err := BuildPodRelocations(plan, policyReader)
	if err != nil {
		return err
	}
	run.Status.Relocations = relocations
	return nil
}

func InitializeNoopExecuteResult(run *repackv1alpha1.RepackRun) {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return
	}
	run.Status.Result = &repackv1alpha1.RepackResult{
		FragAfterPercent: run.Status.Plan.Summary.FragBeforePercent,
		MetricsVerified:  true,
	}
}

// RealizedFreedNodeNames derives source nodes whose complete planned victim set
// was removed and retained as a relocation record. status.plan remains the
// complete proposal, so a source with a genuinely rejected victim is not excluded.
func RealizedFreedNodeNames(run *repackv1alpha1.RepackRun) []string {
	if run == nil || run.Status.Plan == nil {
		return nil
	}
	accepted := make(map[placementexecutor.Identity]struct{}, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		accepted[placementexecutor.IdentityForRelocation(relocation)] = struct{}{}
	}
	var result []string
	for _, nodeName := range run.Status.Plan.FreedNodes {
		hasPlannedVictim := false
		complete := true
		for moveIndex := range run.Status.Plan.Moves {
			move := &run.Status.Plan.Moves[moveIndex]
			for podIndex := range move.Pods {
				pod := &move.Pods[podIndex]
				if pod.FromNode != nodeName {
					continue
				}
				hasPlannedVictim = true
				key := placementexecutor.IdentityForMove(move.Namespace, move.PodGroupName, pod.Name, pod.ToNode)
				if _, found := accepted[key]; !found {
					complete = false
					break
				}
			}
			if !complete {
				break
			}
		}
		if hasPlannedVictim && complete {
			result = append(result, nodeName)
		}
	}
	return result
}

// BuildStatusMoves groups the plan's per-task relocations into per-PodGroup status moves;
// fromNode/toNode live per-pod in pods[] (a gang's pods may spread across nodes).
// moves is a pure plan (identical in DryRun/Execute). Deterministic order.
func BuildStatusMoves(plan *engineapi.RepackPlan, targetResource v1.ResourceName, owners map[string]*repackv1alpha1.WorkloadRef) []repackv1alpha1.RepackMove {
	if plan == nil {
		return nil
	}
	moveIndexByPodGroup := map[string]int{} // JobID ("ns/name") -> index in statusMoves
	statusMoves := []repackv1alpha1.RepackMove{}
	for _, move := range plan.Moves {
		if move == nil || move.Task == nil || move.To == move.From {
			continue
		}
		podGroupID := string(move.Task.Job)
		moveIndex, ok := moveIndexByPodGroup[podGroupID]
		if !ok {
			moveIndex = len(statusMoves)
			moveIndexByPodGroup[podGroupID] = moveIndex
			namespace, podGroupName := SplitPodGroupID(podGroupID)
			statusMoves = append(statusMoves, repackv1alpha1.RepackMove{
				Namespace:    namespace,
				PodGroupName: podGroupName,
				Owner:        owners[podGroupID],
			})
		}
		var cards int64
		if move.Task.Resreq != nil {
			// Report whole devices to users; Resreq is stored in milli-units.
			cards = engineapi.Cards(move.Task.Resreq, targetResource)
		}
		statusMoves[moveIndex].Cards += cards
		statusMoves[moveIndex].Pods = append(statusMoves[moveIndex].Pods, repackv1alpha1.PodMove{
			Name:     move.Task.Name,
			FromNode: move.From,
			ToNode:   move.To,
			Cards:    cards,
		})
	}
	sort.Slice(statusMoves, func(i, j int) bool {
		if statusMoves[i].Namespace != statusMoves[j].Namespace {
			return statusMoves[i].Namespace < statusMoves[j].Namespace
		}
		return statusMoves[i].PodGroupName < statusMoves[j].PodGroupName
	})
	for moveIndex := range statusMoves {
		pods := statusMoves[moveIndex].Pods
		sort.Slice(pods, func(a, b int) bool {
			switch {
			case pods[a].Name != pods[b].Name:
				return pods[a].Name < pods[b].Name
			case pods[a].FromNode != pods[b].FromNode:
				return pods[a].FromNode < pods[b].FromNode
			default:
				return pods[a].ToNode < pods[b].ToNode
			}
		})
	}
	return statusMoves
}

// SplitPodGroupID splits a "namespace/name" JobID; missing "/" -> ("", id).
func SplitPodGroupID(podGroupID string) (namespace, name string) {
	if separatorIndex := strings.IndexByte(podGroupID, '/'); separatorIndex >= 0 {
		return podGroupID[:separatorIndex], podGroupID[separatorIndex+1:]
	}
	return "", podGroupID
}

// SortedFreedNodeNames lists the names of nodes the plan empties (sorted).
func SortedFreedNodeNames(plan *engineapi.RepackPlan) []string {
	if plan == nil {
		return nil
	}
	freedNodeNames := append([]string(nil), plan.FreedNodes...)
	sort.Strings(freedNodeNames)
	return freedNodeNames
}

// BuildRepackSummary renders the flat metrics layer. "Worth repacking?" is not
// here — it is folded into the terminal condition's reason. MovedCardCount is
// filled by ApplyPlan from moves; FragBefore/After are the target resource's
// cluster-wide rates and do not use resolved scope as their denominator.
func BuildRepackSummary(report engineapi.Report) *repackv1alpha1.RepackSummary {
	return &repackv1alpha1.RepackSummary{
		FragBeforePercent: PercentagePoints(report.FragmentationRateBefore),
		FragAfterPercent:  PercentagePoints(report.FragmentationRateAfter),
		FreedNodeCount:    int32(report.NodesFreed),
	}
}

// PercentagePoints rounds a 0-1 fraction to an integer percentage point, clamped to [0,100].
func PercentagePoints(fraction float64) int32 {
	percentage := int32(fraction*100 + 0.5)
	if percentage < 0 {
		return 0
	}
	if percentage > 100 {
		return 100
	}
	return percentage
}

// BuildPodRelocations renders per-Pod relocation journals. PodGroups
// without SubGroup policies are explicitly treated as homogeneous, so they do
// not pay the API-size cost of storing a hash on every relocation. A SubGroup
// policy opts the PodGroup into hash-based matching for renamed heterogeneous
// replacements.
func BuildPodRelocations(
	plan *engineapi.RepackPlan,
	policyReader PodGroupPlacementPolicyReader,
) ([]repackv1alpha1.PodRelocationStatus, error) {
	if plan == nil {
		return nil, nil
	}
	if policyReader == nil {
		return nil, fmt.Errorf("PodGroup placement policy reader is required")
	}
	relocations := make([]repackv1alpha1.PodRelocationStatus, 0, len(plan.Moves))
	for _, move := range plan.Moves {
		if move == nil || move.Task == nil || move.To == move.From {
			continue
		}
		task := move.Task
		namespace, podGroupName, valid := engineapi.PodGroupIdentity(task)
		if !valid {
			return nil, fmt.Errorf(
				"planned victim Pod %s/%s has invalid PodGroup identity %q",
				task.Namespace, task.Name, task.Job)
		}
		var victimPodUID types.UID
		if task.Pod != nil {
			victimPodUID = task.Pod.UID
		}
		schedulingRequirementsHash := ""
		if policyReader.PodGroupUsesSubGroupPolicy(task.Job) {
			var err error
			schedulingRequirementsHash, err = placement.SchedulingRequirementsHash(task.Pod)
			if err != nil {
				return nil, fmt.Errorf(
					"derive scheduling requirements for SubGroup victim Pod %s/%s in PodGroup %s: %w",
					task.Namespace, task.Name, task.Job, err)
			}
			klog.V(4).InfoS("repack: recorded scheduling requirements for SubGroup replacement matching",
				"pod", task.Namespace+"/"+task.Name, "podGroup", task.Job,
				"schedulingRequirementsHash", schedulingRequirementsHash)
		}
		relocations = append(relocations, repackv1alpha1.PodRelocationStatus{
			Namespace:                  namespace,
			PodGroupName:               podGroupName,
			VictimPodName:              task.Name,
			VictimPodUID:               victimPodUID,
			SchedulingRequirementsHash: schedulingRequirementsHash,
			PlannedNodeName:            move.To,
			Eviction: repackv1alpha1.PodEvictionStatus{
				Phase: repackv1alpha1.PodEvictionPending,
			},
			Placement: repackv1alpha1.PodPlacementStatus{
				Phase: repackv1alpha1.PodPlacementWaitingForReplacement,
			},
		})
	}
	sort.Slice(relocations, func(left, right int) bool {
		return placementexecutor.IdentityForRelocation(&relocations[left]).Less(
			placementexecutor.IdentityForRelocation(&relocations[right]))
	})
	return relocations, nil
}
