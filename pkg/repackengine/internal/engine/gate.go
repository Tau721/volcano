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
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/volcano/pkg/controllers/repack/state"
)

// tryAcquireExecute evaluates the K=1/cooldown gate and claims the local Execute
// slot as one atomic operation. The persisted scan is intentionally outside the
// short critical section; the in-memory slot closes the local TOCTOU window
// between that scan and the claim.
func (e *Engine) tryAcquireExecute(currentRunName string, now time.Time) (gate state.GateDecision, executeActive bool, latestExecuteFinishTime time.Time) {
	persistedExecuteActive, persistedExecuteFinishTime := e.persistedExecuteState(currentRunName)

	e.executeStateMutex.Lock()
	defer e.executeStateMutex.Unlock()

	executeActive = persistedExecuteActive || (e.activeExecuteRunName != "" && e.activeExecuteRunName != currentRunName)
	latestExecuteFinishTime = e.lastExecuteFinishTime
	if persistedExecuteFinishTime.After(latestExecuteFinishTime) {
		latestExecuteFinishTime = persistedExecuteFinishTime
	}
	gate = state.EvaluateGate(state.GateInputs{
		Mode:              repackv1alpha1.RepackModeExecute,
		ExecuteActive:     executeActive,
		LastExecuteFinish: latestExecuteFinishTime,
		Cooldown:          e.config.Cooldown,
		Now:               now,
	})
	if gate.Admit {
		e.activeExecuteRunName = currentRunName
	}
	return gate, executeActive, latestExecuteFinishTime
}

// markExecuteDone releases the slot and stamps the cooldown anchor only when
// name still owns it. The boolean lets terminal recovery wake gated Runs once
// without extending cooldown on an idempotent cleanup retry.
func (e *Engine) markExecuteDone(name string) bool {
	e.executeStateMutex.Lock()
	defer e.executeStateMutex.Unlock()
	if e.activeExecuteRunName != name {
		return false
	}
	e.activeExecuteRunName = ""
	e.lastExecuteFinishTime = e.now()
	return true
}

// persistedExecuteState scans the lister for the Execute gate: whether another Execute is
// currently Running, and the most recent terminal Execute completion.
func (e *Engine) persistedExecuteState(currentRunName string) (executeActive bool, latestExecuteFinishTime time.Time) {
	runs, err := e.repackRunLister.List(labels.Everything())
	if err != nil {
		return true, time.Time{} // conservative: assume busy
	}
	for _, run := range runs {
		if run.Spec.Mode != repackv1alpha1.RepackModeExecute {
			continue
		}
		if run.Name != currentRunName && run.Status.Phase == repackv1alpha1.RepackRunning {
			executeActive = true
		}
		if state.IsTerminal(run.Status.Phase) && run.Status.CompletionTime != nil {
			if completionTime := run.Status.CompletionTime.Time; completionTime.After(latestExecuteFinishTime) {
				latestExecuteFinishTime = completionTime
			}
		}
	}
	return executeActive, latestExecuteFinishTime
}

// requeueGatedRuns re-enqueues every non-terminal Execute run so any run that was
// gated on the K=1 slot (reason AnotherRunActive) is re-evaluated now that the
// slot is free. released is the run whose release triggered this call: it must be
// skipped, because its terminal status has not necessarily reached this cache yet,
// so re-enqueuing it would re-gate it on the cooldown stamp its own release just
// set and demote it back to Pending. DryRun runs are never gated, so they are not
// re-enqueued here.
func (e *Engine) requeueGatedRuns(released string) {
	runs, err := e.repackRunLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "repack: list for gated-run requeue")
		return
	}
	woken := 0
	for _, run := range runs {
		if run.Name == released {
			continue
		}
		if run.Spec.Mode == repackv1alpha1.RepackModeExecute && !state.IsTerminal(run.Status.Phase) {
			e.workQueue.Add(run.Name)
			woken++
		}
	}
	if woken > 0 {
		klog.V(4).InfoS("requeued gated Execute runs after slot release", "count", woken)
	}
}
