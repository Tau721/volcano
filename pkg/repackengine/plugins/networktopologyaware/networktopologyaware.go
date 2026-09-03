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
// HyperNode-block shaping. When a run names a target HyperNode tier and block
// size, it registers two plan-score terms (node-block progress, node-block
// distribution), one hard block-count constraint, and one receiver preference
// (nodeBlockPreserve) that steers relocated pods away from the target tier's
// HyperNodes (no-H > other HyperNode > own HyperNode).
//
// It contributes no freeable unit of its own: it reuses nodeconsolidation's
// single-node unit (every candidate frees exactly one node), expressing block
// semantics as constraints. When networkTopology is unset the plugin registers
// nothing, so the engine runs unchanged.
//
// Single-node-unit assumption: all scoring anchors read IncrementalFromNodes()[0],
// relying on the candidate freeing exactly one node so the anchor is unique, and
// freeInHyperNode counts this candidate as +1. If a future domain contributes
// multi-node units these anchors and the accounting must be revisited, and units
// must never span multiple HyperNodes.
//
// Activation — free two blocks of 4 nodes in the tier named "accel", spreading
// them across HyperNodes:
//
//	apiVersion: repack.volcano.sh/v1alpha1
//	kind: RepackRun
//	spec:
//	  mode: Execute
//	  networkTopology:
//	    hyperNodeTierName: "accel"
//	    nodeBlockSize: 4
//	    requiredNodeBlocks: 2
//	    mode: spread
//	  goals:
//	    - resource: nvidia.com/gpu
//
// The plugin is enabled by default in repack-engine.conf's plugins list. Its
// two block-term weights (defaults below) are tunable through the plugin
// arguments there; a zero weight disables the corresponding term:
//
//	plugins:
//	- name: networktopologyaware
//	  arguments:
//	    nodeBlockProgressWeight: 1000000
//	    nodeBlockDistributionWeight: 100
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

// Default weights for the two block score terms. Progress outweighs distribution
// so a progress difference dominates candidate ordering; distribution only
// breaks equal-progress ties.
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
		// Reuses the consolidation domain's single-node units instead of adding its own.
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
// both weights must be non-negative. A zero weight disables the term.
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

// nodeBlockSession holds the per-session topology precompute shared by the
// callbacks. It is built once in OnSessionOpen and must not change during the pass.
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
	// nodeToHyperNode maps each real node at targetTier to its HyperNode (a node
	// is in at most one); nodes outside the tier are absent.
	nodeToHyperNode map[string]string
	// idleInHyperNode / busyInHyperNode are the session-start counts per HyperNode of
	// Empty (zero target-resource usage) and Partial nodes; Unavailable/Full are
	// excluded on purpose (see the ClassifyTargetResourceNode reuse below).
	idleInHyperNode map[string]int
	busyInHyperNode map[string]int
	// maxBlocksInHyperNode is the tier max of floor((idle+busy)/size); spread mode's
	// least-preferred raw score for nodes outside any HyperNode.
	maxBlocksInHyperNode int
}

func (p *networkTopologyAwarePlugin) OnSessionOpen(ssn *framework.Session) {
	run := ssn.Run()
	runName := ""
	if run != nil {
		runName = run.Name
	}
	if run == nil || run.Spec.NetworkTopology == nil {
		// networkTopology unset: register nothing; the engine runs unchanged.
		klog.V(4).InfoS("repack networktopologyaware: networkTopology unset, plugin inactive", "run", runName)
		return
	}
	blockSession, ok := buildNodeBlockSession(ssn, run.Spec.NetworkTopology)
	if !ok {
		// Target tier unresolvable (no HyperNode at it): no topology to plan
		// against, so stay inert — but warn, since the user explicitly configured
		// networkTopology.
		topology := run.Spec.NetworkTopology
		klog.Warningf("repack networktopologyaware: target tier unresolvable (tier=%s tierName=%s), block shaping inactive; run=%s",
			tierString(topology), tierNameString(topology), runName)
		return
	}
	klog.V(4).InfoS("repack networktopologyaware: block shaping enabled",
		"run", runName, "tier", blockSession.targetTier, "blockSize", blockSession.size,
		"requiredBlocks", blockSession.requiredBlocks, "mode", blockSession.mode,
		"hyperNodeCount", len(blockSession.hyperNodesInTier))
	p.registerNodeBlockProgressScore(ssn, blockSession) // always registered
	if blockSession.mode == repackv1alpha1.RepackBlockModeBinpack || blockSession.mode == repackv1alpha1.RepackBlockModeSpread {
		p.registerNodeBlockDistributionScore(ssn, blockSession) // binpack/spread only
	}
	p.registerBlockCountConstraint(ssn, blockSession)        // always registered
	p.registerNodeBlockReceiverPreference(ssn, blockSession) // always registered
}

