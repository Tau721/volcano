/*
Copyright 2025 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License and the specific language governing permissions and
limitations under the License.
*/

package grouptopologyaffinity

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	k8sFramework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/utils/set"

	scheduling "volcano.sh/apis/pkg/apis/scheduling"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
)

const (
	PluginName    = "group-topology-affinity"
	PluginWeight  = "weight"
	DefaultWeight = 1
	FullScore     = 1.0
	ZeroScore     = 0.0

	// noAffinityTermIndex marks reject reasons not tied to a specific affinity term.
	noAffinityTermIndex = -1
)

var emptyHyperNodeGradients = [][]*api.HyperNodeInfo{}

type groupTopologyAffinityPlugin struct {
	pluginArguments framework.Arguments
	weight          int
}

func New(arguments framework.Arguments) framework.Plugin {
	weight := DefaultWeight
	arguments.GetInt(&weight, PluginWeight)
	if weight < 0 {
		weight = DefaultWeight
	}
	return &groupTopologyAffinityPlugin{
		pluginArguments: arguments,
		weight:          weight,
	}
}

func (gta *groupTopologyAffinityPlugin) Name() string {
	return PluginName
}

func (gta *groupTopologyAffinityPlugin) OnSessionOpen(ssn *framework.Session) {
	ssn.AddHyperNodeGradientForJobFn(gta.Name(), func(job *api.JobInfo, hyperNode *api.HyperNodeInfo) api.HyperNodeGradientResult {
		return gta.hyperNodeConstraintForJob(ssn, job, hyperNode)
	})

	ssn.AddHyperNodeGradientForSubJobFn(gta.Name(), func(subJob *api.SubJobInfo, hyperNode *api.HyperNodeInfo) api.HyperNodeGradientResult {
		job, ok := ssn.Jobs[subJob.Job]
		if !ok {
			klog.Errorf("job %s for subJob %s not found", subJob.Job, subJob.UID)
			return api.HyperNodeGradientConstrain(emptyHyperNodeGradients)
		}
		return gta.hyperNodeConstraintForSubJob(ssn, job, subJob, hyperNode)
	})

	ssn.AddHyperNodeOrderFn(gta.Name(), func(subJob *api.SubJobInfo, hyperNodes map[string][]*api.NodeInfo) (map[string]float64, error) {
		job, ok := ssn.Jobs[subJob.Job]
		if !ok {
			return nil, nil
		}
		return gta.hyperNodeOrderFn(ssn, job, subJob, hyperNodes)
	})
}

func (gta *groupTopologyAffinityPlugin) OnSessionClose(ssn *framework.Session) {}

func (gta *groupTopologyAffinityPlugin) hyperNodeConstraintForJob(
	ssn *framework.Session,
	job *api.JobInfo,
	root *api.HyperNodeInfo,
) api.HyperNodeGradientResult {
	hardTerms := job.RequiredPodGroupAntiAffinityTerms()
	if len(hardTerms) == 0 {
		if _, found := preferredSubGroupJobContainerTier(ssn, job); !found {
			return api.HyperNodeGradientAbstain()
		}
		gradients, err := gta.buildFullHyperNodeGradient(
			ssn, root, maxHyperNodeTier(ssn.HyperNodesSetByTier), job.AllocatedHyperNode,
		)
		if err != nil {
			klog.Errorf("build preferred subgroup Job gradient failed, job=%s, err=%v", job.UID, err)
			return api.HyperNodeGradientAbstain()
		}
		orderedGradients, ordered := prioritizePreferredSubGroupJobGradients(ssn, job, gradients)
		if !ordered {
			return api.HyperNodeGradientAbstain()
		}
		return api.HyperNodeGradientPrefer(orderedGradients)
	}
	result, err := gta.buildPodGroupAntiAffinityGradient(
		ssn, job, root, hardTerms, maxHyperNodeTier(ssn.HyperNodesSetByTier), job.AllocatedHyperNode,
	)
	if err != nil {
		klog.Errorf("build podGroup anti-affinity gradient failed, job=%s, err=%v", job.UID, err)
		return api.HyperNodeGradientConstrain(emptyHyperNodeGradients)
	}
	if orderedGradients, ordered := prioritizePreferredSubGroupJobGradients(ssn, job, result); ordered {
		return api.HyperNodeGradientConstrainAndPrefer(orderedGradients)
	}
	return api.HyperNodeGradientConstrain(result)
}

func (gta *groupTopologyAffinityPlugin) hyperNodeConstraintForSubJob(
	ssn *framework.Session,
	job *api.JobInfo,
	subJob *api.SubJobInfo,
	root *api.HyperNodeInfo,
) api.HyperNodeGradientResult {
	hasPodGroupHardTerms := len(job.RequiredPodGroupAntiAffinityTerms()) > 0
	hasSubGroupHardTerms := gta.subJobHasHardTerms(job, subJob)
	if !hasPodGroupHardTerms && !hasSubGroupHardTerms {
		return api.HyperNodeGradientAbstain()
	}

	maxTier := maxHyperNodeTier(ssn.HyperNodesSetByTier)
	var (
		gradients [][]*api.HyperNodeInfo
		err       error
	)
	if hasPodGroupHardTerms {
		gradients, err = gta.buildPodGroupAntiAffinityGradient(
			ssn, job, root, job.RequiredPodGroupAntiAffinityTerms(), maxTier, subJob.AllocatedHyperNode,
		)
	} else {
		gradients, err = gta.buildFullHyperNodeGradient(ssn, root, maxTier, subJob.AllocatedHyperNode)
	}
	if err != nil {
		klog.Errorf("build hard group topology gradient failed, job=%s, subJob=%s, err=%v", job.UID, subJob.UID, err)
		return api.HyperNodeGradientConstrain(emptyHyperNodeGradients)
	}
	if hasSubGroupHardTerms {
		gradients = gta.filterSubGroupHardTerms(ssn, job, subJob, gradients)
	}
	return api.HyperNodeGradientConstrain(gradients)
}

