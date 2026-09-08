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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

const testResource = corev1.ResourceName("example.com/gpu")

type scheduledWake struct {
	key   string
	after time.Duration
}

// fixture wires a Controller to fake clients and plain indexer-backed listers so
// reconcile can be driven without informer machinery, following the staging
// repack-controller test style. clock and scheduled are the seams reconcile
// observes.
type fixture struct {
	t    *testing.T
	vc   *vcfake.Clientset
	c    *Controller
	pol  cache.Indexer
	run  cache.Indexer
	node cache.Indexer
	pod  cache.Indexer

	clock     time.Time
	scheduled []scheduledWake
}

func newFixture(t *testing.T, now time.Time, evalCycle time.Duration) *fixture {
	f := &fixture{
		t:     t,
		vc:    vcfake.NewSimpleClientset(),
		pol:   cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}),
		run:   cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}),
		node:  cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}),
		pod:   cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}),
		clock: now,
	}
	f.c = &Controller{
		vcClient:     f.vc,
		policyLister: repacklisters.NewRepackPolicyLister(f.pol),
		runLister:    repacklisters.NewRepackRunLister(f.run),
		nodeLister:   corelisters.NewNodeLister(f.node),
		podLister:    corelisters.NewPodLister(f.pod),
		queue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		opts:         Options{FragEvalCycle: evalCycle},
		now:          func() time.Time { return f.clock },
		scheduleNext: func(key string, after time.Duration) {
			f.scheduled = append(f.scheduled, scheduledWake{key: key, after: after})
		},
	}
	f.c.opts.applyDefaults()
	return f
}

// addPolicy seeds the same policy into both the fake API and the lister store.
func (f *fixture) addPolicy(pol *repackv1alpha1.RepackPolicy) {
	f.t.Helper()
	if _, err := f.vc.RepackV1alpha1().RepackPolicies().Create(context.Background(), pol.DeepCopy(), metav1.CreateOptions{}); err != nil {
		f.t.Fatalf("seed policy %s: %v", pol.Name, err)
	}
	if err := f.pol.Add(pol.DeepCopy()); err != nil {
		f.t.Fatalf("seed policy indexer %s: %v", pol.Name, err)
	}
}

// addRun seeds the same run into both the fake API and the lister store.
func (f *fixture) addRun(run *repackv1alpha1.RepackRun) {
	f.t.Helper()
	if _, err := f.vc.RepackV1alpha1().RepackRuns().Create(context.Background(), run.DeepCopy(), metav1.CreateOptions{}); err != nil {
		f.t.Fatalf("seed run %s: %v", run.Name, err)
	}
	if err := f.run.Add(run.DeepCopy()); err != nil {
		f.t.Fatalf("seed run indexer %s: %v", run.Name, err)
	}
}

// getPolicy reads back the persisted policy (with reconcile-written status).
func (f *fixture) getPolicy(name string) *repackv1alpha1.RepackPolicy {
	f.t.Helper()
	pol, err := f.vc.RepackV1alpha1().RepackPolicies().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatalf("get policy %s: %v", name, err)
	}
	return pol
}

func (f *fixture) getRun(name string) (*repackv1alpha1.RepackRun, error) {
	return f.vc.RepackV1alpha1().RepackRuns().Get(context.Background(), name, metav1.GetOptions{})
}

// reconcile drives one reconcile and then mirrors the fake-client writes (status
// update, run creation) back into the listers, standing in for the informer that
// would deliver them between reconciles in production. Sequential reconciles in a
// test therefore observe each other's effects.
func (f *fixture) reconcile(name string) {
	f.t.Helper()
	if err := f.c.reconcile(context.Background(), name); err != nil {
		f.t.Fatalf("reconcile(%s): %v", name, err)
	}
	f.refreshFromFake()
}

