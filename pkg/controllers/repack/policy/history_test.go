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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// addSucceededRun seeds a terminal succeeded run for policy "pol" completing at
// the given time.
func addSucceededRun(f *fixture, name string, created, completed time.Time) {
	f.t.Helper()
	r := run(name, "pol", "u1", TriggerCronSchedule, repackv1alpha1.RepackModeExecute, created)
	withRunPhase(r, repackv1alpha1.RepackSucceeded, &completed)
	f.addRun(r)
}

// Convergence: a Succeeded run leaves inProgress, records the snapshot, and
// bumps lastSuccessfulTime to its completion.
func TestHistorySucceededRunConverges(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	seedPolicyWithInProgress(f, "pol", []corev1.ObjectReference{runRef("pol-r1")})
	addSucceededRun(f, "pol-r1", tCreate, tCreate.Add(30*time.Second))

	f.reconcile("pol")

	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 0 {
		t.Errorf("inProgress = %v, want empty", polAfter.Status.InProgress)
	}
	if polAfter.Status.LastSuccessfulTime == nil || !polAfter.Status.LastSuccessfulTime.Time.Equal(tCreate.Add(30*time.Second)) {
		t.Errorf("lastSuccessfulTime = %v, want 00:00:30", polAfter.Status.LastSuccessfulTime)
	}
	ls := polAfter.Status.LastRunStatus
	if ls == nil || ls.Name != "pol-r1" || ls.Trigger != TriggerCronSchedule ||
		ls.Mode != repackv1alpha1.RepackModeExecute || ls.Resource != testResource || ls.Phase != repackv1alpha1.RepackSucceeded {
		t.Errorf("lastRunStatus = %+v, want pol-r1 succeeded snapshot", ls)
	}
}

// A Failed run leaves inProgress and snapshots but never touches
// lastSuccessfulTime.
func TestHistoryFailedRunConverges(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	seedPolicyWithInProgress(f, "pol", []corev1.ObjectReference{runRef("pol-r1")})
	failed := run("pol-r1", "pol", "u1", TriggerOnFrag, repackv1alpha1.RepackModeExecute, tCreate)
	withRunPhase(failed, repackv1alpha1.RepackFailed, timePtr(tCreate.Add(30*time.Second)))
	f.addRun(failed)

	f.reconcile("pol")

	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 0 {
		t.Errorf("inProgress = %v, want empty", polAfter.Status.InProgress)
	}
	if polAfter.Status.LastSuccessfulTime != nil {
		t.Errorf("failed run wrote lastSuccessfulTime = %v", polAfter.Status.LastSuccessfulTime)
	}
	if ls := polAfter.Status.LastRunStatus; ls == nil || ls.Name != "pol-r1" || ls.Phase != repackv1alpha1.RepackFailed {
		t.Errorf("lastRunStatus = %+v, want pol-r1 failed snapshot", ls)
	}
}

// Among several terminal inProgress runs only the newest completion is
// snapshotted, and lastSuccessfulTime keeps the newest Succeeded completion even
// when a Failed run completes later.
func TestHistoryNewestTerminalOverwritesSnapshot(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	seedPolicyWithInProgress(f, "pol", []corev1.ObjectReference{runRef("pol-succ"), runRef("pol-fail")})
	addSucceededRun(f, "pol-succ", tCreate, tCreate.Add(10*time.Second))
	fail := run("pol-fail", "pol", "u1", TriggerOnFrag, repackv1alpha1.RepackModeExecute, tCreate)
	withRunPhase(fail, repackv1alpha1.RepackFailed, timePtr(tCreate.Add(40*time.Second)))
	f.addRun(fail)

	f.reconcile("pol")

	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 0 {
		t.Errorf("inProgress = %v, want empty", polAfter.Status.InProgress)
	}
	if ls := polAfter.Status.LastRunStatus; ls == nil || ls.Name != "pol-fail" {
		t.Errorf("lastRunStatus = %+v, want newest pol-fail", ls)
	}
	if polAfter.Status.LastSuccessfulTime == nil || !polAfter.Status.LastSuccessfulTime.Time.Equal(tCreate.Add(10*time.Second)) {
		t.Errorf("lastSuccessfulTime = %v, want 00:00:10 (newest succeeded)", polAfter.Status.LastSuccessfulTime)
	}
}