func (gta *groupTopologyAffinityPlugin) subJobHasHardTerms(job *api.JobInfo, subJob *api.SubJobInfo) bool {
	policyName := api.SubJobPolicyName(subJob)
	if policyName == "" {
		return false
	}
	for _, term := range job.RequiredSubGroupAffinityTerms() {
		if subGroupTermIncludes(term, policyName) {
			return true
		}
	}
	for _, term := range job.RequiredSubGroupAntiAffinityTerms() {
		if subGroupTermIncludes(term, policyName) {
			return true
		}
	}
	return false
}

// hyperNodeGradientForJob returns HyperNode candidates for podGroupAntiAffinity.
// Hard required terms filter candidates; jobs without hard rules return the full subtree
// so framework intersection and HyperNodeOrderFn can evaluate preferred terms.
func (gta *groupTopologyAffinityPlugin) hyperNodeGradientForJob(
	ssn *framework.Session,
	job *api.JobInfo,
	root *api.HyperNodeInfo,
) [][]*api.HyperNodeInfo {
	gradients := gta.hyperNodeGradient(ssn, job, root, job.AllocatedHyperNode)
	orderedGradients, _ := prioritizePreferredSubGroupJobGradients(ssn, job, gradients)
	return orderedGradients
}

// prioritizePreferredSubGroupJobGradients keeps the complete candidate set but
// tries a Job container that exposes the topology domains needed by preferred
// SubGroup terms before finer, single-domain candidates. The existing SubJob
// order callback can then compare sibling domains after the first peer is
// placed. Coarser and finer candidates remain available as soft fallbacks.
func prioritizePreferredSubGroupJobGradients(
	ssn *framework.Session,
	job *api.JobInfo,
	gradients [][]*api.HyperNodeInfo,
) ([][]*api.HyperNodeInfo, bool) {
	preferredTier, found := preferredSubGroupJobContainerTier(ssn, job)
	if !found || len(gradients) == 0 {
		return gradients, false
	}

	hyperNodesByTier := make(map[int][]*api.HyperNodeInfo)
	for _, gradient := range gradients {
		for _, hyperNode := range gradient {
			hyperNodesByTier[hyperNode.Tier()] = append(hyperNodesByTier[hyperNode.Tier()], hyperNode)
		}
	}

	if len(hyperNodesByTier[preferredTier]) == 0 {
		// If another hard constraint removed the preferred container tier, keep
		// the original ordering. Preferred topology must not become a filter.
		return gradients, false
	}

	coarserTiers := make([]int, 0, len(hyperNodesByTier))
	finerTiers := make([]int, 0, len(hyperNodesByTier))
	for tier := range hyperNodesByTier {
		switch {
		case tier > preferredTier:
			coarserTiers = append(coarserTiers, tier)
		case tier < preferredTier:
			finerTiers = append(finerTiers, tier)
		}
	}
	sort.Ints(coarserTiers)
	sort.Ints(finerTiers)

	tierOrder := make([]int, 0, len(hyperNodesByTier))
	tierOrder = append(tierOrder, preferredTier)
	tierOrder = append(tierOrder, coarserTiers...)
	tierOrder = append(tierOrder, finerTiers...)

	result := make([][]*api.HyperNodeInfo, 0, len(tierOrder))
	for _, tier := range tierOrder {
		hyperNodes := hyperNodesByTier[tier]
		sort.SliceStable(hyperNodes, func(i, j int) bool {
			return hyperNodes[i].Name < hyperNodes[j].Name
		})
		result = append(result, hyperNodes)
	}

	klog.V(3).Infof("subGroup topology affinity: prioritize Job gradient, job=%s, preferredContainerTier=%d, tierOrder=%v",
		klog.KRef(job.Namespace, job.Name), preferredTier, tierOrder)
	return result, true
}

func preferredSubGroupJobContainerTier(ssn *framework.Session, job *api.JobInfo) (int, bool) {
	if job == nil || len(job.SubJobs) < 2 {
		return 0, false
	}

	preferredTier := 0
	found := false
	for _, term := range job.PreferredSubGroupAffinityTerms() {
		if term.Weight < 1 || term.Weight > 100 || !hasSubJobPeerPairForTerm(job, term, false) {
			continue
		}
		tier, err := api.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			klog.V(3).Infof("subGroup affinity: resolve preferred Job container tier failed, job=%s, err=%v",
				klog.KRef(job.Namespace, job.Name), err)
			continue
		}
		if !found || tier > preferredTier {
			preferredTier = tier
			found = true
		}
	}

	availableTiers := make([]int, 0, len(ssn.HyperNodesSetByTier))
	for tier := range ssn.HyperNodesSetByTier {
		availableTiers = append(availableTiers, tier)
	}
	sort.Ints(availableTiers)
	for _, term := range job.PreferredSubGroupAntiAffinityTerms() {
		if term.Weight < 1 || term.Weight > 100 || !hasSubJobPeerPairForTerm(job, term, true) {
			continue
		}
		tier, err := api.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			klog.V(3).Infof("subGroup anti-affinity: resolve preferred Job container tier failed, job=%s, err=%v",
				klog.KRef(job.Namespace, job.Name), err)
			continue
		}
		parentTier, ok := nextHigherTier(availableTiers, tier)
		if !ok {
			continue
		}
		if !found || parentTier > preferredTier {
			preferredTier = parentTier
			found = true
		}
	}
	return preferredTier, found
}

