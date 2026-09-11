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

package policy

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/client-go/tools/cache"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// Enqueue and event filtering.
func TestEnqueuePolicyAdd(t *testing.T) {
	f := newFixture(t, time.Unix(0, 0), DefaultFragEvalCycle)
	f.c.enqueuePolicyAdd(policy("pol-a", "u1", time.Unix(0, 0)))
	if got := drainQueue(f.c); !reflect.DeepEqual(got, []string{"pol-a"}) {
		t.Fatalf("enqueued = %v, want [pol-a]", got)
	}
}

func TestEnqueuePolicyUpdateFiltersByGeneration(t *testing.T) {
	f := newFixture(t, time.Unix(0, 0), DefaultFragEvalCycle)
	oldP := policy("pol", "u1", time.Unix(0, 0))
	oldP.Generation = 1

	specBump := oldP.DeepCopy()
	specBump.Generation = 2
	f.c.enqueuePolicyUpdate(oldP, specBump)
	if got := drainQueue(f.c); !reflect.DeepEqual(got, []string{"pol"}) {
		t.Fatalf("spec update enqueued = %v, want [pol]", got)
	}

	// Our own status-only write keeps the generation: must not re-enqueue.
	statusOnly := oldP.DeepCopy()
	statusOnly.Status.InProgress = append(statusOnly.Status.InProgress, runRef("pol-0"))
	f.c.enqueuePolicyUpdate(oldP, statusOnly)
	if got := drainQueue(f.c); len(got) != 0 {
		t.Fatalf("status-only update enqueued %v, want none", got)
	}
}

func TestEnqueueRunUpdateOnlyOnTerminalTransition(t *testing.T) {
	f := newFixture(t, time.Unix(0, 0), DefaultFragEvalCycle)
	base := run("r", "pol", "u1", TriggerCronSchedule, repackv1alpha1.RepackModeDryRun, time.Unix(0, 0))
	running := base.DeepCopy()
	withRunPhase(running, repackv1alpha1.RepackRunning, nil)

	succeeded := base.DeepCopy()
	withRunPhase(succeeded, repackv1alpha1.RepackSucceeded, timePtr(time.Unix(100, 0)))

	// Non-terminal -> terminal (Running -> Succeeded) enqueues the owner policy.
	f.c.enqueueRunUpdate(running, succeeded)
	if got := drainQueue(f.c); !reflect.DeepEqual(got, []string{"pol"}) {
		t.Fatalf("terminal transition enqueued = %v, want [pol]", got)
	}

	// Non-terminal -> non-terminal (Pending -> Running) does not.
	pending := base.DeepCopy()
	withRunPhase(pending, repackv1alpha1.RepackPending, nil)
	f.c.enqueueRunUpdate(pending, running)
	if got := drainQueue(f.c); len(got) != 0 {
		t.Fatalf("Pending->Running enqueued %v, want none", got)
	}

	// Terminal -> terminal does not.
	f.c.enqueueRunUpdate(succeeded, succeeded.DeepCopy())
	if got := drainQueue(f.c); len(got) != 0 {
		t.Fatalf("terminal->terminal enqueued %v, want none", got)
	}

	// A run without the reserved label never enqueues, even on terminal.
	noLabel := succeeded.DeepCopy()
	noLabel.Labels = nil
	f.c.enqueueRunUpdate(running.DeepCopy(), noLabel)
	if got := drainQueue(f.c); len(got) != 0 {
		t.Fatalf("unlabeled run enqueued %v, want none", got)
	}
}

func TestEnqueueRunDeleteAlways(t *testing.T) {
	f := newFixture(t, time.Unix(0, 0), DefaultFragEvalCycle)
	run := run("r", "pol", "u1", TriggerCronSchedule, repackv1alpha1.RepackModeDryRun, time.Unix(0, 0))
	f.c.enqueueRunDelete(run)
	if got := drainQueue(f.c); !reflect.DeepEqual(got, []string{"pol"}) {
		t.Fatalf("delete enqueued = %v, want [pol]", got)
	}

	// Tombstone form is handled too.
	f.c.enqueueRunDelete(cache.DeletedFinalStateUnknown{Obj: run})
	if got := drainQueue(f.c); !reflect.DeepEqual(got, []string{"pol"}) {
		t.Fatalf("tombstone delete enqueued = %v, want [pol]", got)
	}
}

// enqueueRunDelete must tolerate a missing tombstone payload.
func TestEnqueueRunDeleteMalformed(t *testing.T) {
	f := newFixture(t, time.Unix(0, 0), DefaultFragEvalCycle)
	f.c.enqueueRunDelete("not-a-run")
	f.c.enqueueRunDelete(cache.DeletedFinalStateUnknown{})
	if got := drainQueue(f.c); len(got) != 0 {
		t.Fatalf("malformed deletes enqueued %v, want none", got)
	}
}

// The run informer wires Update+Delete handlers only (no AddFunc), so a startup
// replay of pre-existing runs never reconciles them for nothing — a structural
// property of New().
func drainQueue(c *Controller) []string {
	var keys []string
	for c.queue.Len() > 0 {
		key, _ := c.queue.Get()
		keys = append(keys, key)
		c.queue.Done(key)
	}
	return keys
}
