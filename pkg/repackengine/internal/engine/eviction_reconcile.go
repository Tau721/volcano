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
	"math"
	"math/rand"
	"net"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	engineapi "volcano.sh/volcano/pkg/repackengine/api"
	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
	evictionexecutor "volcano.sh/volcano/pkg/repackengine/executor/eviction"
	placementexecutor "volcano.sh/volcano/pkg/repackengine/executor/placement"
	engineframework "volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

type plannedVictim struct {
	relocationIndex int
	namespace       string
	podGroupName    string
	podName         string
	sourceNode      string
	targetNode      string
	freesNode       bool
}

type evictionSummary struct {
	accepted          int
	indirectlyRemoved int
	rejected          int
}

type evictionRetryState struct {
	failures    int
	nextAttempt time.Time
}

type evictionAttempt struct {
	victim plannedVictim
	pod    *v1.Pod
}

const (
	evictionRetryBaseDelay = 2 * time.Second
	evictionRetryMaxDelay  = 30 * time.Second
	evictionRetryJitter    = 0.20
)

// executePreparedEvictions advances the durable per-Pod eviction journal. Every
// API call is preceded by an InProgress status barrier and followed by a durable
// outcome. If the engine crashes after the API call, the next reconcile observes
// the original Pod UID before deciding whether the request needs to be retried.
func (e *Engine) executePreparedEvictions(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	generation int64,
	targetResource v1.ResourceName,
) engineframework.RuntimeResult {
	return e.executePreparedEvictionsWithClient(
		ctx, run, generation, targetResource, e.clusterCache.Client())
}