func hasSubJobPeerPairForTerm(job *api.JobInfo, term scheduling.SubGroupAffinityTerm, antiAffinity bool) bool {
	subJobs := make([]*api.SubJobInfo, 0, len(job.SubJobs))
	for _, subJob := range job.SubJobs {
		if subJob != nil {
			subJobs = append(subJobs, subJob)
		}
	}
	for i := 0; i < len(subJobs); i++ {
		selfPolicy := api.SubJobPolicyName(subJobs[i])
		for j := i + 1; j < len(subJobs); j++ {
			peerPolicy := api.SubJobPolicyName(subJobs[j])
			if subGroupPeerMatchesTerm(selfPolicy, peerPolicy, term, antiAffinity) {
				return true
			}
		}
	}
	return false
}

func nextHigherTier(sortedTiers []int, tier int) (int, bool) {
	for _, candidate := range sortedTiers {
		if candidate > tier {
			return candidate, true
		}
	}
	return 0, false
}

func (gta *groupTopologyAffinityPlugin) hyperNodeGradientForSubJob(
	ssn *framework.Session,
	job *api.JobInfo,
	subJob *api.SubJobInfo,
	root *api.HyperNodeInfo,
) [][]*api.HyperNodeInfo {
	gradients := gta.hyperNodeGradient(ssn, job, root, subJob.AllocatedHyperNode)
	if !job.ContainsHardSubGroupTopologyAffinity() || len(gradients) == 0 {
		return gradients
	}
	return gta.filterSubGroupHardTerms(ssn, job, subJob, gradients)
}

func (gta *groupTopologyAffinityPlugin) hyperNodeGradient(
	ssn *framework.Session,
	job *api.JobInfo,
	root *api.HyperNodeInfo,
	allocatedHyperNode string,
) [][]*api.HyperNodeInfo {
	maxTier := maxHyperNodeTier(ssn.HyperNodesSetByTier)
	hardTerms := job.RequiredPodGroupAntiAffinityTerms()
	if len(hardTerms) > 0 {
		klog.V(3).Infof("podGroup anti-affinity: evaluate gradient, job=%s, rootHyperNode=%s, allocatedHyperNode=%s",
			klog.KRef(job.Namespace, job.Name), root.Name, allocatedHyperNode)
		result, err := gta.buildPodGroupAntiAffinityGradient(
			ssn, job, root, hardTerms, maxTier, allocatedHyperNode,
		)
		if err != nil {
			klog.Errorf("build podGroup anti-affinity gradient failed, job=%s, err=%v", job.UID, err)
			return emptyHyperNodeGradients
		}
		return result
	}

	klog.V(3).Infof("podGroup anti-affinity: gradient full-subtree, job=%s, rootHyperNode=%s, allocatedHyperNode=%s",
		klog.KRef(job.Namespace, job.Name), root.Name, allocatedHyperNode)
	result, err := gta.buildFullHyperNodeGradient(ssn, root, maxTier, allocatedHyperNode)
	if err != nil {
		klog.Errorf("build podGroup anti-affinity full gradient failed, job=%s, err=%v", job.UID, err)
		return emptyHyperNodeGradients
	}
	return result
}

// buildFullHyperNodeGradient returns every HyperNode under the search root up to highestAllowedTier.
// Used when hard podGroupAntiAffinity does not filter candidates; preferred terms are scored in HyperNodeOrderFn.
func (gta *groupTopologyAffinityPlugin) buildFullHyperNodeGradient(
	ssn *framework.Session,
	root *api.HyperNodeInfo,
	highestAllowedTier int,
	allocatedHyperNode string,
) ([][]*api.HyperNodeInfo, error) {
	searchRoot, err := getSearchRootForGradient(
		ssn.HyperNodes, root, highestAllowedTier, allocatedHyperNode,
	)
	if err != nil {
		return nil, err
	}
	eligibleHyperNodes := gta.bfsEligibleHyperNodesUnderRoot(ssn, searchRoot, highestAllowedTier)
	return groupHyperNodesByTierAsc(eligibleHyperNodes), nil
}

func (gta *groupTopologyAffinityPlugin) bfsEligibleHyperNodesUnderRoot(
	ssn *framework.Session,
	searchRoot *api.HyperNodeInfo,
	highestAllowedTier int,
) map[int][]*api.HyperNodeInfo {
	enqueued := set.New[string]()
	processQueue := []*api.HyperNodeInfo{searchRoot}
	enqueued.Insert(searchRoot.Name)

	eligibleByTier := make(map[int][]*api.HyperNodeInfo)
	for len(processQueue) > 0 {
		current := processQueue[0]
		processQueue = processQueue[1:]

		if current.Tier() <= highestAllowedTier {
			eligibleByTier[current.Tier()] = append(eligibleByTier[current.Tier()], current)
		}

		for child := range current.Children {
			if enqueued.Has(child) {
				continue
			}
			childHN, ok := ssn.HyperNodes[child]
			if !ok {
				continue
			}
			processQueue = append(processQueue, childHN)
			enqueued.Insert(child)
		}
	}
	return eligibleByTier
}

