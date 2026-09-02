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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// This suite verifies the scheduler-faithful feasibility check: repack must honor
// the SAME node constraints the scheduler enforces at bind time (taints, node
// affinity, ...), otherwise an Execute'd relocation bounces straight back. It also
// checks the drain's "prefer staying nodes" receiver ordering.
//
// Every case pins its workloads with the Job template's spec.nodeName to build a
// deterministic layout; the constraints (taint / affinity) are what the feasibility check must
// respect when deciding where a victim could re-land.

const hostnameLabel = "kubernetes.io/hostname"

// taintNode adds a NoSchedule taint the occupy pods do not tolerate. It returns a
// cleanup that removes the taint.
func taintNode(ctx *e2eutil.TestContext, node string) {
	patch := `{"spec":{"taints":[{"key":"repack.volcano.sh/e2e-blocked","value":"yes","effect":"NoSchedule"}]}}`
	_, err := ctx.Kubeclient.CoreV1().Nodes().Patch(
		context.TODO(), node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func untaintNode(ctx *e2eutil.TestContext, node string) {
	patch := `{"spec":{"taints":null}}`
	_, _ = ctx.Kubeclient.CoreV1().Nodes().Patch(
		context.TODO(), node, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
}

// occupyPinnedToHost is occupy() plus a REQUIRED node affinity to the pod's own
// host, so the pod can never be relocated to any other node — the feasibility check must then
// judge its gang un-drainable.
func occupyPinnedToHost(ctx *e2eutil.TestContext, name, node string, cards int) {
	npuQty := resource.MustParse(fmt.Sprintf("%d", cards))
	npuList := v1.ResourceList{npuResource: npuQty}
	spec := &e2eutil.JobSpec{
		Name:      name,
		Namespace: ctx.Namespace,
		NodeName:  node,
		Tasks: []e2eutil.TaskSpec{{
			Name: "w", Min: 1, Rep: 1, Img: e2eutil.DefaultNginxImage,
			Req: npuList, Limit: npuList,
			Affinity: &v1.Affinity{NodeAffinity: &v1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{{MatchExpressions: []v1.NodeSelectorRequirement{{
						Key: hostnameLabel, Operator: v1.NodeSelectorOpIn, Values: []string{node},
					}}}},
				},
			}},
		}},
	}
	job := e2eutil.CreateJob(ctx, spec)
	Expect(e2eutil.WaitTasksReady(ctx, job, 1)).NotTo(HaveOccurred())
}

var _ = Describe("Repack scheduler-faithful feasibility & receiver ordering", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string
	var tainted []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 3)
		tainted = nil
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, n := range tainted {
			untaintNode(ctx, n)
		}
		for _, n := range nodes {
			clearNPU(ctx, n)
		}
	})

	// A tainted node must not be chosen as a receiver. With BOTH occupied nodes
	// tainted, neither gang can re-land on the other, and the empty node is excluded
	// as a receiver — so a fragmented cluster has no worthwhile plan. If taints were
	// ignored (the old bug) this would wrongly recommend a move that bounces back.
	// Pods must be Running BEFORE tainting: NoSchedule blocks new placements onto
	// the node, but already-running pods stay put.
	It("does not relocate onto a tainted node (InsufficientImprovement)", func() {
		occupy(ctx, "taint-a", nodes[0], 4)
		occupy(ctx, "taint-b", nodes[1], 4)
		taintNode(ctx, nodes[0])
		taintNode(ctx, nodes[1])
		tainted = []string{nodes[0], nodes[1]}

		run, err := newRun("taint-block", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("InsufficientImprovement"), "tainted receivers => no feasible consolidation")
		Expect(got.Status.Plan.Summary.FragBeforePercent).To(BeNumerically(">", 0))
		Expect(got.Status.Plan.Moves).To(BeEmpty())
	})

	// A single tainted node can still be DRAINED (its pods relocate onto the
	// untainted node); the tainted node is freed and never used as a receiver.
	It("still drains a tainted node onto an untainted receiver", func() {
		// node0 untainted with room, node1 occupied then tainted (NoSchedule only
		// blocks new placements; the running pod stays).
		// Movable fixtures: this test drains the tainted node, so its pod must be
		// drainable (a nodeName-pinned vcjob template is immovable).
		occupyMovableVCJob(ctx, "recv", nodes[0], 2)
		occupyMovableVCJob(ctx, "tainted-src", nodes[1], 4)
		taintNode(ctx, nodes[1])
		tainted = []string{nodes[1]}

		run, err := newRun("taint-drain", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan.FreedNodes).To(ContainElement(nodes[1]), "the tainted node is the one freed")
		for _, m := range got.Status.Plan.Moves {
			for _, p := range m.Pods {
				Expect(p.ToNode).NotTo(Equal(nodes[1]), "nothing may land on the tainted node")
			}
		}
	})

	// Required node affinity is honored: with each gang pinned to its own host by
	// affinity, neither can move, so a fragmented cluster yields no plan.
	It("honors required node affinity (InsufficientImprovement when pinned)", func() {
		occupyPinnedToHost(ctx, "aff-a", nodes[0], 4)
		occupyPinnedToHost(ctx, "aff-b", nodes[1], 4)

		run, err := newRun("affinity-pin", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(completeReason(got)).To(Equal("InsufficientImprovement"), "host-pinned gangs cannot relocate")
		Expect(got.Status.Plan.Moves).To(BeEmpty())
	})

	// Prefer-staying ordering: a receiver-only node (excluded from draining by
	// scope.nodes) is filled BEFORE a tighter-fitting drainable node. Layout:
	// node0=2 (drained), node1=2 (staying, idle 6), node2=6 (drainable, idle 2).
	// Best-fit alone would pick node2 (tightest); prefer-staying picks node1.
	It("fills a staying (receiver-only) node before a tighter drainable one", func() {
		// Movable fixtures: this test asserts the receiver-choice ORDER of a real
		// consolidation, so the loads must be drainable (pinned templates are
		// immovable and the planner would find nothing to move).
		occupyMovableVCJob(ctx, "stay-src", nodes[0], 2)
		occupyMovableVCJob(ctx, "stay-recv", nodes[1], 2) // excluded from draining below -> staying
		occupyMovableVCJob(ctx, "stay-drain", nodes[2], 6)

		// Exclude node1 from draining: it becomes receiver-only (staying).
		scope := &repackv1alpha1.RepackScope{
			Nodes: &repackv1alpha1.RepackSelectorTerm{
				Exclude: &repackv1alpha1.RepackSelector{Names: []string{nodes[1]}},
			},
		}
		run, err := newRun("prefer-staying", repackv1alpha1.RepackModeDryRun).goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(completeReason(got)).To(Equal("RepackRecommended"))
		Expect(got.Status.Plan.FreedNodes).To(ContainElement(nodes[0]), "the small in-scope node is freed")
		landedOnStaying := false
		for _, m := range got.Status.Plan.Moves {
			for _, p := range m.Pods {
				if p.FromNode == nodes[0] {
					Expect(p.ToNode).To(Equal(nodes[1]), "node0's pod should land on the staying node, not the tighter drainable one")
					landedOnStaying = true
				}
			}
		}
		Expect(landedOnStaying).To(BeTrue(), "expected node0's relocation in the plan")
	})
})
