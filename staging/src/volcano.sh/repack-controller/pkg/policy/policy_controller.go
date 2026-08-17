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

// Package policy implements the RepackPolicy controller — a template-based
// RepackRun generator following the CronJob→Job pattern. It is a standalone
// component in the volcano.sh/repack-controller module: pure client-go informers
// and workqueue, no dependency on the volcano scheduler framework.
package policy

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcinformers "volcano.sh/apis/pkg/client/informers/externalversions"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

// Options are operator-level knobs for the RepackPolicy controller.
type Options struct {
	Workers                   int
	EvalCycle                 time.Duration
	DefaultSuccessHistoryLimit int32
	DefaultFailedHistoryLimit  int32
}

// Default options.
const (
	defaultWorkers            = 1
	defaultEvalCycle          = 10 * time.Minute
	defaultSuccessHistory     = 3
	defaultFailedHistory      = 3
)

// Controller reconciles RepackPolicy objects: evaluates triggers (cron + frag),
// generates RepackRuns, and cleans up history.
type Controller struct {
	volcanoClient vcclientset.Interface

	policyLister         repacklisters.RepackPolicyLister
	policyInformerSynced cache.InformerSynced

	runLister         repacklisters.RepackRunLister
	runInformerSynced cache.InformerSynced

	nodeLister corev1listers.NodeLister
	podLister  corev1listers.PodLister

	workQueue workqueue.TypedRateLimitingInterface[string]

	informerFactory      vcinformers.SharedInformerFactory
	kubeInformerFactory  informers.SharedInformerFactory

	workers             int
	evalCycle           time.Duration
	defaultSuccessLimit int32
	defaultFailedLimit  int32
	now                 func() time.Time
}

// New builds a Controller wired to the given clients and shared informer factories.
func New(volcanoClient vcclientset.Interface,
	informerFactory vcinformers.SharedInformerFactory,
	kubeInformerFactory informers.SharedInformerFactory,
	options Options) *Controller {

	if options.Workers < 1 {
		options.Workers = defaultWorkers
	}
	if options.EvalCycle <= 0 {
		options.EvalCycle = defaultEvalCycle
	}
	if options.DefaultSuccessHistoryLimit <= 0 {
		options.DefaultSuccessHistoryLimit = defaultSuccessHistory
	}
	if options.DefaultFailedHistoryLimit <= 0 {
		options.DefaultFailedHistoryLimit = defaultFailedHistory
	}

	policyInformer := informerFactory.Repack().V1alpha1().RepackPolicies()
	runInformer := informerFactory.Repack().V1alpha1().RepackRuns()
	nodeInformer := kubeInformerFactory.Core().V1().Nodes()
	podInformer := kubeInformerFactory.Core().V1().Pods()

	c := &Controller{
		volcanoClient:        volcanoClient,
		policyLister:         policyInformer.Lister(),
		policyInformerSynced: policyInformer.Informer().HasSynced,
		runLister:            runInformer.Lister(),
		runInformerSynced:    runInformer.Informer().HasSynced,
		nodeLister:           nodeInformer.Lister(),
		podLister:            podInformer.Lister(),
		workQueue:            workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		informerFactory:      informerFactory,
		kubeInformerFactory:  kubeInformerFactory,
		workers:              options.Workers,
		evalCycle:            options.EvalCycle,
		defaultSuccessLimit:  options.DefaultSuccessHistoryLimit,
		defaultFailedLimit:   options.DefaultFailedHistoryLimit,
		now:                  time.Now,
	}

	// Register event handler: only Policy events enqueue the workqueue.
	// Run events, Node events, and Pod events are never enqueued — they are
	// accessed read-only via lister during reconcile or the eval ticker.
	policyInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueue,
		UpdateFunc: func(_, newObj interface{}) { c.enqueue(newObj) },
	})

	return c
}

// enqueue maps a cluster-scoped object to its workqueue key (the name).
func (c *Controller) enqueue(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("repackpolicy key: %w", err))
		return
	}
	c.workQueue.Add(key)
}

