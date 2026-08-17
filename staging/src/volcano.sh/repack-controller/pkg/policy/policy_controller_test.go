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

package policy

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"
)

// fakeClock implements a fixed now() for testing.
type fakeClock struct {
	t time.Time
}

func (f *fakeClock) now() time.Time { return f.t }

// testFixture holds all the pieces needed for a controller test.
type testFixture struct {
	t               *testing.T
	ctx             context.Context
	cancel          context.CancelFunc
	volcanoClient   *vcfake.Clientset
	kubeClient      *kubefake.Clientset
	informerFactory vcinformer.SharedInformerFactory
	kubeInformer    informers.SharedInformerFactory
	controller      *Controller
	clock           *fakeClock
}

// newTestFixture creates a test controller with no objects pre-loaded.
func newTestFixture(t *testing.T) *testFixture {
	t.Helper()

	volcanoClient := vcfake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()

	informerFactory := vcinformer.NewSharedInformerFactory(volcanoClient, 0)
	kubeInformer := informers.NewSharedInformerFactory(kubeClient, 0)

	clock := &fakeClock{t: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}

	ctrl := New(volcanoClient, informerFactory, kubeInformer, Options{
		Workers:                   1,
		EvalCycle:                 10 * time.Minute,
		DefaultSuccessHistoryLimit: 3,
		DefaultFailedHistoryLimit:  3,
	})
	ctrl.now = clock.now

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	return &testFixture{
		t:               t,
		ctx:             ctx,
		cancel:          cancel,
		volcanoClient:   volcanoClient,
		kubeClient:      kubeClient,
		informerFactory: informerFactory,
		kubeInformer:    kubeInformer,
		controller:      ctrl,
		clock:           clock,
	}
}

// addPolicy inserts a RepackPolicy into the fake tracker so the lister sees it.
func (f *testFixture) addPolicy(policy *repackv1alpha1.RepackPolicy) {
	f.volcanoClient.Tracker().Add(policy)
}

// addRun inserts a RepackRun into the fake tracker.
func (f *testFixture) addRun(run *repackv1alpha1.RepackRun) {
	// Use ObjectReaction tracker's Add method.
	f.volcanoClient.Tracker().Add(run)
}

// addNode inserts a Node into the kube fake tracker.
func (f *testFixture) addNode(node *v1.Node) {
	f.kubeClient.Tracker().Add(node)
}

// addPod inserts a Pod into the kube fake tracker.
func (f *testFixture) addPod(pod *v1.Pod) {
	f.kubeClient.Tracker().Add(pod)
}

// startSync starts the informer factories and waits for cache sync.
func (f *testFixture) startSync() {
	f.informerFactory.Start(f.ctx.Done())
	f.kubeInformer.Start(f.ctx.Done())

	f.informerFactory.WaitForCacheSync(f.ctx.Done())
	f.kubeInformer.WaitForCacheSync(f.ctx.Done())
}

// reconcile is a convenience wrapper that calls the controller's reconcile method.
func (f *testFixture) reconcile(key string) error {
	return f.controller.reconcile(f.ctx, key)
}

// checkConditionReason verifies the Healthy condition reason on the policy.
func (f *testFixture) checkConditionReason(policy *repackv1alpha1.RepackPolicy, want string) {
	f.t.Helper()
	for _, c := range policy.Status.Conditions {
		if c.Type == repackv1alpha1.CondHealthy {
			if c.Reason != want {
				f.t.Errorf("condition reason = %q, want %q (message: %s)", c.Reason, want, c.Message)
			}
			return
		}
	}
	f.t.Errorf("no Healthy condition found")
}

// checkInProgressLen verifies the count of in-progress entries.
func (f *testFixture) checkInProgressLen(policy *repackv1alpha1.RepackPolicy, want int) {
	f.t.Helper()
	if got := len(policy.Status.InProgress); got != want {
		f.t.Errorf("InProgress length = %d, want %d", got, want)
	}
}

// checkLastTriggerSet verifies LastTriggerTime is non-nil.
func (f *testFixture) checkLastTriggerSet(policy *repackv1alpha1.RepackPolicy) {
	f.t.Helper()
	if policy.Status.LastTriggerTime == nil {
		f.t.Error("LastTriggerTime is nil, expected non-nil")
	}
}

// checkLastTriggerNotSet verifies LastTriggerTime is nil.
func (f *testFixture) checkLastTriggerNotSet(policy *repackv1alpha1.RepackPolicy) {
	f.t.Helper()
	if policy.Status.LastTriggerTime != nil {
		f.t.Errorf("LastTriggerTime = %v, expected nil", policy.Status.LastTriggerTime)
	}
}

// --- Helpers ---

func boolPtr(v bool) *bool              { return &v }
func int32Ptr(v int32) *int32           { return &v }

