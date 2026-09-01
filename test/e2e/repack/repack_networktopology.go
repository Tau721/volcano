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

// HyperNode-aware repack e2e scenarios E1-E7 (docs/design/repack-hypernode-aware.md
// §5.1.2). A real HyperNode tree is built over the kind worker nodes (route B:
// real nodes carrying the fake NPU resource), and each spec drives a RepackRun
// with spec.networkTopology to verify the block-shaped defragmentation contract:
// E1/E7 Execute main path + post-repack scheduling, E2/E5 the two rejection
// gates (blocks-infeasible, frag-improvement), E3 the R1 no-op without the
// field, E4 the R16 apiserver CEL/enum/minimum rejection, E6 the spread-mode
// preference for HyperNode members over unmanaged nodes. A second Describe at
// the bottom (custom tree) hosts E-RS, the §4.1.3.4 receiver steering: the
// drained pod must land on a no-H receiver rather than the tight own-H receiver
// (which the standard 4-node tree cannot express).
package repack

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// setupRepackTopology builds the shared HyperNode tree over the given worker
// nodes:
//
//	                rt-s3 (tier 3: {nodes[0], nodes[1]})
//	                |
//	           rt-s2 (tier 2: {rt-s0, rt-s1})
//	          /                          \
//	rt-s0 (tier 1: {n0, n1})       rt-s1 (tier 1: {n2, n3})
//
// Tier 1 covers all four nodes (E1/E2/E5/E7 target it). Tier 3 is deliberately
// partial — only nodes[0], nodes[1] belong to a HyperNode at that tier — which
// is exactly the E6 scenario (target tier partly unmanaged). Each spec builds its
// own copy in BeforeEach; the engine cache syncs it during the occupy cycle that
// precedes every repack run, so the session always sees a settled tree.
func setupRepackTopology(ctx *e2eutil.TestContext, nodes []string) {
	Expect(len(nodes)).To(BeNumerically(">=", 4), "the HyperNode tree needs 4 worker nodes")
	hyperNodes := []struct {
		name         string
		members      []string
		tier         int
		memberIsNode bool
	}{
		{"rt-s0", []string{nodes[0], nodes[1]}, 1, true},
		{"rt-s1", []string{nodes[2], nodes[3]}, 1, true},
		{"rt-s2", []string{"rt-s0", "rt-s1"}, 2, false},
		{"rt-s3", []string{nodes[0], nodes[1]}, 3, true},
	}
	for _, hn := range hyperNodes {
		// Leaves reference real nodes directly; the tier-2 HyperNode references
		// its children by name (same shape as gangevict.setupTopoHyperNodes).
		memberType := topologyv1alpha1.MemberTypeHyperNode
		if hn.memberIsNode {
			memberType = topologyv1alpha1.MemberTypeNode
		}
		spec := &topologyv1alpha1.HyperNode{
			ObjectMeta: metav1.ObjectMeta{Name: hn.name},
			Spec:       topologyv1alpha1.HyperNodeSpec{Tier: hn.tier},
		}
		for _, member := range hn.members {
			spec.Spec.Members = append(spec.Spec.Members, topologyv1alpha1.MemberSpec{
				Type:     memberType,
				Selector: topologyv1alpha1.MemberSelector{ExactMatch: &topologyv1alpha1.ExactMatch{Name: member}},
			})
		}
		Expect(e2eutil.SetupHyperNode(ctx, spec)).NotTo(HaveOccurred(), "create HyperNode %s", hn.name)
	}

	// Poll the apiserver so the created HyperNodes are durably readable before
	// any spec submits a RepackRun against them. The engine informer picks them
	// up within its sync period; every spec performs a full occupy+wait cycle
	// before its run is processed, so the cache is settled by the time a session
	// opens (design §5.1.2: poll HyperNodes().Get after creating).
	for _, hn := range hyperNodes {
		name := hn.name
		Eventually(func() error {
			_, err := ctx.Vcclient.TopologyV1alpha1().HyperNodes().Get(context.TODO(), name, metav1.GetOptions{})
			return err
		}, 30*time.Second, time.Second).Should(BeNil(), "HyperNode %s must be readable", name)
	}
}

