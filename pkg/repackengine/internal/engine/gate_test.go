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
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

// When an Execute releases the K=1 slot, requeueGatedRuns must re-enqueue every
// non-terminal Execute run (which may have been gated with reason AnotherRunActive
// and thus never re-queued), and must skip terminal runs and DryRun runs.
func TestRequeueGatedRuns(t *testing.T) {
	mk := func(name string, mode repackv1alpha1.RepackMode, phase repackv1alpha1.RepackPhase) *repackv1alpha1.RepackRun {
		return &repackv1alpha1.RepackRun{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       repackv1alpha1.RepackRunSpec{Mode: mode},
			Status:     repackv1alpha1.RepackRunStatus{Phase: phase},
		}
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	objs := []*repackv1alpha1.RepackRun{
		mk("exec-blocked", repackv1alpha1.RepackModeExecute, repackv1alpha1.RepackPending), // gated → requeue
		mk("exec-running", repackv1alpha1.RepackModeExecute, repackv1alpha1.RepackRunning), // non-terminal → requeue
		mk("exec-done", repackv1alpha1.RepackModeExecute, repackv1alpha1.RepackSucceeded),  // terminal → skip
		mk("exec-failed", repackv1alpha1.RepackModeExecute, repackv1alpha1.RepackFailed),   // terminal → skip
		mk("dry-pending", repackv1alpha1.RepackModeDryRun, repackv1alpha1.RepackPending),   // DryRun → skip
	}
	for _, o := range objs {
		if err := indexer.Add(o); err != nil {
			t.Fatalf("index add: %v", err)
		}
	}

	e := &Engine{
		repackRunLister: repacklisters.NewRepackRunLister(indexer),
		workQueue:       workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
	e.requeueGatedRuns()

	got := map[string]bool{}
	for e.workQueue.Len() > 0 {
		item, _ := e.workQueue.Get()
		got[item] = true
		e.workQueue.Done(item)
	}
	want := map[string]bool{"exec-blocked": true, "exec-running": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("requeued = %v, want %v", got, want)
	}
}

// A Run that held the K=1 Execute slot can be deleted before reaching a
// terminal phase (operator delete, Run GC, or an e2e cleanup tearing down a
// paused-engine journal). The in-memory slot must be released on that delete —
// otherwise every later Execute is permanently gated behind AnotherRunActive.
// The engine releases it from both the informer DeleteFunc and the reconcile
// IsNotFound guard; both funnel through markExecuteDone, which this test
// exercises at the state level.
func TestExecuteSlotReleasedWhenOwningRunDeleted(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	e := &Engine{
		repackRunLister: repacklisters.NewRepackRunLister(indexer),
		now:             func() time.Time { return now },
	}

	// A Running Execute acquires the slot.
	gate, _, _ := e.tryAcquireExecute("deleted-run", now)
	if !gate.Admit {
		t.Fatalf("first Execute must be admitted, gate=%+v", gate)
	}
	if gate, _, _ := e.tryAcquireExecute("next", now); gate.Admit {
		t.Fatal("a second Execute must be gated while the first holds the slot")
	}

	// The owning Run disappears without ever reaching a terminal phase (its
	// reconcile would hit IsNotFound; the informer delivered a Delete). The
	// engine must release the slot so a later Execute can proceed.
	if !e.markExecuteDone("deleted-run") {
		t.Fatal("delete of the owning Run must release the Execute slot")
	}

	// After release a fresh Execute is admitted again (subject to cooldown).
	after := now.Add(e.config.Cooldown + time.Second)
	e.now = func() time.Time { return after }
	if gate, _, _ := e.tryAcquireExecute("next-run", after); !gate.Admit {
		t.Fatalf("slot must be reusable after owning Run deletion, gate=%+v", gate)
	}
}