func (e *Engine) executePreparedEvictionsWithClient(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	generation int64,
	targetResource v1.ResourceName,
	kubernetesClient kubernetes.Interface,
) engineframework.RuntimeResult {
	if run == nil || run.Status.Plan == nil {
		return runtimeError(fmt.Errorf("resume evictions: durable plan is missing"))
	}
	if executionDeadlinePassed(run, e.now()) {
		return runtimeError(e.timeoutExecution(ctx, run, generation, kubernetesClient))
	}
	if remaining := e.evictionRetryWait(run); remaining > 0 {
		return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), remaining)}
	}
	preparedPlacementGroups := plannedPodGroups(run)
	victims := plannedVictims(run)
	if len(victims) == 0 {
		return runtimeError(e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("resume evictions: durable plan contains no matching relocation")))
	}

	executor := evictionexecutor.New(run, kubernetesClient)
	if executor == nil {
		return runtimeError(e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("resume evictions: eviction hook is not configured")))
	}

	missingVictims := make(map[int]string)
	attempts := make([]evictionAttempt, 0, len(victims))
	retryableBeforeBatch := retryableEvictionCount(run)
	statusChanged := false
	for _, victim := range victims {
		relocation := &run.Status.Relocations[victim.relocationIndex]
		if evictionOutcomeIsFinal(relocation.Eviction.Phase) {
			continue
		}

		pod, err := kubernetesClient.CoreV1().Pods(victim.namespace).Get(ctx, victim.podName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if relocation.Eviction.Phase == repackv1alpha1.PodEvictionInProgress {
				statusChanged = setEvictionOutcome(relocation, repackv1alpha1.PodEvictionAccepted,
					"Victim Pod disappeared after the durable eviction intent; treating the request as accepted during recovery.") || statusChanged
			} else {
				missingVictims[victim.relocationIndex] = "Victim Pod was absent before this eviction attempt."
			}
			continue
		case err != nil:
			return runtimeError(fmt.Errorf("observe victim Pod %s/%s before eviction: %w", victim.namespace, victim.podName, err))
		}

		originalInstanceGone := relocation.VictimPodUID != "" && pod.UID != relocation.VictimPodUID
		originalInstanceTerminating := pod.DeletionTimestamp != nil &&
			(relocation.VictimPodUID == "" || pod.UID == relocation.VictimPodUID)
		if originalInstanceGone || originalInstanceTerminating {
			if relocation.Eviction.Phase == repackv1alpha1.PodEvictionInProgress {
				statusChanged = setEvictionOutcome(relocation, repackv1alpha1.PodEvictionAccepted,
					"The original victim is terminating or gone after the durable eviction intent; treating the request as accepted during recovery.") || statusChanged
			} else {
				missingVictims[victim.relocationIndex] = "The planned victim was already terminating or had been replaced before this eviction attempt."
			}
			continue
		}

		attempts = append(attempts, evictionAttempt{victim: victim, pod: pod.DeepCopy()})
	}

	// One durable barrier covers the complete batch. A crash after any API call
	// leaves every attempted Pod InProgress, allowing UID-based observation to
	// distinguish accepted work from a safe replay.
	if len(attempts) > 0 {
		if run.Status.ExecutionDeadline == nil {
			deadline := metav1.NewTime(e.now().Add(e.executionTimeout()))
			run.Status.ExecutionDeadline = &deadline
			statusChanged = true
		}
		for index := range attempts {
			relocation := &run.Status.Relocations[attempts[index].victim.relocationIndex]
			if relocation.VictimPodUID == "" {
				relocation.VictimPodUID = attempts[index].pod.UID
				statusChanged = true
			}
			if relocation.Eviction.Phase != repackv1alpha1.PodEvictionInProgress {
				relocation.Eviction.Phase = repackv1alpha1.PodEvictionInProgress
				relocation.Eviction.Message = "Eviction intent is durable; the Eviction API request may now be issued."
				statusChanged = true
			}
		}
		if statusChanged {
			if err := e.updateStatus(ctx, run); err != nil {
				return runtimeError(fmt.Errorf("persist eviction batch intent: %w", err))
			}
			statusChanged = false
		}
	}

	evictionContext := ctx
	cancelEvictionContext := func() {}
	if run.Status.ExecutionDeadline != nil {
		evictionContext, cancelEvictionContext = context.WithDeadline(ctx, run.Status.ExecutionDeadline.Time)
	}
	defer cancelEvictionContext()

	for index := range attempts {
		attempt := &attempts[index]
		relocation := &run.Status.Relocations[attempt.victim.relocationIndex]
		task := schedapi.NewTaskInfo(attempt.pod)
		task.Job = schedapi.JobID(attempt.victim.namespace + "/" + attempt.victim.podGroupName)
		move := &engineapi.Move{Task: task, From: attempt.victim.sourceNode, To: attempt.victim.targetNode}
		evictionErr := executor.Evict(evictionContext, move)
		switch {
		case evictionErr == nil:
			if setEvictionOutcome(relocation, repackv1alpha1.PodEvictionAccepted,
				"Eviction API accepted the planned victim.") {
				statusChanged = true
			}
		case errors.Is(evictionErr, evictionexecutor.ErrVictimNotFound):
			if setEvictionOutcome(relocation, repackv1alpha1.PodEvictionAccepted,
				"The victim disappeared after the durable eviction intent; treating the request as accepted.") {
				statusChanged = true
			}
		case isRetryableEvictionError(evictionErr):
			if setEvictionOutcome(relocation, repackv1alpha1.PodEvictionInProgress, evictionErr.Error()) {
				statusChanged = true
			}
		default:
			if setEvictionOutcome(relocation, repackv1alpha1.PodEvictionRejected, evictionErr.Error()) {
				statusChanged = true
			}
		}
		klog.V(4).InfoS("repack: eviction batch outcome observed",
			"run", run.Name, "pod", attempt.victim.namespace+"/"+attempt.victim.podName,
			"phase", relocation.Eviction.Phase, "message", relocation.Eviction.Message)
	}

	// A workload-level recreation may indirectly remove remaining siblings after
	// one accepted eviction. Missing is an in-memory observation, not a public
	// phase. Persist only the final outcome after every victim has been visited so
	// classification is deterministic regardless of Pod order.
	statusChanged = classifyMissingVictims(run.Status.Relocations, missingVictims) || statusChanged

	retryable := hasRetryableEvictions(run)
	if retryable {
		metrics.ObserveEvictionRetryBatch(retryableEvictionCount(run))
		delay := e.scheduleEvictionRetry(run, retryableEvictionCount(run) < retryableBeforeBatch)
		if hasUnfinishedAcceptedPlacement(run) {
			initializeExecuteResultFromStatus(run)
			statusChanged = state.MarkRunning(run, state.ReasonReconcilingPlacements,
				enginestatus.PlacementProgressMessage(run, targetResource)) || statusChanged
			if statusChanged {
				if err := e.updateStatus(ctx, run); err != nil {
					return runtimeError(fmt.Errorf("persist mixed eviction batch outcome: %w", err))
				}
				e.recordRunEvent(run, v1.EventTypeWarning, eventReasonEvictionRetryScheduled,
					fmt.Sprintf("%d Pod evictions remain temporarily blocked; accepted replacements are being restored before the next retry.", retryableEvictionCount(run)))
			}
			return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
		}
		statusChanged = state.MarkRunning(run, state.ReasonEvicting,
			"Eviction is temporarily blocked; retrying the remaining victim Pods with backoff.") || statusChanged
		if statusChanged {
			if err := e.updateStatus(ctx, run); err != nil {
				return runtimeError(fmt.Errorf("persist retryable eviction batch outcome: %w", err))
			}
			e.recordRunEvent(run, v1.EventTypeWarning, eventReasonEvictionRetryScheduled,
				fmt.Sprintf("%d Pod evictions remain temporarily blocked; retrying the batch after %s.", retryableEvictionCount(run), delay.Round(time.Millisecond)))
		}
		return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), delay)}
	}
	e.clearEvictionRetry(run)
	if statusChanged {
		if err := e.updateStatus(ctx, run); err != nil {
			return runtimeError(fmt.Errorf("persist eviction batch outcome: %w", err))
		}
	}

	summary := summarizeEvictions(run.Status.Relocations)
	plannedVictimCount := plannedVictimCount(run)
	if classifiedCount := summary.accepted + summary.indirectlyRemoved + summary.rejected; classifiedCount < plannedVictimCount {
		// Rejected relocations are removed after the durable result barrier. If a
		// later placement-reconciliation write is retried, recover their count from the
		// immutable plan instead of losing it from operator output.
		summary.rejected += plannedVictimCount - classifiedCount
	}
	klog.V(3).InfoS("repack: durable eviction journal completed",
		"run", run.Name, "acceptedCount", summary.accepted,
		"indirectlyRemovedCount", summary.indirectlyRemoved, "rejectedCount", summary.rejected)

	retainSuccessfulRelocations(run)
	initializeExecuteResultFromStatus(run)
	groupsToRelease := placementGroupsDifference(preparedPlacementGroups, placementPodGroups(run))

	// Always persist the accepted subset and result before releasing a lease or
	// entering placement. This is the commit barrier for the complete eviction
	// journal, including the all-groups-retained case.
	if err := e.updateStatus(ctx, run); err != nil {
		return runtimeError(fmt.Errorf("persist completed eviction journal before lease cleanup: %w", err))
	}
	if err := e.releasePlacementLeases(ctx, run, groupsToRelease); err != nil {
		placementexecutor.MarkBenefitUnverified(run)
		return runtimeError(e.fail(ctx, run, generation, state.ReasonEvictionFailed, err))
	}

	realizedFreedNodes := enginestatus.RealizedFreedNodeNames(run)
	if summary.accepted == 0 {
		e.observeEvictionSummary(run, summary)
		return runtimeError(e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("all %d planned evictions were rejected; no Pods were moved", summary.rejected)))
	}
	if len(realizedFreedNodes) == 0 {
		e.observeEvictionSummary(run, summary)
		return runtimeError(e.fail(ctx, run, generation, state.ReasonEvictionFailed,
			fmt.Errorf("Eviction API accepted %d Pods and workload recreation removed %d additional planned Pods, but no planned node was fully vacated (%d requests rejected)",
				summary.accepted, summary.indirectlyRemoved, summary.rejected)))
	}

	state.MarkRunning(run, state.ReasonReconcilingPlacements, enginestatus.PlacementProgressMessage(run, targetResource))
	if err := e.updateStatus(ctx, run); err != nil {
		return runtimeError(fmt.Errorf("persist awaiting placement status: %w", err))
	}
	e.recordRunEvent(run, v1.EventTypeNormal, eventReasonReconcilingPlacements,
		enginestatus.PlacementProgressMessage(run, targetResource))
	e.observeEvictionSummary(run, summary)
	klog.V(3).InfoS("repack: accepted evictions are awaiting replacement placement",
		"run", run.Name, "acceptedCount", summary.accepted,
		"indirectlyRemovedCount", summary.indirectlyRemoved,
		"rejectedCount", summary.rejected, "realizedFreedNodes", realizedFreedNodes,
		"relocationCount", len(run.Status.Relocations))
	return engineframework.RuntimeResult{RequeueAfter: capAtExecutionDeadline(run, e.now(), placementRetryInterval)}
}