// buildPodGroupAntiAffinityGradient builds topology-only HyperNode gradients for hard
// podGroupAntiAffinity: BFS from the search root, drop candidates whose ancestor HyperNode
// at any required term tier overlaps a matching PodGroup's allocation.
func (gta *groupTopologyAffinityPlugin) buildPodGroupAntiAffinityGradient(
	ssn *framework.Session,
	job *api.JobInfo,
	root *api.HyperNodeInfo,
	terms []scheduling.PodGroupAffinityTerm,
	highestAllowedTier int,
	allocatedHyperNode string,
) ([][]*api.HyperNodeInfo, error) {
	matchingHyperNodesByTerm, err := collectMatchingHyperNodesByTerm(ssn, job, terms)
	if err != nil {
		return nil, err
	}

	searchRoot, err := getSearchRootForGradient(
		ssn.HyperNodes, root, highestAllowedTier, allocatedHyperNode,
	)
	if err != nil {
		return nil, err
	}

	for index, term := range terms {
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			klog.V(3).Infof("podGroup anti-affinity: resolve term tier failed, job=%s, termIndex=%d, err=%v",
				klog.KRef(job.Namespace, job.Name), index, err)
			continue
		}
		occupiedHyperNodes := matchingHyperNodesByTerm[index].UnsortedList()
		sort.Strings(occupiedHyperNodes)
		klog.V(3).Infof("podGroup anti-affinity: matching occupancy, job=%s, termIndex=%d, tier=%d, occupiedHyperNodes=%s, matchingPodGroups=%s",
			klog.KRef(job.Namespace, job.Name), index, tier,
			strings.Join(occupiedHyperNodes, ","),
			strings.Join(matchingPodGroupPlacementsForTerm(ssn, job, term), "; "))
	}

	eligibleHyperNodes := gta.bfsAntiAffinityEligibleHyperNodes(
		ssn, job, searchRoot, terms, matchingHyperNodesByTerm, highestAllowedTier,
	)
	klog.V(3).Infof("podGroup anti-affinity: gradient result, job=%s, searchRoot=%s, eligibleHyperNodes=%s",
		klog.KRef(job.Namespace, job.Name), searchRoot.Name, hyperNodeNamesByTier(eligibleHyperNodes))
	return groupHyperNodesByTierAsc(eligibleHyperNodes), nil
}

// collectMatchingHyperNodesByTerm resolves, for each required term, the ancestor HyperNodes
// at the term tier where matching PodGroups are already allocated.
func collectMatchingHyperNodesByTerm(
	ssn *framework.Session,
	job *api.JobInfo,
	terms []scheduling.PodGroupAffinityTerm,
) ([]sets.Set[string], error) {
	matchingHyperNodesByTerm := make([]sets.Set[string], len(terms))
	for index, term := range terms {
		matchingHyperNodes, err := api.MatchingPodGroupsAllocatedHyperNodesForTerm(
			ssn.Jobs, ssn.HyperNodes, ssn.HyperNodeTierNameMap, job, term, ssn.RealNodesSet,
		)
		if err != nil {
			return nil, fmt.Errorf("term %d: %w", index, err)
		}
		matchingHyperNodesByTerm[index] = matchingHyperNodes
	}
	return matchingHyperNodesByTerm, nil
}

func (gta *groupTopologyAffinityPlugin) bfsAntiAffinityEligibleHyperNodes(
	ssn *framework.Session,
	job *api.JobInfo,
	searchRoot *api.HyperNodeInfo,
	terms []scheduling.PodGroupAffinityTerm,
	matchingHyperNodesByTerm []sets.Set[string],
	highestAllowedTier int,
) map[int][]*api.HyperNodeInfo {
	enqueued := set.New[string]()
	processQueue := []*api.HyperNodeInfo{searchRoot}
	enqueued.Insert(searchRoot.Name)

	eligibleByTier := make(map[int][]*api.HyperNodeInfo)
	for len(processQueue) > 0 {
		current := processQueue[0]
		processQueue = processQueue[1:]

		if gta.isEligibleForPodGroupAntiAffinity(
			ssn, job, current, terms, matchingHyperNodesByTerm, highestAllowedTier,
		) {
			eligibleByTier[current.Tier()] = append(eligibleByTier[current.Tier()], current)
		}

		for child := range current.Children {
			if enqueued.Has(child) {
				continue
			}
			processQueue = append(processQueue, ssn.HyperNodes[child])
			enqueued.Insert(child)
		}
	}
	return eligibleByTier
}

// groupHyperNodesByTierAsc groups HyperNodes by tier and returns tiers in ascending order.
func groupHyperNodesByTierAsc(eligibleHyperNodes map[int][]*api.HyperNodeInfo) [][]*api.HyperNodeInfo {
	var tiers []int
	for tier := range eligibleHyperNodes {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)

	result := make([][]*api.HyperNodeInfo, 0, len(tiers))
	for _, tier := range tiers {
		result = append(result, eligibleHyperNodes[tier])
	}
	return result
}