// A run that vanished from the run lister (deleted before its terminal state
// was observed) is dropped without writing a snapshot.
func TestHistoryVanishedRunDroppedWithoutSnapshot(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	seedPolicyWithInProgress(f, "pol", []corev1.ObjectReference{runRef("pol-gone")})
	// Deliberately not seeded: the run is absent from the lister.

	f.reconcile("pol")

	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 0 {
		t.Errorf("inProgress = %v, want empty", polAfter.Status.InProgress)
	}
	if polAfter.Status.LastRunStatus != nil || polAfter.Status.LastSuccessfulTime != nil {
		t.Errorf("vanished run wrote a snapshot/lastSuccessfulTime: %+v", polAfter.Status.LastRunStatus)
	}
}

// A run finished while the controller was down (owned, terminal, not in
// inProgress) is mirrored once and advances lastTriggerTime so its fire is not
// re-evaluated after restart.
func TestHistoryTerminalOrphanMirroredAndAdvancesTrigger(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	pol := policy("pol", "u1", tCreate)
	pol.Status.LastTriggerTime = &metav1.Time{Time: tCreate.Add(30 * time.Second)} // last fire predates the orphan
	f.addPolicy(pol)
	addSucceededRun(f, "pol-orphan", tCreate, tCreate.Add(50*time.Second))

	f.reconcile("pol")

	polAfter := f.getPolicy("pol")
	if ls := polAfter.Status.LastRunStatus; ls == nil || ls.Name != "pol-orphan" {
		t.Errorf("orphan mirror = %+v, want pol-orphan", ls)
	}
	if polAfter.Status.LastSuccessfulTime == nil || !polAfter.Status.LastSuccessfulTime.Time.Equal(tCreate.Add(50*time.Second)) {
		t.Errorf("orphan lastSuccessfulTime = %v, want 00:00:50", polAfter.Status.LastSuccessfulTime)
	}
	if polAfter.Status.LastTriggerTime == nil || !polAfter.Status.LastTriggerTime.Time.Equal(clock) {
		t.Errorf("lastTriggerTime = %v, want advanced to %v", polAfter.Status.LastTriggerTime, clock)
	}
}

// Several terminal orphans from downtime mirror only the newest, and a repeated
// reconcile is idempotent (no re-advance of lastTriggerTime).
func TestHistoryTerminalOrphansNewestOnlyAndIdempotent(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	f.addPolicy(policy("pol", "u1", tCreate))
	addSucceededRun(f, "pol-orphan-old", tCreate, tCreate.Add(20*time.Second))
	addSucceededRun(f, "pol-orphan-new", tCreate, tCreate.Add(40*time.Second))

	f.reconcile("pol")
	first := f.getPolicy("pol")
	if ls := first.Status.LastRunStatus; ls == nil || ls.Name != "pol-orphan-new" {
		t.Errorf("first mirror = %+v, want newest pol-orphan-new", ls)
	}
	triggerAfterFirst := first.Status.LastTriggerTime

	f.reconcile("pol")
	second := f.getPolicy("pol")
	if ls := second.Status.LastRunStatus; ls == nil || ls.Name != "pol-orphan-new" {
		t.Errorf("second mirror = %+v, want unchanged pol-orphan-new", ls)
	}
	if triggerAfterFirst == nil || second.Status.LastTriggerTime == nil ||
		!triggerAfterFirst.Time.Equal(second.Status.LastTriggerTime.Time) {
		t.Errorf("repeated reconcile re-advanced lastTriggerTime: %v -> %v", triggerAfterFirst, second.Status.LastTriggerTime)
	}
}

// --- history GC ------------------------------------------------

// Beyond a per-phase limit the oldest terminal runs (by creation time) are
// deleted; the newest is never evicted.
func TestHistoryGCDeletesOldestOverLimit(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	limit := int32(1)
	pol := policy("pol", "u1", tCreate)
	pol.Spec.SuccessfulRunsHistoryLimit = &limit
	f.addPolicy(pol)
	addSucceededRun(f, "pol-run-a", tCreate, tCreate.Add(5*time.Second))
	addSucceededRun(f, "pol-run-b", tCreate.Add(time.Second), tCreate.Add(6*time.Second))
	addSucceededRun(f, "pol-run-c", tCreate.Add(2*time.Second), tCreate.Add(7*time.Second))

	f.reconcile("pol")

	got := listRunNames(f)
	if len(got) != 1 || got[0] != "pol-run-c" {
		t.Fatalf("surviving runs = %v, want only newest pol-run-c", got)
	}
}

