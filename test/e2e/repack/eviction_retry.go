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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// These specs exercise the batch eviction retry and the unified Execute
// deadline: a PDB-blocked eviction keeps its durable InProgress intent, the
// engine retries on a bounded backoff, and at the absolute deadline the Run
// fails with ExecutionTimedOut instead of hanging or silently succeeding.
var _ = Describe("Repack eviction retry & execution deadline", Serial, func() {
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

	// Without any PDB, a node hosting multiple victims is drained in one batch:
	// every victim is Accepted, every replacement is Placed, and the freed-node
	// set is verified against the plan.
	It("batch-evicts multiple victims and completes all replacement placements", func() {
		victimA := occupyNativeDeployment(ctx, "batch-a", nodes[0], "move", 2)
		victimB := occupyNativeDeployment(ctx, "batch-b", nodes[0], "move", 2)
		staying := occupyNativeDeployment(ctx, "batch-staying", nodes[1], "stay", 4)
		defer deleteNativeWorkloads(ctx, victimA, victimB, staying)

		scope := &repackv1alpha1.RepackScope{Nodes: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Names: []string{nodes[0]}},
		}}
		run, err := newRun("batch-evict", repackv1alpha1.RepackModeExecute).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		Expect(got.Status.Plan.FreedNodes).To(Equal([]string{nodes[0]}),
			"the scoped plan must drain exactly the victim node")
		Expect(got.Status.Relocations).To(HaveLen(2),
			"both victims on the drained node must be batch-evicted")
		for _, relocation := range got.Status.Relocations {
			Expect(relocation.Eviction.Phase).To(Equal(repackv1alpha1.PodEvictionAccepted))
			Expect(relocation.Placement.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
			Expect(relocation.Placement.ActualNodeName).To(Equal(relocation.Placement.SelectedNodeName))
		}
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Result.FreedNodes).To(Equal([]string{nodes[0]}),
			"the freed-node set must be verified against the plan")
	})

	// A maxUnavailable=1 PDB admits only one disruption at a time: one victim is
	// Accepted, the sibling is rate-limited (429), and once the first replacement
	// is placed the allowance recovers so the blocked victim is retried and the
	// node still drains to a verified success.
	It("drains PDB-protected victims as the disruption allowance recovers", func() {
		victimA := occupyNativeDeployment(ctx, "pdb1-a", nodes[0], "move", 2)
		victimB := occupyNativeDeployment(ctx, "pdb1-b", nodes[0], "move", 2)
		staying := occupyNativeDeployment(ctx, "pdb1-staying", nodes[1], "stay", 4)
		defer deleteNativeWorkloads(ctx, victimA, victimB, staying)

		allowOne := intstr.FromInt(1)
		_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
			&policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "allow-one"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &allowOne,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						nativeScopeLabel: "move",
					}},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int32 {
			pdb, getErr := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Get(
				context.TODO(), "allow-one", metav1.GetOptions{})
			if getErr != nil {
				return -1
			}
			return pdb.Status.DisruptionsAllowed
		}, repackTimeout, repackPoll).Should(Equal(int32(1)), "PDB must become effective before Execute")

		scope := &repackv1alpha1.RepackScope{Nodes: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Names: []string{nodes[0]}},
		}}
		run, err := newRun("pdb1-evict", repackv1alpha1.RepackModeExecute).
			goal(npuResource).scope(scope).create(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer deleteRun(ctx, run.Name)

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
		Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
		Expect(got.Status.Relocations).To(HaveLen(2),
			"both PDB-protected victims must eventually be evicted and placed")
		for _, relocation := range got.Status.Relocations {
			Expect(relocation.Eviction.Phase).To(Equal(repackv1alpha1.PodEvictionAccepted))
			Expect(relocation.Placement.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
		}
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeTrue())
		Expect(got.Status.Result.FreedNodes).To(Equal([]string{nodes[0]}))
	})

	// A maxUnavailable=0 PDB keeps the Eviction API returning 429 until the
	// absolute Execute deadline. The engine must retry (not mark the victim
	// Rejected on the first 429) and finally fail with ExecutionTimedOut,
	// finalizing the relocation and releasing the placement lease.
	It("retries PDB-blocked evictions and fails with ExecutionTimedOut at the deadline", func() {
		blocked := occupyNativeDeployment(ctx, "retry-blocked", nodes[0], "move", 4)
		defer deleteNativeWorkloads(ctx, blocked)

		blockAll := intstr.FromInt(0)
		_, err := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Create(context.TODO(),
			&policyv1.PodDisruptionBudget{
				ObjectMeta: metav1.ObjectMeta{Name: "block-retry"},
				Spec: policyv1.PodDisruptionBudgetSpec{
					MaxUnavailable: &blockAll,
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
						nativeWorkloadLabel: blocked.deployment.Name,
					}},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int32 {
			pdb, getErr := ctx.Kubeclient.PolicyV1().PodDisruptionBudgets(ctx.Namespace).Get(
				context.TODO(), "block-retry", metav1.GetOptions{})
			if getErr != nil {
				return -1
			}
			return pdb.Status.DisruptionsAllowed
		}, repackTimeout, repackPoll).Should(Equal(int32(0)), "PDB must become effective before Execute")

		// Pause the engine to inject a durable eviction journal whose victim is
		// already selected (bypassing the planning stage, which would reject the
		// PDB-blocked victim and produce an empty plan).
		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		run := prepareEvictionRetryJournal(ctx, "retry-timeout", blocked, nodes[1], nodes[0], 45*time.Second)
		defer deleteRun(ctx, run.Name)

		restoreEngine()

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackFailed))
		Expect(completeReason(got)).To(Equal("ExecutionTimedOut"))
		Expect(got.Status.Relocations).To(HaveLen(1), "the retryable relocation must stay durable until the deadline")
		Expect(got.Status.Relocations[0].Eviction.Phase).To(Equal(repackv1alpha1.PodEvictionRejected),
			"an unfinished eviction must be finalized as Rejected at the deadline")
		Expect(got.Status.Relocations[0].Placement.Phase).To(Equal(repackv1alpha1.PodPlacementTimedOut),
			"an unfinished replacement placement must be finalized as TimedOut at the deadline")
		Expect(got.Status.Result).NotTo(BeNil())
		Expect(got.Status.Result.MetricsVerified).To(BeFalse())
		assertPlacementLeaseReleased(ctx, blocked.podGroup)
	})

	// A Run whose deadline already passed while the engine was stopped must
	// finalize on the first reconcile instead of issuing any new eviction.
	It("finalizes an already-expired journal on the first reconcile", func() {
		blocked := occupyNativeDeployment(ctx, "expired-blocked", nodes[0], "move", 4)
		defer deleteNativeWorkloads(ctx, blocked)

		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		run := prepareEvictionRetryJournal(ctx, "expired-timeout", blocked, nodes[1], nodes[0], -time.Minute)
		defer deleteRun(ctx, run.Name)

		restoreEngine()

		got := waitTerminal(ctx, run.Name)
		Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackFailed))
		Expect(completeReason(got)).To(Equal("ExecutionTimedOut"))
		Expect(got.Status.Relocations).To(HaveLen(1))
		Expect(got.Status.Relocations[0].Eviction.Phase).To(Equal(repackv1alpha1.PodEvictionRejected))
		Expect(got.Status.Relocations[0].Placement.Phase).To(Equal(repackv1alpha1.PodPlacementTimedOut))
		assertPlacementLeaseReleased(ctx, blocked.podGroup)
	})

	// If the engine crashes after the Eviction API accepted a victim but before
	// the Accepted outcome is durable, the next reconcile must observe the
	// terminating original Pod and recover it as Accepted instead of evicting a
	// same-name replacement again.
	It("recovers an in-progress victim as Accepted after engine restart without re-evicting", func() {
		victim := occupyNativeDeployment(ctx, "restart-victim", nodes[0], "move", 4)
		staying := occupyNativeDeployment(ctx, "restart-staying", nodes[1], "stay", 2)
		defer deleteNativeWorkloads(ctx, victim, staying)

		restoreEngine := pauseRepackEngine(ctx)
		defer restoreEngine()

		run := prepareInProgressEvictionJournal(ctx, "restart-recover", victim, nodes[1], nodes[0], time.Hour)
		defer deleteRun(ctx, run.Name)

		// Simulate a crash window: the Eviction API accepted the victim, but the
		// engine stopped before persisting the Accepted outcome.
		Expect(ctx.Kubeclient.PolicyV1().Evictions(ctx.Namespace).Evict(context.TODO(), &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: victim.podName, Namespace: ctx.Namespace},
		})).To(Succeed(), "evict original victim")

		restoreEngine()

		// The recovery path observes the terminating original Pod (same UID) and
		// records Accepted rather than issuing another Eviction API request.
		Eventually(func() repackv1alpha1.PodEvictionPhase {
			return getRun(ctx, run.Name).Status.Relocations[0].Eviction.Phase
		}, repackTimeout, repackPoll).Should(Equal(repackv1alpha1.PodEvictionAccepted),
			"a terminating victim with a durable InProgress intent must be recovered as Accepted")

		if pod, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).Get(
			context.TODO(), victim.podName, metav1.GetOptions{}); err == nil {
			Expect(pod.UID).To(Equal(victim.podUID),
				"recovery must not evict a different Pod under the same name")
		}
	})
})

