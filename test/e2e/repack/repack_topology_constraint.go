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

// HyperNode-level constraint-preservation e2e scenarios (US-02): build a real
// HyperNode tree over the kind workers, declare a hard HyperNode-level
// constraint on a PodGroup, run a RepackRun through the real scheduler, and
// assert the post-repack placement still satisfies the constraint.
//
// Determinism: the drain planner picks the highest-scored candidate first and
// every non-subject node is full or pinned, so only the subject unit can move;
// assertions are order-independent. Symmetric pairs (E10a/E17/E18) assert at
// least one side is freed.
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

// setupRepackTopologyCustom builds an arbitrary HyperNode tree. E10a/E17/E18 need
// a wider tier-1 domain than the standard two-node rt-s0 and E10b a one-node
// second domain, which setupRepackTopology cannot express.
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
// and waits for every replica to be Ready. No spec.nodeName, so replacements stay
// free to follow repack's live receiver selection; `hold` must force the spread.
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
			// n0 fully vacates -> anchor clear -> may land on any tier-1 HyperNode;
			// rt-s0 is full, so the receiver is in rt-s1 (the scheduler must accept it).
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
				"fully-vacated hard tier-1 pod must be allowed to land in rt-s1 (anchor clear)")
		})
	})

	Context("E9: PodGroup anti-affinity with a static peer (US-02)", func() {
		It("keeps the migrating PodGroup out of the static peer's HyperNode domain", func() {
			// B is a full static node (never drains); A is the only movable member
			// and its required anti-affinity must keep it in rt-s0 (n1 receiver).
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
			// A is Required anti-affine to static full B (n2); its only slack
			// receiver n3 sits in B's rt-s1 domain, so A must be rejected as
			// infeasible. Fragmentation is real (4 occupied vs 3 optimal), so the
			// feasibility check is exercised.
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
			// Job-level hard tier-3 topology; taskA's SubJob declares none, so its
			// allowed domain is the Job-entry anchor rt-s3={n0,n1}; n2's slack would
			// tempt a planner ignoring the Job-entry inheritance.
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
			// Mutually anti-affine A (rt-s0) and B (rt-s1), both drain targets; A's
			// only receiver is B's domain, so A is infeasible and stays while B
			// drains within rt-s1. Order-independent.
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
			// Mutually anti-affine A and B in separate domains each have an
			// in-domain receiver, so both may drain independently while staying
			// apart.
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

	// A three-node rt-s0 gives E10a/E17/E18 an in-domain receiver a two-node
	// tree cannot; the one-node rt-s1 is the decoy/forbidden domain.
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
			// Required-affine taskA/taskB spread over n0/n1 (rt-s0). They cannot
			// co-land on a fresh node, so the draining subjob is pinned to its peer's
			// domain and moves to the in-domain receiver n2; whichever source frees,
			// the single-domain invariant holds.
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
			// Required-anti-affine taskA/taskB start in distinct domains (rt-s0 and
			// rt-s1, forced under holdNodesExcept); the drained subjob may move only
			// to the other's complement, so the split survives.
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
			// Hard tier-1 gang of two 5-card replicas over n0/n1 (rt-s0). Draining
			// one node leaves a residual pod, so the anchor keeps the migrating
			// replica inside rt-s0 (n2 receiver), never the decoy rt-s1.
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
			// ==true unit (PartitionPolicy, no hard topology) spread over n0/n1
			// (rt-s0); the only slack receiver is in-domain, so the drained pod
			// lands back in rt-s0.
			//
			// Not a strong discriminator: a soft ==true unit may range over the
			// whole cluster (E19), so a scatter impl would also pass; E20 is the
			// hard-unit discriminator.
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
				// Soft ==true unit (PartitionPolicy, no hard topology) has no anchor:
				// allowed range is ClusterTop (whole cluster), so a cross-domain move
				// is legitimate even with a residual pod.
				//
				// rt-s0 has no slack (n2 full); sole receiver n3 is in rt-s1, where the
				// drained pod lands. Flip side of E20: the same geometry with a HARD
				// unit must reject, since the anchor pins the gang to rt-s0.
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
				// Real anchor discriminator: E19's geometry (no rt-s0 slack, sole
				// receiver n3 in rt-s1) with a HARD tier-1 unit. Draining one replica
				// leaves a residual, so the anchor is NOT cleared; rt-s0's only
				// capacity would be the excluded drained source -> infeasible, nothing
				// drains. An anchor-less planner would split the gang, which the
				// scheduler rejects (contrast soft E19).
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

	Context("E21: ==true whole-gang drain stays whole-or-nothing in one tier-1 domain (US-02, R28 regression)", func() {
		It("co-locates every replacement of a hard tier-1 gang on a single domain when one can host the unit", func() {
			// §4.2.4 Execute regression: rt-s0 (n2) leaves room for exactly one
			// replacement, so a per-pod reconcile would split pod1->rt-s0,
			// pod2->rt-s1. The unit must land whole on rt-s1 (n3).
			job := createMovableJob(ctx, hardTopologyJobSpec(ctx, "e21", 2, 1, 2), nodes[0])
			occupy(ctx, "e21-full1", nodes[1], 8) // rt-s0 node with no slack
			occupy(ctx, "e21-s2", nodes[2], 6)    // rt-s0 leaves exactly one 2-card pod's worth
			occupy(ctx, "e21-s3", nodes[3], 2)    // rt-s1 leaves 6 cards: the whole gang fits

			run, err := newRun("e21", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
			freed := freedSet(got.Status.Plan.FreedNodes)
			Expect(freed).To(Equal(sets.New[string](nodes[0])),
				"the whole-gang source n0 must be the only drained node")

			waitRunningPodCount(ctx, job, 2)
			expectSameTierDomain(ctx, job, 1)
			for _, pod := range runningPodsOfJob(ctx, job) {
				Expect(tierDomainOfPod(ctx, pod, 1)).To(Equal("rt-s1"),
					"the whole unit must land in rt-s1: rt-s0 cannot host the pair")
			}

			// Every SelectedNodeName must fall inside one tier-1 HyperNode.
			Expect(got.Status.Relocations).NotTo(BeEmpty())
			nodeToH := tierNodeToHyperNode(ctx, 1)
			domains := sets.New[string]()
			for _, nomination := range got.Status.Relocations {
				Expect(nomination.Placement.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
				Expect(nomination.Placement.ReplacementPodName).NotTo(BeEmpty())
				Expect(nomination.Placement.SelectedNodeName).NotTo(BeEmpty())
				Expect(nomination.Placement.ActualNodeName).To(Equal(nomination.Placement.SelectedNodeName),
					"the scheduler must bind each replacement to the node the engine chose")
				hyperNode, ok := nodeToH[nomination.Placement.SelectedNodeName]
				Expect(ok).To(BeTrue(), "SelectedNodeName %s must belong to a tier-1 HyperNode",
					nomination.Placement.SelectedNodeName)
				domains.Insert(hyperNode)
			}
			Expect(domains.Len()).To(Equal(1), "all replacement selections must share one tier-1 domain (got %v)",
				domains.UnsortedList())
			Expect(domains.Has("rt-s1")).To(BeTrue(), "the single domain must be the whole-gang receiver rt-s1")
		})

		It("gives up a hard gang whole when no single domain hosts it even though a cross-domain split would fit", func() {
			// Whole-or-nothing: no single domain hosts the gang though a split would
			// fit capacity-wise; the engine must give up whole, not plan a subset.
			// DryRun (never Execute-cooldown-serialized) still exercises the refusal.
			job := createMovableJob(ctx, hardTopologyJobSpec(ctx, "e21b", 2, 1, 2), nodes[0])
			occupy(ctx, "e21b-full1", nodes[1], 8) // rt-s0 node with no slack
			occupy(ctx, "e21b-s2", nodes[2], 6)    // rt-s0: one 2-card pod only
			occupy(ctx, "e21b-s3", nodes[3], 6)    // rt-s1: one 2-card pod only
			before := runningPodCount(ctx)

			run, err := newRun("e21b", repackv1alpha1.RepackModeDryRun).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("InsufficientImprovement"))
			Expect(got.Status.Plan).NotTo(BeNil())
			Expect(got.Status.Plan.Summary.FragBeforePercent).To(BeNumerically(">", 0),
				"fragmentation must be real so the whole-or-nothing check is exercised")
			Expect(len(got.Status.Plan.FreedNodes)).To(Equal(0), "no node may be freed for a unit no single domain can host")
			Expect(len(got.Status.Plan.Moves)).To(Equal(0),
				"no subset may be moved across domains: a split would be capacity-feasible, so this refusal proves no-split")
			Expect(len(got.Status.Relocations)).To(Equal(0),
				"no replacement of the gang may exist, so no SelectedNodeName is ever written for a subset")
			Expect(runningPodCount(ctx)).To(Equal(before), "no pod may be evicted")
			waitRunningPodCount(ctx, job, 2)
		})
	})

	Context("==true whole-gang reconcile reproduces the plan domain (empty-sibling regression)", func() {
		It("does not drift to a sibling domain that only turns feasible after eviction", func() {
			// n1 is empty: empty nodes are no drain receiver, so the plan cannot use
			// rt-s0 and must pick rt-s1. At reconcile the evicted source frees n0 but
			// n1 turns idle, making rt-s0 feasible again — a gradient first-fit would
			// drift the whole unit back onto it.
			job := createMovableJob(ctx, hardTopologyJobSpec(ctx, "e21c", 2, 1, 2), nodes[0])
			occupy(ctx, "e21c-full2", nodes[2], 8) // rt-s0's only occupied node full
			occupy(ctx, "e21c-s3", nodes[3], 2)    // rt-s1 leaves 6 cards: whole gang fits

			run, err := newRun("e21c", repackv1alpha1.RepackModeExecute).
				goal(npuResource).create(ctx)
			Expect(err).NotTo(HaveOccurred())

			got := waitTerminal(ctx, run.Name)
			Expect(got.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded))
			Expect(completeReason(got)).To(Equal("ExecutionCompleted"))
			freed := freedSet(got.Status.Plan.FreedNodes)
			Expect(freed).To(Equal(sets.New[string](nodes[0])),
				"the whole-gang source n0 must be the only drained node")

			waitRunningPodCount(ctx, job, 2)
			expectSameTierDomain(ctx, job, 1)
			for _, pod := range runningPodsOfJob(ctx, job) {
				Expect(tierDomainOfPod(ctx, pod, 1)).To(Equal("rt-s1"),
					"the whole unit must land on the planned rt-s1, not the now-idle rt-s0")
			}

			// Every SelectedNodeName stays on the plan's node and domain.
			Expect(got.Status.Relocations).NotTo(BeEmpty())
			nodeToH := tierNodeToHyperNode(ctx, 1)
			domains := sets.New[string]()
			for _, relocation := range got.Status.Relocations {
				Expect(relocation.Placement.Phase).To(Equal(repackv1alpha1.PodPlacementPlaced))
				Expect(relocation.Placement.ReplacementPodName).NotTo(BeEmpty())
				Expect(relocation.Placement.SelectedNodeName).NotTo(BeEmpty())
				Expect(relocation.Placement.SelectedNodeName).To(Equal(relocation.PlannedNodeName),
					"reconcile must not drift the selection off the plan's receiver")
				hyperNode, ok := nodeToH[relocation.Placement.SelectedNodeName]
				Expect(ok).To(BeTrue(), "SelectedNodeName %s must belong to a tier-1 HyperNode",
					relocation.Placement.SelectedNodeName)
				domains.Insert(hyperNode)
			}
			Expect(domains.Len()).To(Equal(1), "all replacement selections must share one tier-1 domain (got %v)",
				domains.UnsortedList())
			Expect(domains.Has("rt-s1")).To(BeTrue(), "the single domain must be the plan's whole-gang receiver rt-s1")
		})
	})
})
