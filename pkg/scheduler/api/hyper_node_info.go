/*
Copyright 2025 The Volcano Authors.

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

package api

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"
)

// HyperNodesInfo stores and manages the hierarchical structure of HyperNodes.
type HyperNodesInfo struct {
	sync.Mutex

	// hyperNodes stores information about all HyperNodes.
	hyperNodes HyperNodeInfoMap
	// hyperNodesSetByTier stores HyperNode names grouped by their tier.
	hyperNodesSetByTier map[int]sets.Set[string]
	// realNodesSet stores the set of real nodes for each HyperNode, eg, s0 and s1 are members of s4,
	// s0->node0, node1, s1->node2, node3, s4->node0, node1, s1->node2, node3
	realNodesSet map[string]sets.Set[string]
	// nodeLister Lister to list Kubernetes nodes.
	nodeLister        listerv1.NodeLister
	builtErrHyperNode string

	// ready indicates whether the HyperNodesInfo is ready (build process is complete).
	ready *atomic.Bool
}

type HyperNodeInfoMap map[string]*HyperNodeInfo

type HyperNodeTierNameMap map[string]int

// NameForTier returns the tier name for a tier number, falling back to tier-N.
func (m HyperNodeTierNameMap) NameForTier(tier int, hyperNodes HyperNodeInfoMap) string {
	for name, mappedTier := range m {
		if mappedTier == tier {
			return name
		}
	}
	for _, hn := range hyperNodes {
		if hn.Tier() == tier && hn.TierName() != "" {
			return hn.TierName()
		}
	}
	return fmt.Sprintf("tier-%d", tier)
}

// FormatHyperNodeTierListing formats cluster HyperNode tier counts for logging.
// Tiers are listed from coarse to fine (higher tier number first).
func FormatHyperNodeTierListing(
	tiers []int,
	hyperNodesSetByTier map[int]sets.Set[string],
	tierNameMap HyperNodeTierNameMap,
	hyperNodes HyperNodeInfoMap,
) (total int, tierCount int, listing string) {
	if len(hyperNodesSetByTier) == 0 {
		return 0, 0, ""
	}

	sortedTiers := append([]int(nil), tiers...)
	sort.Sort(sort.Reverse(sort.IntSlice(sortedTiers)))

	parts := make([]string, 0, len(sortedTiers))
	for _, tier := range sortedTiers {
		namesSet := hyperNodesSetByTier[tier]
		if namesSet.Len() == 0 {
			continue
		}
		total += namesSet.Len()
		tierCount++
		tierName := tierNameMap.NameForTier(tier, hyperNodes)
		parts = append(parts, fmt.Sprintf("%s(tier=%d): %d", tierName, tier, namesSet.Len()))
	}
	return total, tierCount, strings.Join(parts, "; ")
}

// NewHyperNodesInfo initializes a new HyperNodesInfo instance.
func NewHyperNodesInfo(lister listerv1.NodeLister) *HyperNodesInfo {
	ready := new(atomic.Bool)
	ready.Store(true)
	return &HyperNodesInfo{
		hyperNodes:          make(map[string]*HyperNodeInfo),
		hyperNodesSetByTier: make(map[int]sets.Set[string]),
		realNodesSet:        make(map[string]sets.Set[string]),
		nodeLister:          lister,
		ready:               ready,
	}
}

// NewHyperNodesInfoWithCache initializes a new HyperNodesInfo instance with cache.
// This is just used for ut.
// TODO: abstract an interface to mock for ut.
func NewHyperNodesInfoWithCache(hyperNodesMap map[string]*HyperNodeInfo, hyperNodesSetByTier map[int]sets.Set[string], realNodesSet map[string]sets.Set[string], ready *atomic.Bool) *HyperNodesInfo {
	return &HyperNodesInfo{
		hyperNodes:          hyperNodesMap,
		hyperNodesSetByTier: hyperNodesSetByTier,
		realNodesSet:        realNodesSet,
		ready:               ready,
	}
}

// HyperNodeInfo stores information about a single HyperNode.
type HyperNodeInfo struct {
	Name      string
	HyperNode *topologyv1alpha1.HyperNode

	Parent   string
	Children sets.Set[string]

	tier       int
	tierName   string
	isDeleting bool
}

// HyperNodeInfoOption defines a function type for configuring HyperNodeInfo.
type HyperNodeInfoOption func(*HyperNodeInfo)

// TierOpt returns an option that sets the tier of the HyperNodeInfo.
func TierOpt(tier int) HyperNodeInfoOption {
	return func(hni *HyperNodeInfo) {
		hni.tier = tier
	}
}

// TierNameOpt returns an option that sets the tierName of the HyperNodeInfo.
func TierNameOpt(tierName string) HyperNodeInfoOption {
	return func(hni *HyperNodeInfo) {
		hni.tierName = tierName
	}
}

// ParentOpt returns an option that sets the parent of the HyperNodeInfo.
func ParentOpt(parent string) HyperNodeInfoOption {
	return func(hni *HyperNodeInfo) {
		hni.Parent = parent
	}
}

// IsDeletingOpt returns an option that sets the isDeleting flag of the HyperNodeInfo.
func IsDeletingOpt(isDeleting bool) HyperNodeInfoOption {
	return func(hni *HyperNodeInfo) {
		hni.isDeleting = isDeleting
	}
}

// NewHyperNodeInfo creates a new HyperNodeInfo instance.
func NewHyperNodeInfo(hn *topologyv1alpha1.HyperNode, opts ...HyperNodeInfoOption) *HyperNodeInfo {
	hni := &HyperNodeInfo{
		Name:      hn.Name,
		HyperNode: hn,
		tier:      hn.Spec.Tier,
		tierName:  hn.Spec.TierName,
		Children:  sets.New[string](),
	}

	// Apply all options
	for _, opt := range opts {
		opt(hni)
	}

	return hni
}

// String returns a string representation of the HyperNodeInfo.
func (hni *HyperNodeInfo) String() string {
	return strings.Join([]string{
		fmt.Sprintf("Name: %s", hni.Name),
		fmt.Sprintf(" Tier: %d", hni.tier),
		fmt.Sprintf(" TierName: %s", hni.tierName),
		fmt.Sprintf(" Parent: %s", hni.Parent)},
		",")
}

// Tier returns the tier of the hypernode
func (hni *HyperNodeInfo) Tier() int {
	return hni.tier
}

// TierName returns the tierName of the hypernode.
func (hni *HyperNodeInfo) TierName() string {
	return hni.tierName
}

func (hni *HyperNodeInfo) DeepCopy() *HyperNodeInfo {
	if hni == nil {
		return nil
	}

	copiedHyperNodeInfo := &HyperNodeInfo{
		Name:       hni.Name,
		tier:       hni.tier,
		tierName:   hni.tierName,
		HyperNode:  hni.HyperNode.DeepCopy(),
		isDeleting: hni.isDeleting,
		Parent:     hni.Parent,
		Children:   hni.Children.Clone(),
	}

	return copiedHyperNodeInfo
}

// HyperNodes returns a deep copy of the map that store hypernode info.
// This ensures that the returned map is independent of the original, preventing unintended modifications.
func (hni *HyperNodesInfo) HyperNodes() HyperNodeInfoMap {
	copiedHyperNodes := make(map[string]*HyperNodeInfo, len(hni.hyperNodes))
	for hn, info := range hni.hyperNodes {
		copiedHyperNodes[hn] = info.DeepCopy()
	}

	return copiedHyperNodes
}

// HyperNodeTierNameMap returns the mapping of tierName and tier.
func (hni *HyperNodesInfo) HyperNodeTierNameMap() HyperNodeTierNameMap {
	hyperNodeTierNameMap := make(map[string]int, len(hni.hyperNodes))
	for _, info := range hni.hyperNodes {
		if info.tierName != "" {
			if existingTier, ok := hyperNodeTierNameMap[info.tierName]; ok && existingTier != info.tier {
				klog.Warningf("Conflicting tiers for tierName %s: existing %d, new %d. Using %d.", info.tierName, existingTier, info.tier, info.tier)
			}
			hyperNodeTierNameMap[info.tierName] = info.tier
		}
	}

	return hyperNodeTierNameMap
}

// HyperNode returns a hyperNode by name.
func (hni *HyperNodesInfo) HyperNode(name string) *topologyv1alpha1.HyperNode {
	hn := hni.hyperNodes[name]
	if hn == nil {
		return nil
	}
	return hn.HyperNode
}

// HyperNodesSetByTier returns a deep copy of the map that groups HyperNode names by their tier.
// This ensures that the returned map is independent of the original, preventing unintended modifications.
func (hni *HyperNodesInfo) HyperNodesSetByTier() map[int]sets.Set[string] {
	copiedHyperNodesSetByTier := make(map[int]sets.Set[string], len(hni.hyperNodesSetByTier))
	for tier, row := range hni.hyperNodesSetByTier {
		copiedHyperNodesSetByTier[tier] = row.Clone()
	}

	return copiedHyperNodesSetByTier
}

// RealNodesSet returns a deep copy of the map that stores the sets of real nodes for each HyperNode.
// This ensures that the returned map is independent of the original, preventing unintended modifications.
func (hni *HyperNodesInfo) RealNodesSet() map[string]sets.Set[string] {
	copiedRealNodesSet := make(map[string]sets.Set[string], len(hni.realNodesSet))
	for name, value := range hni.realNodesSet {
		copiedRealNodesSet[name] = value.Clone() // Clone the set to ensure a deep copy.
	}

	return copiedRealNodesSet
}

// DeleteHyperNode deletes a HyperNode from the cache and update hyperNode tree.
func (hni *HyperNodesInfo) DeleteHyperNode(name string) error {
	hni.markHyperNodeIsDeleting(name)
	if err := hni.updateAncestors(name); err != nil {
		return err
	}
	hni.removeParent(name)

	// We can safely delete hyperNode after updated ancestors.
	hni.deleteHyperNode(name)
	return nil
}

func (hni *HyperNodesInfo) UpdateHyperNode(hn *topologyv1alpha1.HyperNode) error {
	hni.updateParent(hn)
	hni.updateHyperNodesSetByTier(hn)

	name := hn.Name
	old, exists := hni.hyperNodes[name]
	if exists {
		old.HyperNode = hn
		old.tier = hn.Spec.Tier
		old.tierName = hn.Spec.TierName
	} else {
		hni.hyperNodes[name] = NewHyperNodeInfo(hn)
	}

	return hni.updateAncestors(name)
}

func (hni *HyperNodesInfo) BuildHyperNodeCache(hn *HyperNodeInfo, processed sets.Set[string], ancestorsChain sets.Set[string], ancestors sets.Set[string], nodes []*corev1.Node) error {
	// Check for cyclic dependency.
	if ancestorsChain.Has(hn.Name) {
		return fmt.Errorf("cyclic dependency detected: HyperNode %s is already in the ancestor chain %v", hn.Name, sets.List(ancestorsChain))
	}
	// Pruning operation: skip if hyperNode already processed.
	if processed.Has(hn.Name) {
		return nil
	}
	// Only process current hyperNode's ancestor, other hyperNodes are no need to update.
	if !ancestors.Has(hn.Name) {
		return nil
	}

	ancestorsChain.Insert(hn.Name)
	defer ancestorsChain.Delete(hn.Name)

	if hni.hyperNodeIsDeleting(hn.Name) {
		klog.Infof("HyperNode is being deleted, name=%s", hn.Name)
		return nil
	}

	for _, member := range hn.HyperNode.Spec.Members {
		switch member.Type {
		case topologyv1alpha1.MemberTypeNode:
			if _, ok := hni.realNodesSet[hn.Name]; !ok {
				hni.realNodesSet[hn.Name] = sets.New[string]()
			}
			members := GetMembers(member.Selector, nodes)
			klog.V(5).Infof("Get members of hyperNode, name=%s, members=%v", hn.Name, members)
			hni.realNodesSet[hn.Name] = hni.realNodesSet[hn.Name].Union(members)

		case topologyv1alpha1.MemberTypeHyperNode:
			// HyperNode member does not support regex match.
			memberName := hni.exactMatchMember(member.Selector)
			if memberName == "" {
				continue
			}

			if err := hni.addChild(hn.Name, memberName); err != nil {
				return err
			}

			memberHn, ok := hni.hyperNodes[memberName]
			if !ok {
				klog.Infof("HyperNode not exists in cache, maybe not created or not be watched, name=%s, parent=%s", memberName, hn.Name)
				continue
			}

			// Recursively build cache for the member HyperNode.
			if err := hni.BuildHyperNodeCache(memberHn, processed, ancestorsChain, ancestors, nodes); err != nil {
				return err
			}

			if _, ok := hni.realNodesSet[hn.Name]; !ok {
				hni.realNodesSet[hn.Name] = sets.New[string]()
			}
			hni.realNodesSet[hn.Name] = hni.realNodesSet[hn.Name].Union(hni.realNodesSet[memberName])

		default:
			klog.Errorf("Unknown member type, type=%v", member.Type)
		}
	}

	processed.Insert(hn.Name)
	klog.V(3).Infof("Successfully built RealNodesSet with members, name=%s, nodeSets=%v", hn.Name, hni.realNodesSet[hn.Name])
	return nil
}

// Ready returns whether the HyperNodesInfo is built complete with no err
// and all members are cached and then scheduler can start schedule based on hyperNodes view.
func (hni *HyperNodesInfo) Ready() bool {
	return hni.ready.Load()
}

// setReady sets the ready flag.
func (hni *HyperNodesInfo) setReady(ready bool) {
	hni.ready.Store(ready)
}

// updateParent updates the parent-child relationships if members are removed.
func (hni *HyperNodesInfo) updateParent(hn *topologyv1alpha1.HyperNode) {
	oldMembers := hni.getChildren(hn.Name)
	newMembers := sets.New[string]()
	for _, member := range hn.Spec.Members {
		if member.Type == topologyv1alpha1.MemberTypeHyperNode && member.Selector.ExactMatch != nil {
			newMembers.Insert(member.Selector.ExactMatch.Name)
		}
	}

	removedMembers := oldMembers.Difference(newMembers)
	if removedMembers.Len() == 0 {
		return
	}
	for member := range removedMembers {
		hni.resetParent(member)
	}
}

// GetDescendants returns all descendants of a given HyperNode.
func (hni *HyperNodesInfo) GetDescendants(hyperNodeName string) sets.Set[string] {
	queue := []string{hyperNodeName}
	descendants := sets.New[string]()

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		descendants.Insert(current)
		if hn, ok := hni.hyperNodes[current]; ok {
			for _, member := range hn.HyperNode.Spec.Members {
				if member.Type == topologyv1alpha1.MemberTypeHyperNode && member.Selector.ExactMatch != nil {
					childName := member.Selector.ExactMatch.Name
					if !descendants.Has(childName) {
						queue = append(queue, childName)
					}
				}
			}
		}
	}
	return descendants
}

// getChildren returns the immediate children of a given HyperNode.
func (hni *HyperNodesInfo) getChildren(hyperNodeName string) sets.Set[string] {
	children := sets.New[string]()
	hn, ok := hni.hyperNodes[hyperNodeName]
	if !ok {
		return children
	}

	for _, member := range hn.HyperNode.Spec.Members {
		if member.Type == topologyv1alpha1.MemberTypeHyperNode && member.Selector.ExactMatch != nil {
			childName := member.Selector.ExactMatch.Name
			children.Insert(childName)
		}
	}
	return children
}

// GetLeafNodes returns the leaf hyperNodes set for a given hyperNode.
func (hni *HyperNodesInfo) GetLeafNodes(hyperNodeName string) sets.Set[string] {
	leafNodes := sets.New[string]()
	ancestorsChain := sets.New[string]()
	hni.findLeafNodesWithCycleCheck(hyperNodeName, leafNodes, ancestorsChain)
	return leafNodes
}

func (hni *HyperNodesInfo) findLeafNodesWithCycleCheck(hyperNodeName string, leafNodes sets.Set[string], ancestorsChain sets.Set[string]) {
	if ancestorsChain.Has(hyperNodeName) {
		klog.Errorf("Cycle detected in HyperNode hierarchy, hyperNode=%s", hyperNodeName)
		return
	}

	hn, ok := hni.hyperNodes[hyperNodeName]
	if !ok {
		return
	}

	ancestorsChain.Insert(hyperNodeName)
	defer ancestorsChain.Delete(hyperNodeName)

	isLeaf := true
	for _, member := range hn.HyperNode.Spec.Members {
		if member.Type == topologyv1alpha1.MemberTypeHyperNode {
			isLeaf = false
			hni.findLeafNodesWithCycleCheck(member.Selector.ExactMatch.Name, leafNodes, ancestorsChain)
		}
	}

	if isLeaf {
		leafNodes.Insert(hyperNodeName)
	}
}

// GetRegexOrLabelMatchLeafHyperNodes returns leaf hyperNodes whose member's selector is regex or label match.
func (hni *HyperNodesInfo) GetRegexOrLabelMatchLeafHyperNodes() sets.Set[string] {
	leaf := sets.New[string]()
	for name, hnInfo := range hni.hyperNodes {
		if hnInfo == nil || hnInfo.HyperNode == nil {
			continue
		}

		isLeaf := true
		hasMatch := false
		for _, member := range hnInfo.HyperNode.Spec.Members {
			if member.Type == topologyv1alpha1.MemberTypeHyperNode {
				isLeaf = false
				break
			}
			if member.Selector.RegexMatch != nil || member.Selector.LabelMatch != nil {
				hasMatch = true
			}
		}
		if isLeaf && hasMatch {
			leaf.Insert(name)
		}
	}
	return leaf
}

// addChild adds the HyperNode member to the parent and sets the parent of a HyperNode member.
func (hni *HyperNodesInfo) addChild(parent, member string) error {
	parentHn, ok := hni.hyperNodes[parent]
	if !ok {
		hni.builtErrHyperNode = parent
		return fmt.Errorf("parent HyperNode %s not exists in cache", parent)
	}

	childHn, ok := hni.hyperNodes[member]
	if !ok {
		klog.Infof("HyperNode not exists in cache, maybe not created or not be watched, will set parent first, name=%s, parent=%s", member, parent)
		childHn = NewHyperNodeInfo(&topologyv1alpha1.HyperNode{ObjectMeta: metav1.ObjectMeta{
			Name: member,
		}})
		hni.hyperNodes[member] = childHn
	}

	if childHn.Parent != "" && childHn.Parent != parent {
		hni.builtErrHyperNode = parent
		return fmt.Errorf("HyperNode %s already has a parent %s, and cannot set another parent %s", member, childHn.Parent, parent)
	}

	childHn.Parent = parent
	parentHn.Children.Insert(member)

	return nil
}

// GetMembers retrieves the members of a HyperNode based on the selector.
func GetMembers(selector topologyv1alpha1.MemberSelector, nodes []*corev1.Node) sets.Set[string] {
	members := sets.New[string]()
	if selector.ExactMatch != nil {
		if selector.ExactMatch.Name == "" {
			return members
		}
		members.Insert(selector.ExactMatch.Name)
	}

	if selector.RegexMatch != nil {
		pattern := selector.RegexMatch.Pattern
		reg, err := regexp.Compile(pattern)
		if err != nil {
			klog.Errorf("Failed to compile regular expression, pattern=%s, err=%v", pattern, err)
			return sets.Set[string]{}
		}
		for _, node := range nodes {
			if reg.MatchString(node.Name) {
				members.Insert(node.Name)
			}
		}
	}

	if selector.LabelMatch != nil {
		if len(selector.LabelMatch.MatchLabels) == 0 && len(selector.LabelMatch.MatchExpressions) == 0 {
			return members
		}
		labelSelector, err := metav1.LabelSelectorAsSelector(selector.LabelMatch)
		if err != nil {
			klog.Errorf("Failed to convert labelMatch to labelSelector, LabelMatch=%v, err=%v", selector.LabelMatch, err)
			return sets.Set[string]{}
		}
		for _, node := range nodes {
			nodeLabels := labels.Set(node.Labels)
			if labelSelector.Matches(nodeLabels) {
				members.Insert(node.Name)
			}
		}
	}

	return members
}

// exactMatchMember retrieves the member name if the selector is an exact match.
func (hni *HyperNodesInfo) exactMatchMember(selector topologyv1alpha1.MemberSelector) string {
	if selector.ExactMatch != nil {
		return selector.ExactMatch.Name
	}
	return ""
}

func (hni *HyperNodesInfo) updateAncestors(name string) error {
	if err := hni.rebuildCache(name); err != nil {
		hni.setReady(false)
		return err
	}
	// When last time BuildHyperNodeCache has an err that a hyperNode has multi parents, after the parent is updated
	// we should find the parent hyperNode to rebuild the correct cache.
	if hni.builtErrHyperNode == name {
		klog.Infof("Rebuilt parent hyperNode, name=%s", hni.builtErrHyperNode)
		leafHyperNodes := hni.GetLeafNodes(name)
		for leaf := range leafHyperNodes {
			if err := hni.rebuildCache(leaf); err != nil {
				return err
			}
		}
	}
	hni.builtErrHyperNode = ""
	hni.setReady(true)
	return nil
}

func (hni *HyperNodesInfo) rebuildCache(name string) error {
	ancestors := hni.hyperNodes.GetAncestors(name)
	// Clear realNodesSet of hyperNode before rebuild hyperNode cache.
	for _, ancestor := range ancestors {
		delete(hni.realNodesSet, ancestor)
		hni.resetParent(ancestor)
		hni.resetChildren(ancestor)
	}

	processed := sets.New[string]()
	ancestorsChain := sets.New[string]()
	nodes, err := hni.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes, hyperNodeName=%s, err=%v", name, err)
		return err
	}

	for _, ancestor := range ancestors {
		if hn, ok := hni.hyperNodes[ancestor]; ok {
			if err := hni.BuildHyperNodeCache(hn, processed, ancestorsChain, sets.New[string](ancestors...), nodes); err != nil {
				return err
			}
		}
	}

	return nil
}

// removeFromTierSet removes a HyperNode from the tier set.
func (hni *HyperNodesInfo) removeFromTierSet(hyperNodeName string, tier int) {
	set, ok := hni.hyperNodesSetByTier[tier]
	if !ok {
		return
	}

	set.Delete(hyperNodeName)
	if set.Len() == 0 {
		delete(hni.hyperNodesSetByTier, tier)
	}
}

func (hni *HyperNodesInfo) updateHyperNodesSetByTier(hyperNode *topologyv1alpha1.HyperNode) {
	tier := hyperNode.Spec.Tier
	old, exists := hni.hyperNodes[hyperNode.Name]
	if exists {
		if old.tier != hyperNode.Spec.Tier {
			hni.removeFromTierSet(hyperNode.Name, old.tier)
		}
	}
	if _, ok := hni.hyperNodesSetByTier[tier]; !ok {
		hni.hyperNodesSetByTier[tier] = sets.New[string]()
	}
	hni.hyperNodesSetByTier[tier].Insert(hyperNode.Name)
}

// removeParent removes the parent of a HyperNode.
func (hni *HyperNodesInfo) removeParent(name string) {
	children := hni.getChildren(name)
	for child := range children {
		hni.resetParent(child)
	}
}

func (hni *HyperNodesInfo) resetParent(name string) {
	if hn, ok := hni.hyperNodes[name]; ok {
		hn.Parent = ""
	}
}

func (hni *HyperNodesInfo) resetChildren(name string) {
	if hn, ok := hni.hyperNodes[name]; ok {
		hn.Children.Clear()
	}
}

func (hni *HyperNodesInfo) deleteHyperNode(name string) {
	hn, ok := hni.hyperNodes[name]
	if !ok {
		klog.Errorf("HyperNode not exists in cache, name=%s", name)
		return
	}
	delete(hni.hyperNodes, name)
	hni.removeFromTierSet(name, hn.tier)
}

// markHyperNodeIsDeleting marks a HyperNode as being deleted.
func (hni *HyperNodesInfo) markHyperNodeIsDeleting(name string) {
	hn, ok := hni.hyperNodes[name]
	if !ok {
		klog.Errorf("HyperNode not exists in cache, name=%s", name)
		return
	}
	hn.isDeleting = true
}

// hyperNodeIsDeleting checks if a HyperNode is being deleted.
func (hni *HyperNodesInfo) hyperNodeIsDeleting(name string) bool {
	hn, ok := hni.hyperNodes[name]
	if !ok {
		klog.Errorf("HyperNode not exists in cache, name=%s", name)
		return false
	}
	return hn.isDeleting
}

// HyperNodesInfo returns the hyperNodes info with a human-readable string.
func (hni *HyperNodesInfo) HyperNodesInfo() map[string]string {
	actualHyperNodes := make(map[string]string)
	for name, hn := range hni.hyperNodes {
		actualHyperNodes[name] = hn.String()
	}
	return actualHyperNodes
}

// NodeRegexOrLabelMatchLeafHyperNode checks if a given node matches the MemberSelector of a HyperNode.
func (hni *HyperNodesInfo) NodeRegexOrLabelMatchLeafHyperNode(nodeName string, hyperNodeName string) (bool, error) {
	hn, ok := hni.hyperNodes[hyperNodeName]
	if !ok {
		return false, fmt.Errorf("HyperNode %s not found in cache", hyperNodeName)
	}

	for _, member := range hn.HyperNode.Spec.Members {
		if member.Type == topologyv1alpha1.MemberTypeNode {
			if hni.nodeMatchSelector(nodeName, member.Selector) {
				return true, nil
			}
		}
	}
	return false, nil
}

// nodeMatchSelector checks if a node matches a MemberSelector.
func (hni *HyperNodesInfo) nodeMatchSelector(nodeName string, selector topologyv1alpha1.MemberSelector) bool {
	if selector.RegexMatch == nil && selector.LabelMatch == nil {
		return false
	}

	if selector.RegexMatch != nil {
		reg, err := regexp.Compile(selector.RegexMatch.Pattern)
		if err != nil {
			klog.Errorf("Failed to compile regex pattern, pattern=%s, err=%v", selector.RegexMatch.Pattern, err)
			return false
		}
		return reg.MatchString(nodeName)
	}
	if selector.LabelMatch != nil {
		if len(selector.LabelMatch.MatchLabels) == 0 && len(selector.LabelMatch.MatchExpressions) == 0 {
			return false
		}
		labelSelector, err := metav1.LabelSelectorAsSelector(selector.LabelMatch)
		if err != nil {
			klog.Errorf("Failed to convert labelMatch to labelSelector, LabelMatch=%v, err=%v", selector.LabelMatch, err)
			return false
		}
		node, err := hni.nodeLister.Get(nodeName)

		if err != nil {
			klog.Errorf("Failed to get node, nodeName=%s, err=%v", nodeName, err)
			return false
		}
		nodeLabels := labels.Set(node.Labels)
		if labelSelector.Matches(nodeLabels) {
			return true
		}
	}
	return false
}

// GetAncestors returns all ancestors of a given HyperNode.
func (hnim HyperNodeInfoMap) GetAncestors(name string) []string {
	ancestors := []string{name}
	queue := []string{name}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		parent := ""
		hn, ok := hnim[current]
		if ok && hn.Parent != "" {
			parent = hn.Parent
		} else {
			parent = hnim.getParent(current)
		}
		if parent != "" && !slices.Contains(ancestors, parent) {
			ancestors = append(ancestors, parent)
			queue = append(queue, parent)
		}
	}
	return ancestors
}

// GetAncestorHyperNode returns the ancestor HyperNode name at the given tier.
func (hnim HyperNodeInfoMap) GetAncestorHyperNode(hyperNodeName string, tier int) string {
	for _, ancestor := range hnim.GetAncestors(hyperNodeName) {
		if hn, ok := hnim[ancestor]; ok && hn.Tier() == tier {
			return ancestor
		}
	}
	return ""
}

// ResolveHyperNodesAtTier maps a placed HyperNode to topology domain name(s) at tier.
// When placement is coarser than tier, all descendant HyperNodes at tier are returned;
// when placement is at or finer than tier, the single ancestor at tier (including self) is returned.
func (hnim HyperNodeInfoMap) ResolveHyperNodesAtTier(hyperNodeName string, tier int) []string {
	hn, ok := hnim[hyperNodeName]
	if !ok {
		return nil
	}
	if hn.Tier() > tier {
		return hnim.hyperNodesAtTierUnder(hyperNodeName, tier)
	}
	if ancestor := hnim.GetAncestorHyperNode(hyperNodeName, tier); ancestor != "" {
		return []string{ancestor}
	}
	return nil
}

// hyperNodesAtTierUnder lists HyperNode names at tier that lie under root in the tree.
// Called when a job's placement is coarser than the anti-affinity term tier (for example
// allocated at supernode while the term compares at rack): every descendant domain at tier
// under root must be treated as occupied.
//
// The scan walks all HyperNodes at tier and keeps those whose ancestor chain includes root.
// Upward lookup via GetAncestors is used instead of BFS from root.Children so this stays
// correct when only Parent links are populated (tests and early cache states).
func (hnim HyperNodeInfoMap) hyperNodesAtTierUnder(root string, tier int) []string {
	names := make([]string, 0)
	for name, hn := range hnim {
		if hn.Tier() != tier {
			continue
		}
		for _, ancestor := range hnim.GetAncestors(name) {
			if ancestor == root {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// GetAncestorHyperNodeByTierName returns the ancestor HyperNode name at HyperNode.spec.tierName.
func (hnim HyperNodeInfoMap) GetAncestorHyperNodeByTierName(hyperNodeName, tierName string) string {
	for _, ancestor := range hnim.GetAncestors(hyperNodeName) {
		if hn, ok := hnim[ancestor]; ok && hn.TierName() == tierName {
			return ancestor
		}
	}
	return ""
}

// getParent returns hyperNode's parent, this is usually used when a new hyperNode is added
// but the BuildHyperNodeCache has not built the hyperNode's cache.
func (hnim HyperNodeInfoMap) getParent(name string) string {
	tier := -1
	if hn, ok := hnim[name]; ok {
		tier = hn.tier
	}
	for _, hn := range hnim {
		if hn.tier <= tier {
			continue
		}
		for _, member := range hn.HyperNode.Spec.Members {
			// hyperNode's parent must also be a hyperNode.
			if member.Type == topologyv1alpha1.MemberTypeNode || member.Selector.ExactMatch == nil {
				continue
			}
			memberName := member.Selector.ExactMatch.Name
			if memberName == name {
				// just find one is ok.
				return hn.Name
			}
		}
	}
	return ""
}

// GetLCAHyperNode returns the least common ancestor hypernode of the hypernode to be allocated and job's already allocated hypernode
func (hnim HyperNodeInfoMap) GetLCAHyperNode(hypernode, jobHyperNode string) string {
	if hypernode == "" {
		return jobHyperNode
	}
	if jobHyperNode == "" {
		return hypernode
	}

	hyperNodeAncestors := hnim.GetAncestors(hypernode)
	jobHyperNodeAncestors := hnim.GetAncestors(jobHyperNode)

	hyperNodeAncestorsMap := make(map[string]bool)
	for _, ancestor := range hyperNodeAncestors {
		hyperNodeAncestorsMap[ancestor] = true
	}

	for _, ancestor := range jobHyperNodeAncestors {
		if hyperNodeAncestorsMap[ancestor] {
			return ancestor
		}
	}
	return ""
}

// GetSearchRoot returns the HyperNode subtree root for gradient search.
// It intersects the caller-provided hyperNode with the envelope implied by allocatedHyperNode.
func GetSearchRoot(
	hyperNodes HyperNodeInfoMap,
	hyperNodeAvailable *HyperNodeInfo,
	highestAllowedTier int,
	allocatedHyperNode string,
) (*HyperNodeInfo, error) {
	if allocatedHyperNode == "" {
		return hyperNodeAvailable, nil
	}

	hyperNodeHighestAllowed, err := GetHighestAllowedHyperNode(hyperNodes, highestAllowedTier, allocatedHyperNode)
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

// GetHighestAllowedHyperNode returns the coarsest ancestor of allocatedHyperNode within highestAllowedTier.
func GetHighestAllowedHyperNode(hyperNodes HyperNodeInfoMap, highestAllowedTier int, allocatedHyperNode string) (string, error) {
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