// tierString / tierNameString render the pointer tier identifiers for logs
// without allocating when nil.
func tierString(topology *repackv1alpha1.NetworkTopology) string {
	if topology.HyperNodeTier == nil {
		return "<unset>"
	}
	return fmt.Sprintf("%d", *topology.HyperNodeTier)
}

func tierNameString(topology *repackv1alpha1.NetworkTopology) string {
	if topology.HyperNodeTierName == nil {
		return "<unset>"
	}
	return *topology.HyperNodeTierName
}

// buildNodeBlockSession resolves the target tier, the node->HyperNode index and
// the session-start idle/busy counts. ok=false when the tier does not exist or
// holds no HyperNode.
func buildNodeBlockSession(ssn *framework.Session, topology *repackv1alpha1.NetworkTopology) (*nodeBlockSession, bool) {
	snapshot := ssn.Snapshot()
	targetTier, ok := resolveTargetTier(snapshot, topology)
	if !ok {
		return nil, false
	}
	// nodeBlockSize is a pointer; the apiserver defaults it to 1 and enforces
	// minimum 1. Defend direct-informer/test inputs: nil or <1 -> 1.
	size := 1
	if topology.NodeBlockSize != nil {
		size = *topology.NodeBlockSize
	}
	if size < 1 {
		size = 1
	}
	blockSession := &nodeBlockSession{
		targetTier:      targetTier,
		size:            size,
		requiredBlocks:  topology.RequiredNodeBlocks,
		mode:            topology.Mode,
		nodeToHyperNode: make(map[string]string),
		idleInHyperNode: make(map[string]int),
		busyInHyperNode: make(map[string]int),
	}
	if blockSession.requiredBlocks < 0 {
		blockSession.requiredBlocks = 0 // defensive: the CRD already guarantees non-negative
	}

	hyperNodesByTier := snapshot.HyperNodesSetByTier()
	realNodesSet := snapshot.RealNodesSet()
	blockSession.hyperNodesInTier = sets.List(hyperNodesByTier[targetTier])
	if len(blockSession.hyperNodesInTier) == 0 {
		return nil, false
	}

	// node -> HyperNode at the target tier. A node is normally in at most one
	// HyperNode per tier; on overlap keep the first hit, warn, and never double count.
	for _, hyperNode := range blockSession.hyperNodesInTier {
		for node := range realNodesSet[hyperNode] {
			if existing, taken := blockSession.nodeToHyperNode[node]; taken && existing != hyperNode {
				klog.Warningf("HyperNode-aware repack: node %s belongs to both %s and %s at tier %d; keeping %s", node, existing, hyperNode, targetTier, existing)
				continue
			}
			blockSession.nodeToHyperNode[node] = hyperNode
		}
	}

	// Session-start idle/busy counts via ClassifyTargetResourceNode, so the
	// "empty vs freeable" split matches nodeconsolidation (one source of truth).
	// Unavailable (capacity 0) and Full nodes are excluded: they can neither host
	// target-resource pods nor be freed for block semantics.
	resource := ssn.Resource()
	// Index snapshot nodes by name once so classification below is O(T), not
	// O(T x C) (a full-cluster scan per tier node).
	nodeByName := make(map[string]*schedapi.NodeInfo, len(snapshot.Nodes()))
	for _, n := range snapshot.Nodes() {
		if n != nil && n.Name != "" {
			nodeByName[n.Name] = n
		}
	}
	for nodeName, hyperNode := range blockSession.nodeToHyperNode {
		nodeInfo := nodeByName[nodeName]
		switch api.ClassifyTargetResourceNode(nodeInfo, resource) {
		case api.TargetResourceNodeEmpty:
			blockSession.idleInHyperNode[hyperNode]++
		case api.TargetResourceNodePartial:
			blockSession.busyInHyperNode[hyperNode]++
		}
	}
	for _, hyperNode := range blockSession.hyperNodesInTier {
		if blocks := (blockSession.idleInHyperNode[hyperNode] + blockSession.busyInHyperNode[hyperNode]) / blockSession.size; blocks > blockSession.maxBlocksInHyperNode {
			blockSession.maxBlocksInHyperNode = blocks
		}
	}
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	klog.V(5).InfoS("repack networktopologyaware: tier block session built",
		"run", runName, "tier", blockSession.targetTier, "blockSize", blockSession.size,
		"requiredBlocks", blockSession.requiredBlocks, "mode", blockSession.mode,
		"hyperNodes", blockSession.hyperNodesInTier, "nodeCount", len(blockSession.nodeToHyperNode),
		"idleInHyperNode", blockSession.idleInHyperNode, "busyInHyperNode", blockSession.busyInHyperNode, "maxBlocksInHyperNode", blockSession.maxBlocksInHyperNode)
	return blockSession, true
}

