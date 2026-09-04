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

package engine

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

// enqueue adds a planning or in-flight placement RepackRun to the workqueue.
func (e *Engine) enqueue(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok || !enginestatus.ShouldReconcile(run) {
		return
	}
	e.workQueue.Add(run.Name)
}

// maxReconcileRetries caps how many times a failing RepackRun is retried before
// it is treated as a poison pill: the engine gives up and marks it Failed rather
// than retrying forever (which would also keep re-panicking on a bad object).
const maxReconcileRetries = 5

// statusPersistenceRequeueInterval is the outer retry delay after bounded local
// status retries are exhausted. Status contention and terminal persistence
// failures must yield the worker without consuming the poison-pill budget.
const statusPersistenceRequeueInterval = time.Second

func (e *Engine) processNext(ctx context.Context) bool {
	key, shutdown := e.workQueue.Get()
	if shutdown {
		return false
	}
	defer e.workQueue.Done(key)

	if err := e.reconcileSafely(ctx, key); err != nil {
		if !reconcileErrorConsumesRetryBudget(err) {
			// AddAfter does not advance the rate-limiter counter, so status contention
			// cannot turn into ReconcileGaveUp. RetryOnConflict already performed
			// exponential backoff inside the status mutation; this delayed retry spans
			// reconcile attempts while preserving any prior real-failure count.
			klog.V(4).InfoS("requeueing RepackRun after retryable status persistence error",
				"run", key, "retryAfter", statusPersistenceRequeueInterval, "error", err)
			e.workQueue.AddAfter(key, statusPersistenceRequeueInterval)
			return true
		}
		utilruntime.HandleError(fmt.Errorf("repack-engine reconcile %q: %w", key, err))
		if e.workQueue.NumRequeues(key) < maxReconcileRetries {
			klog.V(4).InfoS("requeueing RepackRun after error", "run", key, "retries", e.workQueue.NumRequeues(key)+1)
			e.workQueue.AddRateLimited(key)
			return true
		}
		// Poison pill: stop retrying and fail the run so it does not loop forever.
		// A Run that crossed the Execute barrier deliberately retained its slot;
		// the terminal fail path releases it only after Failed status is durable.
		e.workQueue.Forget(key)
		e.failByName(ctx, key, state.ReasonReconcileFailed, fmt.Errorf("gave up after %d retries: %w", maxReconcileRetries, err))
		return true
	}
	e.workQueue.Forget(key)
	return true
}

func reconcileErrorConsumesRetryBudget(err error) bool {
	return err != nil && !apierrors.IsConflict(err) && !isTerminalStatusPersistenceError(err)
}

// reconcileSafely runs reconcile with panic recovery so a single bad RepackRun
// (e.g. a plugin/snapshot panic) cannot crash the engine's worker goroutine. The
// panic is converted to an error; reconcile's defers still run during unwind.
// Before the Execute barrier they release the slot; after the Action marks the
// barrier they retain it so the same Run recovers its durable journal.
func (e *Engine) reconcileSafely(ctx context.Context, name string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in reconcile: %v", r)
			klog.ErrorS(err, "repack: recovered panic", "run", name, "stack", string(debug.Stack()))
		}
	}()
	return e.reconcile(ctx, name)
}

// failByName marks a run Failed by name (poison-pill path); best-effort.
func (e *Engine) failByName(ctx context.Context, name, reason string, cause error) {
	run, err := e.repackRunLister.Get(name)
	if err != nil {
		return // gone or lister error; nothing to write
	}
	work := run.DeepCopy()
	if err := e.fail(ctx, work, work.Generation, reason, cause); err != nil {
		klog.ErrorS(err, "repack: persist poison-pill failure", "run", name)
		if isTerminalStatusPersistenceError(err) {
			e.workQueue.AddAfter(name, statusPersistenceRequeueInterval)
		}
	}
}