// prepareEvictionRetryJournal persists an in-flight Execute journal with a
// single Pending victim while the engine is paused. The victim is a real native
// workload Pod, so a resumed engine observes it and issues a real Eviction API
// request (which the test's PDB then blocks with 429).
func prepareEvictionRetryJournal(
	ctx *e2eutil.TestContext,
	name string,
	workload *nativeWorkload,
	plannedNode, fromNode string,
	deadlineOffset time.Duration,
) *repackv1alpha1.RepackRun {
	run, err := newRun(name, repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
	Expect(err).NotTo(HaveOccurred())

	expires := metav1.NewTime(time.Now().Add(deadlineOffset))
	run.Status = repackv1alpha1.RepackRunStatus{
		Phase:             repackv1alpha1.RepackRunning,
		ExecutionDeadline: &expires,
		Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{
				FreedNodeCount: 1,
				MovedCardCount: 4,
			},
			FreedNodes: []string{fromNode},
			Moves: []repackv1alpha1.RepackMove{{
				Namespace: ctx.Namespace, PodGroupName: workload.podGroup, Cards: 4,
				Pods: []repackv1alpha1.PodMove{{
					Name: workload.podName, FromNode: fromNode, ToNode: plannedNode, Cards: 4,
				}},
			}},
		},
		Relocations: []repackv1alpha1.PodRelocationStatus{{
			Namespace: ctx.Namespace, PodGroupName: workload.podGroup,
			VictimPodName: workload.podName, VictimPodUID: workload.podUID,
			PlannedNodeName: plannedNode,
			Eviction:        repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionPending},
			Placement:       repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
		}},
	}
	run, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(
		context.TODO(), run, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "persist eviction retry journal")
	return run
}