func (e *Engine) executionTimeout() time.Duration {
	if e.config.ExecutionTimeout > 0 {
		return e.config.ExecutionTimeout
	}
	return engineconf.DefaultExecutionTimeout
}

func setEvictionOutcome(relocation *repackv1alpha1.PodRelocationStatus, phase repackv1alpha1.PodEvictionPhase, message string) bool {
	if relocation.Eviction.Phase == phase && relocation.Eviction.Message == message {
		return false
	}
	relocation.Eviction.Phase = phase
	relocation.Eviction.Message = message
	return true
}

func isRetryableEvictionError(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsTooManyRequests(err) || apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}

func hasRetryableEvictions(run *repackv1alpha1.RepackRun) bool {
	return retryableEvictionCount(run) > 0
}

func retryableEvictionCount(run *repackv1alpha1.RepackRun) int {
	if run == nil {
		return 0
	}
	count := 0
	for index := range run.Status.Relocations {
		phase := run.Status.Relocations[index].Eviction.Phase
		if phase == repackv1alpha1.PodEvictionPending || phase == repackv1alpha1.PodEvictionInProgress {
			count++
		}
	}
	return count
}

func hasUnfinishedAcceptedPlacement(run *repackv1alpha1.RepackRun) bool {
	if run == nil {
		return false
	}
	for index := range run.Status.Relocations {
		relocation := &run.Status.Relocations[index]
		if relocation.Eviction.Phase != repackv1alpha1.PodEvictionAccepted &&
			relocation.Eviction.Phase != repackv1alpha1.PodEvictionIndirectlyRemoved {
			continue
		}
		if relocation.Placement.Phase != repackv1alpha1.PodPlacementPlaced &&
			relocation.Placement.Phase != repackv1alpha1.PodPlacementTimedOut {
			return true
		}
	}
	return false
}

