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

// Package networktopologyaware turns RepackRun.spec.networkTopology into
// HyperNode-block shaping: when a run names a target HyperNode tier and block
// size, it registers two plan-score terms (node-block progress, node-block
// distribution), one hard block-count constraint, and one receiver preference
// (nodeBlockPreserve) that steers relocated pods away from the target tier's
// HyperNodes (no-H > other HyperNode > own HyperNode, design §4.1.3.4). It does
// NOT contribute a new freeable unit — it reuses nodeconsolidation's single-node
// unit (design doc §4.1.3): every candidate frees exactly one node, and "block"
// semantics are expressed as constraints rather than units.
//
// The package is dormant by default: when networkTopology is unset (R1) it
// registers nothing and the engine behaves exactly as before.
//
// Single-node-unit assumption (design doc §4.1.3): all scoring anchors on
// IncrementalFromNodes()[0] rely on the candidate unit freeing exactly one node,
// so the anchor is unique. freeInH counts this candidate as +1. If a future
// domain contributes multi-node units, the anchor and freeInH accounting must
// be revisited, and units must never span multiple HyperNodes.
package networktopologyaware

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "networktopologyaware"

// Default weights for the two block score terms. Progress is deliberately far
// above distribution so a difference in block progress dominates the candidate
// ordering (R13); distribution only decides among candidates with equal
// progress (R14).
const (
	weightNodeBlockProgress     int64 = 1000000
	weightNodeBlockDistribution int64 = 100

	argNodeBlockProgressWeight     = "nodeBlockProgressWeight"
	argNodeBlockDistributionWeight = "nodeBlockDistributionWeight"
)

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory:   newPlugin,
		Validator: validateArguments,
		// Requires CapabilityDomain: block shaping consumes the consolidation
		// domain's single-node units instead of contributing its own.
		Requires: []framework.PluginCapability{framework.CapabilityDomain},
	})
}

type networkTopologyAwarePlugin struct {
	progressWeight     int64
	distributionWeight int64
}

func newPlugin(arguments framework.Arguments) framework.Plugin {
	return &networkTopologyAwarePlugin{
		progressWeight:     configuredWeight(arguments, argNodeBlockProgressWeight, weightNodeBlockProgress),
		distributionWeight: configuredWeight(arguments, argNodeBlockDistributionWeight, weightNodeBlockDistribution),
	}
}

func configuredWeight(arguments framework.Arguments, key string, defaultValue int64) int64 {
	value, err := arguments.NonNegativeInt(key, defaultValue)
	if err != nil {
		return defaultValue
	}
	return value
}

// validateArguments mirrors workloaddisruption: unknown keys are rejected and
// both weights must be non-negative (R17). A zero weight disables the term.
func validateArguments(arguments framework.Arguments) error {
	if err := arguments.ValidateKeys(argNodeBlockProgressWeight, argNodeBlockDistributionWeight); err != nil {
		return err
	}
	for _, item := range []struct {
		key          string
		defaultValue int64
	}{
		{argNodeBlockProgressWeight, weightNodeBlockProgress},
		{argNodeBlockDistributionWeight, weightNodeBlockDistribution},
	} {
		if _, err := arguments.NonNegativeInt(item.key, item.defaultValue); err != nil {
			return err
		}
	}
	return nil
}

func (*networkTopologyAwarePlugin) Name() string { return Name }

// nodeBlockSession holds the per-session topology precompute shared by the four
// callbacks. It is built once in OnSessionOpen and captured by value in the
// closures; it must not change during the pass.
type nodeBlockSession struct {
	// targetTier is the HyperNode tier the run plans against.
	targetTier int
	// size is the number of nodes in one block (nodeBlockSize, >= 1).
	size int
	// requiredBlocks is the hard admission floor (requiredNodeBlocks).
	requiredBlocks int
	// mode is the block distribution preference ("" when unset).
	mode repackv1alpha1.RepackBlockMode
	// hyperNodesInTier is the ordered list of HyperNode names at targetTier.
	hyperNodesInTier []string
	// nodeToHyperNode maps each real node to the HyperNode it belongs to at
	// targetTier. A node belongs to at most one HyperNode at a tier (node->H is
	// a function); nodes outside the tier are absent.
	nodeToHyperNode map[string]string
	// idleInH / busyInH are the session-start counts per HyperNode: nodes with
	// zero target-resource usage (Empty) and partially used nodes (Partial).
	// Unavailable (capacity 0) and Full nodes are excluded on purpose — see the
	// ClassifyTargetResourceNode reuse below.
	idleInH map[string]int
	busyInH map[string]int
	// blocksInHMax is max over the tier of floor((idleInH+busyInH)/size): the
	// spread mode's least-preferred raw score for nodes outside any HyperNode.
	blocksInHMax int
}

