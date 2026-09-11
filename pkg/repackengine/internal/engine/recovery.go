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

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/volcano/pkg/controllers/repack/state"

	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

func (e *Engine) pendingTerminalStatus(name string) (*repackv1alpha1.RepackRunStatus, bool) {
	status, found := e.pendingTerminalStatuses[name]
	if !found {
		return nil, false
	}
	return status.DeepCopy(), true
}

func (e *Engine) rememberPendingTerminalStatus(name string, status *repackv1alpha1.RepackRunStatus) {
	if status == nil {
		return
	}
	if e.pendingTerminalStatuses == nil {
		e.pendingTerminalStatuses = make(map[string]*repackv1alpha1.RepackRunStatus)
	}
	e.pendingTerminalStatuses[name] = status.DeepCopy()
}

func (e *Engine) forgetPendingTerminalStatus(name string) {
	delete(e.pendingTerminalStatuses, name)
}

// recoverOrphans fails an interrupted planning run. Durable eviction and
// placement stages are intentionally recoverable: their journal, lease,
// replacement identity and deadline let the Repack Action resume without
// recalculating the plan or repeating an accepted eviction.
func (e *Engine) recoverOrphans(ctx context.Context) {
	runs, err := e.repackRunLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "repack: list for orphan recovery")
		return
	}
	for _, r := range runs {
		if r.Status.Phase != repackv1alpha1.RepackRunning {
			continue
		}
		stage := enginestatus.ResolveStage(r)
		if stage == enginestatus.StageEvicting || stage == enginestatus.StagePlacing {
			e.workQueue.Add(r.Name)
			klog.V(3).InfoS("recovered in-progress Execute run",
				"run", r.Name, "stage", stage)
			continue
		}
		work := r.DeepCopy()
		reason := state.ReasonExecutionInterrupted
		cause := fmt.Errorf("engine restarted while this run was in progress")
		msg := enginestatus.FailureMessage(engineconf.ResolveResource(work, e.config.DefaultResource), reason, cause)
		state.MarkFailed(work, reason, msg)
		enginestatus.StampLifecycle(work, e.now())
		e.rememberPendingTerminalStatus(work.Name, work.Status.DeepCopy())
		e.workQueue.Add(work.Name)
		klog.V(3).InfoS("queued orphaned Running RepackRun for terminal recovery", "run", work.Name)
	}
}