func (e *Engine) evictionRetryKey(run *repackv1alpha1.RepackRun) string {
	if run.UID != "" {
		return string(run.UID)
	}
	return run.Name
}

func (e *Engine) evictionRetryWait(run *repackv1alpha1.RepackRun) time.Duration {
	state, found := e.evictionRetries[e.evictionRetryKey(run)]
	if !found {
		return 0
	}
	remaining := state.nextAttempt.Sub(e.now())
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func (e *Engine) scheduleEvictionRetry(run *repackv1alpha1.RepackRun, progress bool) time.Duration {
	if e.evictionRetries == nil {
		e.evictionRetries = make(map[string]evictionRetryState)
	}
	key := e.evictionRetryKey(run)
	retry := e.evictionRetries[key]
	if progress {
		retry.failures = 0
	}
	exponent := math.Min(float64(retry.failures), 4)
	delay := time.Duration(float64(evictionRetryBaseDelay) * math.Pow(2, exponent))
	if delay > evictionRetryMaxDelay {
		delay = evictionRetryMaxDelay
	}
	// Symmetric jitter prevents multiple blocked Runs or controllers from
	// repeatedly striking the API server at the same instant.
	delay = time.Duration(float64(delay) * (1 - evictionRetryJitter + rand.Float64()*2*evictionRetryJitter))
	if delay > evictionRetryMaxDelay {
		delay = evictionRetryMaxDelay
	}
	retry.failures++
	retry.nextAttempt = e.now().Add(delay)
	e.evictionRetries[key] = retry
	return delay
}

func (e *Engine) clearEvictionRetry(run *repackv1alpha1.RepackRun) {
	if e.evictionRetries != nil && run != nil {
		delete(e.evictionRetries, e.evictionRetryKey(run))
	}
}

func capAtExecutionDeadline(run *repackv1alpha1.RepackRun, now time.Time, delay time.Duration) time.Duration {
	if run == nil || run.Status.ExecutionDeadline == nil {
		return delay
	}
	remaining := run.Status.ExecutionDeadline.Sub(now)
	if remaining <= 0 {
		return time.Nanosecond
	}
	if delay <= 0 || delay > remaining {
		return remaining
	}
	return delay
}

func runtimeError(err error) engineframework.RuntimeResult {
	return engineframework.RuntimeResult{Err: err}
}

func (e *Engine) observeEvictionSummary(run *repackv1alpha1.RepackRun, summary evictionSummary) {
	metrics.ObserveEvictions(summary.accepted, summary.rejected)
	metrics.ObserveIndirectRemovals(summary.indirectlyRemoved)
	eventType := v1.EventTypeNormal
	if summary.rejected > 0 {
		eventType = v1.EventTypeWarning
	}
	e.recordRunEvent(run, eventType, eventReasonEvictionsIssued,
		fmt.Sprintf("Eviction API accepted %d Pods; %d additional planned Pods were indirectly removed; %d requests were rejected.",
			summary.accepted, summary.indirectlyRemoved, summary.rejected))
	if summary.indirectlyRemoved > 0 {
		e.recordRunEvent(run, v1.EventTypeNormal, eventReasonIndirectRemovalObserved,
			fmt.Sprintf("Retained %d replacement placements after their original Pods were indirectly removed.",
				summary.indirectlyRemoved))
	}
}

func classifyMissingVictims(relocations []repackv1alpha1.PodRelocationStatus, missingVictims map[int]string) bool {
	if len(missingVictims) == 0 {
		return false
	}
	acceptedPodGroups := map[string]struct{}{}
	for index := range relocations {
		relocation := &relocations[index]
		if relocation.Eviction.Phase == repackv1alpha1.PodEvictionAccepted {
			acceptedPodGroups[relocation.Namespace+"/"+relocation.PodGroupName] = struct{}{}
		}
	}
	for index, observation := range missingVictims {
		if index < 0 || index >= len(relocations) {
			continue
		}
		relocation := &relocations[index]
		if _, found := acceptedPodGroups[relocation.Namespace+"/"+relocation.PodGroupName]; found {
			relocation.Eviction.Phase = repackv1alpha1.PodEvictionIndirectlyRemoved
			relocation.Eviction.Message = observation +
				" Another eviction in the same PodGroup was accepted, so Repack is treating this victim as indirectly removed and retaining replacement placement."
		} else {
			relocation.Eviction.Phase = repackv1alpha1.PodEvictionRejected
			relocation.Eviction.Message = observation +
				" No accepted eviction in the same PodGroup supports an indirect removal, so replacement placement will not be attempted."
		}
	}
	return true
}
