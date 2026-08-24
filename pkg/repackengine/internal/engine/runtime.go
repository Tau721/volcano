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

// Package engine wires the Repack runtime lifecycle. Planning policy remains in
// actions and plugins; this package coordinates cache sessions, durable execution,
// placement observation, and status persistence.
package engine

import (
	"context"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcinformers "volcano.sh/apis/pkg/client/informers/externalversions"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"

	enginecache "volcano.sh/volcano/pkg/repackengine/cache"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
	"volcano.sh/volcano/pkg/scheduler/conf"
)

type Config = engineconf.Config

// Engine drives RepackRuns against scheduler sessions, event-driven: each
// admitted RepackRun is reconciled once on arrival. A single worker + the Execute
// gate (one-at-a-time + cooldown) serialize eviction.
type Engine struct {
	clusterCache  *enginecache.Cluster
	volcanoClient vcclientset.Interface
	config        Config
	// Explicit command/programmatic overrides take precedence over repack-conf.
	actionsExplicit bool
	pluginsExplicit bool

	informerFactory         vcinformers.SharedInformerFactory
	repackRunLister         repacklisters.RepackRunLister
	repackRunInformerSynced cache.InformerSynced
	workQueue               workqueue.TypedRateLimitingInterface[string]
	recorder                record.EventRecorder
	eventBroadcaster        record.EventBroadcaster
	statusStore             *enginestatus.Store
	now                     func() time.Time

	tiers          []conf.Tier
	configurations []conf.Configuration
	// activeExecuteRunName is the Execute run currently holding the K=1 slot
	// ("" = none); lastExecuteFinishTime is when this engine last finished an Execute.
	// These values bridge informer-cache propagation and are protected by
	// executeStateMutex. The check and claim are performed in one critical section
	// so K=1 remains correct if the worker count is increased later.
	executeStateMutex     sync.Mutex
	activeExecuteRunName  string
	lastExecuteFinishTime time.Time
	// pendingTerminalStatuses retains the exact terminal projection across a
	// bounded write failure, so the queued retry never reruns side effects. It is
	// owned by the single reconcile worker; recoverOrphans seeds it before that
	// worker starts.
	pendingTerminalStatuses map[string]*repackv1alpha1.RepackRunStatus

	placementRepairLimiter placementRepairLimiter
}

// NewEngine builds the engine, wires the RepackRun informer, and applies defaults.
func NewEngine(config *rest.Config, engineConfig Config) (*Engine, error) {
	actionsExplicit, pluginsExplicit := len(engineConfig.Actions) > 0, len(engineConfig.Plugins) > 0
	engineconf.ApplyDefaults(&engineConfig)
	volcanoClient := vcclientset.NewForConfigOrDie(config)
	informerFactory := vcinformers.NewSharedInformerFactory(volcanoClient, engineConfig.ResyncPeriod)
	informer := informerFactory.Repack().V1alpha1().RepackRuns()
	recorder, broadcaster := newEventRecorder(config)
	e := &Engine{
		recorder:                recorder,
		eventBroadcaster:        broadcaster,
		statusStore:             enginestatus.NewStore(volcanoClient),
		clusterCache:            enginecache.NewCluster(config),
		volcanoClient:           volcanoClient,
		config:                  engineConfig,
		actionsExplicit:         actionsExplicit,
		pluginsExplicit:         pluginsExplicit,
		informerFactory:         informerFactory,
		repackRunLister:         informer.Lister(),
		repackRunInformerSynced: informer.Informer().HasSynced,
		workQueue:               workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:                     time.Now,
		pendingTerminalStatuses: make(map[string]*repackv1alpha1.RepackRunStatus),
	}
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: e.enqueue,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldRun, oldOK := oldObj.(*repackv1alpha1.RepackRun)
			newRun, newOK := newObj.(*repackv1alpha1.RepackRun)
			if !oldOK || !newOK {
				return
			}
			// A placement Run is deliberately driven by status transitions from the
			// controller (Gated -> Nominated -> Placed), so it must be requeued on
			// a real update as well. Initial planning still ignores its own status
			// writes; a same-RV update remains the informer-resync safety net.
			if oldRun.ResourceVersion == newRun.ResourceVersion || isEvictionCandidate(newRun) ||
				isPlacementCandidate(newRun) || isPlacementCleanupCandidate(newRun) {
				e.enqueue(newRun)
			}
		},
	})
	return e, nil
}

// Run loads the shared scheduler config and the independent Repack config,
// starts the cache + informer, and serves RepackRun events with a single worker
// until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer e.workQueue.ShutDown()
	if e.eventBroadcaster != nil {
		defer e.eventBroadcaster.Shutdown()
	}

	if err := e.loadConf(); err != nil {
		klog.ErrorS(err, "repack: load scheduler conf")
		return // fail closed: never plan/evict without the scheduler's filter stack
	}
	e.informerFactory.Start(ctx.Done())
	e.clusterCache.Run(ctx)
	if !cache.WaitForCacheSync(ctx.Done(), e.repackRunInformerSynced) {
		klog.Error("repack: RepackRun cache failed to sync")
		return
	}
	e.recoverOrphans(ctx) // fail runs left Running by a crashed predecessor
	klog.V(3).InfoS("repack-engine started (event-driven)",
		"plugins", configuredPluginNames(e.config.Plugins),
		"defaultResource", e.config.DefaultResource, "cooldown", e.config.Cooldown, "resyncPeriod", e.config.ResyncPeriod)
	// Single worker: Execute runs serialize naturally (one reconcile at a time).
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		for e.processNext(ctx) {
		}
	}()
	<-ctx.Done()
	// Unblock a worker waiting in Get, then wait for an in-flight reconcile to
	// observe ctx cancellation before shutting down the event broadcaster.
	e.workQueue.ShutDown()
	worker.Wait()
	klog.V(3).InfoS("repack-engine shut down")
}