// makeCronPolicy creates a minimal RepackPolicy with a cron schedule.
// It is NOT suspended by default — set Suspend=false explicitly for
// active policies.
func makeCronPolicy(name, cron string) *repackv1alpha1.RepackPolicy {
	return &repackv1alpha1.RepackPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.Now(),
			Generation:        1,
		},
		Spec: repackv1alpha1.RepackPolicySpec{
			Trigger: repackv1alpha1.RepackRunTrigger{
				CronSchedule: &cron,
			},
			RunTemplate: repackv1alpha1.RepackRunTemplateSpec{
				Spec: repackv1alpha1.RepackRunSpec{
					Mode: repackv1alpha1.RepackModeDryRun,
				},
			},
			Suspend:                     boolPtr(false),
			SuccessfulRunsHistoryLimit:  int32Ptr(3),
			FailedRunsHistoryLimit:      int32Ptr(3),
		},
	}
}

// makeCronRun creates a minimal RepackRun with the expected labels from
// the policy's cron schedule.
func makeCronRun(policyName string, triggerTime time.Time) *repackv1alpha1.RepackRun {
	name := runName(policyName, triggerTime)
	return &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				repackv1alpha1.RepackPolicyLabel:  policyName,
				repackv1alpha1.RepackTriggerLabel: "cronSchedule",
			},
		},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode: repackv1alpha1.RepackModeDryRun,
		},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
		},
	}
}

func nodeName(i int) string  { return fmt.Sprintf("node-%d", i) }
func podName(i int) string   { return fmt.Sprintf("pod-%d", i) }

// --- Tests ---

func TestReconcileSuspend(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	policy := makeCronPolicy("suspend-policy", "0 * * * *")
	*policy.Spec.Suspend = true
	f.addPolicy(policy)
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Suspended: no Run created, condition is Succeeded with "Suspended" message.
	f.checkConditionReason(policy, repackv1alpha1.ReasonReconcileSucceeded)
	f.checkInProgressLen(policy, 0)
	f.checkLastTriggerNotSet(policy)
}

func TestReconcileCronTrigger(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	// Set clock to exactly on the hour so cron "0 * * * *" fires.
	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	policy := makeCronPolicy("cron-policy", "0 * * * *")
	f.addPolicy(policy)
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	f.checkConditionReason(policy, repackv1alpha1.ReasonReconcileSucceeded)
	f.checkInProgressLen(policy, 1)
	f.checkLastTriggerSet(policy)

	expectedName := runName(policy.Name, f.clock.t)
	_, err := f.controller.runLister.Get(expectedName)
	if err != nil {
		t.Errorf("expected Run %s to be created: %v", expectedName, err)
	}
}

func TestReconcileCronDedup(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	policy := makeCronPolicy("dedup-policy", "0 * * * *")
	// LastTriggerTime already records this time window.
	policy.Status.LastTriggerTime = &metav1.Time{Time: f.clock.t}
	f.addPolicy(policy)
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Dedup: no new Run should be created.
	f.checkInProgressLen(policy, 0)
}

func TestReconcileInProgressGate(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	policy := makeCronPolicy("gate-policy", "0 * * * *")

	// Existing Run still active, tracked in inProgress.
	existingTime := f.clock.t.Add(-time.Hour)
	existing := makeCronRun(policy.Name, existingTime)
	existing.Status.Phase = repackv1alpha1.RepackRunning
	f.addRun(existing)

	policy.Status.InProgress = []v1.ObjectReference{
		{Name: existing.Name},
	}
	f.addPolicy(policy)
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Concurrency gate: no new Run because inProgress non-empty.
	f.checkInProgressLen(policy, 1) // still the existing one
	f.checkLastTriggerNotSet(policy)
}

func TestReconcileInProgressCleanup(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	policy := makeCronPolicy("cleanup-policy", "0 * * * *")

	// Succeeded Run — should be removed from inProgress and record its completion time.
	completionTime := metav1.Time{Time: f.clock.t.Add(-30 * time.Minute)}
	oldRun := makeCronRun(policy.Name, f.clock.t.Add(-2*time.Hour))
	oldRun.Status.Phase = repackv1alpha1.RepackSucceeded
	oldRun.Status.CompletionTime = &completionTime
	f.addRun(oldRun)

	policy.Status.InProgress = []v1.ObjectReference{
		{Name: oldRun.Name},
	}
	f.addPolicy(policy)
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Old succeeded Run removed, new Run created.
	f.checkInProgressLen(policy, 1)
	if policy.Status.LastSuccessfulTime == nil || !policy.Status.LastSuccessfulTime.Time.Equal(completionTime.Time) {
		t.Errorf("LastSuccessfulTime = %v, want %v", policy.Status.LastSuccessfulTime, completionTime)
	}
}

func TestReconcileFragNoTrigger(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	frag := int32(20)
	policy := makeCronPolicy("frag-nop-policy", "")
	policy.Spec.Trigger.CronSchedule = nil
	policy.Spec.Trigger.OnFragAbovePercent = &frag
	policy.Spec.RunTemplate.Spec.Goals = []repackv1alpha1.RepackGoal{
		{Resource: v1.ResourceCPU},
	}
	f.addPolicy(policy)

	// Single node with CPU — no pods → frag rate = 0 < 20%.
	f.addNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			},
		},
	})
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	f.checkConditionReason(policy, repackv1alpha1.ReasonReconcileSucceeded)
	f.checkInProgressLen(policy, 0)
}