// A limit of 0 evicts the whole phase category.
func TestHistoryGCLimitZeroDeletesAll(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	zero := int32(0)
	pol := policy("pol", "u1", tCreate)
	pol.Spec.SuccessfulRunsHistoryLimit = &zero
	f.addPolicy(pol)
	addSucceededRun(f, "pol-run-a", tCreate, tCreate.Add(5*time.Second))
	addSucceededRun(f, "pol-run-b", tCreate.Add(time.Second), tCreate.Add(6*time.Second))

	f.reconcile("pol")

	if got := listRunNames(f); len(got) != 0 {
		t.Fatalf("surviving runs = %v, want none under limit=0", got)
	}
}

// Succeeded and failed phases recycle independently under their own limits.
func TestHistoryGCPhasesIndependent(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	succLimit, failLimit := int32(1), int32(0)
	pol := policy("pol", "u1", tCreate)
	pol.Spec.SuccessfulRunsHistoryLimit = &succLimit
	pol.Spec.FailedRunsHistoryLimit = &failLimit
	f.addPolicy(pol)
	addSucceededRun(f, "pol-succ-a", tCreate, tCreate.Add(5*time.Second))
	addSucceededRun(f, "pol-succ-b", tCreate.Add(time.Second), tCreate.Add(6*time.Second))
	failed := run("pol-fail-a", "pol", "u1", TriggerOnFrag, repackv1alpha1.RepackModeExecute, tCreate)
	withRunPhase(failed, repackv1alpha1.RepackFailed, timePtr(tCreate.Add(7*time.Second)))
	f.addRun(failed)

	f.reconcile("pol")

	got := listRunNames(f)
	if len(got) != 1 || got[0] != "pol-succ-b" {
		t.Fatalf("surviving runs = %v, want only newest succeeded pol-succ-b", got)
	}
}

// A deleted-and-recreated policy's old-incarnation runs (same label, different
// owner UID, older creation) are evicted first without touching the new
// incarnation — GC never needs to inspect ownership.
func TestHistoryGCOldIncarnationRecycledFirst(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	limit := int32(1)
	pol := policy("pol", "u1", tCreate)
	pol.Spec.SuccessfulRunsHistoryLimit = &limit
	f.addPolicy(pol)
	// Old incarnation: same repack-policy label, a different owner UID.
	old := run("pol-old-inc", "pol", "old-uid", TriggerCronSchedule, repackv1alpha1.RepackModeExecute, tCreate)
	withRunPhase(old, repackv1alpha1.RepackSucceeded, timePtr(tCreate.Add(5*time.Second)))
	f.addRun(old)
	addSucceededRun(f, "pol-current", tCreate.Add(time.Second), tCreate.Add(6*time.Second))

	f.reconcile("pol")

	got := listRunNames(f)
	if len(got) != 1 || got[0] != "pol-current" {
		t.Fatalf("surviving runs = %v, want the new incarnation pol-current", got)
	}
}

// A stuck non-terminal run is never recycled by GC — it stays in inProgress
// (gate remains closed) even past any history limit.
func TestHistoryStuckRunningNeverRecycled(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	limit := int32(0)
	pol := policy("pol", "u1", tCreate)
	pol.Spec.SuccessfulRunsHistoryLimit = &limit
	pol.Status.InProgress = []corev1.ObjectReference{runRef("pol-stuck")}
	f.addPolicy(pol)
	stuck := run("pol-stuck", "pol", "u1", TriggerCronSchedule, repackv1alpha1.RepackModeExecute, tCreate)
	withRunPhase(stuck, repackv1alpha1.RepackRunning, nil)
	f.addRun(stuck)

	f.reconcile("pol")

	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 1 || polAfter.Status.InProgress[0].Name != "pol-stuck" {
		t.Errorf("inProgress = %v, want [pol-stuck] kept", polAfter.Status.InProgress)
	}
	if got := listRunNames(f); len(got) != 1 || got[0] != "pol-stuck" {
		t.Fatalf("running run deleted by GC: %v", got)
	}
}

// seedPolicyWithInProgress adds a policy whose status already references the
// given inProgress runs so a test can drive convergence.
func seedPolicyWithInProgress(f *fixture, name string, refs []corev1.ObjectReference) {
	f.t.Helper()
	pol := policy(name, "u1", tCreate)
	pol.Status.InProgress = append([]corev1.ObjectReference(nil), refs...)
	f.addPolicy(pol)
}
