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

// RepackPolicy e2e: the RepackPolicy controller (inside volcano-controller-manager,
// enabled by the same repack_enable flag as the lifecycle controller) derives
// RepackRuns from cron / cluster-wide fragmentation triggers and maintains the
// CronJob-style status + history. Real-pod-movement behaviour of Execute runs is
// covered by execute_lifecycle.go; these specs drive the controller's trigger and
// bookkeeping determinism, so derived runs are DryRun (engine still runs and
// reports a terminal result) and the cluster fragmentation is controlled by
// adding/removing occupying workloads rather than by relying on relocation.

package repack

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	apiextclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// Trigger source literals recorded on derived runs (protocol values shared with
// the controller; kept as strings here so the e2e module does not import the
// controller package).
const (
	policyTriggerCron = "cronSchedule"
	policyTriggerFrag = "onFragAbovePercent"
	// policyCronYearly is a valid cron that never fires again this year, so
	// schema/admission specs can create a live policy without it deriving runs.
	policyCronYearly = "0 0 1 1 *"
)

var _ = Describe("RepackPolicy CRD & controller", Serial, func() {
	var ctx *e2eutil.TestContext
	var nodes []string

	BeforeEach(func() {
		ctx = e2eutil.InitTestContext(e2eutil.Options{})
		nodes = nil
	})
	AfterEach(func() {
		recordSpecFailureDiagnostics(ctx)
		e2eutil.CleanupTestContext(ctx)
		for _, n := range nodes {
			clearNPU(ctx, n)
			clearResource(ctx, n, altNPUResource)
		}
	})

	// ---- CRD schema & admission ----------------------------------------------

	It("rejects a policy with no trigger source (CEL XValidation)", func() {
		pol := newPolicy("pol-no-trigger", repackv1alpha1.RepackModeDryRun).goal(npuResource).pol
		_, err := ctx.Vcclient.RepackV1alpha1().RepackPolicies().Create(context.TODO(), pol, metav1.CreateOptions{})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("trigger must set at least one"))
	})

	It("accepts a cron-only, an onFrag-only and a dual-source policy", func() {
		for _, pol := range []*repackv1alpha1.RepackPolicy{
			newPolicy("pol-cron-only", repackv1alpha1.RepackModeDryRun).cron(policyCronYearly).goal(npuResource).pol,
			newPolicy("pol-frag-only", repackv1alpha1.RepackModeDryRun).onFrag(0).goal(npuResource).pol,
			newPolicy("pol-both", repackv1alpha1.RepackModeDryRun).cron(policyCronYearly).onFrag(0).goal(npuResource).pol,
		} {
			created, err := createPolicy(ctx, pol)
			Expect(err).NotTo(HaveOccurred(), "policy %s must be accepted", pol.Name)
			Expect(created.UID).NotTo(BeEmpty())
		}
	})

	It("defaults suspend=false and both history limits to 3 on read-back", func() {
		pol := newPolicy("pol-defaults", repackv1alpha1.RepackModeDryRun).cron(policyCronYearly).goal(npuResource).pol
		created, err := createPolicy(ctx, pol)
		Expect(err).NotTo(HaveOccurred())
		got := getPolicy(ctx, created.Name)
		Expect(got.Spec.Suspend).NotTo(BeNil())
		Expect(*got.Spec.Suspend).To(BeFalse())
		Expect(got.Spec.SuccessfulRunsHistoryLimit).NotTo(BeNil())
		Expect(*got.Spec.SuccessfulRunsHistoryLimit).To(BeEquivalentTo(3))
		Expect(got.Spec.FailedRunsHistoryLimit).NotTo(BeNil())
		Expect(*got.Spec.FailedRunsHistoryLimit).To(BeEquivalentTo(3))
	})

	It("rejects onFragAbovePercent outside [0,100]; accepts 0 and 100", func() {
		for _, bad := range []int32{-1, 101} {
			pol := newPolicy("pol-oob", repackv1alpha1.RepackModeDryRun).onFrag(bad).goal(npuResource).pol
			_, err := ctx.Vcclient.RepackV1alpha1().RepackPolicies().Create(context.TODO(), pol, metav1.CreateOptions{})
			Expect(err).To(HaveOccurred(), "onFragAbovePercent=%d must be rejected", bad)
		}
		for _, ok := range []int32{0, 100} {
			// unique name per value: the acceptance create must succeed, so a second
			// iteration may not collide with the first
			pol := newPolicy(fmt.Sprintf("pol-bounds-%d", ok), repackv1alpha1.RepackModeDryRun).onFrag(ok).goal(npuResource).pol
			created, err := createPolicy(ctx, pol)
			Expect(err).NotTo(HaveOccurred(), "onFragAbovePercent=%d must be accepted", ok)
			Expect(created.UID).NotTo(BeEmpty())
		}
	})

	It("rejects a cronSchedule carrying TZ/CRON_TZ (no timeZone field to carry it)", func() {
		// Mirrors upstream CronJob's create rule: a schedule embedding a zone is
		// only meaningful with a timeZone field, which RepackPolicy has none of.
		for _, sched := range []string{"TZ=UTC 0 0 * * *", "CRON_TZ=Asia/Shanghai 0 0 * * *"} {
			pol := newPolicy("pol-tz", repackv1alpha1.RepackModeDryRun).cron(sched).goal(npuResource).pol
			_, err := ctx.Vcclient.RepackV1alpha1().RepackPolicies().Create(context.TODO(), pol, metav1.CreateOptions{})
			Expect(err).To(HaveOccurred(), "cronSchedule %q must be rejected at admission", sched)
			Expect(err.Error()).To(ContainSubstring("cannot contain TZ or CRON_TZ"))
		}
	})

	It("installs the STATUS printer column with the Healthy filter", func() {
		// The kubernetes Clientset does not expose apiextensions, so read the CRD
		// through the apiextensions clientset built from the same kubeconfig.
		config, err := clientcmd.BuildConfigFromFlags(e2eutil.MasterURL(), e2eutil.KubeconfigPath(e2eutil.HomeDir()))
		Expect(err).NotTo(HaveOccurred())
		extClient := apiextclientset.NewForConfigOrDie(config)
		crd, err := extClient.ApiextensionsV1().CustomResourceDefinitions().Get(
			context.TODO(), "repackpolicies.repack.volcano.sh", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		found := false
		for _, version := range crd.Spec.Versions {
			for _, column := range version.AdditionalPrinterColumns {
				if column.Name == "STATUS" && strings.Contains(column.JSONPath, `?(@.type=="Healthy")`) {
					found = true
				}
			}
		}
		Expect(found).To(BeTrue(), "CRD must carry the STATUS print column over conditions[?(@.type==\"Healthy\")].reason")
	})

	// ---- cron trigger loop & history GC --------------------------------------

	It("derives a cron run each slot, converges status, and history-GCs to the limit", func() {
		nodes = npuFixture(ctx, 1)
		pol, err := createPolicy(ctx, newPolicy("pol-cron-loop", repackv1alpha1.RepackModeDryRun).
			cron("* * * * *").goal(npuResource).successLimit(1).pol)
		Expect(err).NotTo(HaveOccurred())

		// First slot: a derived run appears within one minute, is labelled by its
		// source, completes, and the policy converges (inProgress cleared, snapshot
		// written, Healthy).
		r1 := waitForFirstRun(ctx, pol.Name)
		Expect(r1.Labels[repackv1alpha1.RepackTriggerLabel]).To(Equal(policyTriggerCron))
		Expect(r1.Labels[repackv1alpha1.RepackPolicyLabel]).To(Equal(pol.Name))
		Expect(ownedByUID(ctx, r1.Name, pol.UID)).To(BeTrue(), "derived run must carry the policy owner reference")
		waitTerminal(ctx, r1.Name)
		Eventually(func() bool {
			got := getPolicy(ctx, pol.Name)
			return len(got.Status.InProgress) == 0 &&
				got.Status.LastRunStatus != nil &&
				got.Status.LastRunStatus.Name == r1.Name &&
				healthyReason(got) == repackv1alpha1.ReasonReconcileSucceeded
		}, repackTimeout, repackPoll).Should(BeTrue(), "policy must converge after the first run")

		// Second slot: a later cron slot derives a replacement run. Under history
		// limit 1 the first run is recycled as soon as the second reaches terminal
		// (DryRun finishes in ms and GC trims on the next reconcile), so the two
		// never coexist at poll granularity — assert the second incarnation by the
		// surviving run's name rotating past r1, not by counting coexisting runs.
		var survivor string
		Eventually(func() string {
			got := getPolicy(ctx, pol.Name)
			if len(got.Status.InProgress) != 0 || got.Status.LastRunStatus == nil ||
				got.Status.LastRunStatus.Name == r1.Name ||
				healthyReason(got) != repackv1alpha1.ReasonReconcileSucceeded {
				return ""
			}
			names := policyRunNames(ctx, pol.Name)
			if len(names) != 1 {
				return ""
			}
			survivor = names[0]
			return survivor
		}, repackTimeout, repackPoll).ShouldNot(BeEmpty(),
			"a later cron slot must converge the policy to a single replacement run (first was %s)", r1.Name)
		r2, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(context.TODO(), survivor, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(r2.Name).NotTo(Equal(r1.Name))
		Expect(r2.Labels[repackv1alpha1.RepackTriggerLabel]).To(Equal(policyTriggerCron))
		Expect(r2.Status.Phase).To(Equal(repackv1alpha1.RepackSucceeded),
			"the surviving run is the newest terminal run (history GC kept only it)")
	})

	It("suspend freezes derivation; resume derives at most one run", func() {
		nodes = npuFixture(ctx, 1)
		pol, err := createPolicy(ctx, newPolicy("pol-suspend", repackv1alpha1.RepackModeDryRun).
			cron("* * * * *").goal(npuResource).suspend(true).pol)
		Expect(err).NotTo(HaveOccurred())

		// Hold suspension across at least one cron slot: nothing is derived.
		time.Sleep(75 * time.Second)
		Expect(policyRunNames(ctx, pol.Name)).To(BeEmpty(), "a suspended policy must not derive runs")

		// Resume: the controller catches up at most one missed slot, at now — not a
		// per-slot timestamp replay.
		got := getPolicy(ctx, pol.Name)
		got.Spec.Suspend = boolPtr(false)
		_, err = ctx.Vcclient.RepackV1alpha1().RepackPolicies().Update(context.TODO(), got, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		runs := waitForPolicyRuns(ctx, pol.Name, 1)
		Expect(runs[0].Labels[repackv1alpha1.RepackTriggerLabel]).To(Equal(policyTriggerCron))
		// No burst of backfilled runs: until the next slot the count stays at one.
		Consistently(func() int { return len(policyRunNames(ctx, pol.Name)) }, 15*time.Second, repackPoll).
			Should(BeEquivalentTo(1), "resume must derive exactly one run, not one per missed slot")
	})

	// ---- onFrag trigger: threshold, label, self-stop -------------------------

	It("does not fire at the exact threshold, fires above it, and stops once fragmentation drops", func() {
		nodes = npuFixture(ctx, 2)
		// Deterministic fragmented layout (4/8 on each node -> occupied=2,
		// optimal=1, rate 0.5) using movable fixtures so the derived DryRun run has
		// a well-formed engine outcome.
		jobA := occupyMovableVCJob(ctx, "pol-frag-a", nodes[0], 4)
		jobB := occupyMovableVCJob(ctx, "pol-frag-b", nodes[1], 4)

		// Boundary: rate == 50% must not fire (strictly-greater rule). Two eval
		// cycles of 30s each pass with no run.
		atBoundary, err := createPolicy(ctx, newPolicy("pol-frag-boundary", repackv1alpha1.RepackModeDryRun).
			onFrag(50).goal(npuResource).pol)
		Expect(err).NotTo(HaveOccurred())
		time.Sleep(75 * time.Second)
		Expect(policyRunNames(ctx, atBoundary.Name)).To(BeEmpty(),
			"fragmentation exactly equal to the threshold must not trigger")

		// Strictly above: a labelled DryRun run is derived promptly.
		above, err := createPolicy(ctx, newPolicy("pol-frag-above", repackv1alpha1.RepackModeDryRun).
			onFrag(40).goal(npuResource).pol)
		Expect(err).NotTo(HaveOccurred())
		r1 := waitForFirstRun(ctx, above.Name)
		Expect(r1.Labels[repackv1alpha1.RepackTriggerLabel]).To(Equal(policyTriggerFrag))
		waitTerminal(ctx, r1.Name)

		// Clearing the occupying workloads drops fragmentation below the threshold;
		// the level trigger must then stay quiet (no self-sustaining loop).
		_ = ctx.Vcclient.BatchV1alpha1().Jobs(ctx.Namespace).Delete(context.TODO(), jobA.Name, metav1.DeleteOptions{})
		_ = ctx.Vcclient.BatchV1alpha1().Jobs(ctx.Namespace).Delete(context.TODO(), jobB.Name, metav1.DeleteOptions{})
		time.Sleep(75 * time.Second)
		Expect(policyRunNames(ctx, above.Name)).To(HaveLen(1),
			"onFrag must stop deriving once fragmentation is below the threshold")
	})

	// ---- deletion cascade & TTL / history GC coexistence ---------------------

	It("cascades deletion to derived runs via the owner reference", func() {
		nodes = npuFixture(ctx, 1)
		pol, err := createPolicy(ctx, newPolicy("pol-cascade", repackv1alpha1.RepackModeDryRun).
			cron("* * * * *").goal(npuResource).pol)
		Expect(err).NotTo(HaveOccurred())
		r1 := waitForFirstRun(ctx, pol.Name)
		waitTerminal(ctx, r1.Name)

		deletePolicy(ctx, pol.Name)
		Eventually(func() bool {
			runs, listErr := ctx.Vcclient.RepackV1alpha1().RepackRuns().List(context.TODO(), metav1.ListOptions{})
			if listErr != nil {
				return false
			}
			for i := range runs.Items {
				if runs.Items[i].Labels[repackv1alpha1.RepackPolicyLabel] == pol.Name {
					return false
				}
			}
			return true
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"deleting the policy must cascade-delete its derived runs (foreground ownerRef)")
	})

	It("lets a derived run's TTL delete it before the policy history limit is reached", func() {
		nodes = npuFixture(ctx, 1)
		// History limit 10 is never reached by the single run this spec observes, so
		// only the run-side TTL GC (not the policy's history GC) can remove it.
		pol, err := createPolicy(ctx, newPolicy("pol-ttl", repackv1alpha1.RepackModeDryRun).
			cron("* * * * *").goal(npuResource).successLimit(10).ttl(8).pol)
		Expect(err).NotTo(HaveOccurred())
		r1 := waitForFirstRun(ctx, pol.Name)
		waitTerminal(ctx, r1.Name)

		Eventually(func() bool {
			_, getErr := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(context.TODO(), r1.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(getErr)
		}, repackTimeout, repackPoll).Should(BeTrue(),
			"finished run with ttlSecondsAfterFinished must be removed by the run TTL GC")

		// The policy snapshot survives the run's TTL deletion: coexistence means the
		// history GC would have kept it (limit not reached) yet TTL still won.
		got := getPolicy(ctx, pol.Name)
		Expect(got.Status.LastRunStatus).NotTo(BeNil())
		Expect(got.Status.LastRunStatus.Name).To(Equal(r1.Name))
	})
})

// ---- policy helpers --------------------------------------------------------

type policyBuilder struct{ pol *repackv1alpha1.RepackPolicy }

func newPolicy(name string, mode repackv1alpha1.RepackMode) *policyBuilder {
	return &policyBuilder{pol: &repackv1alpha1.RepackPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: repackv1alpha1.RepackPolicySpec{
			RunTemplate: repackv1alpha1.RepackRunTemplateSpec{
				Spec: repackv1alpha1.RepackRunSpec{Mode: mode},
			},
		},
	}}
}
func (b *policyBuilder) goal(res v1.ResourceName) *policyBuilder {
	b.pol.Spec.RunTemplate.Spec.Goals = []repackv1alpha1.RepackGoal{{Resource: res}}
	return b
}
func (b *policyBuilder) cron(expr string) *policyBuilder {
	b.pol.Spec.Trigger.CronSchedule = &expr
	return b
}
func (b *policyBuilder) onFrag(percent int32) *policyBuilder {
	b.pol.Spec.Trigger.OnFragAbovePercent = &percent
	return b
}
func (b *policyBuilder) suspend(v bool) *policyBuilder {
	b.pol.Spec.Suspend = &v
	return b
}
func (b *policyBuilder) successLimit(v int32) *policyBuilder {
	b.pol.Spec.SuccessfulRunsHistoryLimit = &v
	return b
}
func (b *policyBuilder) ttl(seconds int64) *policyBuilder {
	b.pol.Spec.RunTemplate.Spec.TTLSecondsAfterFinished = &seconds
	return b
}

func createPolicy(ctx *e2eutil.TestContext, pol *repackv1alpha1.RepackPolicy) (*repackv1alpha1.RepackPolicy, error) {
	created, err := ctx.Vcclient.RepackV1alpha1().RepackPolicies().Create(context.TODO(), pol, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	DeferCleanup(func() { deletePolicy(ctx, created.Name) })
	return created, nil
}

func deletePolicy(ctx *e2eutil.TestContext, name string) {
	_ = ctx.Vcclient.RepackV1alpha1().RepackPolicies().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

func getPolicy(ctx *e2eutil.TestContext, name string) *repackv1alpha1.RepackPolicy {
	pol, err := ctx.Vcclient.RepackV1alpha1().RepackPolicies().Get(context.TODO(), name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return pol
}

func policyRunNames(ctx *e2eutil.TestContext, policyName string) []string {
	runs, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().List(context.TODO(), metav1.ListOptions{
		LabelSelector: repackv1alpha1.RepackPolicyLabel + "=" + policyName,
	})
	Expect(err).NotTo(HaveOccurred())
	names := make([]string, 0, len(runs.Items))
	for i := range runs.Items {
		names = append(names, runs.Items[i].Name)
	}
	return names
}

// waitForFirstRun returns the first derived run (appears within one cron slot).
func waitForFirstRun(ctx *e2eutil.TestContext, policyName string) *repackv1alpha1.RepackRun {
	return waitForPolicyRuns(ctx, policyName, 1)[0]
}

func waitForPolicyRuns(ctx *e2eutil.TestContext, policyName string, want int) []*repackv1alpha1.RepackRun {
	var last []*repackv1alpha1.RepackRun
	Eventually(func() int {
		runs, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().List(context.TODO(), metav1.ListOptions{
			LabelSelector: repackv1alpha1.RepackPolicyLabel + "=" + policyName,
		})
		if err != nil {
			return 0
		}
		last = nil
		for i := range runs.Items {
			last = append(last, runs.Items[i].DeepCopy())
		}
		return len(last)
	}, repackTimeout, repackPoll).Should(BeNumerically(">=", want),
		"policy %s never derived %d run(s)", policyName, want)
	return last
}

func ownedByUID(ctx *e2eutil.TestContext, runName string, uid types.UID) bool {
	r, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(context.TODO(), runName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, owner := range r.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.UID == uid {
			return true
		}
	}
	return false
}

func healthyReason(pol *repackv1alpha1.RepackPolicy) string {
	for i := range pol.Status.Conditions {
		if pol.Status.Conditions[i].Type == repackv1alpha1.CondHealthy {
			return pol.Status.Conditions[i].Reason
		}
	}
	return ""
}

func boolPtr(v bool) *bool { return &v }