// isEligibleForPodGroupAntiAffinity checks whether hn may host the job without violating
// hard podGroupAntiAffinity at each required term tier.
func (gta *groupTopologyAffinityPlugin) isEligibleForPodGroupAntiAffinity(
	ssn *framework.Session,
	job *api.JobInfo,
	hn *api.HyperNodeInfo,
	terms []scheduling.PodGroupAffinityTerm,
	matchingHyperNodesByTerm []sets.Set[string],
	highestAllowedTier int,
) bool {
	if hn.Tier() > highestAllowedTier {
		klog.V(3).Infof("podGroup anti-affinity: reject hyperNode, job=%s, hyperNode=%s, reason=tierAboveHighestAllowed, termIndex=%d, tier=%d, conflictHyperNode=",
			klog.KRef(job.Namespace, job.Name), hn.Name, noAffinityTermIndex, hn.Tier())
		return false
	}

	for index, term := range terms {
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			klog.V(3).Infof("podGroup anti-affinity: reject hyperNode, job=%s, hyperNode=%s, reason=resolveTermTierFailed, termIndex=%d, tier=0, conflictHyperNode=",
				klog.KRef(job.Namespace, job.Name), hn.Name, index)
			return false
		}
		// Compare at the term tier: reject if this candidate shares an ancestor HyperNode
		// with any matching PodGroup that is already placed there.
		ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hn.Name, tier)
		if ancestorHyperNode == "" {
			klog.V(3).Infof("podGroup anti-affinity: reject hyperNode, job=%s, hyperNode=%s, reason=emptyAncestorHyperNode, termIndex=%d, tier=%d, conflictHyperNode=",
				klog.KRef(job.Namespace, job.Name), hn.Name, index, tier)
			return false
		}
		if matchingHyperNodesByTerm[index].Has(ancestorHyperNode) {
			klog.V(3).Infof("podGroup anti-affinity: reject hyperNode, job=%s, hyperNode=%s, reason=conflictWithMatchingPodGroup, termIndex=%d, tier=%d, conflictHyperNode=%s",
				klog.KRef(job.Namespace, job.Name), hn.Name, index, tier, ancestorHyperNode)
			return false
		}
	}
	return true
}

func (gta *groupTopologyAffinityPlugin) filterSubGroupHardTerms(
	ssn *framework.Session,
	job *api.JobInfo,
	subJob *api.SubJobInfo,
	gradients [][]*api.HyperNodeInfo,
) [][]*api.HyperNodeInfo {
	result := make([][]*api.HyperNodeInfo, 0, len(gradients))
	for _, tierGroup := range gradients {
		filtered := make([]*api.HyperNodeInfo, 0, len(tierGroup))
		for _, hn := range tierGroup {
			if gta.isEligibleForSubGroupHardTerms(ssn, job, subJob, hn) {
				filtered = append(filtered, hn)
			}
		}
		if len(filtered) > 0 {
			result = append(result, filtered)
		}
	}
	return result
}

func (gta *groupTopologyAffinityPlugin) isEligibleForSubGroupHardTerms(
	ssn *framework.Session,
	job *api.JobInfo,
	subJob *api.SubJobInfo,
	hn *api.HyperNodeInfo,
) bool {
	for termIndex, term := range job.RequiredSubGroupAffinityTerms() {
		if !subGroupTermIncludes(term, api.SubJobPolicyName(subJob)) {
			continue
		}
		tier, err := api.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			klog.V(3).Infof("subGroup affinity: reject hyperNode, job=%s, subJob=%s, hyperNode=%s, reason=resolveTermTierFailed, termIndex=%d",
				klog.KRef(job.Namespace, job.Name), subJob.UID, hn.Name, termIndex)
			return false
		}
		ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hn.Name, tier)
		if ancestorHyperNode == "" {
			klog.V(3).Infof("subGroup affinity: reject hyperNode, job=%s, subJob=%s, hyperNode=%s, reason=emptyAncestorHyperNode, termIndex=%d, tier=%d",
				klog.KRef(job.Namespace, job.Name), subJob.UID, hn.Name, termIndex, tier)
			return false
		}
		peerHyperNodes := peerSubJobOccupiedHyperNodesAtTier(job, subJob, term, ssn.HyperNodes, tier, ssn.RealNodesSet, false)
		if peerHyperNodes.Len() == 0 {
			continue
		}
		if peerHyperNodes.Len() != 1 || !peerHyperNodes.Has(ancestorHyperNode) {
			klog.V(3).Infof("subGroup affinity: reject hyperNode, job=%s, subJob=%s, hyperNode=%s, reason=notWithPeerSubGroups, termIndex=%d, tier=%d, candidateHyperNode=%s, peerHyperNodes=%s",
				klog.KRef(job.Namespace, job.Name), subJob.UID, hn.Name, termIndex, tier, ancestorHyperNode, strings.Join(sortedSet(peerHyperNodes), ","))
			return false
		}
	}

	for termIndex, term := range job.RequiredSubGroupAntiAffinityTerms() {
		if !subGroupTermIncludes(term, api.SubJobPolicyName(subJob)) {
			continue
		}
		tier, err := api.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			klog.V(3).Infof("subGroup anti-affinity: reject hyperNode, job=%s, subJob=%s, hyperNode=%s, reason=resolveTermTierFailed, termIndex=%d",
				klog.KRef(job.Namespace, job.Name), subJob.UID, hn.Name, termIndex)
			return false
		}
		ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hn.Name, tier)
		if ancestorHyperNode == "" {
			klog.V(3).Infof("subGroup anti-affinity: reject hyperNode, job=%s, subJob=%s, hyperNode=%s, reason=emptyAncestorHyperNode, termIndex=%d, tier=%d",
				klog.KRef(job.Namespace, job.Name), subJob.UID, hn.Name, termIndex, tier)
			return false
		}
		peerHyperNodes := peerSubJobOccupiedHyperNodesAtTier(job, subJob, term, ssn.HyperNodes, tier, ssn.RealNodesSet, true)
		if peerHyperNodes.Has(ancestorHyperNode) {
			klog.V(3).Infof("subGroup anti-affinity: reject hyperNode, job=%s, subJob=%s, hyperNode=%s, reason=conflictWithPeerSubGroup, termIndex=%d, tier=%d, conflictHyperNode=%s",
				klog.KRef(job.Namespace, job.Name), subJob.UID, hn.Name, termIndex, tier, ancestorHyperNode)
			return false
		}
	}

	return true
}