// prepareInProgressEvictionJournal persists an Execute journal whose single
// victim already holds a durable InProgress intent. It reproduces the crash
// window between an accepted Eviction API call and its durable Accepted
// outcome, so a resumed engine must recover from the original Pod UID.
func prepareInProgressEvictionJournal(
	ctx *e2eutil.TestContext,
	name string,
	workload *nativeWorkload,
	plannedNode, fromNode string,
	deadlineOffset time.Duration,
) *repackv1alpha1.RepackRun {
	run, err := newRun(name, repackv1alpha1.RepackModeExecute).goal(npuResource).create(ctx)
	Expect(err).NotTo(HaveOccurred())

	expires := metav1.NewTime(time.Now().Add(deadlineOffset))
	run.Status = repackv1alpha1.RepackRunStatus{
		Phase:             repackv1alpha1.RepackRunning,
		ExecutionDeadline: &expires,
		Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{
				FreedNodeCount: 1,
				MovedCardCount: 4,
			},
			FreedNodes: []string{fromNode},
			Moves: []repackv1alpha1.RepackMove{{
				Namespace: ctx.Namespace, PodGroupName: workload.podGroup, Cards: 4,
				Pods: []repackv1alpha1.PodMove{{
					Name: workload.podName, FromNode: fromNode, ToNode: plannedNode, Cards: 4,
				}},
			}},
		},
		Relocations: []repackv1alpha1.PodRelocationStatus{{
			Namespace: ctx.Namespace, PodGroupName: workload.podGroup,
			VictimPodName: workload.podName, VictimPodUID: workload.podUID,
			PlannedNodeName: plannedNode,
			Eviction: repackv1alpha1.PodEvictionStatus{
				Phase:   repackv1alpha1.PodEvictionInProgress,
				Message: "Eviction intent is durable; the request may be submitted or retried.",
			},
			Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
		}},
	}
	run, err = ctx.Vcclient.RepackV1alpha1().RepackRuns().UpdateStatus(
		context.TODO(), run, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "persist in-progress eviction journal")
	return run
}