// refreshFromFake re-lists policies and runs from the fake clientset and reloads
// the two listers so a later reconcile sees created runs and status updates.
func (f *fixture) refreshFromFake() {
	f.t.Helper()
	pols, err := f.vc.RepackV1alpha1().RepackPolicies().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		f.t.Fatalf("list policies for refresh: %v", err)
	}
	polIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for i := range pols.Items {
		if err := polIndexer.Add(pols.Items[i].DeepCopy()); err != nil {
			f.t.Fatalf("reload policy %s: %v", pols.Items[i].Name, err)
		}
	}
	f.pol = polIndexer

	runs, err := f.vc.RepackV1alpha1().RepackRuns().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		f.t.Fatalf("list runs for refresh: %v", err)
	}
	runIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for i := range runs.Items {
		if err := runIndexer.Add(runs.Items[i].DeepCopy()); err != nil {
			f.t.Fatalf("reload run %s: %v", runs.Items[i].Name, err)
		}
	}
	f.run = runIndexer

	f.c.policyLister = repacklisters.NewRepackPolicyLister(f.pol)
	f.c.runLister = repacklisters.NewRepackRunLister(f.run)
}

func (f *fixture) listPolicyRuns(name string) []*repackv1alpha1.RepackRun {
	runs, err := f.c.runLister.List(labels.SelectorFromSet(labels.Set{repackv1alpha1.RepackPolicyLabel: name}))
	if err != nil {
		f.t.Fatalf("list runs: %v", err)
	}
	return runs
}

func (f *fixture) hasScheduled(key string) bool {
	for _, w := range f.scheduled {
		if w.key == key {
			return true
		}
	}
	return false
}

func (f *fixture) lastScheduled(key string) (time.Duration, bool) {
	for i := len(f.scheduled) - 1; i >= 0; i-- {
		if f.scheduled[i].key == key {
			return f.scheduled[i].after, true
		}
	}
	return 0, false
}

func (f *fixture) assertHealthy(name, reason, wantSub string) {
	f.t.Helper()
	cond := healthyCondition(f.getPolicy(name))
	if cond == nil {
		f.t.Fatalf("policy %s has no Healthy condition", name)
	}
	if cond.Reason != reason {
		f.t.Errorf("policy %s Healthy reason = %q, want %q", name, cond.Reason, reason)
	}
	if wantSub != "" && !strings.Contains(cond.Message, wantSub) {
		f.t.Errorf("policy %s message %q missing %q", name, cond.Message, wantSub)
	}
}

func (f *fixture) assertUnhealthy(name, reason, wantSub string) {
	f.t.Helper()
	cond := healthyCondition(f.getPolicy(name))
	if cond == nil {
		f.t.Fatalf("policy %s has no Healthy condition", name)
	}
	if cond.Status != metav1.ConditionFalse {
		f.t.Errorf("policy %s Healthy status = %s, want False", name, cond.Status)
	}
	if cond.Reason != reason {
		f.t.Errorf("policy %s Healthy reason = %q, want %q", name, cond.Reason, reason)
	}
	if wantSub != "" && !strings.Contains(cond.Message, wantSub) {
		f.t.Errorf("policy %s message %q missing %q", name, cond.Message, wantSub)
	}
}

func healthyCondition(pol *repackv1alpha1.RepackPolicy) *metav1.Condition {
	for i := range pol.Status.Conditions {
		if pol.Status.Conditions[i].Type == repackv1alpha1.CondHealthy {
			return &pol.Status.Conditions[i]
		}
	}
	return nil
}

// --- object builders -----------------------------------------------------

func policy(name string, uid string, created time.Time) *repackv1alpha1.RepackPolicy {
	return &repackv1alpha1.RepackPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			UID:               types.UID(uid),
			CreationTimestamp: metav1.NewTime(created),
			Generation:        1,
		},
		Spec: repackv1alpha1.RepackPolicySpec{
			RunTemplate: repackv1alpha1.RepackRunTemplateSpec{
				Spec: repackv1alpha1.RepackRunSpec{
					Mode:  repackv1alpha1.RepackModeExecute,
					Goals: []repackv1alpha1.RepackGoal{{Resource: testResource}},
				},
			},
		},
	}
}

