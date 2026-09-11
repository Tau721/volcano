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
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// TestOnFragFires: onFrag=0 fires on any positive fragmentation; the derived run
// carries the onFragAbovePercent trigger label.
func TestOnFragFiresAboveZero(t *testing.T) {
	f := newFixture(t, tCreate, DefaultFragEvalCycle)
	halfFragCluster(f)
	f.addPolicy(withOnFrag(policy("pol", "u1", tCreate), 0))

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
	created := f.listPolicyRuns("pol")[0]
	if created.Labels[repackv1alpha1.RepackTriggerLabel] != TriggerOnFrag {
		t.Errorf("trigger label = %q, want onFragAbovePercent", created.Labels[repackv1alpha1.RepackTriggerLabel])
	}
	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 1 {
		t.Errorf("inProgress = %v, want the created run", polAfter.Status.InProgress)
	}
	if polAfter.Status.LastTriggerTime == nil || !polAfter.Status.LastTriggerTime.Time.Equal(tCreate) {
		t.Errorf("lastTriggerTime = %v, want %v", polAfter.Status.LastTriggerTime, tCreate)
	}
}

// TestOnFragStrictlyGreater: fragmentation exactly at the threshold does not
// fire (== is not >); one point below it does.
func TestOnFragStrictlyGreaterThanThreshold(t *testing.T) {
	f := newFixture(t, tCreate, DefaultFragEvalCycle)
	halfFragCluster(f)
	equal := withOnFrag(policy("at-threshold", "u1", tCreate), 50) // rate 0.5 == 50%
	f.addPolicy(equal)

	f.reconcile("at-threshold")
	if got := len(listRunNames(f)); got != 0 {
		t.Fatalf("rate == threshold fired: %d runs", got)
	}
	f.assertHealthy("at-threshold", repackv1alpha1.ReasonReconcileSucceeded, "no trigger fired")

	below := withOnFrag(policy("just-below", "u2", tCreate), 49)
	f.addPolicy(below)
	f.reconcile("just-below")
	if got := len(listRunNames(f)); got != 1 {
		t.Fatalf("rate 0.5 > 0.49 did not fire: %d runs", got)
	}
}

// TestOnFragThrottle: within an eval cycle of the last create a hit is blocked
// (Healthy, no create) and lastTriggerTime is not advanced; once the cycle
// elapses the hit fires again.
func TestOnFragThrottleBlocksAndRecovers(t *testing.T) {
	const evalCycle = 5 * time.Minute
	lastCreate := tCreate
	f := newFixture(t, lastCreate.Add(time.Minute), evalCycle)
	halfFragCluster(f)
	pol := withOnFrag(policy("pol", "u1", lastCreate), 0)
	pol.Status.LastTriggerTime = &metav1.Time{Time: lastCreate} // a run was derived at tCreate
	f.addPolicy(pol)

	// now = lastCreate + 1m < evalCycle: throttle holds.
	f.reconcile("pol")
	if got := len(listRunNames(f)); got != 0 {
		t.Fatalf("throttled hit created %d runs", got)
	}
	f.assertHealthy("pol", repackv1alpha1.ReasonReconcileSucceeded, "no trigger fired")
	if got := f.getPolicy("pol").Status.LastTriggerTime; got == nil || !got.Time.Equal(lastCreate) {
		t.Errorf("throttled reconcile advanced lastTriggerTime to %v, want %v", got, lastCreate)
	}

	// Now exactly evalCycle later: the throttle lapses and the hit fires.
	f.clock = lastCreate.Add(evalCycle)
	f.reconcile("pol")
	if got := len(listRunNames(f)); got != 1 {
		t.Fatalf("post-throttle hit created %d runs, want 1", got)
	}
}

// TestOnFragUnmeasurableEmptyGoals: onFrag configured with no template goal
// resource cannot be measured, so it never fires but still reconciles Healthy
// (and keeps the self-sustaining schedule); no crash.
func TestOnFragUnmeasurableEmptyGoals(t *testing.T) {
	f := newFixture(t, tCreate, DefaultFragEvalCycle)
	halfFragCluster(f)
	pol := withOnFrag(withNoGoals(policy("pol", "u1", tCreate)), 0)
	f.addPolicy(pol)

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 0 {
		t.Fatalf("unmeasurable onFrag created %d runs", got)
	}
	f.assertHealthy("pol", repackv1alpha1.ReasonReconcileSucceeded, "unmeasurable")
	if _, ok := f.lastScheduled("pol"); !ok {
		t.Error("unmeasurable onFrag still arms a wakeup (no trigger fired but not frozen)")
	}
}

