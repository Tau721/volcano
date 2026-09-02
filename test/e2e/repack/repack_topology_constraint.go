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

// HyperNode-level constraint-preservation e2e scenarios (US-02). Every scenario
// builds a real HyperNode tree over the kind workers, declares a hard
// HyperNode-level constraint on the PodGroup, runs a RepackRun through the real
// scheduler, and asserts the post-repack placement still satisfies the
// constraint — the plan is only "right" if the real scheduler (which
// re-evaluates the constraint during Execute replacement scheduling) accepts it.
//
// Determinism: the drain planner selects the highest-scored candidate first, so
// these specs never depend on WHICH node drains. Every non-subject node is either
// full (never a drain target) or pinned (spec.nodeName makes its replacement
// immovable, so repack's relocation predicate rejects it and the node never
// drains). Only the subject unit has a feasible migration, and assertions check
// order-independent properties: which domains the pods end up in and which node
// set was freed — "constraint still satisfied" rather than "exact target node".
// Scenarios that could free either of a symmetric pair (E10a/E17/E18) assert
// that at least one of the pair is freed and the constraint holds.
package repack

import (
	"context"
	"fmt"
	"strings"
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
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// ---- topology fixture helpers --------------------------------------------

type hyperNodeFixture struct {
	name         string
	tier         int
	members      []string
	memberIsNode bool
}

// setupRepackTopologyCustom builds an arbitrary HyperNode tree. E10a/E17/E18
// need a tier-1 domain wider than the standard 2-node rt-s0 so an in-domain
// receiver exists; E10b needs a one-node second domain. setupRepackTopology
// (E8/E9/E11/E12/E13/E16) cannot express those, so these specs build their own
// tree.
func setupRepackTopologyCustom(ctx *e2eutil.TestContext, fixtures []hyperNodeFixture) {
	for _, hn := range fixtures {
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
	for _, hn := range fixtures {
		name := hn.name
		Eventually(func() error {
			_, err := ctx.Vcclient.TopologyV1alpha1().HyperNodes().Get(context.TODO(), name, metav1.GetOptions{})
			return err
		}, 30*time.Second, time.Second).Should(BeNil(), "HyperNode %s must be readable", name)
	}
}

// holdNodesExcept taints every schedulable worker except the given nodes so the
// scheduler deterministically places the next job's pods within `keep`. The
// returned func removes the taints again.
func holdNodesExcept(ctx *e2eutil.TestContext, keep ...string) func() {
	keepSet := sets.New[string](keep...)
	taint := v1.Taint{Key: nativePlacementTaint, Value: "true", Effect: v1.TaintEffectNoSchedule}
	var added []string
	for _, nodeName := range schedulableNodes(ctx) {
		if keepSet.Has(nodeName) {
			continue
		}
		addedNode, err := patchNodeTaint(ctx, nodeName, taint)
		Expect(err).NotTo(HaveOccurred(), "hold fixture nodes")
		if addedNode {
			added = append(added, nodeName)
		}
	}
	return func() {
		for _, nodeName := range added {
			Expect(clearNodeTaint(ctx, nodeName, taint)).NotTo(HaveOccurred(), "release fixture nodes")
		}
	}
}

// ---- job builders ---------------------------------------------------------

func npuQuantity(cards int) v1.ResourceList {
	return v1.ResourceList{npuResource: resource.MustParse(fmt.Sprintf("%d", cards))}
}

// hardTopologyJobSpec returns a vcjob whose PodGroup carries a hard network
// topology (highestTierAllowed = tier). With replicas > 1 the requested cards
// must exceed one node's capacity for the scheduler to spread the gang.
func hardTopologyJobSpec(ctx *e2eutil.TestContext, name string, cards, tier int, replicas int32) *e2eutil.JobSpec {
	return &e2eutil.JobSpec{
		Name:      name,
		Namespace: ctx.Namespace,
		Min:       replicas,
		Tasks: []e2eutil.TaskSpec{{
			Name: "w", Min: replicas, Rep: replicas, Img: e2eutil.DefaultNginxImage,
			Req: npuQuantity(cards), Limit: npuQuantity(cards),
		}},
		NetworkTopology: &batchv1alpha1.NetworkTopologySpec{
			Mode:               batchv1alpha1.HardNetworkTopologyMode,
			HighestTierAllowed: ptr.To(tier),
		},
	}
}

type repackSubGroupTask struct {
	name  string
	cards int
	reps  int32
	tier  *int // optional hard topology declared on the subGroupPolicy itself
}

// subGroupJobSpec returns a vcjob whose tasks carry PartitionPolicy, so the
// controller creates one SubGroupPolicy per task (name = task name). jobTier, if
// set, adds a job-level hard network topology that the subGroupPolicies do NOT
// inherit (E12).
func subGroupJobSpec(ctx *e2eutil.TestContext, name string, jobTier *int, tasks ...repackSubGroupTask) *e2eutil.JobSpec {
	var min int32
	var taskSpecs []e2eutil.TaskSpec
	for _, task := range tasks {
		min += task.reps
		partition := &batchv1alpha1.PartitionPolicySpec{
			TotalPartitions: 1,
			PartitionSize:   task.reps,
			MinPartitions:   1,
		}
		if task.tier != nil {
			partition.NetworkTopology = &batchv1alpha1.NetworkTopologySpec{
				Mode:               batchv1alpha1.HardNetworkTopologyMode,
				HighestTierAllowed: task.tier,
			}
		}
		taskSpecs = append(taskSpecs, e2eutil.TaskSpec{
			Name: task.name, Min: task.reps, Rep: task.reps, Img: e2eutil.DefaultNginxImage,
			Req: npuQuantity(task.cards), Limit: npuQuantity(task.cards), PartitionPolicy: partition,
		})
	}
	spec := &e2eutil.JobSpec{
		Name: name, Namespace: ctx.Namespace, Min: min, Tasks: taskSpecs,
	}
	if jobTier != nil {
		spec.NetworkTopology = &batchv1alpha1.NetworkTopologySpec{
			Mode:               batchv1alpha1.HardNetworkTopologyMode,
			HighestTierAllowed: jobTier,
		}
	}
	return spec
}

// createMovableJob creates the job while only the `hold` nodes are schedulable
// and waits for every replica to be Ready. The pod template carries no
// spec.nodeName, so replacements stay free to follow repack's live receiver
// selection. hold must be sized so the requested total cards force the intended
// spread.
func createMovableJob(ctx *e2eutil.TestContext, spec *e2eutil.JobSpec, hold ...string) *batchv1alpha1.Job {
	release := holdNodesExcept(ctx, hold...)
	defer release()
	job := e2eutil.CreateJob(ctx, spec)
	replicas := 0
	for _, task := range spec.Tasks {
		replicas += int(task.Rep)
	}
	Expect(e2eutil.WaitTasksReady(ctx, job, replicas)).NotTo(HaveOccurred(),
		"job %s must schedule %d replicas", spec.Name, replicas)
	return job
}

// ---- PodGroup patching ----------------------------------------------------

// patchPodGroupTopologyAffinity reads the PodGroup owning the given vcjob and
// sets spec.topologyAffinity and/or labels, then updates it so the webhook
// validates the change. The job controller does not sync these fields, so the
// patched value survives reconciliation; the repack engine's session reads it.
func patchPodGroupTopologyAffinity(ctx *e2eutil.TestContext, job *batchv1alpha1.Job, ta *schedulingv1beta1.TopologyAffinitySpec, labels map[string]string) {
	pgRef := podGroupNameForOwner(ctx, job.UID) // "namespace/name"
	pgName := strings.TrimPrefix(pgRef, ctx.Namespace+"/")
	pg, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Get(context.TODO(), pgName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	pg = pg.DeepCopy()
	if ta != nil {
		pg.Spec.TopologyAffinity = ta
	}
	if labels != nil {
		if pg.Labels == nil {
			pg.Labels = map[string]string{}
		}
		for key, value := range labels {
			pg.Labels[key] = value
		}
	}
	_, err = ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).Update(context.TODO(), pg, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "patch PodGroup %s topologyAffinity", pgName)
}

func podGroupAntiAffinity(selector map[string]string) *schedulingv1beta1.TopologyAffinitySpec {
	return &schedulingv1beta1.TopologyAffinitySpec{
		PodGroupAntiAffinity: &schedulingv1beta1.PodGroupAntiAffinity{
			Required: []schedulingv1beta1.PodGroupAffinityTerm{{
				PodGroupSelector: &metav1.LabelSelector{MatchLabels: selector},
				TopologyTier:     ptr.To(int32(1)),
			}},
		},
	}
}

func subGroupAffinity(subGroups ...string) *schedulingv1beta1.TopologyAffinitySpec {
	return &schedulingv1beta1.TopologyAffinitySpec{
		SubGroupAffinity: &schedulingv1beta1.SubGroupAffinity{
			Required: []schedulingv1beta1.SubGroupAffinityTerm{{
				SubGroups:    subGroups,
				TopologyTier: ptr.To(int32(1)),
			}},
		},
	}
}

func subGroupAntiAffinity(subGroups ...string) *schedulingv1beta1.TopologyAffinitySpec {
	return &schedulingv1beta1.TopologyAffinitySpec{
		SubGroupAntiAffinity: &schedulingv1beta1.SubGroupAntiAffinity{
			Required: []schedulingv1beta1.SubGroupAffinityTerm{{
				SubGroups:    subGroups,
				TopologyTier: ptr.To(int32(1)),
			}},
		},
	}
}

// ---- placement assertions -------------------------------------------------

func runningPodsOfJob(ctx *e2eutil.TestContext, job *batchv1alpha1.Job) []*v1.Pod {
	all := e2eutil.GetTasksOfJob(ctx, job)
	var running []*v1.Pod
	for _, pod := range all {
		if pod.Status.Phase == v1.PodRunning && pod.DeletionTimestamp == nil {
			running = append(running, pod)
		}
	}
	return running
}

func waitRunningPodCount(ctx *e2eutil.TestContext, job *batchv1alpha1.Job, count int) {
	Eventually(func() int {
		return len(runningPodsOfJob(ctx, job))
	}, fixtureTimeout, repackPoll).Should(Equal(count), "job %s must settle with %d running pods", job.Name, count)
}

func nodeOfPod(pod *v1.Pod) string {
	Expect(pod.Spec.NodeName).NotTo(BeEmpty(), "pod %s must be scheduled", pod.Name)
	return pod.Spec.NodeName
}

// tierDomainOfPod returns the HyperNode at `tier` that contains the pod's node.
func tierDomainOfPod(ctx *e2eutil.TestContext, pod *v1.Pod, tier int) string {
	nodeToH := tierNodeToHyperNode(ctx, tier)
	domain := nodeToH[nodeOfPod(pod)]
	Expect(domain).NotTo(BeEmpty(), "node %s of pod %s must belong to a tier-%d HyperNode",
		pod.Spec.NodeName, pod.Name, tier)
	return domain
}

// expectSameTierDomain asserts every running pod of the job lands in one single
// HyperNode domain at `tier` (E10a/E17/E18).
func expectSameTierDomain(ctx *e2eutil.TestContext, job *batchv1alpha1.Job, tier int) {
	pods := runningPodsOfJob(ctx, job)
	Expect(pods).NotTo(BeEmpty(), "job %s must have running pods", job.Name)
	domains := sets.New[string]()
	for _, pod := range pods {
		domains.Insert(tierDomainOfPod(ctx, pod, tier))
	}
	Expect(domains.Len()).To(Equal(1), "all pods of job %s must land in a single tier-%d HyperNode domain (got %v)",
		job.Name, tier, domains.UnsortedList())
}

func freedSet(freed []string) sets.Set[string] {
	return sets.New[string](freed...)
}

// ---- scenarios ------------------------------------------------------------

// These tests require the repack CRDs, the volcano-repack-engine (helm
// custom.repack_enable=true), and HyperNode CRDs in the cluster.
var _ = Describe("Repack HyperNode-aware constraint preservation (US-02)", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	// Standard tree (same as US-01): rt-s0 {n0,n1}, rt-s1 {n2,n3}, rt-s2, rt-s3
	// {n0,n1}. E8/E9/E11/E12/E13/E16 need the two tier-1 domains and the partial
	// rt-s3.
	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 4) // advertise fake NPUs on the 4 workers
		setupRepackTopology(ctx, nodes)
	})
	AfterEach(func() {
		e2eutil.CleanupTestContext(ctx) // also wipes this spec's HyperNodes
		for _, n := range nodes {
			clearNPU(ctx, n)
		}
	})

	Context("E8: hard network topology holds across a cross-tier drain (US-02)", func() {
		It("moves a tier-1 hard-topology pod across domains when the gang fully vacates", func() {
			// A hard tier-1 job is the only movable load on n0. The gang (a
			// single pod) fully vacates, so the H1 anchor clear applies and the
			// pod may land on any tier-1 HyperNode — rt-s0's only node is full,
			// so the receiver is in rt-s1. The real scheduler must accept that
			// cross-domain placement during Execute or the run fails.
			hardJob := createMovableJob(ctx, hardTopologyJobSpec(ctx, "e8-topo", 4, 1, 1), nodes[0])
			occupy(ctx, "e8-full1", nodes[1], 8) // rt-s0 full -> no in-domain receiver
			occupy(ctx, "e8-s2", nodes[2], 2)    // rt-s1 static receiver
			occupy(ctx, "e8-full3", nodes[3], 8) // rt-s1 full

			run, err := newRun("e8", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
			Expect(got.Status.Plan).NotTo(BeNil())
			freed := freedSet(got.Status.Plan.FreedNodes)
			Expect(freed).To(HaveKey(nodes[0]), "the hard pod's source must be drained")
			Expect(len(got.Status.Plan.FreedNodes)).To(Equal(1),
				"only the movable hard pod's node may be freed (pinned/full nodes never drain)")

			waitRunningPodCount(ctx, hardJob, 1)
			pod := runningPodsOfJob(ctx, hardJob)[0]
			Expect(nodeOfPod(pod)).NotTo(Equal(nodes[0]), "pod must leave the drained source")
			Expect(tierDomainOfPod(ctx, pod, 1)).To(Equal("rt-s1"),
				"fully-vacated hard tier-1 pod must be allowed to land in rt-s1 (H1 anchor clear)")
		})
	})

	Context("E9: PodGroup anti-affinity with a static peer (US-02)", func() {
		It("keeps the migrating PodGroup out of the static peer's HyperNode domain", func() {
			// B fills n2 entirely -> full node, never a drain target, so B is
			// static. A is the only movable member; its required anti-affinity
			// against B must keep A inside rt-s0 (n1 is the only in-domain
			// receiver).
			jobA := occupyMovableVCJob(ctx, "e9-a", nodes[0], 2)
			jobB := occupy(ctx, "e9-b", nodes[2], 8) // full node -> static peer
			occupy(ctx, "e9-s1", nodes[1], 2)        // rt-s0 static receiver for A
			occupy(ctx, "e9-full3", nodes[3], 8)
			patchPodGroupTopologyAffinity(ctx, jobA, podGroupAntiAffinity(map[string]string{"app": "e9-b"}), map[string]string{"app": "e9-a"})
			patchPodGroupTopologyAffinity(ctx, jobB, podGroupAntiAffinity(map[string]string{"app": "e9-a"}), map[string]string{"app": "e9-b"})

			run, err := newRun("e9", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(freedSet(got.Status.Plan.FreedNodes)).To(HaveKey(nodes[0]), "A's source must be drained")

			waitRunningPodCount(ctx, jobA, 1)
			waitRunningPodCount(ctx, jobB, 1)
			domainA := tierDomainOfPod(ctx, runningPodsOfJob(ctx, jobA)[0], 1)
			domainB := tierDomainOfPod(ctx, runningPodsOfJob(ctx, jobB)[0], 1)
			Expect(domainA).NotTo(Equal(domainB), "A must not share a tier-1 domain with the static peer B")
			Expect(nodeOfPod(runningPodsOfJob(ctx, jobB)[0])).To(Equal(nodes[2]), "the static peer must not move")
		})
	})

	Context("E11: the only feasible receiver violates the constraint -> no migration (US-02)", func() {
		It("rejects the unit as infeasible and plans no violating move", func() {
			// A (movable, on n0) is Required anti-affine to B, which sits static and
			// full on n2. A's only capacity receiver is n3: n1 is full (no in-domain
			// receiver) and n3 carries the only slack — but n3 lies in rt-s1, B's
			// tier-1 domain, which the anti-affinity forbids. The cluster is
			// genuinely fragmented (4 occupied nodes vs 3 optimal, requests
			// [2,8,8,2]), so the planner must run candidate evaluation and reject
			// the unit — a constraint-blind planner would move A to n3, free n0, and
			// report ExecutionCompleted, so this scenario discriminates the
			// feasibility check from the old no-check behavior.
			jobA := occupyMovableVCJob(ctx, "e11-a", nodes[0], 2)
			jobB := occupy(ctx, "e11-b", nodes[2], 8) // static full peer -> B never drains
			occupy(ctx, "e11-full1", nodes[1], 8)     // rt-s0 full -> no in-domain receiver
			occupy(ctx, "e11-s3", nodes[3], 2)        // n3 has slack but sits in B's rt-s1 domain
			patchPodGroupTopologyAffinity(ctx, jobA, podGroupAntiAffinity(map[string]string{"app": "e11-b"}), map[string]string{"app": "e11-a"})
			patchPodGroupTopologyAffinity(ctx, jobB, podGroupAntiAffinity(map[string]string{"app": "e11-a"}), map[string]string{"app": "e11-b"})
			before := runningPodCount(ctx)

			run, err := newRun("e11", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("InsufficientImprovement"))
			Expect(got.Status.Plan).NotTo(BeNil())
			Expect(got.Status.Plan.Summary.FragBeforePercent).To(BeNumerically(">", 0),
				"fragmentation must be real so the feasibility check is exercised")
			Expect(len(got.Status.Plan.FreedNodes)).To(Equal(0), "no node may be freed")
			Expect(len(got.Status.Plan.Moves)).To(Equal(0), "no migration may be planned")
			Expect(runningPodCount(ctx)).To(Equal(before), "no pod may be evicted")
		})
	})

	Context("E12: SubGroup unit inherits the Job-level hard topology (US-02)", func() {
		It("keeps the SubJob pod inside the Job-entry anchored subtree", func() {
			// Job-level hard tier-3 topology, subGroupPolicy taskA declares none.
			// The SubJob unit's allowed domains are the intersection of the
			// Job-entry (anchored to rt-s3) and SubJob-entry (abstains -> whole
			// cluster) gradients, so the pod must stay within rt-s3 = {n0,n1}.
			// n2 has slack a constraint-blind planner would use, discriminating
			// the Job-entry inheritance from a naive per-SubJob view.
			job := createMovableJob(ctx, subGroupJobSpec(ctx, "e12", ptr.To(3),
				repackSubGroupTask{name: "task-a", cards: 4, reps: 1}), nodes[0])
			occupy(ctx, "e12-s1", nodes[1], 2)    // in-rt-s3 receiver (tier-3 ancestor = rt-s3)
			occupy(ctx, "e12-s2", nodes[2], 4)    // rt-s1 slack a buggy planner would use
			occupy(ctx, "e12-full3", nodes[3], 8) // rt-s1 full

			run, err := newRun("e12", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(freedSet(got.Status.Plan.FreedNodes)).To(HaveKey(nodes[0]), "taskA's source must be drained")

			waitRunningPodCount(ctx, job, 1)
			pod := runningPodsOfJob(ctx, job)[0]
			// rt-s3 anchors taskA: it may move only within {n0, n1}.
			Expect(tierDomainOfPod(ctx, pod, 3)).To(Equal("rt-s3"),
				"the SubJob pod must stay inside the Job-entry anchored subtree rt-s3, never rt-s1")
		})
	})

	Context("E13: both-moving self-harm is rejected at planning (US-02)", func() {
		It("never moves the mutually anti-affine PodGroups into one domain", func() {
			// A (rt-s0) and B (rt-s1) are mutually anti-affine and both are drain
			// targets. A's only potential receiver is rt-s1 (B's domain), so
			// freeing n0 would force a violation -> A is infeasible and stays;
			// B drains within rt-s1 (n3). This holds for either processing order.
			jobA := occupyMovableVCJob(ctx, "e13-a", nodes[0], 4)
			jobB := occupyMovableVCJob(ctx, "e13-b", nodes[2], 4)
			occupy(ctx, "e13-full1", nodes[1], 8)
			occupy(ctx, "e13-s3", nodes[3], 2) // rt-s1 receiver for B
			patchPodGroupTopologyAffinity(ctx, jobA, podGroupAntiAffinity(map[string]string{"app": "e13-b"}), map[string]string{"app": "e13-a"})
			patchPodGroupTopologyAffinity(ctx, jobB, podGroupAntiAffinity(map[string]string{"app": "e13-a"}), map[string]string{"app": "e13-b"})

			run, err := newRun("e13", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			freed := freedSet(got.Status.Plan.FreedNodes)
			Expect(freed).NotTo(HaveKey(nodes[0]),
				"freeing n0 would violate the anti-affinity and must be refused")
			Expect(freed).To(HaveKey(nodes[2]), "B must drain within its own domain")

			waitRunningPodCount(ctx, jobA, 1)
			waitRunningPodCount(ctx, jobB, 1)
			domainA := tierDomainOfPod(ctx, runningPodsOfJob(ctx, jobA)[0], 1)
			domainB := tierDomainOfPod(ctx, runningPodsOfJob(ctx, jobB)[0], 1)
			Expect(domainA).To(Equal("rt-s0"), "A must not leave its own domain")
			Expect(domainB).To(Equal("rt-s1"), "B must stay in its own domain")
		})
	})

	Context("E16: positive both-moving, each within its own domain (US-02)", func() {
		It("drains both anti-affine PodGroups and lands each in a distinct domain", func() {
			// A and B are mutually anti-affine in separate domains; each source
			// has an in-domain receiver, so both may drain independently. The
			// plan-state incremental rerun must keep them apart or Execute fails.
			jobA := occupyMovableVCJob(ctx, "e16-a", nodes[0], 4)
			jobB := occupyMovableVCJob(ctx, "e16-b", nodes[2], 4)
			occupy(ctx, "e16-s1", nodes[1], 2)
			occupy(ctx, "e16-s3", nodes[3], 2)
			patchPodGroupTopologyAffinity(ctx, jobA, podGroupAntiAffinity(map[string]string{"app": "e16-b"}), map[string]string{"app": "e16-a"})
			patchPodGroupTopologyAffinity(ctx, jobB, podGroupAntiAffinity(map[string]string{"app": "e16-a"}), map[string]string{"app": "e16-b"})

			run, err := newRun("e16", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			freed := freedSet(got.Status.Plan.FreedNodes)
			Expect(freed).To(HaveKey(nodes[0]), "A's source must be drained")
			Expect(freed).To(HaveKey(nodes[2]), "B's source must be drained")

			waitRunningPodCount(ctx, jobA, 1)
			waitRunningPodCount(ctx, jobB, 1)
			domainA := tierDomainOfPod(ctx, runningPodsOfJob(ctx, jobA)[0], 1)
			domainB := tierDomainOfPod(ctx, runningPodsOfJob(ctx, jobB)[0], 1)
			Expect(domainA).NotTo(Equal(domainB), "both-moving must still keep the PodGroups in distinct domains")
		})
	})
})

var _ = Describe("Repack HyperNode-aware constraint preservation (custom tree)", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	// A three-node tier-1 domain gives E10a/E17/E18 an in-domain receiver for a
	// same-domain gang, which the standard two-node tree cannot express; the
	// one-node rt-s1 is the decoy domain (the forbidden/other domain for
	// anti-affinity).
	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = npuFixture(ctx, 4)
		setupRepackTopologyCustom(ctx, []hyperNodeFixture{
			{name: "rt-s0", tier: 1, members: []string{nodes[0], nodes[1], nodes[2]}, memberIsNode: true},
			{name: "rt-s1", tier: 1, members: []string{nodes[3]}, memberIsNode: true},
		})
	})
	AfterEach(func() {
		e2eutil.CleanupTestContext(ctx) // also wipes this spec's HyperNodes
		for _, n := range nodes {
			clearNPU(ctx, n)
		}
	})

	Context("E10a: SubGroup affinity is preserved after repack (US-02)", func() {
		It("keeps the affinity-linked subjobs in one HyperNode domain", func() {
			// taskA/taskB are Required-affine and spread over n0/n1 (rt-s0).
			// rt-s1 is full and rt-s0's n2 fits only one 5-card pod, so the
			// affinity pair cannot co-land on a fresh node: whichever subjob
			// drains is pinned to its peer's domain rt-s0 and moves to the
			// in-domain receiver n2, the peer stays -> both remain in rt-s0.
			// Which of the pair's sources frees depends on candidate ordering,
			// but the single-domain invariant holds either way and the decoy
			// domain B never settles a pod.
			job := createMovableJob(ctx, subGroupJobSpec(ctx, "e10a", nil,
				repackSubGroupTask{name: "task-a", cards: 5, reps: 1},
				repackSubGroupTask{name: "task-b", cards: 5, reps: 1}), nodes[0], nodes[1])
			patchPodGroupTopologyAffinity(ctx, job, subGroupAffinity("task-a", "task-b"), nil)
			occupy(ctx, "e10a-s2", nodes[2], 2)    // rt-s0 in-domain receiver
			occupy(ctx, "e10a-full3", nodes[3], 8) // rt-s1 full -> no decoy receiver

			run, err := newRun("e10a", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			freed := freedSet(got.Status.Plan.FreedNodes)
			Expect(freed.Intersection(sets.New[string](nodes[0], nodes[1])).Len()).To(BeNumerically(">=", 1),
				"at least one of the affinity pair's source nodes must be drained")

			waitRunningPodCount(ctx, job, 2)
			expectSameTierDomain(ctx, job, 1)
			for _, pod := range runningPodsOfJob(ctx, job) {
				Expect(tierDomainOfPod(ctx, pod, 1)).To(Equal("rt-s0"),
					"the affinity-linked subjobs must not drift into the decoy domain rt-s1")
			}
		})
	})

	Context("E10b: SubGroup anti-affinity is preserved after repack (US-02)", func() {
		It("keeps the anti-affine subjobs in distinct HyperNode domains", func() {
			// taskA/taskB are Required-anti-affine and start in distinct domains
			// (rt-s0 and rt-s1, forced by the constraint under holdNodesExcept).
			// The drained subjob's only allowed receivers are in the other
			// subjob's complement, so the domain split survives the run.
			job := createMovableJob(ctx, subGroupJobSpec(ctx, "e10b", nil,
				repackSubGroupTask{name: "task-a", cards: 5, reps: 1},
				repackSubGroupTask{name: "task-b", cards: 5, reps: 1}), nodes[0], nodes[3])
			patchPodGroupTopologyAffinity(ctx, job, subGroupAntiAffinity("task-a", "task-b"), nil)
			occupy(ctx, "e10b-s1", nodes[1], 2)
			occupy(ctx, "e10b-s2", nodes[2], 2) // rt-s0 receivers for whichever subjob drains

			run, err := newRun("e10b", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))

			waitRunningPodCount(ctx, job, 2)
			pods := runningPodsOfJob(ctx, job)
			domainA := tierDomainOfPod(ctx, pods[0], 1)
			domainB := tierDomainOfPod(ctx, pods[1], 1)
			Expect(domainA).NotTo(Equal(domainB), "the anti-affine subjobs must stay in distinct domains")
		})
	})

	Context("E17: partial-evacuation anchor keeps the pod in its original subtree (US-02)", func() {
		It("keeps a partially-evacuated tier-1 gang inside its anchored domain", func() {
			// A hard tier-1 gang of two 5-card replicas spreads over n0 and n1
			// (rt-s0). Only one node is drained, so the gang retains a residual
			// pod and the H1 anchor keeps the migrating replica inside rt-s0 (n2
			// is the in-domain receiver) - never the decoy rt-s1, which the real
			// scheduler would also reject for a hard tier-1 gang with a residual.
			job := createMovableJob(ctx, hardTopologyJobSpec(ctx, "e17", 5, 1, 2), nodes[0], nodes[1])
			occupy(ctx, "e17-s2", nodes[2], 2)
			occupy(ctx, "e17-full3", nodes[3], 8)

			run, err := newRun("e17", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			freed := freedSet(got.Status.Plan.FreedNodes)
			Expect(freed.Intersection(sets.New[string](nodes[0], nodes[1])).Len()).To(BeNumerically(">=", 1),
				"the partially-evacuated gang must free one of its two source nodes")

			waitRunningPodCount(ctx, job, 2)
			expectSameTierDomain(ctx, job, 1)
			for _, pod := range runningPodsOfJob(ctx, job) {
				Expect(tierDomainOfPod(ctx, pod, 1)).To(Equal("rt-s0"),
					"the residual-pod anchor must keep the whole gang inside rt-s0")
			}
		})
	})

	Context("E18: a no-hard-item ==true job keeps whole-unit single-domain (US-02)", func() {
		It("keeps a SubGroupPolicy job's pods within one HyperNode domain", func() {
			// taskA carries a PartitionPolicy (so RequiresHyperNodeAllocate()==true
			// via ContainsSubJobPolicy) but no hard topology. The two pods spread
			// over n0/n1 (rt-s0). Draining one frees its node; the only receiver
			// with slack is n2 — inside rt-s0 (n3 in rt-s1 is full), so the
			// drained pod necessarily lands back in the source domain and the
			// whole unit ends in rt-s0, accepted by the real scheduler.
			//
			// Discrimination caveat: because the only feasible receiver is in the
			// source domain, this case cannot tell a correct single-domain
			// placement from a buggy multi-domain/scatter one — a scatter
			// implementation would also land on n2. Moreover, the "single domain"
			// for this soft ==true unit is ClusterTop (the whole cluster): its
			// allowed range covers both tier-1 domains, so a cross-domain receiver
			// would legitimately split the unit across rt-s0 and rt-s1 (see E19).
			// The in-domain assertion below is therefore geometry-dependent, not a
			// general invariant; the real M3 anchor discriminator needs a HARD unit
			// with a feasible cross-domain sole receiver, which E20 covers.
			job := createMovableJob(ctx, subGroupJobSpec(ctx, "e18", nil,
				repackSubGroupTask{name: "task-a", cards: 5, reps: 2}), nodes[0], nodes[1])
			occupy(ctx, "e18-s2", nodes[2], 2)
			occupy(ctx, "e18-full3", nodes[3], 8)

			run, err := newRun("e18", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			freed := freedSet(got.Status.Plan.FreedNodes)
			Expect(freed.Intersection(sets.New[string](nodes[0], nodes[1])).Len()).To(BeNumerically(">=", 1),
				"the unit must drain one of its two source nodes")

			waitRunningPodCount(ctx, job, 2)
			expectSameTierDomain(ctx, job, 1)
			for _, pod := range runningPodsOfJob(ctx, job) {
				Expect(tierDomainOfPod(ctx, pod, 1)).To(Equal("rt-s0"),
					"the ==true unit must keep both pods in a single tier-1 domain")
			}
		})

		Context("E19: soft ==true unit schedules within ClusterTop -> cross-domain move is legitimate (US-02)", func() {
			It("moves the drained pod across tier-1 domains when the only receiver is in another domain", func() {
				// A soft ==true unit (taskA carries a PartitionPolicy, so
				// RequiresHyperNodeAllocate()==true via ContainsSubJobPolicy, but no
				// hard topology) has no anchor: its allowed range is the whole
				// candidate universe, i.e. ClusterTop — the highest HyperNode that
				// contains every node. The real scheduler places such a unit anywhere
				// within the cluster, so a cross-tier-1-domain move is legitimate even
				// when the unit keeps a residual pod in rt-s0.
				//
				// rt-s0 has NO slack (n2 is full) and the only receiver with slack is
				// n3 in rt-s1 — a cross-domain sole receiver. The correct planner uses
				// it: drain n0, land the evicted pod on n3 (rt-s1), keep the residual
				// in rt-s0. Both pods stay within the unit's allowed range (ClusterTop),
				// so the run completes ExecutionCompleted rather than rejecting.
				//
				// This is the flip side of E20: the same geometry with a HARD tier-1
				// unit must reject, because the hard anchor pins the gang to rt-s0.
				// Together the pair discriminates "soft schedules cluster-wide vs hard
				// is anchored" — E18 alone cannot, since its only feasible receiver is
				// in-domain.
				job := createMovableJob(ctx, subGroupJobSpec(ctx, "e19", nil,
					repackSubGroupTask{name: "task-a", cards: 5, reps: 2}), nodes[0], nodes[1])
				occupy(ctx, "e19-full2", nodes[2], 8) // rt-s0 full -> no in-domain receiver
				occupy(ctx, "e19-s3", nodes[3], 2)    // sole receiver has slack but sits in rt-s1
				before := runningPodCount(ctx)

				run, err := newRun("e19", repackv1alpha1.RepackModeExecute).
					goal(npuResource).create(ctx)
				Expect(err).NotTo(HaveOccurred())

				got := waitTerminal(ctx, run.Name)
				Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
				Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
				freed := freedSet(got.Status.Plan.FreedNodes)
				Expect(freed.Intersection(sets.New[string](nodes[0], nodes[1])).Len()).To(BeNumerically(">=", 1),
					"the soft unit must drain one of its two source nodes")
				waitRunningPodCount(ctx, job, 2)
				Expect(runningPodCount(ctx)).To(Equal(before), "the move must not lose pods")
				domains := sets.New[string]()
				for _, pod := range runningPodsOfJob(ctx, job) {
					domains.Insert(tierDomainOfPod(ctx, pod, 1))
				}
				Expect(domains.Has("rt-s0")).To(BeTrue(), "the residual pod stays in the source domain rt-s0")
				Expect(domains.Has("rt-s1")).To(BeTrue(), "the drained pod legitimately crosses into rt-s1 (still within ClusterTop)")
			})
		})

		Context("E20: hard tier-1 unit with residual -> sole cross-domain receiver is rejected (US-02, F4)", func() {
			It("anchors the partially-evacuated hard gang to its source domain and plans no violating move", func() {
				// The real M3 anchor discriminator: E19's geometry (rt-s0 has no
				// slack, the only feasible receiver n3 sits in rt-s1) applied to a
				// HARD tier-1 unit. Only one of the two replicas is drained, so the
				// gang keeps a residual pod and the H1 anchor is NOT cleared: the
				// allowed domain narrows to rt-s0. rt-s0's only free capacity would
				// be the drained source itself (excluded as a receiver), n1 holds the
				// residual (3 free < 5), n2 is full -> no feasible in-domain receiver
				// -> the drain candidate is infeasible and no node is freed. A buggy
				// anchor-less planner would drain n0 and land the pod on n3, splitting
				// the hard gang across rt-s0/rt-s1 (which the real scheduler rejects
				// for a hard tier-1 gang with a residual), so this spec discriminates
				// anchor enforcement. Contrast E19: the same geometry with a soft
				// ==true unit legitimately crosses into rt-s1 (schedules within
				// ClusterTop).
				job := createMovableJob(ctx, hardTopologyJobSpec(ctx, "e20", 5, 1, 2), nodes[0], nodes[1])
				occupy(ctx, "e20-full2", nodes[2], 8) // rt-s0 full -> no in-domain receiver
				occupy(ctx, "e20-s3", nodes[3], 2)    // sole receiver has slack but sits in rt-s1
				before := runningPodCount(ctx)

				run, err := newRun("e20", repackv1alpha1.RepackModeExecute).
					goal(npuResource).create(ctx)
				Expect(err).NotTo(HaveOccurred())

				got := waitTerminal(ctx, run.Name)
				Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
				Expect(completeReason(got)).To(Equal("InsufficientImprovement"))
				Expect(got.Status.Plan).NotTo(BeNil())
				Expect(got.Status.Plan.Summary.FragBeforePercent).To(BeNumerically(">", 0),
					"fragmentation must be real so the anchor check is exercised")
				Expect(len(got.Status.Plan.FreedNodes)).To(Equal(0), "no node may be freed: the hard unit has no in-domain receiver")
				Expect(len(got.Status.Plan.Moves)).To(Equal(0), "no cross-domain move may be planned for a hard unit with a residual")
				Expect(runningPodCount(ctx)).To(Equal(before), "no pod may be evicted")
				waitRunningPodCount(ctx, job, 2)
			})
		})
	})
})
