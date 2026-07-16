/*
Copyright 2017 The Kubernetes Authors.
Copyright 2017-2025 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"volcano.sh/apis/pkg/apis/scheduling"
)

// ContainsHardPodGroupAntiAffinity returns whether the job has hard cross-PodGroup anti-affinity.
func (ji *JobInfo) ContainsHardPodGroupAntiAffinity() bool {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil {
		return false
	}
	anti := ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity
	return anti != nil && len(anti.Required) > 0
}

// HasPreferredPodGroupAntiAffinity returns whether the job has soft cross-PodGroup anti-affinity.
func (ji *JobInfo) HasPreferredPodGroupAntiAffinity() bool {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil {
		return false
	}
	anti := ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity
	return anti != nil && len(anti.Preferred) > 0
}

// ContainsHardSubGroupAffinity returns whether the job has hard intra-PodGroup SubJob affinity.
func (ji *JobInfo) ContainsHardSubGroupAffinity() bool {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil {
		return false
	}
	affinity := ji.PodGroup.Spec.TopologyAffinity.SubGroupAffinity
	return affinity != nil && len(affinity.Required) > 0
}

// ContainsHardSubGroupAntiAffinity returns whether the job has hard intra-PodGroup SubJob anti-affinity.
func (ji *JobInfo) ContainsHardSubGroupAntiAffinity() bool {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil {
		return false
	}
	anti := ji.PodGroup.Spec.TopologyAffinity.SubGroupAntiAffinity
	return anti != nil && len(anti.Required) > 0
}

// ContainsHardSubGroupTopologyAffinity returns whether the job has any hard SubGroup topology affinity rule.
func (ji *JobInfo) ContainsHardSubGroupTopologyAffinity() bool {
	return ji.ContainsHardSubGroupAffinity() || ji.ContainsHardSubGroupAntiAffinity()
}

// HasPreferredSubGroupAffinity returns whether the job has soft intra-PodGroup SubJob affinity.
func (ji *JobInfo) HasPreferredSubGroupAffinity() bool {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil {
		return false
	}
	affinity := ji.PodGroup.Spec.TopologyAffinity.SubGroupAffinity
	return affinity != nil && len(affinity.Preferred) > 0
}

// HasPreferredSubGroupAntiAffinity returns whether the job has soft intra-PodGroup SubJob anti-affinity.
func (ji *JobInfo) HasPreferredSubGroupAntiAffinity() bool {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil {
		return false
	}
	anti := ji.PodGroup.Spec.TopologyAffinity.SubGroupAntiAffinity
	return anti != nil && len(anti.Preferred) > 0
}

// HasPreferredSubGroupTopologyAffinity returns whether the job has any soft SubGroup topology affinity rule.
func (ji *JobInfo) HasPreferredSubGroupTopologyAffinity() bool {
	return ji.HasPreferredSubGroupAffinity() || ji.HasPreferredSubGroupAntiAffinity()
}

// RequiredPodGroupAntiAffinityTerms returns hard cross-PodGroup anti-affinity terms.
func (ji *JobInfo) RequiredPodGroupAntiAffinityTerms() []scheduling.PodGroupAffinityTerm {
	if !ji.ContainsHardPodGroupAntiAffinity() {
		return nil
	}
	return ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity.Required
}

// PreferredPodGroupAntiAffinityTerms returns soft cross-PodGroup anti-affinity terms.
func (ji *JobInfo) PreferredPodGroupAntiAffinityTerms() []scheduling.PodGroupAffinityTerm {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil ||
		ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity == nil {
		return nil
	}
	return ji.PodGroup.Spec.TopologyAffinity.PodGroupAntiAffinity.Preferred
}

// RequiredSubGroupAffinityTerms returns hard intra-PodGroup SubJob affinity terms.
func (ji *JobInfo) RequiredSubGroupAffinityTerms() []scheduling.SubGroupAffinityTerm {
	if !ji.ContainsHardSubGroupAffinity() {
		return nil
	}
	return ji.PodGroup.Spec.TopologyAffinity.SubGroupAffinity.Required
}

// PreferredSubGroupAffinityTerms returns soft intra-PodGroup SubJob affinity terms.
func (ji *JobInfo) PreferredSubGroupAffinityTerms() []scheduling.SubGroupAffinityTerm {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil ||
		ji.PodGroup.Spec.TopologyAffinity.SubGroupAffinity == nil {
		return nil
	}
	return ji.PodGroup.Spec.TopologyAffinity.SubGroupAffinity.Preferred
}

// RequiredSubGroupAntiAffinityTerms returns hard intra-PodGroup SubJob anti-affinity terms.
func (ji *JobInfo) RequiredSubGroupAntiAffinityTerms() []scheduling.SubGroupAffinityTerm {
	if !ji.ContainsHardSubGroupAntiAffinity() {
		return nil
	}
	return ji.PodGroup.Spec.TopologyAffinity.SubGroupAntiAffinity.Required
}

// PreferredSubGroupAntiAffinityTerms returns soft intra-PodGroup SubJob anti-affinity terms.
func (ji *JobInfo) PreferredSubGroupAntiAffinityTerms() []scheduling.SubGroupAffinityTerm {
	if ji.PodGroup == nil || ji.PodGroup.Spec.TopologyAffinity == nil ||
		ji.PodGroup.Spec.TopologyAffinity.SubGroupAntiAffinity == nil {
		return nil
	}
	return ji.PodGroup.Spec.TopologyAffinity.SubGroupAntiAffinity.Preferred
}

// WithTopologyAffinity returns whether the job declares topologyAffinity.
func (ji *JobInfo) WithTopologyAffinity() bool {
	return ji.PodGroup != nil && ji.PodGroup.Spec.TopologyAffinity != nil
}

// ResolvePodGroupTermTier resolves topologyTier or topologyTierName on a PodGroupAffinityTerm.
func ResolvePodGroupTermTier(term scheduling.PodGroupAffinityTerm, tierNameMap HyperNodeTierNameMap) (int, error) {
	if term.TopologyTier != nil && term.TopologyTierName != "" {
		return 0, fmt.Errorf("topologyTier and topologyTierName are mutually exclusive")
	}
	if term.TopologyTier != nil {
		return int(*term.TopologyTier), nil
	}
	if term.TopologyTierName != "" {
		tier, ok := tierNameMap[term.TopologyTierName]
		if !ok {
			return 0, fmt.Errorf("unknown topologyTierName %q", term.TopologyTierName)
		}
		return tier, nil
	}
	return 0, fmt.Errorf("topologyTier or topologyTierName must be set")
}

// ResolveSubGroupTermTier resolves topologyTier or topologyTierName on a SubGroupAffinityTerm.
func ResolveSubGroupTermTier(term scheduling.SubGroupAffinityTerm, tierNameMap HyperNodeTierNameMap) (int, error) {
	if term.TopologyTier != nil && term.TopologyTierName != "" {
		return 0, fmt.Errorf("topologyTier and topologyTierName are mutually exclusive")
	}
	if term.TopologyTier != nil {
		return int(*term.TopologyTier), nil
	}
	if term.TopologyTierName != "" {
		tier, ok := tierNameMap[term.TopologyTierName]
		if !ok {
			return 0, fmt.Errorf("unknown topologyTierName %q", term.TopologyTierName)
		}
		return tier, nil
	}
	return 0, fmt.Errorf("topologyTier or topologyTierName must be set")
}

// SubJobPolicyName returns the SubGroupPolicy name for a SubJob created from a policy.
func SubJobPolicyName(subJob *SubJobInfo) string {
	if subJob == nil {
		return ""
	}
	prefix := fmt.Sprintf("%s/", subJob.Job)
	gid := string(subJob.GID)
	if strings.HasPrefix(gid, prefix) {
		return strings.TrimPrefix(gid, prefix)
	}
	return ""
}

// PodGroupMatchesTerm reports whether otherJob is selected by term's podGroupSelector (excluding selfJob).
func PodGroupMatchesTerm(term scheduling.PodGroupAffinityTerm, selfJob, otherJob *JobInfo) bool {
	if otherJob == nil || otherJob.PodGroup == nil || selfJob == nil || selfJob.UID == otherJob.UID {
		return false
	}
	if !matchesNamespaceSelector(term.NamespaceSelector, selfJob.Namespace, otherJob.Namespace) {
		return false
	}
	if term.PodGroupSelector == nil {
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(term.PodGroupSelector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(otherJob.PodGroup.Labels))
}

func matchesNamespaceSelector(namespaceSelector *metav1.LabelSelector, selfNamespace, otherNamespace string) bool {
	if namespaceSelector == nil {
		return selfNamespace == otherNamespace
	}
	// Cross-namespace matching via namespaceSelector requires Namespace informer (future work).
	return selfNamespace == otherNamespace
}

// ComputeSubJobAllocatedHyperNode returns the HyperNode that contains all allocated tasks in subJob.
func ComputeSubJobAllocatedHyperNode(
	subJob *SubJobInfo,
	hyperNodes HyperNodeInfoMap,
	nodesByHyperNode map[string]sets.Set[string],
) string {
	if subJob == nil || len(hyperNodes) == 0 || len(nodesByHyperNode) == 0 {
		return ""
	}
	hyperNodeSet := sets.New[string]()
	for name := range hyperNodes {
		hyperNodeSet.Insert(name)
	}
	return getSubJobAllocatedHyperNodeFromTasks(subJob, hyperNodeSet, nodesByHyperNode, hyperNodes)
}

// ComputeJobAllocatedHyperNode returns the HyperNode that contains all allocated tasks in job.
func ComputeJobAllocatedHyperNode(
	job *JobInfo,
	hyperNodes HyperNodeInfoMap,
	nodesByHyperNode map[string]sets.Set[string],
) string {
	if job == nil || len(hyperNodes) == 0 || len(nodesByHyperNode) == 0 {
		return ""
	}
	hyperNodeSet := sets.New[string]()
	for name := range hyperNodes {
		hyperNodeSet.Insert(name)
	}

	var lca string
	for _, subJob := range job.SubJobs {
		subJobHyperNode := ComputeSubJobAllocatedHyperNode(subJob, hyperNodes, nodesByHyperNode)
		if subJobHyperNode == "" {
			continue
		}
		lca = hyperNodes.GetLCAHyperNode(lca, subJobHyperNode)
	}
	if lca != "" {
		return lca
	}
	return getAllocatedHyperNodeFromTasks(collectJobAllocatedTasks(job), hyperNodeSet, nodesByHyperNode, hyperNodes)
}

// SyncJobAllocatedHyperNode refreshes job and subJob AllocatedHyperNode from remaining allocated tasks.
// Call this when task placement changes (for example pod deletion) so placement tracks running pods.
func SyncJobAllocatedHyperNode(
	job *JobInfo,
	hyperNodes HyperNodeInfoMap,
	nodesByHyperNode map[string]sets.Set[string],
) {
	if job == nil {
		return
	}

	for _, subJob := range job.SubJobs {
		if subJob.AllocatedTaskNum() == 0 {
			subJob.AllocatedHyperNode = ""
			continue
		}
		if len(hyperNodes) == 0 || len(nodesByHyperNode) == 0 {
			continue
		}
		subJob.AllocatedHyperNode = ComputeSubJobAllocatedHyperNode(subJob, hyperNodes, nodesByHyperNode)
	}

	if !jobHasAllocatedTasks(job) {
		job.AllocatedHyperNode = ""
		return
	}
	if len(hyperNodes) == 0 || len(nodesByHyperNode) == 0 {
		return
	}
	job.AllocatedHyperNode = ComputeJobAllocatedHyperNode(job, hyperNodes, nodesByHyperNode)
}

func jobHasAllocatedTasks(job *JobInfo) bool {
	if job.AllocatedTaskNum() > 0 {
		return true
	}
	for _, subJob := range job.SubJobs {
		if subJob.AllocatedTaskNum() > 0 {
			return true
		}
	}
	return false
}

// getJobAllocatedHyperNode returns job.AllocatedHyperNode when set, otherwise infers it from
// placed tasks (for example matching PodGroups without network topology).
func getJobAllocatedHyperNode(
	job *JobInfo,
	hyperNodes HyperNodeInfoMap,
	nodesByHyperNode map[string]sets.Set[string],
) string {
	if job == nil || len(hyperNodes) == 0 {
		return ""
	}
	if job.AllocatedHyperNode != "" {
		return job.AllocatedHyperNode
	}
	if len(nodesByHyperNode) == 0 {
		return ""
	}

	hyperNodeSet := sets.New[string]()
	for name := range hyperNodes {
		hyperNodeSet.Insert(name)
	}

	var lca string
	for _, subJob := range job.SubJobs {
		subJobHyperNode := subJob.AllocatedHyperNode
		if subJobHyperNode == "" {
			subJobHyperNode = getSubJobAllocatedHyperNodeFromTasks(subJob, hyperNodeSet, nodesByHyperNode, hyperNodes)
		}
		if subJobHyperNode == "" {
			continue
		}
		lca = hyperNodes.GetLCAHyperNode(lca, subJobHyperNode)
	}
	if lca != "" {
		return lca
	}

	return getAllocatedHyperNodeFromTasks(collectJobAllocatedTasks(job), hyperNodeSet, nodesByHyperNode, hyperNodes)
}

func collectJobAllocatedTasks(job *JobInfo) []*TaskInfo {
	tasks := make([]*TaskInfo, 0)
	for _, subJob := range job.SubJobs {
		tasks = append(tasks, collectSubJobAllocatedTasks(subJob)...)
	}
	if len(tasks) > 0 {
		return tasks
	}
	for status, taskMap := range job.TaskStatusIndex {
		if !AllocatedStatus(status) {
			continue
		}
		for _, task := range taskMap {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func collectSubJobAllocatedTasks(subJob *SubJobInfo) []*TaskInfo {
	if subJob == nil {
		return nil
	}
	tasks := make([]*TaskInfo, 0, subJob.AllocatedTaskNum())
	for status, taskMap := range subJob.TaskStatusIndex {
		if !AllocatedStatus(status) {
			continue
		}
		for _, task := range taskMap {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func getSubJobAllocatedHyperNodeFromTasks(
	subJob *SubJobInfo,
	hyperNodeSet sets.Set[string],
	nodesByHyperNode map[string]sets.Set[string],
	hyperNodes HyperNodeInfoMap,
) string {
	return getAllocatedHyperNodeFromTasks(
		collectSubJobAllocatedTasks(subJob), hyperNodeSet, nodesByHyperNode, hyperNodes,
	)
}

func getAllocatedHyperNodeFromTasks(
	tasks []*TaskInfo,
	hyperNodeSet sets.Set[string],
	nodesByHyperNode map[string]sets.Set[string],
	hyperNodes HyperNodeInfoMap,
) string {
	if len(tasks) == 0 {
		return ""
	}

	var candidateHyperNodes sets.Set[string]
	for _, task := range tasks {
		if task.NodeName == "" {
			continue
		}

		search := hyperNodeSet
		if candidateHyperNodes != nil {
			search = candidateHyperNodes
		}

		taskHyperNodes := sets.New[string]()
		for hyperNode := range search {
			if nodes, found := nodesByHyperNode[hyperNode]; found && nodes.Has(task.NodeName) {
				taskHyperNodes.Insert(hyperNode)
			}
		}
		if taskHyperNodes.Len() == 0 {
			return ""
		}
		candidateHyperNodes = taskHyperNodes
	}
	return getLowestTierHyperNode(candidateHyperNodes, hyperNodes)
}

func getLowestTierHyperNode(hyperNodeNames sets.Set[string], hyperNodes HyperNodeInfoMap) string {
	if hyperNodeNames == nil || hyperNodeNames.Len() == 0 {
		return ""
	}

	var lowest *HyperNodeInfo
	for name := range hyperNodeNames {
		hyperNode, found := hyperNodes[name]
		if !found {
			continue
		}
		if lowest == nil || hyperNode.Tier() < lowest.Tier() {
			lowest = hyperNode
		}
	}
	if lowest == nil {
		return ""
	}
	return lowest.Name
}

// CollectJobOccupiedHyperNodesAtTier returns HyperNode names at tier where matching
// job tasks are placed. Each allocated task contributes its ancestor at tier; when a job
// spans multiple sibling domains (for example hn-A and hn-B), all occupied domains are
// returned instead of expanding an LCA to every sibling (which would incorrectly block hn-C).
func CollectJobOccupiedHyperNodesAtTier(
	job *JobInfo,
	hyperNodes HyperNodeInfoMap,
	tier int,
	nodesByHyperNode map[string]sets.Set[string],
) sets.Set[string] {
	occupied := sets.New[string]()
	if job == nil {
		return occupied
	}
	for _, task := range collectJobAllocatedTasks(job) {
		if hyperNode := taskOccupiedHyperNodeAtTier(task, hyperNodes, tier, nodesByHyperNode); hyperNode != "" {
			occupied.Insert(hyperNode)
		}
	}
	if occupied.Len() > 0 {
		return occupied
	}

	// Fallback: use job.AllocatedHyperNode when the task path yields nothing.
	//
	// Primary path is per-task placement (above). Steady-state Running jobs with a
	// complete node→HyperNode mapping should always hit that path.
	//
	// Fallback is for cases where placement is recorded on the job but tasks cannot
	// be mapped to HyperNodes at tier, for example:
	//   - unit tests that set AllocatedHyperNode without populating tasks;
	//   - scheduler restart or early session: annotation/cache has AllocatedHyperNode
	//     before allocated tasks are fully synced into JobInfo;
	//   - HyperNode RealNodesSet not ready or incomplete (nodeName present but no
	//     matching entry in nodesByHyperNode).
	//
	// Limitation: when AllocatedHyperNode is an LCA spanning sibling domains (e.g.
	// root while pods sit on hn-A and hn-B), ResolveHyperNodesAtTier expands to all
	// tier siblings and may over-block. That is why the task path is preferred; this
	// fallback is best-effort for single-domain placement or transient cache gaps.
	allocatedHyperNode := getJobAllocatedHyperNode(job, hyperNodes, nodesByHyperNode)
	for _, hyperNode := range hyperNodes.ResolveHyperNodesAtTier(allocatedHyperNode, tier) {
		occupied.Insert(hyperNode)
	}
	return occupied
}

// CollectSubJobOccupiedHyperNodesAtTier returns HyperNode names at tier where a SubJob is already placed.
func CollectSubJobOccupiedHyperNodesAtTier(
	subJob *SubJobInfo,
	hyperNodes HyperNodeInfoMap,
	tier int,
	nodesByHyperNode map[string]sets.Set[string],
) sets.Set[string] {
	occupied := sets.New[string]()
	if subJob == nil {
		return occupied
	}
	for _, task := range collectSubJobAllocatedTasks(subJob) {
		if hyperNode := taskOccupiedHyperNodeAtTier(task, hyperNodes, tier, nodesByHyperNode); hyperNode != "" {
			occupied.Insert(hyperNode)
		}
	}
	if occupied.Len() > 0 {
		return occupied
	}
	for _, hyperNode := range hyperNodes.ResolveHyperNodesAtTier(subJob.AllocatedHyperNode, tier) {
		occupied.Insert(hyperNode)
	}
	return occupied
}

func taskOccupiedHyperNodeAtTier(
	task *TaskInfo,
	hyperNodes HyperNodeInfoMap,
	tier int,
	nodesByHyperNode map[string]sets.Set[string],
) string {
	if task == nil || task.NodeName == "" || len(nodesByHyperNode) == 0 {
		return ""
	}

	finestName := ""
	finestTier := 0
	for name, nodes := range nodesByHyperNode {
		if !nodes.Has(task.NodeName) {
			continue
		}
		hyperNode, ok := hyperNodes[name]
		if !ok {
			continue
		}
		if finestName == "" || hyperNode.Tier() < finestTier {
			finestName = name
			finestTier = hyperNode.Tier()
		}
	}
	if finestName == "" {
		return ""
	}
	return hyperNodes.GetAncestorHyperNode(finestName, tier)
}

// MatchingPodGroupsAllocatedHyperNodesForTerm returns ancestor HyperNodes at the term tier
// where matching PodGroups (other than selfJob) are already allocated.
// nodesByHyperNode is used to infer placement for matching PodGroups without AllocatedHyperNode.
func MatchingPodGroupsAllocatedHyperNodesForTerm(
	jobs map[JobID]*JobInfo,
	hyperNodes HyperNodeInfoMap,
	tierNameMap HyperNodeTierNameMap,
	selfJob *JobInfo,
	term scheduling.PodGroupAffinityTerm,
	nodesByHyperNode map[string]sets.Set[string],
) (sets.Set[string], error) {
	tier, err := ResolvePodGroupTermTier(term, tierNameMap)
	if err != nil {
		return nil, err
	}

	matchingHyperNodes := sets.New[string]()
	for _, matchingJob := range jobs {
		if !PodGroupMatchesTerm(term, selfJob, matchingJob) {
			continue
		}
		occupiedHyperNodes := CollectJobOccupiedHyperNodesAtTier(matchingJob, hyperNodes, tier, nodesByHyperNode)
		if occupiedHyperNodes.Len() == 0 {
			// matching job not yet placed, it occupies no domain, skip it.
			continue
		}
		resolvedHyperNodes := occupiedHyperNodes.UnsortedList()
		sort.Strings(resolvedHyperNodes)
		allocatedHyperNode := getJobAllocatedHyperNode(matchingJob, hyperNodes, nodesByHyperNode)
		klog.V(3).Infof("podGroup anti-affinity: matching job hyperNode, job=%s, matchingJob=%s, termTier=%d, allocatedHyperNode=%s, resolvedHyperNodes=%s",
			klog.KRef(selfJob.Namespace, selfJob.Name),
			klog.KRef(matchingJob.Namespace, matchingJob.Name),
			tier, allocatedHyperNode, strings.Join(resolvedHyperNodes, ","))
		for _, hyperNode := range resolvedHyperNodes {
			matchingHyperNodes.Insert(hyperNode)
		}
	}
	return matchingHyperNodes, nil
}