func (p *networkTopologyAwarePlugin) OnSessionOpen(ssn *framework.Session) {
	run := ssn.Run()
	runName := ""
	if run != nil {
		runName = run.Name
	}
	if run == nil || run.Spec.NetworkTopology == nil {
		// R1: networkTopology unset — register nothing; behavior identical to the
		// engine running without this plugin.
		klog.V(4).InfoS("repack networktopologyaware: networkTopology unset, plugin inactive", "run", runName)
		return
	}
	bsn, ok := buildNodeBlockSession(ssn, run.Spec.NetworkTopology)
	if !ok {
		// The target tier is unresolvable (the tier/tierName does not exist, or the
		// tier has no HyperNode): there is no topology to plan against, so the plugin
		// has no effect. The user explicitly configured networkTopology yet it does
		// not apply — a config-vs-cluster-state mismatch, logged as a warning so it
		// can be diagnosed.
		topo := run.Spec.NetworkTopology
		klog.Warningf("repack networktopologyaware: target tier unresolvable (tier=%s tierName=%s), block shaping inactive; run=%s",
			tierString(topo), tierNameString(topo), runName)
		return
	}
	klog.V(4).InfoS("repack networktopologyaware: block shaping enabled",
		"run", runName, "tier", bsn.targetTier, "blockSize", bsn.size,
		"requiredBlocks", bsn.requiredBlocks, "mode", bsn.mode,
		"hyperNodeCount", len(bsn.hyperNodesInTier))
	p.registerNodeBlockProgressScore(ssn, bsn) // 4.1.3.1, always registered
	if bsn.mode == repackv1alpha1.RepackBlockModeBinpack || bsn.mode == repackv1alpha1.RepackBlockModeSpread {
		p.registerNodeBlockDistributionScore(ssn, bsn) // 4.1.3.2, binpack/spread only
	}
	p.registerBlockCountConstraint(ssn, bsn)        // 4.1.3.3, always registered
	p.registerNodeBlockReceiverPreference(ssn, bsn) // 4.1.3.4, always registered
}

// tierString / tierNameString render the pointer tier identifiers for logs
// without allocating when nil.
func tierString(topo *repackv1alpha1.NetworkTopology) string {
	if topo.HyperNodeTier == nil {
		return "<unset>"
	}
	return fmt.Sprintf("%d", *topo.HyperNodeTier)
}

func tierNameString(topo *repackv1alpha1.NetworkTopology) string {
	if topo.HyperNodeTierName == nil {
		return "<unset>"
	}
	return *topo.HyperNodeTierName
}

