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

// Package repack owns the RepackRun lifecycle controller, the replacement Pod
// nominator and the RepackPolicy controller, plus the state machine, placement
// protocol and fragmentation model they share with the repack engine. It is
// deliberately framework-light: plain client-go informers + workqueue, depending
// only on the CRD types/generated client (volcano.sh/apis) and the pure decision
// logic in ./state. It owns admission, phase/conditions projection,
// active-deadline and TTL GC. Execute serialization (one-at-a-time + cooldown)
// lives in the engine — the worker that actually evicts — not here. It does NOT
// open a scheduler Session or move pods; planning/eviction is the
// volcano-repack-engine's job. The two communicate only through the RepackRun
// object.
package repack

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcinformers "volcano.sh/apis/pkg/client/informers/externalversions"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"

	"volcano.sh/volcano/pkg/controllers/repack/state"
)

// Options are operator-level knobs not carried on individual RepackRuns.
type Options struct {
	// Workers is the number of reconcile workers (default 1).
	Workers int
	// ExecuteCooldown is the minimum gap the engine enforces after an Execute run
	// finishes before the next may start. GC keeps a finished Execute run alive at
	// least this long so its completionTime survives as the engine's cooldown
	// anchor (defends against TTL < cooldown). <=0 defaults to
	// state.DefaultExecuteCooldown; keep in sync with the engine's flag.
	ExecuteCooldown time.Duration
}

// Controller reconciles RepackRun objects.
type Controller struct {
	volcanoClient           vcclientset.Interface
	repackRunLister         repacklisters.RepackRunLister
	repackRunInformerSynced cache.InformerSynced
	workQueue               workqueue.TypedRateLimitingInterface[string]

	informerFactory vcinformers.SharedInformerFactory
	options         Options
	// executeCooldown is the GC retention floor for finished Execute runs.
	executeCooldown time.Duration
	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// New builds a Controller wired to the given clientset and shared informer
// factory. The caller owns starting the factory and the lifecycle context.
func New(volcanoClient vcclientset.Interface, informerFactory vcinformers.SharedInformerFactory, options Options) *Controller {
	if options.Workers < 1 {
		options.Workers = 1
	}
	if options.ExecuteCooldown <= 0 {
		options.ExecuteCooldown = state.DefaultExecuteCooldown
	}
	informer := informerFactory.Repack().V1alpha1().RepackRuns()
	c := &Controller{
		volcanoClient:           volcanoClient,
		repackRunLister:         informer.Lister(),
		repackRunInformerSynced: informer.Informer().HasSynced,
		workQueue:               workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		informerFactory:         informerFactory,
		options:                 options,
		executeCooldown:         options.ExecuteCooldown,
		now:                     time.Now,
	}
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, newObj interface{}) { c.enqueue(newObj) },
		DeleteFunc: c.enqueue,
	})
	return c
}

// enqueue maps an object to its workqueue key (cluster-scoped: the name).
func (c *Controller) enqueue(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("repackrun key: %w", err))
		return
	}
	c.workQueue.Add(key)
}

// Run starts the factory, waits for cache sync, and launches workers until ctx
// is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer c.workQueue.ShutDown()

	c.informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.repackRunInformerSynced) {
		return fmt.Errorf("repackrun controller: cache failed to sync")
	}
	klog.V(3).InfoS("Starting repackrun controller", "workers", c.options.Workers, "executeCooldown", c.executeCooldown)

	for i := 0; i < c.options.Workers; i++ {
		go func() {
			for c.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	klog.V(3).InfoS("Shutting down repackrun controller")
	return nil
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.workQueue.Get()
	if shutdown {
		return false
	}
	defer c.workQueue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("reconcile repackrun %q: %w", key, err))
		klog.V(4).InfoS("repack controller: reconcile failed; requeueing with rate limit",
			"run", key, "retryCount", c.workQueue.NumRequeues(key)+1, "error", err)
		c.workQueue.AddRateLimited(key)
		return true
	}
	c.workQueue.Forget(key)
	return true
}

// reconcile handles only RepackRun GC: a finished run is deleted once its TTL
// elapses (or requeued precisely at expiry). Admission is enforced by CEL at the
// apiserver; the engine owns all non-terminal lifecycle (phase/conditions); the
// nomination reconciler (nominate.go) steers replacement pods.
func (c *Controller) reconcile(ctx context.Context, name string) error {
	klog.V(4).InfoS("repack controller: reconciling RepackRun lifecycle", "run", name)
	run, err := c.repackRunLister.Get(name)
	if apierrors.IsNotFound(err) {
		return nil // deleted; nothing to do
	}
	if err != nil {
		return err
	}
	if !state.IsTerminal(run.Status.Phase) {
		return nil // engine owns non-terminal runs
	}
	now := c.now()
	// Preserve the cooldown anchor: never delete a finished Execute run while its
	// completionTime + cooldown window is still open, even if TTL already elapsed.
	// The engine (which rebuilds the anchor from persisted completionTime after a
	// restart) would otherwise forget the cooldown and admit the next Execute early.
	if state.CooldownRetained(run, c.executeCooldown, now) {
		if d := state.CooldownRemaining(run, c.executeCooldown, now); d > 0 {
			klog.V(4).InfoS("GC: retaining finished Execute run as cooldown anchor", "name", name, "retainFor", d)
			c.workQueue.AddAfter(name, d) // revisit right when the window lifts
		}
		return nil
	}
	if state.TTLExpired(run, now) {
		klog.V(3).InfoS("GC: deleting expired RepackRun", "name", name, "phase", run.Status.Phase)
		return ignoreNotFound(c.volcanoClient.RepackV1alpha1().RepackRuns().Delete(ctx, name, metav1.DeleteOptions{}))
	}
	if d := ttlRemaining(run, now); d > 0 {
		klog.V(5).InfoS("GC: run not yet expired, requeueing at TTL", "name", name, "ttlRemaining", d)
		c.workQueue.AddAfter(name, d)
	}
	return nil
}

func ttlRemaining(run *repackv1alpha1.RepackRun, now time.Time) time.Duration {
	if run.Spec.TTLSecondsAfterFinished == nil || run.Status.CompletionTime == nil {
		return 0
	}
	deadline := run.Status.CompletionTime.Time.Add(time.Duration(*run.Spec.TTLSecondsAfterFinished) * time.Second)
	return deadline.Sub(now)
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
