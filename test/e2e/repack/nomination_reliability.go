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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var _ = Describe("Repack relocations & reliability boundaries", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 3)
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, n := range nodes {
			clearNPU(ctx, n)
		}
	})

	// Execute records well-formed relocations wired to the plan. All assertions read
	// the terminal status object, so this is fully deterministic and stable.
	//
	// The fixture uses scheduler-placed native workloads, so the terminal status
	// covers the full replacement protocol rather than a Pod pinned by spec.nodeName.
	It("records well-formed relocations wired to the plan", func() {
		moving := occupyNativeDeployment(ctx, "nom-moving", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "nom-staying", nodes[1], "stay", 2)
		defer deleteNativeWorkloads(ctx, moving, staying)

		run, err := newRun("relocations", repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		Expect(got.Status.Plan).NotTo(BeNil())
		Expect(got.Status.Relocations).NotTo(BeEmpty(), "Execute must record placement relocations")

		// Lookup sets built entirely from the plan (all from status -> stable).
		freed := map[string]bool{}
		for _, n := range got.Status.Plan.FreedNodes {
			freed[n] = true
		}
		moveTargets := map[string]bool{}
		movePGs := map[string]bool{}
		totalMovedPods := 0
		for _, m := range got.Status.Plan.Moves {
			movePGs[m.PodGroupName] = true
			totalMovedPods += len(m.Pods)
			for _, pm := range m.Pods {
				if pm.ToNode != "" {
					moveTargets[pm.ToNode] = true
				}
			}
		}
		fixture := map[string]bool{}
		for _, n := range nodes {
			fixture[n] = true
		}

		Expect(got.Status.ExecutionDeadline).NotTo(BeNil(), "Execute needs one shared deadline")
		Expect(got.Status.ExecutionDeadline.Time.After(time.Now())).To(BeTrue(), "execution deadline must be in the future")

		// Each relocation is well-formed and never targets a freed node.
		for _, nom := range got.Status.Relocations {
			Expect(nom.Namespace).To(Equal(ctx.Namespace), "relocation namespace")
			Expect(nom.PlannedNodeName).NotTo(BeEmpty(), "relocation must name a planned node")
			Expect(fixture).To(HaveKey(nom.PlannedNodeName), "planned node must be a real fixture node")
			Expect(freed).NotTo(HaveKey(nom.PlannedNodeName), "a relocation must never target a freed node")
			Expect(moveTargets).To(HaveKey(nom.PlannedNodeName), "planned node must coincide with a plan move ToNode")
			Expect(movePGs).To(HaveKey(nom.PodGroupName), "relocation must reference a moved PodGroup")
			Expect(nom.Placement.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced), "terminal success requires verified placement")
			Expect(nom.Placement.SelectedNodeName).NotTo(BeEmpty(), "live receiver selection must be recorded")
			Expect(nom.Placement.ActualNodeName).To(Equal(nom.Placement.SelectedNodeName), "actual node must match the selected receiver")
		}

		// Exact linkage: one status record per relocated Pod.
		Expect(len(got.Status.Relocations)).To(Equal(totalMovedPods), "one relocation status per relocated Pod")
	})

	// Boundary: a goal for a resource that no node provides must not divide-by-zero
	// on the fragmentation denominator (M=0); it reports NoFragmentation, before==0.
	It("reports NoFragmentation for a goal resource no node provides", func() {
		occupy(ctx, "absent-a", nodes[0], 4)
		occupy(ctx, "absent-b", nodes[1], 2)

		// Fully-qualified (passes the CEL "/" rule) but advertised by no node.
		absent := v1.ResourceName("volcano.sh/e2e-absent")
		run, err := newRun("absent-res", repackv1alpha1.RepackModeDryRun).goal(absent).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("NoFragmentation"))
		Expect(got.Status.Plan.Summary.FragBeforePercent).To(BeEquivalentTo(0), "no provider -> 0% fragmentation, no NaN")
		Expect(got.Status.Plan.Moves).To(BeEmpty())
	})

	// Reliability: DryRun is read-only, so two identical runs against an unchanged
	// cluster must produce identical fragmentation numbers and plan shape.
	It("produces identical results for repeated DryRuns on an unchanged cluster", func() {
		occupy(ctx, "det-a", nodes[0], 4)
		occupy(ctx, "det-b", nodes[1], 2)

		first, err := newRun("determinism-1", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, first.Name)
		a := waitTerminal(ctx, first.Name)

		second, err := newRun("determinism-2", repackv1alpha1.RepackModeDryRun).goal(npuResource).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, second.Name)
		b := waitTerminal(ctx, second.Name)

		Expect(a.Status.Plan).NotTo(BeNil())
		Expect(b.Status.Plan).NotTo(BeNil())
		Expect(b.Status.Plan.Summary.FragBeforePercent).To(Equal(a.Status.Plan.Summary.FragBeforePercent))
		Expect(b.Status.Plan.Summary.FragAfterPercent).To(Equal(a.Status.Plan.Summary.FragAfterPercent))
		Expect(b.Status.Plan.Summary.FreedNodeCount).To(Equal(a.Status.Plan.Summary.FreedNodeCount))
		Expect(b.Status.Plan).To(Equal(a.Status.Plan),
			"the complete deterministic plan, not only its length, must remain identical")
	})
})
