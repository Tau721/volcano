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
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

var (
	tCreate  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	everyMin = "* * * * *"
)

// A cron hit creates a run with the reserved labels, owner reference, inProgress
// append and lastTriggerTime, then re-arms the next slot.
func TestReconcileCronCreate(t *testing.T) {
	clock := tCreate.Add(time.Minute) // 00:01:00, the cron minute boundary
	f := newFixture(t, clock, DefaultFragEvalCycle)
	pol := withCron(policy("pol", "u1", tCreate), everyMin)
	f.addPolicy(pol)

	f.reconcile("pol")

	runs := f.listPolicyRuns("pol")
	if len(runs) != 1 {
		t.Fatalf("created %d runs, want 1", len(runs))
	}
	created := runs[0]
	wantName := "pol-" + clock.UTC().Format(runNameFormat)
	if created.Name != wantName {
		t.Errorf("run name = %q, want %q", created.Name, wantName)
	}
	if created.Labels[repackv1alpha1.RepackPolicyLabel] != "pol" || created.Labels[repackv1alpha1.RepackTriggerLabel] != TriggerCronSchedule {
		t.Errorf("run labels = %v, want policy=pol trigger=cronSchedule", created.Labels)
	}
	owner := created.OwnerReferences
	// Kind is a bare string in the production literal, so pin it to the registered
	// GVK — a typo would silently break kube GC's owner lookup.
	wantGVK := repackv1alpha1.SchemeGroupVersion.WithKind("RepackPolicy")
	if len(owner) != 1 {
		t.Fatalf("owner references = %+v, want exactly one", owner)
	}
	if owner[0].Kind != wantGVK.Kind || owner[0].APIVersion != wantGVK.GroupVersion().String() {
		t.Errorf("owner reference = %+v, want kind=%s apiVersion=%s", owner[0], wantGVK.Kind, wantGVK.GroupVersion().String())
	}
	if owner[0].UID != "u1" || owner[0].Controller == nil || !*owner[0].Controller ||
		owner[0].BlockOwnerDeletion == nil || *owner[0].BlockOwnerDeletion {
		t.Errorf("owner reference = %+v, want {u1 controller=true blockOwnerDeletion=false}", owner[0])
	}

	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 1 || polAfter.Status.InProgress[0].Name != created.Name {
		t.Errorf("inProgress = %v, want [%s]", polAfter.Status.InProgress, created.Name)
	}
	if polAfter.Status.LastTriggerTime == nil || !polAfter.Status.LastTriggerTime.Time.Equal(clock) {
		t.Errorf("lastTriggerTime = %v, want %v", polAfter.Status.LastTriggerTime, clock)
	}
	f.assertHealthy("pol", repackv1alpha1.ReasonReconcileSucceeded, "created RepackRun")

	// Step 7 re-arms the strictly-future slot 00:02:00.
	if d, ok := f.lastScheduled("pol"); !ok || d != time.Minute {
		t.Errorf("schedule after = %v (present=%v), want 1m", d, ok)
	}
}

// An unparseable cronSchedule must surface as Healthy=False / ReconcileFailed
// instead of a silent never-firing "no trigger" success. Admission only rejects
// TZ/CRON_TZ, so full robfig validation lands here (upstream CronJob rejects it at
// admission; RepackPolicy has no webhook).
func TestReconcileInvalidCronSurfacesFailure(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	f.addPolicy(withCron(policy("pol", "u1", tCreate), "bad schedule"))

	f.reconcile("pol")

	f.assertUnhealthy("pol", repackv1alpha1.ReasonReconcileFailed, "invalid cronSchedule")
	if got := f.listPolicyRuns("pol"); len(got) != 0 {
		t.Fatalf("invalid cron must not derive runs, got %v", got)
	}
	if _, ok := f.lastScheduled("pol"); ok {
		t.Fatal("invalid cron must not arm a wakeup (nothing can fire until the schedule is fixed)")
	}
}

// The same fire point delivered twice (duplicate wake + terminal event collision)
// deduplicates on lastTriggerTime and yields exactly one run.
func TestReconcileCronDuplicateWakeDedups(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	f.addPolicy(withCron(policy("pol", "u1", tCreate), everyMin))

	f.reconcile("pol")
	f.reconcile("pol") // same instant again

	if got := len(listRunNames(f)); got != 1 {
		t.Fatalf("runs after duplicate wake = %d, want 1", got)
	}
}