// Run starts the factories, waits for cache sync, and launches workers plus the
// eval ticker loop. Blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer c.workQueue.ShutDown()

	c.informerFactory.Start(ctx.Done())
	c.kubeInformerFactory.Start(ctx.Done())

	// Wait for all informer caches to sync.
	syncFns := []cache.InformerSynced{
		c.policyInformerSynced,
		c.runInformerSynced,
	}
	// Node and pod informers may not be started if the factory was already
	// started elsewhere; just wait for their caches.
	syncFns = append(syncFns,
		c.kubeInformerFactory.Core().V1().Nodes().Informer().HasSynced,
		c.kubeInformerFactory.Core().V1().Pods().Informer().HasSynced,
	)

	if !cache.WaitForCacheSync(ctx.Done(), syncFns...) {
		return fmt.Errorf("repackpolicy controller: cache failed to sync")
	}

	klog.V(3).InfoS("Starting RepackPolicy controller", "workers", c.workers, "evalCycle", c.evalCycle)

	// Launch reconcile workers.
	for i := 0; i < c.workers; i++ {
		go func() {
			for c.processNext(ctx) {
			}
		}()
	}

	// Launch eval ticker for reactive (onFragAbovePercent) evaluation.
	// This ticker enqueues policies that have a frag trigger set, ensuring
	// they are periodically re-evaluated even when no Policy event fires.
	go func() {
		ticker := time.NewTicker(c.evalCycle)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.enqueueFragPolicies()
			}
		}
	}()

	<-ctx.Done()
	klog.V(3).InfoS("Shutting down RepackPolicy controller")
	return nil
}

// enqueueFragPolicies lists all policies and enqueues those with an
// onFragAbovePercent trigger configured.
func (c *Controller) enqueueFragPolicies() {
	policies, err := c.policyLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "Failed to list policies for frag eval")
		return
	}
	for _, p := range policies {
		if p.Spec.Trigger.OnFragAbovePercent != nil && *p.Spec.Trigger.OnFragAbovePercent > 0 {
			c.workQueue.Add(p.Name)
		}
	}
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.workQueue.Get()
	if shutdown {
		return false
	}
	defer c.workQueue.Done(key)

	if err := c.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("reconcile repackpolicy %q: %w", key, err))
		klog.V(4).InfoS("RepackPolicy controller: reconcile failed; requeueing with rate limit",
			"policy", key, "retryCount", c.workQueue.NumRequeues(key)+1, "error", err)
		c.workQueue.AddRateLimited(key)
		return true
	}
	c.workQueue.Forget(key)
	return true
}

// reconcile implements the 9-step reconcile loop per the design doc.
func (c *Controller) reconcile(ctx context.Context, key string) error {
	now := c.now()

	// --- Step 1: Get Policy ---
	policy, err := c.policyLister.Get(key)
	if apierrors.IsNotFound(err) {
		return nil // deleted; nothing to do
	}
	if err != nil {
		return err
	}
	// Deep-copy at the start so we can safely mutate status without
	// corrupting the lister cache.
	policy = policy.DeepCopy()

	// --- Step 2: Clean inProgress[] ---
	cleanedInProgress, lastSuccessfulTime := c.cleanInProgress(policy.Status.InProgress)
	policy.Status.InProgress = cleanedInProgress
	if !lastSuccessfulTime.IsZero() {
		policy.Status.LastSuccessfulTime = &metav1.Time{Time: lastSuccessfulTime}
	}

	// --- Step 3: Suspend check ---
	if policy.Spec.Suspend != nil && *policy.Spec.Suspend {
		setCondition(policy, metav1.ConditionTrue, repackv1alpha1.ReasonReconcileSucceeded, "Suspended")
		return c.finalize(ctx, policy, now)
	}

	// --- Step 4: Global Execute gate ---
	if policy.Spec.RunTemplate.Spec.Mode == repackv1alpha1.RepackModeExecute {
		if c.hasActiveExecuteRun() {
			setCondition(policy, metav1.ConditionTrue, repackv1alpha1.ReasonReconcileSucceeded,
				"Skipped: another Execute Run active or in cooldown")
			return c.finalize(ctx, policy, now)
		}
	}

	// --- Step 5: Trigger evaluation ---
	triggered, triggerSource := c.evaluateTriggers(policy, now)
	policy.Status.LastEvaluationTime = &metav1.Time{Time: now}

	if !triggered {
		msg := buildNoTriggerMessage(policy, now)
		setCondition(policy, metav1.ConditionTrue, repackv1alpha1.ReasonReconcileSucceeded, msg)
		return c.finalize(ctx, policy, now)
	}

	// --- Step 6: Policy concurrency gate ---
	if hasInProgressRun(policy.Status.InProgress) {
		setCondition(policy, metav1.ConditionTrue, repackv1alpha1.ReasonReconcileSucceeded,
			"Skipped: previous Run still active (inProgress non-empty)")
		return c.finalize(ctx, policy, now)
	}

	// --- Step 7: Create Run ---
	run := ConstructRunFromTemplate(policy, triggerSource, now)
	createdRun, err := c.volcanoClient.RepackV1alpha1().RepackRuns().Create(ctx, run, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another worker created the same run; treat as success.
			klog.V(4).InfoS("RepackPolicy: Run already exists (dedup)", "run", run.Name)
			// Fetch the existing run for the ObjectReference.
			existingRun, getErr := c.runLister.Get(run.Name)
			if getErr == nil {
				createdRun = existingRun
			} else {
				createdRun = run
			}
		} else {
			setCondition(policy, metav1.ConditionFalse, repackv1alpha1.ReasonReconcileFailed,
				fmt.Sprintf("Failed to create Run: %v", err))
			return c.finalize(ctx, policy, now)
		}
	}

	// Success: update status.
	inProgress := append(policy.Status.InProgress, MakeObjectReference(createdRun))
	policy.Status.InProgress = inProgress
	policy.Status.LastTriggerTime = &metav1.Time{Time: now}
	setCondition(policy, metav1.ConditionTrue, repackv1alpha1.ReasonReconcileSucceeded,
		fmt.Sprintf("Created Run %s via %s", createdRun.Name, triggerSource))

	return c.finalize(ctx, policy, now)
}