// resolveTargetTier maps the run's tier identifier to a numeric tier.
func resolveTargetTier(snapshot framework.Snapshot, topology *repackv1alpha1.NetworkTopology) (int, bool) {
	if topology.HyperNodeTier != nil {
		return *topology.HyperNodeTier, true
	}
	if topology.HyperNodeTierName != nil {
		tier, ok := snapshot.HyperNodeTierNameMap()[*topology.HyperNodeTierName]
		return tier, ok
	}
	return 0, false
}

// freedInHyperNode counts how many of the plan's freed nodes belong to hyperNode at
// the target tier. Nodes outside every HyperNode count 0.
func freedInHyperNode(freedNodes []string, hyperNode string, nodeToHyperNode map[string]string) int {
	count := 0
	for _, node := range freedNodes {
		if nodeToHyperNode[node] == hyperNode {
			count++
		}
	}
	return count
}

// nodeBlockProgressScore implements the block-progress formula over the anchor
// HyperNode once this candidate's plan-freed nodes are counted:
// freeInHyperNode = session idle + plan-freed (its free node count),
// freeableInHyperNode = session busy - plan-freed (still drainable). size >= 1.
func nodeBlockProgressScore(freeInHyperNode, freeableInHyperNode, size int) int64 {
	if size < 1 {
		size = 1
	}
	r := freeInHyperNode % size
	switch {
	case r == 0:
		return int64(size) // candidate completes a block
	case freeableInHyperNode < size-r:
		return 0 // a complete block is unreachable
	default:
		return int64(r) // block still reachable: closer to full wins
	}
}