// The anchor is max(lastTriggerTime, creationTimestamp): after a restart a
// recorded slot is not re-fired; a fresh policy only waits for the next future
// slot instead of firing slots predating its creation.
func TestReconcileCronAnchorRespectsLastTriggerAndCreation(t *testing.T) {
	// Recorded fire at 00:01:00; now sits before the 00:02:00 slot.
	f := newFixture(t, tCreate.Add(90*time.Second), DefaultFragEvalCycle)
	pol := withCron(policy("pol", "u1", tCreate), everyMin)
	pol.Status.LastTriggerTime = &metav1.Time{Time: tCreate.Add(time.Minute)}
	f.addPolicy(pol)
	f.reconcile("pol")
	if got := len(listRunNames(f)); got != 0 {
		t.Fatalf("re-fired a recorded slot: %d runs created", got)
	}

	// A policy created mid-minute must not fire the boundary before its creation
	// (00:00:00 here); Next(00:00:20) is 00:01:00 which is not reached yet.
	f2 := newFixture(t, tCreate.Add(30*time.Second), DefaultFragEvalCycle)
	f2.addPolicy(withCron(policy("pol2", "u2", tCreate.Add(20*time.Second)), everyMin))
	f2.reconcile("pol2")
	if got := len(listRunNames(f2)); got != 0 {
		t.Fatalf("fired a slot predating creation: %d runs", got)
	}
}

// A policy suspended across several cron slots only catches up one missed slot
// on resume, executed at now (not a timestamp replay).
func TestReconcileCronResumeCatchesUpOne(t *testing.T) {
	clock := tCreate.Add(10 * time.Minute) // controller idle through 10 slots
	f := newFixture(t, clock, DefaultFragEvalCycle)
	f.addPolicy(withCron(policy("pol", "u1", tCreate), everyMin))

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 1 {
		t.Fatalf("runs after 10 missed slots = %d, want 1", got)
	}
	wantName := "pol-" + clock.UTC().Format(runNameFormat)
	if runs := listRunNames(f); runs[0] != wantName {
		t.Errorf("caught-up run = %q, want name stamped at now %q", runs[0], wantName)
	}
}

// A policy that vanished (lister NotFound) reconciles silently — no create, no
// status write, no wakeup.
func TestReconcilePolicyNotFound(t *testing.T) {
	f := newFixture(t, tCreate.Add(time.Minute), DefaultFragEvalCycle)
	if err := f.c.reconcile(context.Background(), "ghost"); err != nil {
		t.Fatalf("reconcile ghost = %v, want nil", err)
	}
	if got := len(listRunNames(f)); got != 0 {
		t.Fatalf("created runs for ghost policy = %d", got)
	}
	if len(f.scheduled) != 0 {
		t.Fatalf("ghost policy scheduled a wakeup")
	}
}

// Suspend is a full stop — a due cron slot creates nothing, lastEvaluationTime is
// untouched, and no wakeup is armed. Terminal convergence still runs (history
// precedes the suspend branch).
func TestReconcileSuspendStopsEverything(t *testing.T) {
	clock := tCreate.Add(time.Minute) // cron slot due
	f := newFixture(t, clock, DefaultFragEvalCycle)
	pol := withCron(withSuspend(policy("pol", "u1", tCreate), true), everyMin)
	f.addPolicy(pol)

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 0 {
		t.Fatalf("suspended policy created %d runs", got)
	}
	polAfter := f.getPolicy("pol")
	f.assertHealthy("pol", repackv1alpha1.ReasonReconcileSucceeded, "Suspended")
	if polAfter.Status.LastEvaluationTime != nil {
		t.Errorf("suspend advanced lastEvaluationTime = %v", polAfter.Status.LastEvaluationTime)
	}
	if len(f.scheduled) != 0 {
		t.Fatalf("suspended policy armed a wakeup: %v", f.scheduled)
	}
}