// reconcile is the controller driver: load/admit the Run, invoke configured
// Actions, and apply their retry/Execute-slot result. Repack business stages are
// dispatched only by the Repack Action.
func (e *Engine) reconcile(ctx context.Context, name string) error {
	run, err := e.repackRunLister.Get(name)
	if apierrors.IsNotFound(err) {
		e.forgetPendingTerminalStatus(name)
		// A Run deleted before reaching a terminal phase must still release the
		// K=1 Execute slot it holds. Run GC or an operator can remove a Running
		// Execute at any time; without this release the in-memory
		// activeExecuteRunName would pin the slot forever and gate every later
		// Execute behind AnotherRunActive. markExecuteDone is owner-checked, so
		// DryRun runs (never holding the slot) and foreign names are untouched.
		if e.markExecuteDone(name) {
			e.requeueGatedRuns()
		}
		return nil
	}
	if err != nil {
		return err
	}
	if desired, found := e.pendingTerminalStatus(name); found {
		work := run.DeepCopy()
		desired.DeepCopyInto(&work.Status)
		if err := e.updateStatusTerminal(ctx, work); err != nil {
			return err
		}
		if work.Spec.Mode == repackv1alpha1.RepackModeExecute {
			if e.markExecuteDone(work.Name) {
				e.requeueGatedRuns()
			}
		}
		result := engineframework.RunActions(e.config.Actions, &engineframework.ActionContext{
			Context: ctx, Run: work, Runtime: e.actionRuntime(),
		})
		return result.Err
	}
	if !enginestatus.ShouldReconcile(run) {
		return nil // already picked up / terminal
	}
	klog.V(4).InfoS("reconciling RepackRun", "run", name, "mode", run.Spec.Mode)
	work := run.DeepCopy()

	// Acknowledge as Pending so `kubectl get repackrun` shows a phase before the
	// engine starts (deferred Execute runs also settle here via the gate below).
	if work.Status.Phase == "" {
		work.Status.Phase = repackv1alpha1.RepackPending
		if err := e.updateStatus(ctx, work); err != nil {
			return err
		}
	}

	stage := enginestatus.ResolveStage(work)
	// A terminal status is the durable release barrier. Usually the Runtime that
	// wrote it releases the slot immediately; this path closes the small panic
	// window between that status write and the in-memory release.
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute && stage == enginestatus.StageCleanup {
		if e.markExecuteDone(work.Name) {
			e.requeueGatedRuns()
		}
	}
	active, lastFinish := false, time.Time{}
	gate := state.GateDecision{Admit: true} // DryRun is never serialized by Execute.
	// Terminal cleanup does not consume the global Execute slot.
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute && stage != enginestatus.StageCleanup {
		gate, active, lastFinish = e.tryAcquireExecute(work.Name, e.now())
	}
	klog.V(4).InfoS("repack: execute gate evaluated", "run", work.Name, "mode", work.Spec.Mode,
		"executeActive", active, "lastExecuteFinish", lastFinish, "cooldown", e.config.Cooldown,
		"admit", gate.Admit, "reason", gate.Reason, "requeueAfter", gate.RequeueAfter)
	if !gate.Admit {
		metrics.ObserveGateRejection(gate.Reason)
		klog.V(3).InfoS("RepackRun deferred by execute gate",
			"run", name, "reason", gate.Reason, "requeueAfter", gate.RequeueAfter)
		message := "Waiting to execute: another Execute RepackRun is active; this run will be retried when the active run finishes."
		if gate.Reason == state.ReasonExecuteCooldownActive {
			message = fmt.Sprintf(
				"Waiting to execute: the previous Execute RepackRun is cooling down; retrying after %s.",
				gate.RequeueAfter.Round(time.Second))
		}
		conditionChanged := state.MarkPending(work, gate.Reason, message)
		if err := e.updateStatus(ctx, work); err != nil {
			return err
		}
		if conditionChanged {
			e.recordRunEvent(work, v1.EventTypeNormal, gate.Reason, message)
		}
		if gate.RequeueAfter > 0 {
			e.workQueue.AddAfter(name, gate.RequeueAfter)
		}
		return nil
	}
	if work.Spec.Mode == repackv1alpha1.RepackModeExecute && stage != enginestatus.StageCleanup {
		klog.V(3).InfoS("repack: Execute slot acquired", "run", work.Name, "cooldown", e.config.Cooldown)
	}
	releaseExecuteSlot := work.Spec.Mode == repackv1alpha1.RepackModeExecute && stage != enginestatus.StageCleanup
	actionCtx := &engineframework.ActionContext{Context: ctx, Run: work, Runtime: e.actionRuntime()}
	defer func() {
		if releaseExecuteSlot && !actionCtx.ExecuteSlotHeld() {
			if e.markExecuteDone(work.Name) {
				e.requeueGatedRuns()
			}
		}
	}()
	result := engineframework.RunActions(e.config.Actions, actionCtx)
	if result.Requeue {
		e.workQueue.Add(work.Name)
	}
	if result.RequeueAfter > 0 {
		e.workQueue.AddAfter(work.Name, result.RequeueAfter)
	}
	return result.Err
}