func (gta *groupTopologyAffinityPlugin) hyperNodeOrderFn(
	ssn *framework.Session,
	job *api.JobInfo,
	subJob *api.SubJobInfo,
	hyperNodes map[string][]*api.NodeInfo,
) (map[string]float64, error) {
	podGroupAntiTerms := job.PreferredPodGroupAntiAffinityTerms()
	subGroupAffinityTerms := job.PreferredSubGroupAffinityTerms()
	subGroupAntiTerms := job.PreferredSubGroupAntiAffinityTerms()
	if len(podGroupAntiTerms) == 0 && len(subGroupAffinityTerms) == 0 && len(subGroupAntiTerms) == 0 {
		return nil, nil
	}

	hyperNodeCandidates := make([]string, 0, len(hyperNodes))
	for hyperNode := range hyperNodes {
		hyperNodeCandidates = append(hyperNodeCandidates, hyperNode)
	}
	sort.Strings(hyperNodeCandidates)
	klog.V(3).Infof("podGroup anti-affinity: evaluate preferred, job=%s, hyperNodeCandidates=%s",
		klog.KRef(job.Namespace, job.Name), strings.Join(hyperNodeCandidates, ","))

	scores := make(map[string]float64, len(hyperNodes))
	for hyperNode := range hyperNodes {
		scores[hyperNode] = FullScore
	}

	if err := gta.scorePreferredPodGroupAntiAffinityTerms(ssn, job, hyperNodes, scores, podGroupAntiTerms); err != nil {
		return nil, err
	}
	if err := gta.scorePreferredSubGroupAffinityTerms(ssn, job, subJob, hyperNodes, scores, subGroupAffinityTerms); err != nil {
		return nil, err
	}
	if err := gta.scorePreferredSubGroupAntiAffinityTerms(ssn, job, subJob, hyperNodes, scores, subGroupAntiTerms); err != nil {
		return nil, err
	}

	for hyperNode, score := range scores {
		scores[hyperNode] = float64(gta.weight) * score * float64(k8sFramework.MaxNodeScore)
	}
	if len(scores) > 0 {
		scoredHyperNodes := make([]string, 0, len(scores))
		for hyperNode := range scores {
			scoredHyperNodes = append(scoredHyperNodes, hyperNode)
		}
		sort.Strings(scoredHyperNodes)

		details := make([]string, 0, len(scoredHyperNodes))
		for _, hyperNode := range scoredHyperNodes {
			details = append(details, fmt.Sprintf("%s:%.2f", hyperNode, scores[hyperNode]))
		}
		klog.V(3).Infof("podGroup anti-affinity: preferred final scores, job=%s, pluginWeight=%d, scores=%s",
			klog.KRef(job.Namespace, job.Name), gta.weight, strings.Join(details, ","))
	}
	return scores, nil
}

func (gta *groupTopologyAffinityPlugin) scorePreferredPodGroupAntiAffinityTerms(
	ssn *framework.Session,
	job *api.JobInfo,
	hyperNodes map[string][]*api.NodeInfo,
	scores map[string]float64,
	terms []scheduling.PodGroupAffinityTerm,
) error {
	matchingHyperNodesByTerm := make([]sets.Set[string], len(terms))
	for termIndex, term := range terms {
		matchingHyperNodes, err := api.MatchingPodGroupsAllocatedHyperNodesForTerm(
			ssn.Jobs, ssn.HyperNodes, ssn.HyperNodeTierNameMap, job, term, ssn.RealNodesSet,
		)
		if err != nil {
			return err
		}
		matchingHyperNodesByTerm[termIndex] = matchingHyperNodes
	}
	for index, term := range terms {
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			klog.V(3).Infof("podGroup anti-affinity: resolve term tier failed, job=%s, termIndex=%d, err=%v",
				klog.KRef(job.Namespace, job.Name), index, err)
			continue
		}
		occupiedHyperNodes := matchingHyperNodesByTerm[index].UnsortedList()
		sort.Strings(occupiedHyperNodes)
		klog.V(3).Infof("podGroup anti-affinity: matching occupancy, job=%s, termIndex=%d, tier=%d, occupiedHyperNodes=%s, matchingPodGroups=%s",
			klog.KRef(job.Namespace, job.Name), index, tier,
			strings.Join(occupiedHyperNodes, ","),
			strings.Join(matchingPodGroupPlacementsForTerm(ssn, job, term), "; "))
	}

	for termIndex, term := range terms {
		matchingHyperNodes := matchingHyperNodesByTerm[termIndex]
		tier, err := api.ResolvePodGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return err
		}

		if term.Weight < 1 || term.Weight > 100 {
			continue
		}
		weightFactor := float64(term.Weight) / 100.0
		for hyperNode := range hyperNodes {
			ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hyperNode, tier)
			if ancestorHyperNode != "" && matchingHyperNodes.Has(ancestorHyperNode) {
				scoreBefore := scores[hyperNode]
				klog.V(3).Infof("podGroup anti-affinity: preferred penalty, job=%s, hyperNode=%s, termIndex=%d, tier=%d, conflictHyperNode=%s",
					klog.KRef(job.Namespace, job.Name), hyperNode, termIndex, tier, ancestorHyperNode)
				scores[hyperNode] -= weightFactor * FullScore
				if scores[hyperNode] < ZeroScore {
					scores[hyperNode] = ZeroScore
				}
				klog.V(4).Infof("podGroup anti-affinity: preferred score detail, job=%s, hyperNode=%s, termIndex=%d, weight=%d, weightFactor=%.2f, scoreBefore=%.2f, scoreAfter=%.2f, penalty=%.2f",
					klog.KRef(job.Namespace, job.Name), hyperNode, termIndex, term.Weight, weightFactor,
					scoreBefore, scores[hyperNode], scoreBefore-scores[hyperNode])
			}
		}
	}
	return nil
}