// While suspended, a terminal run event still converges inProgress and writes the
// snapshot (passive response is not frozen).
func TestReconcileSuspendStillConvergesTerminalRun(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	pol := withCron(withSuspend(policy("pol", "u1", tCreate), true), everyMin)
	pol.Status.InProgress = []corev1.ObjectReference{runRef("pol-runA")}
	f.addPolicy(pol)
	done := run("pol-runA", "pol", "u1", TriggerCronSchedule, repackv1alpha1.RepackModeExecute, tCreate.Add(30*time.Second))
	withRunPhase(done, repackv1alpha1.RepackSucceeded, timePtr(tCreate.Add(50*time.Second)))
	f.addRun(done)

	f.reconcile("pol")

	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 0 {
		t.Errorf("inProgress after convergence = %v, want empty", polAfter.Status.InProgress)
	}
	if polAfter.Status.LastRunStatus == nil || polAfter.Status.LastRunStatus.Name != "pol-runA" {
		t.Errorf("lastRunStatus = %v, want snapshot of pol-runA", polAfter.Status.LastRunStatus)
	}
	if len(f.scheduled) != 0 {
		t.Errorf("suspended policy armed a wakeup")
	}
}

// With a run still in progress, both trigger sources skip creation and report the
// gate on the condition.
func TestReconcileGateBlocksWhileRunInProgress(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	pol := withCron(policy("pol", "u1", tCreate), everyMin)
	pol.Status.InProgress = []corev1.ObjectReference{runRef("pol-runX")}
	f.addPolicy(pol)
	live := run("pol-runX", "pol", "u1", TriggerCronSchedule, repackv1alpha1.RepackModeExecute, tCreate.Add(30*time.Second))
	withRunPhase(live, repackv1alpha1.RepackRunning, nil)
	f.addRun(live)

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 1 { // only the seeded run
		t.Fatalf("gate let a second run through: %d runs", got)
	}
	f.assertHealthy("pol", repackv1alpha1.ReasonReconcileSucceeded, "still in progress")
	// Step 7 still re-arms the next slot (00:02:00).
	if d, ok := f.lastScheduled("pol"); !ok || d != time.Minute {
		t.Errorf("gated reconcile schedule = %v (present=%v), want 1m", d, ok)
	}
}

// A live crash leftover (owned, non-terminal, not in inProgress) is adopted
// instead of creating a duplicate — even with a name different from the would-be
// creation.
func TestReconcileAdoptsLiveOrphan(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	pol := withCron(policy("pol", "u1", tCreate), everyMin)
	f.addPolicy(pol)
	orphan := run("pol-leftover", "pol", "u1", TriggerCronSchedule, repackv1alpha1.RepackModeExecute, tCreate.Add(30*time.Second))
	withRunPhase(orphan, repackv1alpha1.RepackRunning, nil)
	f.addRun(orphan)

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 1 || listRunNames(f)[0] != "pol-leftover" {
		t.Fatalf("adopt created a duplicate: runs = %v", listRunNames(f))
	}
	polAfter := f.getPolicy("pol")
	if len(polAfter.Status.InProgress) != 1 || polAfter.Status.InProgress[0].Name != "pol-leftover" {
		t.Errorf("inProgress = %v, want adopted [pol-leftover]", polAfter.Status.InProgress)
	}
	if polAfter.Status.LastTriggerTime == nil {
		t.Errorf("orphan adopt did not advance lastTriggerTime")
	}
	f.assertHealthy("pol", repackv1alpha1.ReasonReconcileSucceeded, "adopted RepackRun pol-leftover")
}

// Template labels/annotations merge in, but the reserved policy/trigger keys are
// always the controller's values.
func TestReconcileReservedLabelsOverrideTemplate(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	pol := withCron(policy("pol", "u1", tCreate), everyMin)
	pol.Spec.RunTemplate.ObjectMeta.Labels = map[string]string{
		repackv1alpha1.RepackPolicyLabel:  "other-policy",
		repackv1alpha1.RepackTriggerLabel: "bogus-source",
		"app":                             "web",
	}
	pol.Spec.RunTemplate.ObjectMeta.Annotations = map[string]string{"k": "v"}
	f.addPolicy(pol)

	f.reconcile("pol")

	created := f.listPolicyRuns("pol")[0]
	if created.Labels[repackv1alpha1.RepackPolicyLabel] != "pol" {
		t.Errorf("policy label = %q, want overridden %q", created.Labels[repackv1alpha1.RepackPolicyLabel], "pol")
	}
	if created.Labels[repackv1alpha1.RepackTriggerLabel] != TriggerCronSchedule {
		t.Errorf("trigger label = %q, want %q", created.Labels[repackv1alpha1.RepackTriggerLabel], TriggerCronSchedule)
	}
	if created.Labels["app"] != "web" {
		t.Errorf("template label app lost: %v", created.Labels)
	}
	if created.Annotations["k"] != "v" {
		t.Errorf("template annotation lost: %v", created.Annotations)
	}
}

