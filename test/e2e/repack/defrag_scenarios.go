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

package repack

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// This suite is the positive-path regression net for the core defragmentation
// capability: a battery of distinct fragmentation shapes that must each yield a
// real consolidation plan. Every case is DryRun, so it is fully deterministic
// (nothing is evicted) and exercises the whole planning pipeline
// — fragmentation measurement, gang-aware drain, reschedulability feasibility,
// best-fit receiver selection and scoring.
//
// Each scenario advertises the fake NPU on len(cardsPerNode) worker nodes and
// places one single-pod gang of the given card count deterministically on each
// non-zero node (a 0 entry is an advertised-but-empty node, used to check empty
// nodes are excluded as receivers). Fixtures must be movable — never vcjob
// templates that persist spec.nodeName, which the engine treats as pinned and
// refuses to drain — so the fragmented gangs can actually consolidate.
// expectedFreed is the optimal number of nodes the plan should empty; the greedy
// drain reaches the optimum for these shapes, so we assert FreedNodeCount >= it.
var _ = Describe("Repack defragmentation scenarios (DryRun consolidation)", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = nil
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, node := range nodes {
			clearNPU(ctx, node)
		}
	})

	type scenario struct {
		name          string
		cardsPerNode  []int
		expectedFreed int32
	}
	scenarios := []scenario{
		// Two half-used nodes collapse into one.
		{"two-halves-into-one", []int{4, 2}, 1},
		// An empty advertised node must NOT be used as a receiver; the fragmented
		// pair still collapses among themselves, and the empty node stays empty.
		{"empty-node-not-a-receiver", []int{4, 2, 0}, 1},
		// Four scattered small gangs pack onto a single node, freeing three.
		{"four-small-into-one", []int{2, 2, 2, 2}, 3},
		// Three gangs whose sum fits one node.
		{"three-into-one", []int{3, 3, 2}, 2},
		// A near-full node is the natural receiver; the two tiny gangs drain onto it.
		{"near-full-receiver", []int{6, 1, 1}, 2},
		// Best-fit: 5 + 3 exactly fills one node.
		{"best-fit-pair", []int{5, 3}, 1},
		// A large gang plus a tiny one collapse into a single full node.
		{"big-plus-tiny", []int{7, 1}, 1},
		// Partial consolidation: 5+3 pack together, 4 stays alone -> free one node.
		{"partial-two-bins", []int{5, 4, 3}, 1},
		// A full node can be neither donor nor receiver; the fragments around it
		// still consolidate to free one node.
		{"full-node-stays-put", []int{8, 3, 1}, 1},
	}

	for _, sc := range scenarios {
		sc := sc // capture per iteration
		It("consolidates a "+sc.name+" layout", func() {
			nodes = npuFixture(ctx, len(sc.cardsPerNode))

			occupied := map[string]bool{}
			for i, cards := range sc.cardsPerNode {
				if cards > 0 {
					// The consolidation fixtures must be movable: the engine treats
					// vcjob templates that persist spec.nodeName as pinned and never
					// drains them (the controller would recreate a replacement on the
					// same node), so a nodeName-pinned layout can never consolidate.
					occupyMovableVCJob(ctx, fmt.Sprintf("%s-%d", sc.name, i), nodes[i], cards)
					occupied[nodes[i]] = true
				}
			}
			runningBefore := runningPodCount(ctx)

			run, err := newRun("defrag", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer deleteRun(ctx, run.Name)

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("RepackRecommended"), "a fragmented cluster must recommend consolidation")
			Expect(got.Status.Plan).NotTo(BeNil())

			summary := got.Status.Plan.Summary
			Expect(summary.FragBeforePercent).To(BeNumerically(">", 0), "the layout is fragmented")
			Expect(summary.FragAfterPercent).To(BeNumerically("<", summary.FragBeforePercent), "consolidation must reduce fragmentation")
			Expect(summary.FreedNodeCount).To(BeNumerically(">=", sc.expectedFreed), "should free the expected number of nodes")
			Expect(len(got.Status.Plan.Moves)).To(BeNumerically(">=", 1), "consolidation implies at least one move")

			// A freed node must have actually hosted workload (never the empty spare).
			for _, freedNode := range got.Status.Plan.FreedNodes {
				Expect(occupied).To(HaveKey(freedNode), "a freed node must have hosted a gang")
			}

			// DryRun is read-only: no eviction, no relocations.
			Expect(got.Status.Relocations).To(BeEmpty(), "DryRun must not nominate")
			Expect(runningPodCount(ctx)).To(Equal(runningBefore), "DryRun must not evict pods")
		})
	}

	// Historical regression: both directions free one node, but draining the
	// lightly used node moves one PodGroup while draining the packed node moves
	// three. The planner must preserve the lower-disruption B -> A direction.
	It("drains one lightly used PodGroup instead of three packed PodGroups", func() {
		nodes = npuFixture(ctx, 3)
		packedWorkloads := []*nativeWorkload{
			occupyNativeDeployment(ctx, "direction-packed-1", nodes[0], "packed", 2),
			occupyNativeDeployment(ctx, "direction-packed-2", nodes[0], "packed", 2),
			occupyNativeDeployment(ctx, "direction-packed-3", nodes[0], "packed", 2),
		}
		lightWorkload := occupyNativeDeployment(ctx, "direction-light", nodes[1], "light", 2)
		allWorkloads := append(append([]*nativeWorkload{}, packedWorkloads...), lightWorkload)
		defer deleteNativeWorkloads(ctx, allWorkloads...)

		for _, workload := range allWorkloads {
			podGroup, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Get(
				context.TODO(), workload.podGroup, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(podGroup.Spec.MinMember).To(BeEquivalentTo(1),
				"the regression layout requires each workload to remain an independent minAvailable=1 PodGroup")
		}

		scope := &repackv1alpha1.RepackScope{
			Nodes: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{Names: []string{nodes[0], nodes[1]}},
			},
		}
		run, err := newRun("direction-regression", repackv1alpha1.RepackModeDryRun).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan.FreedNodes).To(Equal([]string{nodes[1]}),
			"the lightly used node must be the exact drain target")
		Expect(got.Status.Plan.Moves).To(HaveLen(1),
			"lower disruption means moving only the single PodGroup from the lightly used node")
		Expect(got.Status.Plan.Moves[0].PodGroupName).To(Equal(lightWorkload.podGroup))
		Expect(got.Status.Plan.Moves[0].Pods).To(HaveLen(1))
		Expect(got.Status.Plan.Moves[0].Pods[0].FromNode).To(Equal(nodes[1]))
		Expect(got.Status.Plan.Moves[0].Pods[0].ToNode).To(Equal(nodes[0]))
	})
})
