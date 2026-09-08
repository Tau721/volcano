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

// Package policy implements the RepackPolicy controller: a RepackRun template
// generator (CronJob->Job pattern) fired by a cron schedule and/or a
// cluster-wide fragmentation threshold.
package policy

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"

	repackstate "volcano.sh/repack-controller/pkg/state"
)

// Trigger source literals, recorded in the derived run's RepackTriggerLabel and
// in status.lastRunStatus.trigger.
const (
	TriggerCronSchedule = "cronSchedule"
	TriggerOnFrag       = "onFragAbovePercent"
)

// Controller reconciles RepackPolicy objects. All objects are cluster-scoped,
// so workqueue keys are plain names.
type Controller struct {
	opts         Options
	vcClient     vcclientset.Interface
	vcFactory    vcinformer.SharedInformerFactory
	coreFactory  informers.SharedInformerFactory
	policyLister repacklisters.RepackPolicyLister
	policySynced cache.InformerSynced
	runLister    repacklisters.RepackRunLister
	runSynced    cache.InformerSynced
	nodeLister   corelisters.NodeLister
	nodeSynced   cache.InformerSynced
	podLister    corelisters.PodLister
	podSynced    cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string]

	// now and scheduleNext are injectable seams for tests.
	now          func() time.Time
	scheduleNext func(key string, after time.Duration)
}

// New wires the controller onto the shared informer factories. Run re-starts
// them (idempotently) so the controller stays self-sufficient when repack is the
// only enabled controller tree.
func New(vcClient vcclientset.Interface,
	coreFactory informers.SharedInformerFactory, vcFactory vcinformer.SharedInformerFactory,
	opts Options) *Controller {
	opts.applyDefaults()

	policyInformer := vcFactory.Repack().V1alpha1().RepackPolicies()
	runInformer := vcFactory.Repack().V1alpha1().RepackRuns()
	nodeInformer := coreFactory.Core().V1().Nodes()
	podInformer := coreFactory.Core().V1().Pods()

	c := &Controller{
		opts:         opts,
		vcClient:     vcClient,
		vcFactory:    vcFactory,
		coreFactory:  coreFactory,
		policyLister: policyInformer.Lister(),
		policySynced: policyInformer.Informer().HasSynced,
		runLister:    runInformer.Lister(),
		runSynced:    runInformer.Informer().HasSynced,
		nodeLister:   nodeInformer.Lister(),
		nodeSynced:   nodeInformer.Informer().HasSynced,
		podLister:    podInformer.Lister(),
		podSynced:    podInformer.Informer().HasSynced,
		queue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:          time.Now,
	}
	c.scheduleNext = c.queue.AddAfter

	// Policy events reconcile the policy. Update is filtered on Generation so our
	// own status writes (which keep it) do not loop; no Delete handler — kube GC
	// owns a deleted policy's runs.
	policyInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueuePolicyAdd,
		UpdateFunc: c.enqueuePolicyUpdate,
	})

	// Run Update/Delete only — no Add: a startup replay of existing runs would
	// reconcile them for nothing.
	runInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: c.enqueueRunUpdate,
		DeleteFunc: c.enqueueRunDelete,
	})
	return c
}

func (c *Controller) enqueuePolicyAdd(obj interface{}) {
	if pol, ok := obj.(*repackv1alpha1.RepackPolicy); ok {
		c.queue.Add(pol.Name)
	}
}

func (c *Controller) enqueuePolicyUpdate(oldObj, newObj interface{}) {
	oldPol, ok1 := oldObj.(*repackv1alpha1.RepackPolicy)
	newPol, ok2 := newObj.(*repackv1alpha1.RepackPolicy)
	if ok1 && ok2 && oldPol.Generation != newPol.Generation {
		c.queue.Add(newPol.Name)
	}
}

func (c *Controller) enqueueRunUpdate(oldObj, newObj interface{}) {
	oldRun, ok1 := oldObj.(*repackv1alpha1.RepackRun)
	newRun, ok2 := newObj.(*repackv1alpha1.RepackRun)
	if ok1 && ok2 && !repackstate.IsTerminal(oldRun.Status.Phase) && repackstate.IsTerminal(newRun.Status.Phase) {
		c.enqueueOwner(newRun)
	}
}

func (c *Controller) enqueueRunDelete(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			run, _ = tombstone.Obj.(*repackv1alpha1.RepackRun)
		}
	}
	if run != nil {
		c.enqueueOwner(run)
	}
}

// enqueueOwner maps a derived run back to its owning policy via the reserved
// label; runs without the label are not ours.
func (c *Controller) enqueueOwner(run *repackv1alpha1.RepackRun) {
	if name := run.Labels[repackv1alpha1.RepackPolicyLabel]; name != "" {
		c.queue.Add(name)
	}
}

// Run waits for the informer caches the controller depends on, then runs workers
// until ctx is done. It (re)starts both shared factories — idempotently — so a
// repack-only enablement stays self-sufficient.
func (c *Controller) Run(ctx context.Context) error {
	defer c.queue.ShutDown()
	c.vcFactory.Start(ctx.Done())
	c.coreFactory.Start(ctx.Done())

	cacheSyncs := []cache.InformerSynced{c.policySynced, c.runSynced, c.nodeSynced, c.podSynced}
	if !cache.WaitForCacheSync(ctx.Done(), cacheSyncs...) {
		return fmt.Errorf("repackpolicy controller: cache failed to sync")
	}
	klog.InfoS("RepackPolicy controller synced and starting workers", "workers", c.opts.Workers)
	for i := 0; i < c.opts.Workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}
	<-ctx.Done()
	return nil
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)
	if err := c.reconcile(ctx, key); err != nil {
		runtime.HandleError(fmt.Errorf("repackpolicy %q reconcile failed: %w", key, err))
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}