// tierNodeToHyperNode indexes real node -> HyperNode for a single tier by
// listing all HyperNodes and expanding their members. A node belongs to at most
// one HyperNode at a tier; the first hit wins (mirrors the plugin's R5 guard).
func tierNodeToHyperNode(ctx *e2eutil.TestContext, tier int) map[string]string {
	hyperNodes, err := ctx.Vcclient.TopologyV1alpha1().HyperNodes().List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	nodeToH := make(map[string]string)
	for i := range hyperNodes.Items {
		hn := &hyperNodes.Items[i]
		if hn.Spec.Tier != tier {
			continue
		}
		for _, node := range e2eutil.GetNodesOfHyperNode(ctx, hn, nil) {
			if _, taken := nodeToH[node]; !taken {
				nodeToH[node] = hn.Name
			}
		}
	}
	return nodeToH
}

// freedBlocksAtTier counts the complete blocks (floor(freed-in-H / size)) the
// plan freed within HyperNodes of the target tier — the observable counterpart
// of the plugin's totalBlocksInTier, read back from the live cluster.
func freedBlocksAtTier(ctx *e2eutil.TestContext, freed []string, tier, size int) int {
	nodeToH := tierNodeToHyperNode(ctx, tier)
	freedInH := make(map[string]int)
	for _, node := range freed {
		if hn := nodeToH[node]; hn != "" {
			freedInH[hn]++
		}
	}
	total := 0
	for _, count := range freedInH {
		total += count / size
	}
	return total
}

// invalidTopologies enumerates the R16 violations the real apiserver must reject
// at creation (design §5.1.2 E4). nodeBlockSize is a pointer (omitempty) with
// default=1, so an explicit 0 is distinguishable from "absent" and reaches the
// minimum:1 rule — both 0 and -1 are rejected; omitting the field applies the
// default 1 and is valid.
var invalidTopologies = []struct {
	name string
	topo *repackv1alpha1.NetworkTopology
}{
	{
		name: "both hyperNodeTier and hyperNodeTierName set (CEL)",
		topo: &repackv1alpha1.NetworkTopology{
			HyperNodeTier:      ptr.To(1),
			HyperNodeTierName:  ptr.To("rt-s0"),
			NodeBlockSize:      ptr.To(2),
			RequiredNodeBlocks: 1,
		},
	},
	{
		name: "neither hyperNodeTier nor hyperNodeTierName set (CEL)",
		topo: &repackv1alpha1.NetworkTopology{
			NodeBlockSize:      ptr.To(2),
			RequiredNodeBlocks: 1,
		},
	},
	{
		name: "mode outside the binpack/spread enum",
		topo: &repackv1alpha1.NetworkTopology{
			HyperNodeTier:      ptr.To(1),
			NodeBlockSize:      ptr.To(2),
			RequiredNodeBlocks: 1,
			Mode:               repackv1alpha1.RepackBlockMode("bogus"),
		},
	},
	{
		name: "nodeBlockSize below the minimum of 1",
		topo: &repackv1alpha1.NetworkTopology{
			HyperNodeTier:      ptr.To(1),
			NodeBlockSize:      ptr.To(-1),
			RequiredNodeBlocks: 1,
		},
	},
	{
		name: "nodeBlockSize explicit 0 (minimum of 1)",
		topo: &repackv1alpha1.NetworkTopology{
			HyperNodeTier:      ptr.To(1),
			NodeBlockSize:      ptr.To(0),
			RequiredNodeBlocks: 1,
		},
	},
	{
		name: "requiredNodeBlocks below the minimum of 0",
		topo: &repackv1alpha1.NetworkTopology{
			HyperNodeTier:      ptr.To(1),
			NodeBlockSize:      ptr.To(2),
			RequiredNodeBlocks: -1,
		},
	},
}