// finalize runs history GC (step 8) and updates the status subresource (step 9).
func (c *Controller) finalize(ctx context.Context, policy *repackv1alpha1.RepackPolicy, now time.Time) error {
	// Step 8: History GC (best-effort — errors are logged but not fatal).
	if err := gcHistory(ctx, c.volcanoClient, c.runLister, policy,
		c.defaultSuccessLimit, c.defaultFailedLimit); err != nil {
		klog.ErrorS(err, "RepackPolicy: history GC failed", "policy", policy.Name)
	}

	// Step 9: Update status subresource (best-effort — errors are logged).
	if _, err := c.volcanoClient.RepackV1alpha1().RepackPolicies().UpdateStatus(ctx, policy, metav1.UpdateOptions{}); err != nil {
		klog.ErrorS(err, "RepackPolicy: status update failed", "policy", policy.Name)
	}

	// Schedule next cron requeue (after status update so we don't requeue on failed status).
	if policy.Spec.Trigger.CronSchedule != nil && *policy.Spec.Trigger.CronSchedule != "" {
			lastTrigger := time.Time{}
			if policy.Status.LastTriggerTime != nil {
				lastTrigger = policy.Status.LastTriggerTime.Time
			}
			nextFire := nextCronFire(*policy.Spec.Trigger.CronSchedule,
				lastTrigger,
				policy.CreationTimestamp.Time,
				now)
		if !nextFire.IsZero() {
			delay := nextFire.Sub(now)
			if delay > 0 {
				klog.V(5).InfoS("RepackPolicy: scheduling next cron requeue",
					"policy", policy.Name, "nextFire", nextFire, "delay", delay)
				c.workQueue.AddAfter(policy.Name, delay)
			}
		}
	}

	return nil
}

// cleanInProgress iterates through each entry in inProgress, checks the
// actual Run status via the lister, and removes terminal runs. Returns the
// cleaned list and the latest completion time among Succeeded runs.
func (c *Controller) cleanInProgress(inProgress []v1.ObjectReference) ([]v1.ObjectReference, time.Time) {
	var cleaned []v1.ObjectReference
	var latestSuccessful time.Time

	for _, ref := range inProgress {
		run, err := c.runLister.Get(ref.Name)
		if err != nil {
			// Run not found (deleted by TTL or GC); remove from inProgress.
			klog.V(5).InfoS("RepackPolicy: inProgress run not found, removing", "run", ref.Name)
			continue
		}
		if run.Status.Phase == repackv1alpha1.RepackSucceeded {
			if run.Status.CompletionTime != nil && run.Status.CompletionTime.Time.After(latestSuccessful) {
				latestSuccessful = run.Status.CompletionTime.Time
			}
			continue // remove from inProgress
		}
		if run.Status.Phase == repackv1alpha1.RepackFailed {
			continue // remove from inProgress
		}
		// Still active (Pending or Running); keep in inProgress.
		cleaned = append(cleaned, ref)
	}
	return cleaned, latestSuccessful
}

// hasActiveExecuteRun returns true if there is any non-terminal Run with
// mode=Execute in the cluster.
func (c *Controller) hasActiveExecuteRun() bool {
	allRuns, err := c.runLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "RepackPolicy: failed to list all Runs for global Execute gate")
		return true // conservative: assume active on error
	}
	for _, r := range allRuns {
		if r.Spec.Mode != repackv1alpha1.RepackModeExecute {
			continue
		}
		switch r.Status.Phase {
		case repackv1alpha1.RepackPending, repackv1alpha1.RepackRunning:
			return true
		}
	}
	return false
}