// buildNodeBlockSession resolves the target tier, the node->HyperNode index and
// the session-start idle/busy counts. ok=false when the tier does not exist or
// holds no HyperNode.
func buildNodeBlockSession(ssn *framework.Session, topo *repackv1alpha1.NetworkTopology) (*nodeBlockSession, bool) {
	snapshot := ssn.Snapshot()
	targetTier, ok := resolveTargetTier(snapshot, topo)
	if !ok {
		return nil, false
	}
	// nodeBlockSize is a pointer: the apiserver defaults an omitted value to 1 and
	// rejects an explicit 0 via minimum:1. Defensive fallback for inputs that
	// bypass the apiserver (unit tests / direct informers): nil -> 1, <1 -> 1.
	size := 1
	if topo.NodeBlockSize != nil {
		size = *topo.NodeBlockSize
	}
	if size < 1 {
		size = 1
	}
	bsn := &nodeBlockSession{
		targetTier:      targetTier,
		size:            size,
		requiredBlocks:  topo.RequiredNodeBlocks,
		mode:            topo.Mode,
		nodeToHyperNode: make(map[string]string),
		idleInH:         make(map[string]int),
		busyInH:         make(map[string]int),
	}
	if bsn.requiredBlocks < 0 {
		bsn.requiredBlocks = 0 // defensive: the CRD already guarantees non-negative
	}

	hyperNodesByTier := snapshot.HyperNodesSetByTier()
	realNodesSet := snapshot.RealNodesSet()
	bsn.hyperNodesInTier = sets.List(hyperNodesByTier[targetTier])
	if len(bsn.hyperNodesInTier) == 0 {
		return nil, false
	}

	// node -> HyperNode at the target tier. Per design constraint, a node
	// belongs to at most one HyperNode at one tier; an overlap is a config
	// anomaly — keep the first hit, warn, and never double count (R5).
	for _, hn := range bsn.hyperNodesInTier {
		for node := range realNodesSet[hn] {
			if existing, taken := bsn.nodeToHyperNode[node]; taken && existing != hn {
				klog.Warningf("HyperNode-aware repack: node %s belongs to both %s and %s at tier %d; keeping %s", node, existing, hn, targetTier, existing)
				continue
			}
			bsn.nodeToHyperNode[node] = hn
		}
	}

	// Session-start idle/busy counts, reusing ClassifyTargetResourceNode so the
	// "empty vs freeable" classification matches nodeconsolidation and the
	// resource accounting is exactly one source of truth. Unavailable (capacity
	// 0) and Full nodes are excluded on both sides: they can neither host new
	// target-resource pods nor be freed for the block semantics.
	resource := ssn.Resource()
	// Index the snapshot nodes by name once so classification below is O(T)
	// instead of O(T x C) (one linear scan of the whole cluster per tier node).
	// Reuses the snapshot captured at the top of this function (not a second
	// ssn.Snapshot() call).
	nodeByName := make(map[string]*schedapi.NodeInfo, len(snapshot.Nodes()))
	for _, n := range snapshot.Nodes() {
		if n != nil && n.Name != "" {
			nodeByName[n.Name] = n
		}
	}
	for nodeName, hn := range bsn.nodeToHyperNode {
		ni := nodeByName[nodeName]
		switch api.ClassifyTargetResourceNode(ni, resource) {
		case api.TargetResourceNodeEmpty:
			bsn.idleInH[hn]++
		case api.TargetResourceNodePartial:
			bsn.busyInH[hn]++
		}
	}
	for _, hn := range bsn.hyperNodesInTier {
		if blocks := (bsn.idleInH[hn] + bsn.busyInH[hn]) / bsn.size; blocks > bsn.blocksInHMax {
			bsn.blocksInHMax = blocks
		}
	}
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	klog.V(5).InfoS("repack networktopologyaware: tier block session built",
		"run", runName, "tier", bsn.targetTier, "blockSize", bsn.size,
		"requiredBlocks", bsn.requiredBlocks, "mode", bsn.mode,
		"hyperNodes", bsn.hyperNodesInTier, "nodeCount", len(bsn.nodeToHyperNode),
		"idleInH", bsn.idleInH, "busyInH", bsn.busyInH, "blocksInHMax", bsn.blocksInHMax)
	return bsn, true
}

// resolveTargetTier maps the run's tier identifier to a numeric tier.
func resolveTargetTier(snapshot framework.Snapshot, topo *repackv1alpha1.NetworkTopology) (int, bool) {
	if topo.HyperNodeTier != nil {
		return *topo.HyperNodeTier, true
	}
	if topo.HyperNodeTierName != nil {
		tier, ok := snapshot.HyperNodeTierNameMap()[*topo.HyperNodeTierName]
		return tier, ok
	}
	return 0, false
}

// freedInHyperNode counts how many of the plan's freed nodes belong to hn at
// the target tier. Nodes outside every HyperNode count 0 (R9).
func freedInHyperNode(freedNodes []string, hn string, nodeToHyperNode map[string]string) int {
	count := 0
	for _, node := range freedNodes {
		if nodeToHyperNode[node] == hn {
			count++
		}
	}
	return count
}

// nodeBlockProgressScore implements the 4.1.3.1 block-progress formula (R7).
// freeInH = idleInH + freedInH and freeableInH = busyInH - freedInH are the
// caller-supplied per-HyperNode counters. size is the block size (>= 1; the
// caller normalizes before calling, and the function defends size < 1 anyway).
func nodeBlockProgressScore(freeInH, freeableInH, size int) int64 {
	if size < 1 {
		size = 1
	}
	r := freeInH % size
	switch {
	case r == 0:
		return int64(size) // this candidate exactly completes a block: full score
	case freeableInH < size-r:
		return 0 // this HyperNode can never form another complete block: give up
	default:
		return int64(r) // a complete block is still reachable: closer to full wins
	}
}