func TestReconcileFragTriggerHit(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	frag := int32(10)
	policy := makeCronPolicy("frag-hit-policy", "")
	policy.Spec.Trigger.CronSchedule = nil
	policy.Spec.Trigger.OnFragAbovePercent = &frag
	policy.Spec.RunTemplate.Spec.Goals = []repackv1alpha1.RepackGoal{
		{Resource: v1.ResourceCPU},
	}
	f.addPolicy(policy)

	// 10 nodes × 4 CPU each. 5 nodes occupied with 1 CPU each → frag rate = 30%.
	// optimal = ceil(5000/4000) = 2 nodes. Frag = (5-2)/10*100 = 30%.
	cpu4 := resource.MustParse("4")
	cpu1 := resource.MustParse("1")
	for i := 1; i <= 10; i++ {
		f.addNode(&v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName(i)},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{
					v1.ResourceCPU: cpu4,
				},
			},
		})
	}
	for i := 1; i <= 5; i++ {
		f.addPod(&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName(i)},
			Spec: v1.PodSpec{
				NodeName: nodeName(i),
				Containers: []v1.Container{
					{Name: "c", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: cpu1}}},
				},
			},
			Status: v1.PodStatus{Phase: v1.PodRunning},
		})
	}
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	f.checkConditionReason(policy, repackv1alpha1.ReasonReconcileSucceeded)
	f.checkInProgressLen(policy, 1)
}

func TestReconcileGlobalExecuteGateBlocks(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	policy := makeCronPolicy("exec-policy", "0 * * * *")
	policy.Spec.RunTemplate.Spec.Mode = repackv1alpha1.RepackModeExecute

	// Another Execute Run still active.
	other := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-execute",
			Labels: map[string]string{
				repackv1alpha1.RepackPolicyLabel: "other-policy",
			},
		},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode: repackv1alpha1.RepackModeExecute,
		},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackPending,
		},
	}
	f.addRun(other)
	f.addPolicy(policy)
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	f.checkInProgressLen(policy, 0)
	f.checkConditionReason(policy, repackv1alpha1.ReasonReconcileSucceeded)
}

func TestReconcileGlobalExecuteGateDryRunPasses(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	policy := makeCronPolicy("dryrun-policy", "0 * * * *")
	// Mode is DryRun (already set by makeCronPolicy) — should pass even
	// when another Execute Run is active.

	other := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "active-execute",
		},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode: repackv1alpha1.RepackModeExecute,
		},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
		},
	}
	f.addRun(other)
	f.addPolicy(policy)
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// DryRun passes the global Execute gate.
	f.checkInProgressLen(policy, 1)
}

func TestReconcileAlreadyExists(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	policy := makeCronPolicy("exists-policy", "0 * * * *")
	f.addPolicy(policy)

	// Pre-create the Run with the same deterministic name.
	existingRun := makeCronRun(policy.Name, f.clock.t)
	// The fake tracker already has this Run — Create will return AlreadyExists.
	f.addRun(existingRun)

	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Should handle AlreadyExists: condition is Succeeded, Run tracked in InProgress.
	f.checkConditionReason(policy, repackv1alpha1.ReasonReconcileSucceeded)

	// The reconcile loop should append the already-existing Run to InProgress.
	f.checkInProgressLen(policy, 1)
}

func TestReconcileGC(t *testing.T) {
	f := newTestFixture(t)
	defer f.cancel()

	f.clock.t = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	policy := makeCronPolicy("gc-policy", "0 * * * *")
	policy.Spec.SuccessfulRunsHistoryLimit = int32Ptr(2)
	f.addPolicy(policy)

	// Add 3 succeeded Runs. Oldest should be deleted on reconcile.
	// The controller's reconcile also creates a new Run, but only after GC.
	// The GC itself runs before status update; it's best-effort.
	for i := 1; i <= 3; i++ {
		ts := f.clock.t.Add(-time.Duration(4-i) * time.Hour)
		r := makeCronRun(policy.Name, ts)
		r.Status.Phase = repackv1alpha1.RepackSucceeded
		r.Status.CompletionTime = &metav1.Time{Time: ts}
		f.addRun(r)
	}
	f.startSync()

	if err := f.reconcile(policy.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// List remaining runs to verify GC.
	runs, err := f.controller.runLister.List(labels.Everything())
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	var succeeded int
	for _, r := range runs {
		if r.Status.Phase == repackv1alpha1.RepackSucceeded {
			succeeded++
		}
	}
	if succeeded > 2 {
		t.Errorf("GC left %d succeeded runs, want at most 2", succeeded)
	}
}

// verify runtime.Object implementations satisfy the tracker's type assertions.
var _ runtime.Object = &repackv1alpha1.RepackPolicy{}
var _ runtime.Object = &repackv1alpha1.RepackPolicyList{}
var _ runtime.Object = &repackv1alpha1.RepackRun{}
var _ runtime.Object = &repackv1alpha1.RepackRunList{}