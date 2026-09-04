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

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

var _ = Describe("Repack static PDB planning constraint", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 3)
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, node := range nodes {
			clearNPU(ctx, node)
		}
	})

	It("filters a deterministic zero-disruption PDB in DryRun and Execute, then plans after it is relaxed", func() {
		protected := occupyNativeDeployment(ctx, "pdb-constraint-protected", nodes[0], "protected", 2)
		staying := occupyNativeDeployment(ctx, "pdb-constraint-staying", nodes[1], "staying", 4)
		defer deleteNativeWorkloads(ctx, protected, staying)

		blockAll := intstr.FromInt(0)
		pdb, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
			&policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "repack-zero-disruption"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &blockAll,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						nativeWorkloadLabel: protected.deployment.Name,
					}},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Delete(
				context.TODO(), pdb.Name, metav1.DeleteOptions{})
		})
		waitPDBStatus(ctx, pdb.Name, 1, 1)

		scope := &repackv1alpha1.RepackScope{Nodes: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Names: []string{nodes[0]}},
		}}
		waitPDBConstraintObserved(ctx, "pdb-constraint-sync-blocked", scope, func(probe *repackv1alpha1.RepackRun) bool {
			return completeReason(probe) == "InsufficientImprovement" &&
				probe.Status.Plan != nil && len(probe.Status.Plan.Moves) == 0
		})
		blockedDryRun, err := newRun("pdb-constraint-dry-blocked", repackv1alpha1.RepackModeDryRun).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, blockedDryRun.Name)
		blockedDryResult := waitTerminal(ctx, blockedDryRun.Name)
		Expect(completeReason(blockedDryResult)).To(Equal("InsufficientImprovement"))
		Expect(blockedDryResult.Status.Plan.Moves).To(BeEmpty())
		Expect(blockedDryResult.Status.Relocations).To(BeEmpty())

		blockedExecute, err := newRun("pdb-constraint-exec-blocked", repackv1alpha1.RepackModeExecute).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, blockedExecute.Name)
		blockedExecuteResult := waitTerminal(ctx, blockedExecute.Name)
		Expect(blockedExecuteResult.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(blockedExecuteResult)).To(Equal("InsufficientImprovement"))
		Expect(blockedExecuteResult.Status.Plan.Moves).To(BeEmpty())
		Expect(blockedExecuteResult.Status.Relocations).To(BeEmpty(),
			"a statically blocked Pod must not get an eviction journal")

		pdb, err = ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Get(
			context.TODO(), pdb.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		allowOne := intstr.FromInt(1)
		pdb = pdb.DeepCopy()
		pdb.Spec.MaxUnavailable = &allowOne
		pdb, err = ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Update(
			context.TODO(), pdb, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		waitPDBStatus(ctx, pdb.Name, 1, 0)
		waitPDBConstraintObserved(ctx, "pdb-constraint-sync-allowed", scope, func(probe *repackv1alpha1.RepackRun) bool {
			return completeReason(probe) == "RepackRecommended" &&
				probe.Status.Plan != nil && len(probe.Status.Plan.Moves) > 0
		})

		allowedDryRun, err := newRun("pdb-constraint-dry-allowed", repackv1alpha1.RepackModeDryRun).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, allowedDryRun.Name)
		allowedDryResult := waitTerminal(ctx, allowedDryRun.Name)
		Expect(completeReason(allowedDryResult)).To(Equal("RepackRecommended"))
		Expect(allowedDryResult.Status.Plan.Moves).NotTo(BeEmpty())
		Expect(allowedDryResult.Status.Plan.Moves[0].PodGroupName).To(Equal(protected.podGroup))

		allowedExecute, err := newRun("pdb-constraint-exec-allowed", repackv1alpha1.RepackModeExecute).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, allowedExecute.Name)
		allowedExecuteResult := waitTerminal(ctx, allowedExecute.Name)
		Expect(allowedExecuteResult.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(allowedExecuteResult)).To(Equal("ExecutionCompleted"))
		Expect(allowedExecuteResult.Status.Relocations).NotTo(BeEmpty())
		Expect(allowedExecuteResult.Status.Relocations[0].Eviction.Phase).To(Equal(repackv1alpha1.PodEvictionAccepted))
	})

})

func waitPDBStatus(ctx *e2eutil.TestContext, name string, expectedPods, desiredHealthy int32) {
	Eventually(func() bool {
		pdb, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Get(
			context.TODO(), name, metav1.GetOptions{})
		return err == nil && pdb.Status.ObservedGeneration == pdb.Generation &&
			pdb.Status.ExpectedPods == expectedPods && pdb.Status.DesiredHealthy == desiredHealthy
	}, repackTimeout, repackPoll).Should(BeTrue(),
		"PDB controller status must be fresh before planning")
}