// evaluateTriggers checks both cron and frag triggers. Returns whether
// at least one triggered, and the winning trigger source label.
func (c *Controller) evaluateTriggers(policy *repackv1alpha1.RepackPolicy, now time.Time) (bool, string) {
	triggered := false
	triggerSource := ""

	// Check cron schedule.
	if policy.Spec.Trigger.CronSchedule != nil && *policy.Spec.Trigger.CronSchedule != "" {
			lastTrigger := time.Time{}
			if policy.Status.LastTriggerTime != nil {
				lastTrigger = policy.Status.LastTriggerTime.Time
			}
			if isCronDue(*policy.Spec.Trigger.CronSchedule, lastTrigger, policy.CreationTimestamp.Time, now) {

			// Dedup check: if lastTriggerTime already records this time window, skip.
			if policy.Status.LastTriggerTime == nil || !isSameTimeWindow(policy.Status.LastTriggerTime.Time, now) {
				triggered = true
				triggerSource = "cronSchedule"
				klog.V(4).InfoS("RepackPolicy: cron trigger hit",
					"policy", policy.Name, "schedule", *policy.Spec.Trigger.CronSchedule)
			}
		}
	}

	// Check onFragAbovePercent.
	if policy.Spec.Trigger.OnFragAbovePercent != nil && *policy.Spec.Trigger.OnFragAbovePercent > 0 {
		// Determine which resource to measure. If goals are set, use the first goal's resource.
		var resourceName v1.ResourceName
		if len(policy.Spec.RunTemplate.Spec.Goals) > 0 {
			resourceName = policy.Spec.RunTemplate.Spec.Goals[0].Resource
		}
		if resourceName != "" {
			nodes, err := c.nodeLister.List(labels.Everything())
			if err != nil {
				klog.ErrorS(err, "RepackPolicy: failed to list nodes for frag eval", "policy", policy.Name)
			} else {
				pods, err := c.podLister.List(labels.Everything())
				if err != nil {
					klog.ErrorS(err, "RepackPolicy: failed to list pods for frag eval", "policy", policy.Name)
				} else {
					fragResult := ComputeFragRate(nodes, pods, resourceName)
					klog.V(5).InfoS("RepackPolicy: frag evaluation",
						"policy", policy.Name, "resource", resourceName,
						"fragRate", fragResult.FragRatePercent,
						"threshold", *policy.Spec.Trigger.OnFragAbovePercent)

					if fragResult.FragRatePercent >= *policy.Spec.Trigger.OnFragAbovePercent {
						if !triggered {
							triggered = true
							triggerSource = "onFragAbovePercent"
						}
						klog.V(4).InfoS("RepackPolicy: frag trigger hit",
							"policy", policy.Name,
							"fragRate", fragResult.FragRatePercent,
							"threshold", *policy.Spec.Trigger.OnFragAbovePercent)
					}
				}
			}
		}
	}

	return triggered, triggerSource
}

// isSameTimeWindow checks whether a reference time falls within the same
// second as now. Used for cron dedup.
func isSameTimeWindow(ref, now time.Time) bool {
	return ref.Truncate(time.Second).Equal(now.Truncate(time.Second))
}

// buildNoTriggerMessage builds a human-readable condition message when
// no trigger fired.
func buildNoTriggerMessage(policy *repackv1alpha1.RepackPolicy, now time.Time) string {
	msg := "No trigger fired"

	if policy.Spec.Trigger.CronSchedule != nil && *policy.Spec.Trigger.CronSchedule != "" {
			lastTrigger := time.Time{}
			if policy.Status.LastTriggerTime != nil {
				lastTrigger = policy.Status.LastTriggerTime.Time
			}
			nextFire := nextCronFire(*policy.Spec.Trigger.CronSchedule,
				lastTrigger,
				policy.CreationTimestamp.Time,
				now)
		if !nextFire.IsZero() {
			msg += fmt.Sprintf(", next cron at %s", nextFire.UTC().Format(time.RFC3339))
		}
	}
	if policy.Spec.Trigger.OnFragAbovePercent != nil && *policy.Spec.Trigger.OnFragAbovePercent > 0 {
		if len(policy.Spec.RunTemplate.Spec.Goals) > 0 {
			msg += fmt.Sprintf(", threshold %d%%", *policy.Spec.Trigger.OnFragAbovePercent)
		}
	}

	return msg
}

// setCondition upserts the Healthy condition on the policy status.
func setCondition(policy *repackv1alpha1.RepackPolicy, status metav1.ConditionStatus,
	reason, message string) bool {

	return meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               repackv1alpha1.CondHealthy,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: policy.Generation,
	})
}