// nodeBlockDistributionScore implements the 4.1.3.2 raw score (R8).
// hasH=false means the anchor node belongs to no HyperNode at the target tier:
// take a sentinel one below the mode's real-candidate minimum raw — -1 for
// binpack (below the [0, +blocksInHMax] floor) and -(blocksInHMax+1) for spread
// (below the [-blocksInHMax, 0] floor) — so a no-H candidate scores strictly
// worse than any real candidate (R6), and spread never ties a no-H candidate
// with a zero-block H when blocksInHMax==0 (sparse tier, every HyperNode smaller
// than nodeBlockSize), where the zero-block H is the highest score, not lowest.
// hasH=true: blocks is the anchor HyperNode's complete-block count; binpack
// returns +blocks, spread returns -blocks.
func nodeBlockDistributionScore(mode repackv1alpha1.RepackBlockMode, hasH bool, blocks, blocksInHMax int) int64 {
	if !hasH {
		switch mode {
		case repackv1alpha1.RepackBlockModeBinpack:
			return -1 // one below the real minimum raw 0 (zero-block H)
		case repackv1alpha1.RepackBlockModeSpread:
			return -int64(blocksInHMax) - 1 // one below the real minimum raw -blocksInHMax
		}
		return 0
	}
	switch mode {
	case repackv1alpha1.RepackBlockModeBinpack:
		return int64(blocks) // more blocks is better (concentrate)
	case repackv1alpha1.RepackBlockModeSpread:
		return -int64(blocks) // fewer blocks is better (disperse)
	}
	return 0
}

// totalBlocksInTier sums the completed blocks over every HyperNode of the target
// tier (R9): floor((idleInH[hn] + freedInH[hn]) / size). freedInH[hn] counts the
// plan-freed nodes that belong to hn; nodes outside the tier contribute 0.
func totalBlocksInTier(idleInH, freedInH map[string]int, hyperNodesInTier []string, size int) int {
	if size < 1 {
		size = 1
	}
	total := 0
	for _, hn := range hyperNodesInTier {
		total += (idleInH[hn] + freedInH[hn]) / size
	}
	return total
}

// ---- 4.1.3.1 node-block progress score ----

func (p *networkTopologyAwarePlugin) registerNodeBlockProgressScore(ssn *framework.Session, bsn *nodeBlockSession) {
	ssn.AddPlanScoreFn("nodeBlockProgress", p.progressWeight, func(_ *api.PlanContext, plan *api.CandidatePlan) int64 {
		anchor := plan.IncrementalFromNodes()
		if len(anchor) == 0 {
			return 0
		}
		// Single-node unit: the anchor is the unique node freed by this candidate (R4).
		hn, ok := bsn.nodeToHyperNode[anchor[0]]
		if !ok {
			return 0 // node belongs to no HyperNode at the target tier: least preferred (R6)
		}
		freedInH := freedInHyperNode(plan.FreedNodes(), hn, bsn.nodeToHyperNode)
		return nodeBlockProgressScore(
			bsn.idleInH[hn]+freedInH, // freeInH: historical idle + plan-freed (incl. this candidate)
			bsn.busyInH[hn]-freedInH, // freeableInH: still drainable (optimistic upper bound)
			bsn.size,
		)
	})
}

// ---- 4.1.3.2 node-block distribution score ----

func (p *networkTopologyAwarePlugin) registerNodeBlockDistributionScore(ssn *framework.Session, bsn *nodeBlockSession) {
	ssn.AddPlanScoreFn("nodeBlockDistribution", p.distributionWeight, func(_ *api.PlanContext, plan *api.CandidatePlan) int64 {
		anchor := plan.IncrementalFromNodes()
		if len(anchor) == 0 {
			return 0
		}
		hn, ok := bsn.nodeToHyperNode[anchor[0]]
		if !ok {
			// No H: take the least-preferred value for the mode (R6); see nodeBlockDistributionScore.
			return nodeBlockDistributionScore(bsn.mode, false, 0, bsn.blocksInHMax)
		}
		freedInH := freedInHyperNode(plan.FreedNodes(), hn, bsn.nodeToHyperNode)
		blocks := (bsn.idleInH[hn] + freedInH) / bsn.size
		return nodeBlockDistributionScore(bsn.mode, true, blocks, bsn.blocksInHMax)
	})
}