func (gta *groupTopologyAffinityPlugin) scorePreferredSubGroupAffinityTerms(
	ssn *framework.Session,
	job *api.JobInfo,
	subJob *api.SubJobInfo,
	hyperNodes map[string][]*api.NodeInfo,
	scores map[string]float64,
	terms []scheduling.SubGroupAffinityTerm,
) error {
	for termIndex, term := range terms {
		if !subGroupTermIncludes(term, api.SubJobPolicyName(subJob)) || term.Weight < 1 || term.Weight > 100 {
			continue
		}
		tier, err := api.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return err
		}
		peerHyperNodes := peerSubJobOccupiedHyperNodesAtTier(job, subJob, term, ssn.HyperNodes, tier, ssn.RealNodesSet, false)
		if peerHyperNodes.Len() == 0 {
			continue
		}
		weightFactor := float64(term.Weight) / 100.0
		for hyperNode := range hyperNodes {
			ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hyperNode, tier)
			if ancestorHyperNode == "" {
				continue
			}
			if peerHyperNodes.Len() == 1 && peerHyperNodes.Has(ancestorHyperNode) {
				continue
			}
			scoreBefore := scores[hyperNode]
			scores[hyperNode] -= weightFactor * FullScore
			if scores[hyperNode] < ZeroScore {
				scores[hyperNode] = ZeroScore
			}
			klog.V(4).Infof("subGroup affinity: preferred score detail, job=%s, subJob=%s, hyperNode=%s, termIndex=%d, weight=%d, scoreBefore=%.2f, scoreAfter=%.2f",
				klog.KRef(job.Namespace, job.Name), subJob.UID, hyperNode, termIndex, term.Weight, scoreBefore, scores[hyperNode])
		}
	}
	return nil
}

func (gta *groupTopologyAffinityPlugin) scorePreferredSubGroupAntiAffinityTerms(
	ssn *framework.Session,
	job *api.JobInfo,
	subJob *api.SubJobInfo,
	hyperNodes map[string][]*api.NodeInfo,
	scores map[string]float64,
	terms []scheduling.SubGroupAffinityTerm,
) error {
	for termIndex, term := range terms {
		if !subGroupTermIncludes(term, api.SubJobPolicyName(subJob)) || term.Weight < 1 || term.Weight > 100 {
			continue
		}
		tier, err := api.ResolveSubGroupTermTier(term, ssn.HyperNodeTierNameMap)
		if err != nil {
			return err
		}
		peerHyperNodes := peerSubJobOccupiedHyperNodesAtTier(job, subJob, term, ssn.HyperNodes, tier, ssn.RealNodesSet, true)
		if peerHyperNodes.Len() == 0 {
			continue
		}
		weightFactor := float64(term.Weight) / 100.0
		for hyperNode := range hyperNodes {
			ancestorHyperNode := ssn.HyperNodes.GetAncestorHyperNode(hyperNode, tier)
			if ancestorHyperNode == "" || !peerHyperNodes.Has(ancestorHyperNode) {
				continue
			}
			scoreBefore := scores[hyperNode]
			scores[hyperNode] -= weightFactor * FullScore
			if scores[hyperNode] < ZeroScore {
				scores[hyperNode] = ZeroScore
			}
			klog.V(4).Infof("subGroup anti-affinity: preferred score detail, job=%s, subJob=%s, hyperNode=%s, termIndex=%d, weight=%d, scoreBefore=%.2f, scoreAfter=%.2f",
				klog.KRef(job.Namespace, job.Name), subJob.UID, hyperNode, termIndex, term.Weight, scoreBefore, scores[hyperNode])
		}
	}
	return nil
}

func peerSubJobOccupiedHyperNodesAtTier(
	job *api.JobInfo,
	selfSubJob *api.SubJobInfo,
	term scheduling.SubGroupAffinityTerm,
	hyperNodes api.HyperNodeInfoMap,
	tier int,
	nodesByHyperNode map[string]sets.Set[string],
	antiAffinity bool,
) sets.Set[string] {
	occupied := sets.New[string]()
	selfPolicy := api.SubJobPolicyName(selfSubJob)
	for _, peerSubJob := range job.SubJobs {
		if peerSubJob == nil || selfSubJob == nil || peerSubJob.UID == selfSubJob.UID {
			continue
		}
		peerPolicy := api.SubJobPolicyName(peerSubJob)
		if !subGroupPeerMatchesTerm(selfPolicy, peerPolicy, term, antiAffinity) {
			continue
		}
		for hyperNode := range api.CollectSubJobOccupiedHyperNodesAtTier(peerSubJob, hyperNodes, tier, nodesByHyperNode) {
			occupied.Insert(hyperNode)
		}
	}
	return occupied
}