// These tests require the repack CRDs, the volcano-repack-engine (helm
// custom.repack_enable=true), and HyperNode CRDs in the cluster.
var _ = Describe("Repack HyperNode-aware network topology", Serial, func() {
	var ctx *e2eutil.TestContext // per-spec context (namespace workload isolation)
	var nodes []string           // worker node names the H-tree is built over

	// Each spec builds its OWN H-tree in BeforeEach: e2eutil.CleanupTestContext
	// wipes ALL HyperNodes cluster-wide (CleanupHyperNodes -> DeleteCollection),
	// so a BeforeAll-shared tree would be destroyed by the first spec's AfterEach
	// and every later spec would silently degrade to node-level planning.
	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 4) // advertise fake NPUs on the same 4 worker nodes
		setupRepackTopology(ctx, nodes)
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx) // also wipes this spec's HyperNodes
		for _, n := range nodes {
			clearNPU(ctx, n)
		}
	})

	Context("E1: block-shaped Execute main path (US-01)", func() {
		It("frees a complete block in the target tier and realizes the plan", func() {
			// 4 fragmented nodes x 1 card -> Execute must consolidate onto one
			// node, freeing 3 nodes that form >= 1 block of size 2 in tier 1
			// (requiredNodeBlocks=1). Which 3 nodes is the receiver-tie dependent,
			// but 3 nodes and >= 1 complete block hold in every outcome.
			for i := 0; i < 4; i++ {
				occupyMovableVCJob(ctx, fmt.Sprintf("e1-w%d", i), nodes[i], 1)
			}

			run, err := newRun("e1", repackv1alpha1.RepackModeExecute).
				goal(npuResource).
				networkTopology(&repackv1alpha1.NetworkTopology{
					HyperNodeTier:      ptr.To(1),
					NodeBlockSize:      ptr.To(2),
					RequiredNodeBlocks: 1,
					Mode:               repackv1alpha1.RepackBlockModeBinpack,
				}).
				create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
			Expect(got.Status.Plan).NotTo(BeNil())
			Expect(len(got.Status.Plan.FreedNodes)).To(Equal(3), "4x1-card consolidates onto one receiver")
			Expect(freedBlocksAtTier(ctx, got.Status.Plan.FreedNodes, 1, 2)).
				To(BeNumerically(">=", 1), "the plan must free >= requiredNodeBlocks=1 block in tier 1")
			// Execute must realize exactly the planned freed set. ConsistOf needs
			// []any, so widen the plan slice in place.
			Expect(got.Status.Result).NotTo(BeNil())
			planFreed := make([]any, len(got.Status.Plan.FreedNodes))
			for i, node := range got.Status.Plan.FreedNodes {
				planFreed[i] = node
			}
			Expect(got.Status.Result.FreedNodes).To(ConsistOf(planFreed...))
			Expect(got.Status.Result.FreedNodeCount).To(Equal(int32(3)))
		})
	})

	Context("E2: required blocks unmet -> no defragmentation (R10 reject, R11)", func() {
		It("leaves the cluster untouched when the block target is infeasible", func() {
			// Movable workloads (no spec.nodeName), so the planner produces a
			// plan freeing nodes; the block-count gate must reject it.
			for i := 0; i < 4; i++ {
				occupyMovableVCJob(ctx, fmt.Sprintf("e2-w%d", i), nodes[i], 1)
			}
			before := runningPodCount(ctx)

			// requiredNodeBlocks=3 with size 2: tier 1 has exactly 2 HyperNodes,
			// each capable of at most one complete block -> max 2 < 3, infeasible.
			run, err := newRun("e2", repackv1alpha1.RepackModeDryRun).
				goal(npuResource).
				networkTopology(&repackv1alpha1.NetworkTopology{
					HyperNodeTier:      ptr.To(1),
					NodeBlockSize:      ptr.To(2),
					RequiredNodeBlocks: 3,
				}).
				create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			// The block-count gate (R10) rejects with its own reason, not the
			// fragmentation-improvement InsufficientImprovement (design §4.1.3.3).
			Expect(completeReason(got)).To(Equal("RequiredNodeBlocksNotMet"))
			Expect(got.Status.Plan).NotTo(BeNil())
			Expect(len(got.Status.Plan.FreedNodes)).To(Equal(0), "no block can be formed -> nothing freed")
			Expect(len(got.Status.Plan.Moves)).To(Equal(0), "no migration may be planned")
			Expect(runningPodCount(ctx)).To(Equal(before), "DryRun must not evict")
		})
	})

	Context("E3: no networkTopology -> node-level semantics (R1)", func() {
		It("defragments at node granularity when networkTopology is absent", func() {
			for i := 0; i < 4; i++ {
				occupy(ctx, fmt.Sprintf("e3-w%d", i), nodes[i], 1)
			}

			// The same fragmented 4x1-card cluster as E1/E2, but WITHOUT
			// networkTopology: the block callbacks must not participate and the
			// run degrades to the node-level plan.
			run, err := newRun("e3", repackv1alpha1.RepackModeDryRun).
				goal(npuResource).
				create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("RepackRecommended"))
			Expect(got.Status.Plan).NotTo(BeNil())
			Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeNumerically(">=", 1))
			Expect(len(got.Status.Plan.Moves)).To(BeNumerically(">=", 1), "node-level repack still recommends moves")
		})
	})

	Context("E4: invalid networkTopology is rejected at admission (R16)", func() {
		It("rejects each invalid configuration at Create", func() {
			// Admission is apiserver-side (CEL enum + minimums), so these runs are
			// never persisted and no engine involvement is exercised.
			for _, tc := range invalidTopologies {
				tc := tc
				_, err := newRun("e4-invalid", repackv1alpha1.RepackModeDryRun).
					goal(npuResource).
					networkTopology(tc.topo).
					create(ctx)
				Expect(err).To(HaveOccurred(), "creation with %s must be rejected by the apiserver", tc.name)
			}
		})
	})

	Context("E5: blocks OK but fragmentation improvement below gate (R11)", func() {
		It("rejects a plan whose block target is met but frag improvement is not", func() {
			for i := 0; i < 4; i++ {
				occupy(ctx, fmt.Sprintf("e5-w%d", i), nodes[i], 1)
			}
			before := runningPodCount(ctx)

			// 4x1-card -> 3 freed nodes is only 75pp fragmentation improvement,
			// below the 100pp gate, even though a complete block IS available.
			// The AND aggregation (block gate AND frag gate) must reject.
			run, err := newRun("e5", repackv1alpha1.RepackModeDryRun).
				goalWithMinFragImprovement(npuResource, 100).
				networkTopology(&repackv1alpha1.NetworkTopology{
					HyperNodeTier:      ptr.To(1),
					NodeBlockSize:      ptr.To(2),
					RequiredNodeBlocks: 1,
					Mode:               repackv1alpha1.RepackBlockModeBinpack,
				}).
				create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("InsufficientImprovement"))
			Expect(got.Status.Plan).NotTo(BeNil())
			Expect(len(got.Status.Plan.FreedNodes)).To(Equal(0))
			Expect(len(got.Status.Plan.Moves)).To(Equal(0))
			Expect(runningPodCount(ctx)).To(Equal(before), "DryRun must not evict")
		})
	})

	Context("E6: spread prefers HyperNode members over unmanaged nodes (R6)", func() {
		It("frees the tier-3 HyperNode members, not the unmanaged nodes", func() {
			// Deterministic partial-H layout: tier-3 HyperNode rt-s3 =
			// {nodes[0], nodes[1]}; nodes[2], nodes[3] belong to NO tier-3
			// HyperNode. Cards 4/5/3/3. In spread mode the H members score far
			// above the unmanaged nodes (block progress 1e6 vs 0, distribution
			// -0 vs -blocksInHMax), so the greedy planner frees exactly
			// {nodes[0], nodes[1]}: whatever the first receiver tie (nodes[2] or
			// nodes[3]), the second H member then absorbs the remaining slack and
			// the unmanaged victims are left with no receiver -> stuck.
			occupy(ctx, "e6-w0", nodes[0], 4)
			occupy(ctx, "e6-w1", nodes[1], 5)
			occupy(ctx, "e6-w2", nodes[2], 3)
			occupy(ctx, "e6-w3", nodes[3], 3)

			run, err := newRun("e6", repackv1alpha1.RepackModeDryRun).
				goal(npuResource).
				networkTopology(&repackv1alpha1.NetworkTopology{
					HyperNodeTier:      ptr.To(3),
					NodeBlockSize:      ptr.To(2),
					RequiredNodeBlocks: 1,
					Mode:               repackv1alpha1.RepackBlockModeSpread,
				}).
				create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("RepackRecommended"))
			Expect(got.Status.Plan).NotTo(BeNil())
			// Exactly the two HyperNode members are freed (order-insensitive).
			Expect(got.Status.Plan.FreedNodes).To(ConsistOf(nodes[0], nodes[1]))
			Expect(freedBlocksAtTier(ctx, got.Status.Plan.FreedNodes, 3, 2)).
				To(BeNumerically(">=", 1), "the two freed members form one tier-3 block")
		})
	})

	Context("E7: a topology-constrained job becomes schedulable after repack (US-01)", func() {
		It("schedules an 8-card hard-topology job onto a freed node", func() {
			for i := 0; i < 4; i++ {
				occupyMovableVCJob(ctx, fmt.Sprintf("e7-w%d", i), nodes[i], 1)
			}

			run, err := newRun("e7", repackv1alpha1.RepackModeExecute).
				goal(npuResource).
				networkTopology(&repackv1alpha1.NetworkTopology{
					HyperNodeTier:      ptr.To(1),
					NodeBlockSize:      ptr.To(2),
					RequiredNodeBlocks: 1,
					Mode:               repackv1alpha1.RepackBlockModeBinpack,
				}).
				create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
			Expect(got.Status.Plan).NotTo(BeNil())
			Expect(freedBlocksAtTier(ctx, got.Status.Plan.FreedNodes, 1, 2)).
				To(BeNumerically(">=", 1))
			Expect(got.Status.Result).NotTo(BeNil())
			freed := sets.New[string](got.Status.Result.FreedNodes...)
			Expect(freed.Len()).To(Equal(3))

			// A job requesting a full node (8 cards) with a hard tier-1 topology
			// constraint can only land on a node the run freed: only freed nodes
			// have 8 free cards (the consolidated receiver keeps 4/8 used), and
			// every node is a tier-1 member so the hard-topology constraint is
			// satisfiable. The job must therefore move Pending -> Running onto
			// repacked space — the direct measure of the US-01 acceptance.
			quantity := resource.MustParse("8")
			topoJob := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
				Name:      "e7-topo",
				Namespace: ctx.Namespace,
				Min:       1,
				NetworkTopology: &batchv1alpha1.NetworkTopologySpec{
					Mode:               batchv1alpha1.HardNetworkTopologyMode,
					HighestTierAllowed: ptr.To(1),
				},
				Tasks: []e2eutil.TaskSpec{{
					Name: "t", Min: 1, Rep: 1, Img: e2eutil.DefaultNginxImage,
					Req:   v1.ResourceList{npuResource: quantity},
					Limit: v1.ResourceList{npuResource: quantity},
				}},
			})
			Expect(e2eutil.WaitTasksReady(ctx, topoJob, 1)).NotTo(HaveOccurred(), "topology job must become Running")

			pods := e2eutil.GetTasksOfJob(ctx, topoJob)
			Expect(pods).To(HaveLen(1))
			Expect(pods[0].Spec.NodeName).NotTo(BeEmpty())
			Expect(freed.Has(pods[0].Spec.NodeName)).To(BeTrue(),
				"topology job pod %q must land on a freed node, got %q (freed: %v)",
				pods[0].Name, pods[0].Spec.NodeName, got.Status.Result.FreedNodes)
		})
	})
})