// TestOnFragDryRunNotBackToBack: a DryRun hit that finishes does not immediately
// re-trigger (level trigger + DryRun leaves fragmentation high, but the eval
// cycle throttle spaces derived runs apart).
func TestOnFragDryRunNotBackToBack(t *testing.T) {
	const evalCycle = 5 * time.Minute
	first := tCreate
	f := newFixture(t, first, evalCycle)
	halfFragCluster(f)
	f.addPolicy(withOnFrag(withMode(policy("pol", "u1", first), repackv1alpha1.RepackModeDryRun), 0))

	// First hit derives run1 (DryRun), which then completes.
	f.reconcile("pol")
	runs := listRunNames(f)
	if len(runs) != 1 {
		t.Fatalf("first hit runs = %d, want 1", len(runs))
	}
	f.finishRun(runs[0], repackv1alpha1.RepackSucceeded, first.Add(10*time.Second))

	// Within the eval cycle the terminal event lets the gate pass, but the level
	// trigger is still throttled: no second run.
	f.clock = first.Add(time.Minute)
	f.reconcile("pol")
	if got := len(listRunNames(f)); got != 1 {
		t.Fatalf("back-to-back DryRun derived %d runs, want 1", got)
	}
	if got := f.getPolicy("pol").Status.LastTriggerTime; got == nil || !got.Time.Equal(first) {
		t.Errorf("lastTriggerTime = %v, want unchanged %v during throttle", got, first)
	}

	// After evalCycle a fresh DryRun run is derived (interval >= evalCycle).
	f.clock = first.Add(evalCycle)
	f.reconcile("pol")
	if got := len(listRunNames(f)); got != 2 {
		t.Fatalf("post-throttle runs = %d, want 2 (interval >= evalCycle)", got)
	}
}

// TestOnFragGateBlocksWhileRunInProgress: the same concurrency gate covers the
// frag source — an in-flight run stops a new frag-derived run.
func TestOnFragGateBlocksWhileRunInProgress(t *testing.T) {
	f := newFixture(t, tCreate, DefaultFragEvalCycle)
	halfFragCluster(f)
	pol := withOnFrag(policy("pol", "u1", tCreate), 0)
	pol.Status.InProgress = []v1.ObjectReference{runRef("pol-live")}
	f.addPolicy(pol)
	live := run("pol-live", "pol", "u1", TriggerOnFrag, repackv1alpha1.RepackModeDryRun, tCreate)
	withRunPhase(live, repackv1alpha1.RepackRunning, nil)
	f.addRun(live)

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 1 {
		t.Fatalf("gate let a second run through: %d runs", got)
	}
	f.assertHealthy("pol", repackv1alpha1.ReasonReconcileSucceeded, "still in progress")
}

// finishRun marks an existing derived run terminal in both the fake API and the
// lister, as the engine would on completion.
func (f *fixture) finishRun(name string, phase repackv1alpha1.RepackPhase, completion time.Time) {
	f.t.Helper()
	run, err := f.getRun(name)
	if err != nil {
		f.t.Fatalf("get run %s to finish: %v", name, err)
	}
	withRunPhase(run, phase, &completion)
	if _, err := f.vc.RepackV1alpha1().RepackRuns().Update(context.Background(), run, metav1.UpdateOptions{}); err != nil {
		f.t.Fatalf("update run %s status: %v", name, err)
	}
	if err := f.run.Update(run); err != nil {
		f.t.Fatalf("update run %s indexer: %v", name, err)
	}
}

// errNodeLister makes node listing fail, probing the measureFrag error path that
// real informers cannot exercise under test.
type errNodeLister struct{}

func (errNodeLister) List(labels.Selector) ([]*v1.Node, error) { return nil, errFragList }
func (errNodeLister) Get(string) (*v1.Node, error)             { return nil, errFragList }

var errFragList = fmt.Errorf("lister unavailable")

// TestOnFragMeasurementFailureSurfacesDegraded: a lister failure during onFrag
// measurement must not read as "0% fragmented, all healthy". Reconcile reports
// Healthy=False/ReconcileFailed, derives nothing, and still arms the next eval
// cycle so it self-heals once the cache recovers (unlike an invalid cron, which
// arms no wakeup because nothing can fix it until the spec changes).
func TestOnFragMeasurementFailureSurfacesDegraded(t *testing.T) {
	f := newFixture(t, tCreate, DefaultFragEvalCycle)
	f.addPolicy(withOnFrag(policy("pol", "u1", tCreate), 0))
	f.c.nodeLister = errNodeLister{}

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 0 {
		t.Fatalf("measurement failure derived %d runs", got)
	}
	f.assertUnhealthy("pol", repackv1alpha1.ReasonReconcileFailed, "fragmentation measurement failed")
	if d, ok := f.lastScheduled("pol"); !ok || d != DefaultFragEvalCycle {
		t.Errorf("wakeup after measurement failure = %v (present=%v), want next eval cycle %v", d, ok, DefaultFragEvalCycle)
	}
}
