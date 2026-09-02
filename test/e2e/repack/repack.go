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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// These tests require the repack CRDs and the volcano-repack-engine running
// (helm custom.repack_enable=true, run via E2E_TYPE=REPACK).

var _ = Describe("Repack DryRun & admission", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 3) // advertise fake NPUs on 3 worker nodes
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, n := range nodes {
			clearNPU(ctx, n)
			clearResource(ctx, n, altNPUResource)
		}
	})

	// B1/B2/B3: fragmented cluster -> DryRun recommends consolidation, reports a
	// plan with moves + freed nodes + reduced fragmentation, and evicts nothing.
	It("recommends consolidation on a fragmented cluster (no eviction)", func() {
		// Movable fixtures: a nodeName-pinned vcjob template is immovable, and a
		// fragmented cluster whose gangs cannot drain can never recommend.
		occupyMovableVCJob(ctx, "frag-a", nodes[0], 4) // node0: 4/8
		occupyMovableVCJob(ctx, "frag-b", nodes[1], 2) // node1: 2/8 (node2 empty)
		before := runningPodCount(ctx)

		run, err := newRun("dryrun-recommend", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeNumerically(">=", 1))
		Expect(got.Status.Plan.Summary.FragAfterPercent).To(BeNumerically("<", got.Status.Plan.Summary.FragBeforePercent))
		Expect(got.Status.Plan.Summary.ResolvedScope).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.ResolvedScope.NodeCount).To(BeEquivalentTo(3))
		Expect(got.Status.Plan.Summary.ResolvedScope.PodGroupCount).To(BeNumerically(">=", 2))
		Expect(len(got.Status.Plan.Moves)).To(BeNumerically(">=", 1))
		// DryRun must not evict or nominate.
		Expect(got.Status.Relocations).To(BeEmpty())
		Expect(runningPodCount(ctx)).To(Equal(before), "DryRun must not evict pods")
		// Moves carry per-pod from/to and cards.
		mv := got.Status.Plan.Moves[0]
		Expect(mv.PodGroupName).NotTo(BeEmpty())
		Expect(len(mv.Pods)).To(BeNumerically(">=", 1))
		Expect(mv.Pods[0].ToNode).NotTo(BeEmpty())
	})

	// B4: an already-optimal (single occupied node) cluster has no fragmentation.
	It("reports NoFragmentation on a clean cluster", func() {
		occupy(ctx, "clean", nodes[0], 4) // only one node occupied -> optimal

		run, err := newRun("dryrun-clean", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("NoFragmentation"))
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeEquivalentTo(0))
		Expect(got.Status.Plan.Summary.ResolvedScope).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.ResolvedScope.NodeCount).To(BeEquivalentTo(3))
		Expect(got.Status.Plan.Summary.ResolvedScope.PodGroupCount).To(BeEquivalentTo(1))
		Expect(got.Status.Plan.Moves).To(BeEmpty())
	})

	// B5: fragmented but nothing in scope is movable -> InsufficientImprovement (the
	// "fragmented but no worthwhile plan" case), with before==after > 0.
	It("reports InsufficientImprovement when fragmented but nothing is movable", func() {
		occupy(ctx, "stuck-a", nodes[0], 4)
		occupy(ctx, "stuck-b", nodes[1], 2)

		// scope includes only a non-existent PodGroup -> no gang is movable.
		scope := &repackv1alpha1.RepackScope{
			PodGroups: &repackv1alpha1.RepackSelectorTerm{
				Include: &repackv1alpha1.RepackSelector{Names: []string{ctx.Namespace + "/does-not-exist"}},
			},
		}
		run, err := newRun("dryrun-stuck", repackv1alpha1.RepackModeDryRun).goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("InsufficientImprovement"))
		Expect(got.Status.Plan.Summary.FragBeforePercent).To(BeNumerically(">", 0))
		Expect(got.Status.Plan.Summary.FragAfterPercent).To(Equal(got.Status.Plan.Summary.FragBeforePercent))
		Expect(got.Status.Plan.Summary.ResolvedScope).NotTo(BeNil())
		Expect(got.Status.Plan.Summary.ResolvedScope.NodeCount).To(BeEquivalentTo(3))
		Expect(got.Status.Plan.Summary.ResolvedScope.PodGroupCount).To(BeEquivalentTo(0))
		Expect(got.Status.Plan.Moves).To(BeEmpty())
	})

	// D10/D11: empty goals falls back to the engine's --repack-default-resource
	// (set to volcano.sh/e2e-npu for the e2e), so it still defragments.
	It("uses the engine default resource when goals is empty", func() {
		// Movable fixtures: this tests default-resource resolution, which only
		// matters when the default-resource workload can actually consolidate.
		occupyMovableVCJob(ctx, "def-a", nodes[0], 4)
		occupyMovableVCJob(ctx, "def-b", nodes[1], 2)

		run, err := newRun("dryrun-default", repackv1alpha1.RepackModeDryRun).create(ctx) // no goal()
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeNumerically(">=", 1))
	})

	// D10: an explicit goal must take precedence over --repack-default-resource.
	// The e2e engine defaults to npuResource, so put the fragmented workload on a
	// different resource. If the goal were ignored, this run would see an empty
	// default resource and report NoFragmentation instead of a recommendation.
	It("uses the explicitly requested goal resource ahead of the engine default", func() {
		for _, n := range nodes {
			advertiseResource(ctx, n, altNPUResource, npuPerNode)
		}
		// Movable fixtures on the goal resource: consolidation can only be
		// recommended when the fragmented gangs are actually drainable.
		occupyMovableResource(ctx, "goal-a", nodes[0], altNPUResource, 4)
		occupyMovableResource(ctx, "goal-b", nodes[1], altNPUResource, 2)

		run, err := newRun("explicit-goal", repackv1alpha1.RepackModeDryRun).goal(altNPUResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan.Summary.FreedNodeCount).To(BeNumerically(">=", 1))
	})

	// The fixture has three providing nodes. Moving the 2-card gang alongside
	// the 4-card gang reduces fragmentation by exactly 1/3, so a 33pp gate
	// admits the plan whereas 34pp rejects it.
	It("honors goals.minFragImprovementPercent as the plan benefit gate", func() {
		// Movable fixtures: the gate compares real consolidation candidates, so the
		// 4- and 2-card gangs must be drainable.
		occupyMovableVCJob(ctx, "threshold-a", nodes[0], 4)
		occupyMovableVCJob(ctx, "threshold-b", nodes[1], 2)

		admit, err := newRun("threshold-admit", repackv1alpha1.RepackModeDryRun).
			goalWithMinFragImprovement(npuResource, 33).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, admit.Name)
		admitted := waitTerminal(ctx, admit.Name)
		Expect(completeReason(admitted)).To(Equal("RepackRecommended"))
		Expect(admitted.Status.Plan.Summary.FreedNodeCount).To(BeEquivalentTo(1))

		reject, err := newRun("threshold-reject", repackv1alpha1.RepackModeDryRun).
			goalWithMinFragImprovement(npuResource, 34).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, reject.Name)
		rejected := waitTerminal(ctx, reject.Name)
		Expect(rejected.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(rejected)).To(Equal("InsufficientImprovement"))
		Expect(rejected.Status.Plan.Moves).To(BeEmpty())
	})

	// D12: CEL rejects a core resource target at apply time (no engine needed).
	It("rejects a RepackRun whose goal targets a core resource (cpu)", func() {
		run := &repackv1alpha1.RepackRun{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "bad-resource-"},
			Spec: repackv1alpha1.RepackRunSpec{
				Mode:  repackv1alpha1.RepackModeDryRun,
				Goals: []repackv1alpha1.RepackGoal{{Resource: "cpu"}},
			},
		}
		created, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Create(context.TODO(), run, metav1.CreateOptions{})
		if err == nil {
			deleteRun(ctx, created.Name)
		}
		Expect(err).To(HaveOccurred(), "cpu goal must be rejected by CEL")
	})

	// D13: P0 supports at most one goal (one accelerator resource per Run).
	It("rejects a RepackRun with multiple goals", func() {
		run := &repackv1alpha1.RepackRun{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "multiple-goals-"},
			Spec: repackv1alpha1.RepackRunSpec{
				Mode: repackv1alpha1.RepackModeDryRun,
				Goals: []repackv1alpha1.RepackGoal{
					{Resource: npuResource},
					{Resource: altNPUResource},
				},
			},
		}
		created, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Create(context.TODO(), run, metav1.CreateOptions{})
		if err == nil {
			deleteRun(ctx, created.Name)
		}
		Expect(err).To(HaveOccurred(), "multiple goals must be rejected by the CRD maxItems validation")
	})

	// G22: spec is immutable once created (CEL self==oldSelf).
	It("rejects a spec mutation on an existing RepackRun", func() {
		run, err := newRun("immutable", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)
		waitTerminal(ctx, run.Name)

		latest := getRun(ctx, run.Name)
		latest.Spec.Mode = repackv1alpha1.RepackModeExecute // mutate spec
		_, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().Update(context.TODO(), latest, metav1.UpdateOptions{})
		Expect(err).To(HaveOccurred(), "spec is immutable; the mutation must be rejected")
	})
})