// E-RS: receiver steering (design §4.1.3.4). US-01's block shaping constrains
// which node to free (candidate scoring + block-count gate) but, before this
// enhancement, NOT where the freed pod lands: the receiver choice was
// block-agnostic, so the drained pod could fall back onto the same HyperNode's
// other Partial node (filling it Full, shrinking that H's future freeable
// pool) or into another HyperNode. nodeBlockPreserve now steers relocations
// away from the target tier's HyperNodes: no-H ({3}) > other-H ({2}) > own-H
// ({1}), decided at the Topology key before bestFit. This spec needs a tree
// the shared 4-node setup cannot express (a no-H receiver alongside an
// own-H receiver), so it builds its own.
var _ = Describe("Repack HyperNode-aware receiver steering (custom tree)", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 4)
		// One tier-1 HyperNode over nodes[0..2]; nodes[3] belongs to no
		// tier-1 HyperNode -> the no-H receiver the standard tree cannot give.
		setupRepackTopologyCustom(ctx, []hyperNodeFixture{
			{name: "e8-hna", tier: 1, members: []string{nodes[0], nodes[1], nodes[2]}, memberIsNode: true},
		})
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx) // also wipes this spec's HyperNodes
		for _, n := range nodes {
			clearNPU(ctx, n)
		}
	})

	Context("E-RS: drained pod steers to the no-H receiver, not the own-H one (US-01)", func() {
		It("relocates onto the no-H node while the tight own-H receiver stays Partial", func() {
			// Layout (tier 1, size 2, requiredNodeBlocks 1):
			//   nodes[0] (a1): 1 movable card  -> the only feasible victim
			//   nodes[1] (a2): 7 movable cards -> own-H receiver, slack=1
			//   nodes[2] (a3): idle            -> completes the block with a1
			//   nodes[3] (out1): 1 movable card -> no-H receiver, slack=7
			//
			// maxPerRun.resources[npu]=1 prunes a2 (7 cards > 1) and out1's
			// later drain (cumulative 2 > 1), so exactly a1 drains: idle a3 +
			// freed a1 = 2 -> 1 complete block (the R10 gate counts idleInH,
			// so a3's pre-existing idle completes the block; freedBlocksAtTier
			// over FreedNodes alone would read 0 and is deliberately not
			// asserted here). The drained pod must then land on out1 (no-H,
			// {3}), NOT a2 (own-H, {1}) — bestFit would reverse that (a2
			// slack=1 -> {-1} > out1 slack=7 -> {-7}), so this is decisive
			// proof the Topology-key preference steers the relocation.
			occupyMovableVCJob(ctx, "e8-a1", nodes[0], 1)
			occupyMovableVCJob(ctx, "e8-a2", nodes[1], 7)
			occupyMovableVCJob(ctx, "e8-out1", nodes[3], 1)
			// nodes[2] deliberately left idle.

			run, err := newRun("e8-rs", repackv1alpha1.RepackModeDryRun).
				goal(npuResource).
				networkTopology(&repackv1alpha1.NetworkTopology{
					HyperNodeTier:      ptr.To(1),
					NodeBlockSize:      ptr.To(2),
					RequiredNodeBlocks: 1,
					Mode:               repackv1alpha1.RepackBlockModeBinpack,
				}).
				maxPerRun(&repackv1alpha1.MaxPerRun{
					Resources: v1.ResourceList{npuResource: resource.MustParse("1")},
				}).
				create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("RepackRecommended"))
			Expect(got.Status.Plan).NotTo(BeNil())

			// Exactly a1 drains (budget-pruned a2, and out1 scores 0 as a no-H
			// victim): the own-H tight receiver a2 must survive untouched.
			Expect(got.Status.Plan.FreedNodes).To(Equal([]string{nodes[0]}),
				"only a1 (nodes[0]) may be freed; the own-H receiver a2 must stay Partial")
			Expect(sets.New[string](got.Status.Plan.FreedNodes...).Has(nodes[1])).To(BeFalse(),
				"the tight own-H receiver a2 must not be drained")

			// The single relocation's plan-time target is the no-H receiver.
			Expect(len(got.Status.Plan.Moves)).To(Equal(1))
			Expect(len(got.Status.Plan.Moves[0].Pods)).To(Equal(1))
			Expect(got.Status.Plan.Moves[0].Pods[0].ToNode).To(Equal(nodes[3]),
				"the drained pod must be steered to the no-H receiver out1, not the own-H receiver a2")
		})
	})
})