func withCron(pol *repackv1alpha1.RepackPolicy, expr string) *repackv1alpha1.RepackPolicy {
	pol.Spec.Trigger.CronSchedule = ptr.To(expr)
	return pol
}

func withOnFrag(pol *repackv1alpha1.RepackPolicy, percent int32) *repackv1alpha1.RepackPolicy {
	pol.Spec.Trigger.OnFragAbovePercent = ptr.To(percent)
	return pol
}

func withSuspend(pol *repackv1alpha1.RepackPolicy, suspend bool) *repackv1alpha1.RepackPolicy {
	pol.Spec.Suspend = ptr.To(suspend)
	return pol
}

func withHistoryLimits(pol *repackv1alpha1.RepackPolicy, success, failed int32) *repackv1alpha1.RepackPolicy {
	pol.Spec.SuccessfulRunsHistoryLimit = ptr.To(success)
	pol.Spec.FailedRunsHistoryLimit = ptr.To(failed)
	return pol
}

func withMode(pol *repackv1alpha1.RepackPolicy, mode repackv1alpha1.RepackMode) *repackv1alpha1.RepackPolicy {
	pol.Spec.RunTemplate.Spec.Mode = mode
	return pol
}

func withNoGoals(pol *repackv1alpha1.RepackPolicy) *repackv1alpha1.RepackPolicy {
	pol.Spec.RunTemplate.Spec.Goals = nil
	return pol
}

// run builds a derived run carrying the reserved labels and the policy owner
// reference that the controller itself would attach.
func run(name, policyName, policyUID, trigger string, mode repackv1alpha1.RepackMode, created time.Time) *repackv1alpha1.RepackRun {
	return &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Labels:            map[string]string{repackv1alpha1.RepackPolicyLabel: policyName, repackv1alpha1.RepackTriggerLabel: trigger},
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "repack.volcano.sh/v1alpha1", Kind: "RepackPolicy", Name: policyName, UID: types.UID(policyUID), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(false)}},
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode:  mode,
			Goals: []repackv1alpha1.RepackGoal{{Resource: testResource}},
		},
	}
}

func withRunPhase(run *repackv1alpha1.RepackRun, phase repackv1alpha1.RepackPhase, completion *time.Time) *repackv1alpha1.RepackRun {
	run.Status.Phase = phase
	if completion != nil {
		t := metav1.NewTime(*completion)
		run.Status.CompletionTime = &t
	}
	return run
}

// node & pod builders for fragmentation measurement. Requests/capacity are in
// device counts; measureFrag reads them via MilliValue, matching the engine's
// milli-unit scalar convention.
func addNode(f *fixture, name string, devices int64) {
	f.t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{testResource: *resource.NewQuantity(devices, resource.DecimalSI)},
		},
	}
	if err := f.node.Add(node); err != nil {
		f.t.Fatalf("seed node %s: %v", name, err)
	}
}

func addPod(f *fixture, name, nodeName string, phase corev1.PodPhase, devices int64) {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{testResource: *resource.NewQuantity(devices, resource.DecimalSI)},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if err := f.pod.Add(pod); err != nil {
		f.t.Fatalf("seed pod %s: %v", name, err)
	}
}

// halfFragCluster is the canonical half-fragmented cluster shared by the frag
// unit tests and the onFrag threshold paths: providing=2, occupied=2, optimal=1,
// rate exactly 0.5 (the strict-greater boundary probe).
func halfFragCluster(f *fixture) {
	addNode(f, "n1", 8)
	addNode(f, "n2", 8)
	addPod(f, "p1", "n1", corev1.PodRunning, 4)
	addPod(f, "p2", "n2", corev1.PodRunning, 4)
}

func notFoundErr(err error) bool {
	return apierrors.IsNotFound(err)
}

func timePtr(t time.Time) *time.Time { return &t }

func listRunNames(f *fixture) []string {
	items, err := f.vc.RepackV1alpha1().RepackRuns().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		f.t.Fatalf("list repackruns: %v", err)
	}
	names := make([]string, 0, len(items.Items))
	for i := range items.Items {
		names = append(names, items.Items[i].Name)
	}
	return names
}