// ---- 4.1.3.3 block-count admission (hard gate) ----

func (p *networkTopologyAwarePlugin) registerBlockCountConstraint(ssn *framework.Session, bsn *nodeBlockSession) {
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	ssn.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) (bool, string) {
		if plan == nil {
			return false, ""
		}
		if bsn.requiredBlocks == 0 {
			return true, "" // R10: 0 (default) always passes — degrades to pure soft guidance
		}
		freedByH := make(map[string]int, len(bsn.hyperNodesInTier))
		for _, node := range plan.FreedNodes {
			if hn, ok := bsn.nodeToHyperNode[node]; ok {
				freedByH[hn]++
			}
		}
		total := totalBlocksInTier(bsn.idleInH, freedByH, bsn.hyperNodesInTier, bsn.size)
		admitted := total >= bsn.requiredBlocks
		klog.V(4).InfoS("repack networktopologyaware: block-count gate", "run", runName,
			"requiredBlocks", bsn.requiredBlocks, "blockSize", bsn.size,
			"freedNodeCount", len(plan.FreedNodes), "freedByH", freedByH,
			"completeBlocks", total, "admitted", admitted)
		if admitted {
			return true, ""
		}
		// Not enough complete blocks formed: report the block-specific reason,
		// not the fragmentation-improvement one.
		return false, state.ReasonRequiredNodeBlocksNotMet
	})
}

// ---- 4.1.3.4 node-block receiver preference (receiver steering) ----

// registerNodeBlockReceiverPreference steers relocated pods away from the target
// tier's HyperNodes, closing the receiver side of block shaping (design §4.1.3.4).
// The preference is lexicographic per receiver, ordered no-HyperNode ({3}) >
// another HyperNode ({2}) > own HyperNode ({1}); abstain ({}) when the candidate
// frees no node, letting later keys decide. Registering unconditionally is
// deliberate: it preserves block progress (mode-independent), orthogonal to the
// binpack/spread distribution score; R1 dormancy (networkTopology unset) already
// prevents registration entirely.
//
// Only ever reorders the receiver list — firstFeasibleReceiver still takes the
// first *feasible* receiver, so no new infeasibility is introduced and the
// 4.1.3.3 block-count gate is unaffected (it counts freed nodes, not destinations).
//
// The Topology phase guarantees the key order staysOccupied -> nodeBlockPreserve
// -> futureGangImpact -> bestFit independent of the plugin list: stability
// policies (which prefer sacrificial non-drainable receivers — filled, immovable,
// scope-excluded, proven stuck) always win, and filling those never hurts the
// block pool since they could not be drained anyway.
func (p *networkTopologyAwarePlugin) registerNodeBlockReceiverPreference(ssn *framework.Session, bsn *nodeBlockSession) {
	ssn.AddReceiverPreferenceFn("nodeBlockPreserve", framework.ReceiverPreferencePhaseTopology,
		func(_ *api.PlanContext, candidate *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverPreference {
			anchors := candidate.Plan.IncrementalFromNodes()
			if len(anchors) == 0 {
				return framework.ReceiverPreference{} // no anchor: abstain, let later keys decide
			}
			// Anchor HyperNode set: the drain unit is single-node, so this collapses
			// to one HyperNode, identical to the scoring fns' [0] convention; the set
			// form is kept for generality — preserve every anchor HyperNode, not just
			// the first.
			ownHs := make(map[string]bool, len(anchors))
			for _, n := range anchors {
				if h, ok := bsn.nodeToHyperNode[n]; ok {
					ownHs[h] = true
				}
			}
			recvH, inTier := bsn.nodeToHyperNode[receiver.Node.Name]
			switch {
			case !inTier:
				return framework.ReceiverPreference{3} // export the load outside the tier
			case ownHs[recvH]:
				return framework.ReceiverPreference{1} // own HyperNode: last resort
			default:
				return framework.ReceiverPreference{2} // another HyperNode
			}
		})
}

func (*networkTopologyAwarePlugin) OnSessionClose(*framework.Session) {}