func subGroupPeerMatchesTerm(selfPolicy, peerPolicy string, term scheduling.SubGroupAffinityTerm, antiAffinity bool) bool {
	if selfPolicy == "" || peerPolicy == "" || !subGroupTermIncludes(term, selfPolicy) || !subGroupTermIncludes(term, peerPolicy) {
		return false
	}
	if !antiAffinity {
		return true
	}
	if len(term.SubGroups) == 1 {
		return peerPolicy == selfPolicy
	}
	return peerPolicy != selfPolicy
}

func subGroupTermIncludes(term scheduling.SubGroupAffinityTerm, policy string) bool {
	for _, subGroup := range term.SubGroups {
		if subGroup == policy {
			return true
		}
	}
	return false
}

func sortedSet(values sets.Set[string]) []string {
	result := values.UnsortedList()
	sort.Strings(result)
	return result
}

func maxHyperNodeTier(hyperNodesSetByTier map[int]sets.Set[string]) int {
	maxTier := 0
	for tier := range hyperNodesSetByTier {
		if tier > maxTier {
			maxTier = tier
		}
	}
	return maxTier
}

func getSearchRootForGradient(
	hyperNodes api.HyperNodeInfoMap,
	hyperNodeAvailable *api.HyperNodeInfo,
	highestAllowedTier int,
	allocatedHyperNode string,
) (*api.HyperNodeInfo, error) {
	if allocatedHyperNode == "" {
		return hyperNodeAvailable, nil
	}

	hyperNodeHighestAllowed, err := getHighestAllowedHyperNode(hyperNodes, highestAllowedTier, allocatedHyperNode)
	if err != nil {
		return nil, fmt.Errorf("get highest allowed hyperNode failed: %w", err)
	}

	lca := hyperNodes.GetLCAHyperNode(hyperNodeAvailable.Name, hyperNodeHighestAllowed)
	if lca == hyperNodeHighestAllowed {
		return hyperNodeAvailable, nil
	}
	if lca == hyperNodeAvailable.Name {
		hni, ok := hyperNodes[hyperNodeHighestAllowed]
		if !ok {
			return nil, fmt.Errorf("failed to get highest allowed HyperNode info for %s", hyperNodeHighestAllowed)
		}
		return hni, nil
	}

	return nil, fmt.Errorf("there is no intersection between hyperNodeAvailable %s and hyperNodeHighestAllowed %s",
		hyperNodeAvailable.Name, hyperNodeHighestAllowed)
}

func getHighestAllowedHyperNode(hyperNodes api.HyperNodeInfoMap, highestAllowedTier int, allocatedHyperNode string) (string, error) {
	var highestAllowedHyperNode string

	for _, ancestor := range hyperNodes.GetAncestors(allocatedHyperNode) {
		hni, ok := hyperNodes[ancestor]
		if !ok {
			return "", fmt.Errorf("allocated hyperNode %s ancestor %s not found", allocatedHyperNode, ancestor)
		}
		if hni.Tier() > highestAllowedTier {
			break
		}
		highestAllowedHyperNode = ancestor
	}

	if highestAllowedHyperNode == "" {
		return "", fmt.Errorf("allocated hyperNode %s tier is greater than highest allowed tier %d", allocatedHyperNode, highestAllowedTier)
	}

	return highestAllowedHyperNode, nil
}

func describeMatchingPodGroupPlacement(
	job *api.JobInfo,
	hyperNodes api.HyperNodeInfoMap,
	nodesByHyperNode map[string]sets.Set[string],
) string {
	if job == nil {
		return ""
	}
	allocatedHyperNode := job.AllocatedHyperNode
	if allocatedHyperNode == "" {
		allocatedHyperNode = api.ComputeJobAllocatedHyperNode(job, hyperNodes, nodesByHyperNode)
	}
	if allocatedHyperNode == "" {
		return ""
	}
	pgName := job.Name
	if job.PodGroup != nil && job.PodGroup.Name != "" {
		pgName = job.PodGroup.Name
	}
	return fmt.Sprintf("%s/%s(hyperNode=%s)", job.Namespace, pgName, allocatedHyperNode)
}

func matchingPodGroupPlacementsForTerm(
	ssn *framework.Session,
	selfJob *api.JobInfo,
	term scheduling.PodGroupAffinityTerm,
) []string {
	placements := make([]string, 0)
	for _, matchingJob := range ssn.Jobs {
		if !api.PodGroupMatchesTerm(term, selfJob, matchingJob) {
			continue
		}
		placement := describeMatchingPodGroupPlacement(matchingJob, ssn.HyperNodes, ssn.RealNodesSet)
		if placement == "" {
			continue
		}
		placements = append(placements, placement)
	}
	sort.Strings(placements)
	return placements
}

func hyperNodeNamesByTier(hyperNodesByTier map[int][]*api.HyperNodeInfo) string {
	if len(hyperNodesByTier) == 0 {
		return "{}"
	}
	tiers := make([]int, 0, len(hyperNodesByTier))
	for tier := range hyperNodesByTier {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)

	parts := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		names := make([]string, 0, len(hyperNodesByTier[tier]))
		for _, hn := range hyperNodesByTier[tier] {
			names = append(names, hn.Name)
		}
		sort.Strings(names)
		parts = append(parts, fmt.Sprintf("tier-%d:[%s]", tier, strings.Join(names, ",")))
	}
	return "{" + strings.Join(parts, " ") + "}"
}
