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
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

func (e *Engine) fail(ctx context.Context, run *repackv1alpha1.RepackRun, generation int64, reason string, err error) error {
	klog.ErrorS(err, "repack: run failed", "run", run.Name, "reason", reason)
	message := enginestatus.FailureMessage(engineconf.ResolveResource(run, e.config.DefaultResource), reason, err)
	state.MarkFailed(run, reason, message)
	if err := e.updateStatusTerminal(ctx, run); err != nil {
		return err
	}
	if run.Spec.Mode != repackv1alpha1.RepackModeExecute {
		return nil
	}
	e.clearEvictionRetry(run)
	// A failure after the prepare barrier must release its PodGroup lease.
	// Pod-level gate cleanup is driven by the placement controller from this
	// terminal status. Return lease cleanup failures so the terminal-only
	// reconcile path can retry without replaying an eviction.
	if e.markExecuteDone(run.Name) {
		e.requeueGatedRuns()
	}
	if err := e.cleanupPlacement(ctx, run); err != nil {
		return fmt.Errorf("cleanup placement after failure: %w", err)
	}
	return nil
}

func (e *Engine) updateStatus(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	enginestatus.StampLifecycle(run, time.Now())
	desired := run.Status.DeepCopy()
	err := e.writeStatus(ctx, run.Name, desired)
	if err != nil {
		klog.ErrorS(err, "repack: update status", "run", run.Name)
	}
	return err
}

func (e *Engine) writeStatus(ctx context.Context, name string, desired *repackv1alpha1.RepackRunStatus) error {
	if e.statusStore == nil {
		e.statusStore = enginestatus.NewStore(e.volcanoClient)
	}
	return e.statusStore.Write(ctx, name, desired)
}

const terminalStatusWriteAttempts = 3

type terminalStatusPersistenceError struct {
	runName string
	err     error
}

func (e *terminalStatusPersistenceError) Error() string {
	return fmt.Sprintf("persist terminal status for %s: %v", e.runName, e.err)
}

func (e *terminalStatusPersistenceError) Unwrap() error { return e.err }

func isTerminalStatusPersistenceError(err error) bool {
	var target *terminalStatusPersistenceError
	return errors.As(err, &target)
}

// updateStatusTerminal performs a bounded local retry. The workqueue owns the
// longer-lived retry so a persistently broken object cannot monopolize the
// engine's only worker and starve unrelated RepackRuns.
func (e *Engine) updateStatusTerminal(ctx context.Context, run *repackv1alpha1.RepackRun) error {
	enginestatus.StampLifecycle(run, time.Now())
	desired := run.Status.DeepCopy()
	name := run.Name
	e.rememberPendingTerminalStatus(name, desired)
	backoff := retry.DefaultBackoff
	backoff.Steps = terminalStatusWriteAttempts
	err := retry.OnError(backoff, func(err error) bool {
		return !apierrors.IsNotFound(err) && ctx.Err() == nil
	}, func() error {
		return e.writeStatus(ctx, name, desired)
	})
	if apierrors.IsNotFound(err) {
		e.forgetPendingTerminalStatus(name)
		return nil // explicitly deleted; no terminal object remains to persist
	}
	if err != nil {
		klog.ErrorS(err, "repack: terminal status persistence exhausted local retry budget",
			"run", name, "attempts", terminalStatusWriteAttempts)
		return &terminalStatusPersistenceError{runName: name, err: err}
	}
	e.forgetPendingTerminalStatus(name)

	outcome := enginestatus.TerminalOutcome(run)
	metrics.ObserveRun(string(run.Spec.Mode), outcome)
	klog.V(4).InfoS("repack: terminal status persisted", "run", run.Name, "mode", run.Spec.Mode,
		"phase", run.Status.Phase, "outcome", outcome, "relocationCount", len(run.Status.Relocations))
	if e.recorder != nil {
		etype := v1.EventTypeNormal
		if run.Status.Phase == repackv1alpha1.RepackFailed {
			etype = v1.EventTypeWarning
		}
		message := run.Status.Message
		if message == "" {
			message = "RepackRun reached a terminal state."
		}
		e.recorder.Event(run, etype, outcome, message)
	}
	return nil
}
