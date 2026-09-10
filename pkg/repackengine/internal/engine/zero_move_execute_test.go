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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
)

// This file pins the zero-move Execute regression: the Run terminalizes in its own
// reconcile, is re-enqueued, and a stale cache observation then demotes it back to
// Pending. It drives the real reconcile entry point and is deliberately
// self-contained (no helpers from gate_test.go) so it compiles and reproduces
// against the pre-fix tree.

// reproZeroMoveAction reproduces the engine-visible half of the zero-move Execute
// path (actions/repack complete()): publish a Succeeded terminal status and do NOT
// hold the Execute slot, so the engine's release defer releases it and stamps the
// cooldown anchor. A stub keeps this reachable without a scheduler session.
const reproZeroMoveAction = "repro-zero-move"

type reproZeroMoveActionType struct{}

func (reproZeroMoveActionType) Name() string { return reproZeroMoveAction }

func (reproZeroMoveActionType) Execute(ctx *engineframework.ActionContext) engineframework.ActionResult {
	state.MarkSucceeded(ctx.Run, state.ReasonNoFragmentation, "no repack needed")
	if err := ctx.Runtime.UpdateTerminalStatus(ctx.Context, ctx.Run); err != nil {
		return engineframework.ActionResult{Stop: true, Err: err}
	}
	return engineframework.ActionResult{Stop: true}
}

func init() {
	engineframework.RegisterAction(reproZeroMoveAction,
		engineframework.ActionRegistration{Factory: func() engineframework.Action { return reproZeroMoveActionType{} }})
}

// reproEngine builds an Engine whose informer cache holds objs and whose
// workqueue is drained by reproQueued. It mirrors newRequeueTestEngine but is
// local so this file stands alone against the pre-fix tree.
func reproEngine(objs ...*repackv1alpha1.RepackRun) *Engine {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, o := range objs {
		if err := indexer.Add(o); err != nil {
			panic(err)
		}
	}
	return &Engine{
		repackRunLister: repacklisters.NewRepackRunLister(indexer),
		workQueue:       workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:             time.Now,
	}
}

func reproQueued(e *Engine) []string {
	var got []string
	for e.workQueue.Len() > 0 {
		item, _ := e.workQueue.Get()
		got = append(got, item)
		e.workQueue.Done(item)
	}
	return got
}

// A zero-move Execute Run terminalizes inside its own reconcile and releases the
// K=1 slot from that same call. The informer cache can still hold the pre-terminal
// copy at that instant — in production this is a race decided in ~80µs, which is
// why it reproduces only sometimes. Here the stale copy is modeled explicitly and
// never updated, so the losing side of the race is deterministic.
//
// Two engine contracts keep the Run terminal; if either is missing the Run cycles
// Succeeded -> Pending -> Succeeded once per cooldown and re-runs every time:
//
//  1. the release must not re-enqueue the run it just finished, and
//  2. a stale pre-terminal observation must not re-open the terminal status.
func TestZeroMoveExecuteRunStaysTerminalDespiteStaleCache(t *testing.T) {
	const name = "noop-execute"
	now := time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC)

	live := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status:     repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackPending},
	}
	volcanoClient := vcfake.NewSimpleClientset(live)

	// Informer cache lag: the copy the release scans predates the terminal write,
	// and nothing ever feeds the indexer the update.
	e := reproEngine(live.DeepCopy())
	e.volcanoClient = volcanoClient
	e.now = func() time.Time { return now }
	e.config.Actions = []string{reproZeroMoveAction}
	e.config.Cooldown = 30 * time.Second

	// First reconcile: the Action writes Succeeded, then the release defer frees the
	// slot and wakes gated runs. Waking the run it just finished is the bug.
	if err := e.reconcile(context.Background(), name); err != nil {
		t.Fatalf("first reconcile() error = %v", err)
	}
	if woke := reproQueued(e); len(woke) != 0 {
		t.Errorf("release re-enqueued %v, want none: the releasing run's terminal status is not in this cache yet, so it re-gates itself on the cooldown stamp its own release just set", woke)
	}

	// If it self-woke, replay that queued reconcile exactly as the single worker
	// would. It reads the same stale copy, the cooldown gate rejects it, and the
	// deferred Pending write must not demote the already-durable terminal status.
	if err := e.reconcile(context.Background(), name); err != nil {
		t.Fatalf("second reconcile() error = %v", err)
	}
	got, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != repackv1alpha1.RepackSucceeded {
		t.Errorf("phase = %q, want %q: a stale Pending observation demoted the terminal status",
			got.Status.Phase, repackv1alpha1.RepackSucceeded)
	}
}