// An AlreadyExists with a foreign owner (pre-seeded same-name run) is not
// adopted: Healthy=False/ReconcileFailed, no inProgress append, lastTriggerTime
// untouched.
func TestReconcileAlreadyExistsForeignOwnerRejected(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	pol := withCron(policy("pol", "u1", tCreate), everyMin)
	f.addPolicy(pol)
	wantName := "pol-" + clock.UTC().Format(runNameFormat)
	foreign := run(wantName, "pol", "someone-else", TriggerCronSchedule, repackv1alpha1.RepackModeExecute, tCreate.Add(30*time.Second))
	withRunPhase(foreign, repackv1alpha1.RepackRunning, nil)
	f.addRun(foreign)

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 1 {
		t.Fatalf("foreign-name collision produced %d runs", got)
	}
	polAfter := f.getPolicy("pol")
	cond := healthyCondition(polAfter)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != repackv1alpha1.ReasonReconcileFailed {
		t.Errorf("condition = %+v, want Healthy=False/ReconcileFailed", cond)
	}
	if len(polAfter.Status.InProgress) != 0 {
		t.Errorf("inProgress appended a foreign run: %v", polAfter.Status.InProgress)
	}
	if polAfter.Status.LastTriggerTime != nil {
		t.Errorf("lastTriggerTime advanced on rejected collision: %v", polAfter.Status.LastTriggerTime)
	}
}

// An API create failure becomes Healthy=False/ReconcileFailed without appending
// inProgress or spinning.
func TestReconcileCreateFailure(t *testing.T) {
	clock := tCreate.Add(time.Minute)
	f := newFixture(t, clock, DefaultFragEvalCycle)
	f.addPolicy(withCron(policy("pol", "u1", tCreate), everyMin))
	f.vc.PrependReactor("create", "repackruns", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected create failure")
	})

	f.reconcile("pol")

	if got := len(listRunNames(f)); got != 0 {
		t.Fatalf("failed create left a run: %v", listRunNames(f))
	}
	polAfter := f.getPolicy("pol")
	cond := healthyCondition(polAfter)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != repackv1alpha1.ReasonReconcileFailed {
		t.Errorf("condition = %+v, want Healthy=False/ReconcileFailed", cond)
	}
	if len(polAfter.Status.InProgress) != 0 {
		t.Errorf("inProgress appended despite failed create: %v", polAfter.Status.InProgress)
	}
}

// Pure-cron writes lastEvaluationTime and arms only the strictly future cron
// slot; pure-frag arms lastEvaluationTime+evalCycle.
func TestReconcileStep7SchedulesCronAndFrag(t *testing.T) {
	// Pure cron, not yet due: next slot 00:01:00 is 50s ahead.
	f := newFixture(t, tCreate.Add(10*time.Second), DefaultFragEvalCycle)
	f.addPolicy(withCron(policy("pol", "u1", tCreate), everyMin))
	f.reconcile("pol")
	if pol := f.getPolicy("pol"); pol.Status.LastEvaluationTime == nil {
		t.Error("pure cron did not refresh lastEvaluationTime")
	}
	if d, ok := f.lastScheduled("pol"); !ok || d != 50*time.Second {
		t.Errorf("pure-cron schedule = %v (present=%v), want 50s", d, ok)
	}

	// Pure frag below threshold (no cluster fragmentation): eval cadence only.
	f2 := newFixture(t, tCreate, 5*time.Minute)
	f2.addPolicy(withOnFrag(policy("frag", "u2", tCreate), 50))
	f2.reconcile("frag")
	if d, ok := f2.lastScheduled("frag"); !ok || d != 5*time.Minute {
		t.Errorf("pure-frag schedule = %v (present=%v), want 5m", d, ok)
	}
}