// nodeBlockDistributionScore is the raw score of the node-block distribution
// term. For an anchor outside every HyperNode (hasHyperNode=false) it returns a
// sentinel one below the mode's real minimum — -1 for binpack and
// -(maxBlocksInHyperNode+1) for spread — so a no-H candidate always loses and,
// for spread, never ties a zero-block HyperNode when the tier is sparse (every
// HyperNode smaller than the block size), where zero blocks is the best score.
// Otherwise blocks is the anchor HyperNode's complete-block count and the score
// is +blocks (binpack: concentrate) or -blocks (spread: disperse).
func nodeBlockDistributionScore(mode repackv1alpha1.RepackBlockMode, hasHyperNode bool, blocks, maxBlocksInHyperNode int) int64 {
	if !hasHyperNode {
		switch mode {
		case repackv1alpha1.RepackBlockModeBinpack:
			return -1 // one below the real minimum raw 0 (zero-block H)
		case repackv1alpha1.RepackBlockModeSpread:
			return -int64(maxBlocksInHyperNode) - 1 // one below the real minimum raw -maxBlocksInHyperNode
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

// totalBlocksInTier sums floor((idleInHyperNode[h] + freedByHyperNode[h]) / size) over the
// target tier's HyperNodes; nodes outside the tier contribute 0.
func totalBlocksInTier(idleInHyperNode, freedByHyperNode map[string]int, hyperNodesInTier []string, size int) int {
	if size < 1 {
		size = 1
	}
	total := 0
	for _, hyperNode := range hyperNodesInTier {
		total += (idleInHyperNode[hyperNode] + freedByHyperNode[hyperNode]) / size
	}
	return total
}

// ---- node-block progress score ----

func (p *networkTopologyAwarePlugin) registerNodeBlockProgressScore(ssn *framework.Session, blockSession *nodeBlockSession) {
	ssn.AddPlanScoreFn("nodeBlockProgress", p.progressWeight, func(_ *api.PlanContext, plan *api.CandidatePlan) int64 {
		anchor := plan.IncrementalFromNodes()
		if len(anchor) == 0 {
			return 0
		}
		// Single-node unit: the anchor is the unique node freed by this candidate.
		hyperNode, ok := blockSession.nodeToHyperNode[anchor[0]]
		if !ok {
			return 0 // node belongs to no HyperNode at the target tier: least preferred
		}
		freedCount := freedInHyperNode(plan.FreedNodes(), hyperNode, blockSession.nodeToHyperNode)
		return nodeBlockProgressScore(
			blockSession.idleInHyperNode[hyperNode]+freedCount, // freeInHyperNode: idle + plan-freed (incl. this candidate)
			blockSession.busyInHyperNode[hyperNode]-freedCount, // freeableInHyperNode: still drainable
			blockSession.size,
		)
	})
}

// ---- node-block distribution score ----

func (p *networkTopologyAwarePlugin) registerNodeBlockDistributionScore(ssn *framework.Session, blockSession *nodeBlockSession) {
	ssn.AddPlanScoreFn("nodeBlockDistribution", p.distributionWeight, func(_ *api.PlanContext, plan *api.CandidatePlan) int64 {
		anchor := plan.IncrementalFromNodes()
		if len(anchor) == 0 {
			return 0
		}
		hyperNode, ok := blockSession.nodeToHyperNode[anchor[0]]
		if !ok {
			// No H: least-preferred value for the mode.
			return nodeBlockDistributionScore(blockSession.mode, false, 0, blockSession.maxBlocksInHyperNode)
		}
		freedCount := freedInHyperNode(plan.FreedNodes(), hyperNode, blockSession.nodeToHyperNode)
		blocks := (blockSession.idleInHyperNode[hyperNode] + freedCount) / blockSession.size
		return nodeBlockDistributionScore(blockSession.mode, true, blocks, blockSession.maxBlocksInHyperNode)
	})
}

// ---- block-count admission (hard gate) ----

func (p *networkTopologyAwarePlugin) registerBlockCountConstraint(ssn *framework.Session, blockSession *nodeBlockSession) {
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	ssn.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) (bool, string) {
		// requiredBlocks==0 (default): always admit — pure soft guidance, and skips
		// the nil-plan guard (PlanAdmissible only evaluates non-nil plans today).
		if blockSession.requiredBlocks == 0 {
			return true, ""
		}
		if plan == nil {
			return false, ""
		}
		freedByHyperNode := make(map[string]int, len(blockSession.hyperNodesInTier))
		for _, node := range plan.FreedNodes {
			if hyperNode, ok := blockSession.nodeToHyperNode[node]; ok {
				freedByHyperNode[hyperNode]++
			}
		}
		total := totalBlocksInTier(blockSession.idleInHyperNode, freedByHyperNode, blockSession.hyperNodesInTier, blockSession.size)
		admitted := total >= blockSession.requiredBlocks
		klog.V(4).InfoS("repack networktopologyaware: block-count gate", "run", runName,
			"requiredBlocks", blockSession.requiredBlocks, "blockSize", blockSession.size,
			"freedNodeCount", len(plan.FreedNodes), "freedByHyperNode", freedByHyperNode,
			"completeBlocks", total, "admitted", admitted)
		if admitted {
			return true, ""
		}
		// Report the block-specific reason, not the fragmentation-improvement one.
		return false, state.ReasonRequiredNodeBlocksNotMet
	})
}

// ---- node-block receiver preference (receiver steering) ----

// registerNodeBlockReceiverPreference steers relocated pods away from the target
// tier's HyperNodes, closing the receiver side of block shaping. Per receiver it
// prefers no-HyperNode ({3}) > another HyperNode ({2}) > own HyperNode ({1}), and
// abstains ({}) when the candidate frees no node.
//
// It only reorders the receiver list — firstFeasibleReceiver still takes the first
// feasible receiver — so it adds no infeasibility and the block-count gate (which
// counts freed nodes, not destinations) is unaffected. Registering in the Topology
// phase keeps the stability policies (staysOccupied: filled/immovable/scope-excluded/
// stuck receivers) ahead of this key; filling those never hurts the block pool since
// they could not be drained anyway. Registering unconditionally preserves block
// progress independently of the binpack/spread mode.
func (p *networkTopologyAwarePlugin) registerNodeBlockReceiverPreference(ssn *framework.Session, blockSession *nodeBlockSession) {
	ssn.AddReceiverPreferenceFn("nodeBlockPreserve", framework.ReceiverPreferencePhaseTopology,
		func(_ *api.PlanContext, candidate *framework.PlanningCandidate, receiver *framework.ReceiverCandidate) framework.ReceiverPreference {
			anchors := candidate.Plan.IncrementalFromNodes()
			if len(anchors) == 0 {
				return framework.ReceiverPreference{} // no anchor: abstain, let later keys decide
			}
			// Preserve every anchor HyperNode (a single-node unit collapses this to
			// one; set form kept for generality).
			ownHs := make(map[string]bool, len(anchors))
			for _, n := range anchors {
				if h, ok := blockSession.nodeToHyperNode[n]; ok {
					ownHs[h] = true
				}
			}
			receiverHyperNode, inTier := blockSession.nodeToHyperNode[receiver.Node.Name]
			switch {
			case !inTier:
				return framework.ReceiverPreference{3} // export the load outside the tier
			case ownHs[receiverHyperNode]:
				return framework.ReceiverPreference{1} // own HyperNode: last resort
			default:
				return framework.ReceiverPreference{2} // another HyperNode
			}
		})
}

func (*networkTopologyAwarePlugin) OnSessionClose(*framework.Session) {}
