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

// Package metrics defines the volcano-repack-engine Prometheus metrics, plus the
// translation from a terminal RepackRun's status into observations. They are
// registered on the default registry (via promauto), which the engine's /metrics
// endpoint serves; the engine runtime decides when to call the Observe functions.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	engineconf "volcano.sh/volcano/pkg/repackengine/conf"
)

const subsystem = "volcano_repack"

var (
	// RunsTotal counts finished RepackRuns by mode (DryRun/Execute) and terminal
	// outcome (the Complete/Failed condition reason: RepackRecommended, Executed,
	// NoFragmentation, BelowGoalThreshold, ExecuteFailed, ...).
	RunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "runs_total",
		Help:      "Number of finished RepackRuns by mode and terminal outcome.",
	}, []string{"mode", "outcome"})

	// EvictionsTotal counts planned Pod disruption outcomes during Execute. resource
	// is the Run's target, not the victim Pod's own resource.
	EvictionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "evictions_total",
		Help:      "Number of planned Pod disruption outcomes during Execute, by result (evicted/rejected/indirectly_removed) and target resource.",
	}, []string{"resource", "result"})

	EvictionRetryBatchesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "eviction_retry_batches_total",
		Help:      "Number of Execute eviction batches scheduled for retry after transient failures such as PDB rejection.",
	})

	EvictionRetryPodsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "eviction_retry_pods_total",
		Help:      "Number of transiently blocked Pod evictions included in retry batches.",
	})

	// CycleDurationSeconds observes how long one reconcile's plan/act took.
	CycleDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "cycle_duration_seconds",
		Help:      "Wall time of one RepackRun reconcile (plan + optional evict), by mode.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"mode"})

	// GateRejectionsTotal counts Execute runs the K=1/cooldown gate deferred, by
	// reason (AnotherRunActive, ExecuteCoolingDown, ...).
	GateRejectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "gate_rejections_total",
		Help:      "Number of times the Execute serialization gate deferred a run, by reason.",
	}, []string{"reason"})

	// PlannerCandidatesEvaluated observes how many active drain-unit evaluations
	// occur across all steps in one planning pass. It exposes the cheap search
	// width independently of expensive feasibility simulations and wall time.
	PlannerCandidatesEvaluated = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "planner_candidates_evaluated",
		Help:      "Number of drain candidates evaluated in one planning pass.",
		Buckets:   []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000},
	}, []string{"mode"})

	// PlannerFeasibilitySimulations observes how many candidates reached the
	// expensive scheduler-faithful feasibility simulation in one planning pass.
	PlannerFeasibilitySimulations = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "planner_feasibility_simulations",
		Help:      "Number of drain candidates that invoked scheduler-feasibility simulation in one planning pass.",
		Buckets:   []float64{0, 1, 5, 10, 25, 50, 100, 250, 500, 1000},
	}, []string{"mode"})

	// PlannerCandidatesPrunedTotal counts candidates rejected before or during
	// feasibility evaluation. reason has a bounded internal vocabulary.
	PlannerCandidatesPrunedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "planner_candidates_pruned_total",
		Help:      "Number of drain candidates rejected by planning stage and bounded reason.",
	}, []string{"mode", "reason"})

	// MovedCardsTotal counts the cards Execute runs actually relocate, by target
	// resource. A Counter because only the total is asked, not a per-run shape; cards
	// of accepted evictions only (the CRD's MovedCardCount contract), so this is not
	// "cards that changed nodes".
	MovedCardsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "moved_cards_total",
		Help:      "Accelerator cards actually relocated by Execute RepackRuns, by target resource.",
	}, []string{"resource"})

	// AffectedPods is the per-run total of the two below, which split it by cause.
	// Three families rather than one with a "total" label value: the parts cannot
	// reconstruct the total's shape, and a total sitting beside its own parts doubles
	// any unfiltered sum.
	AffectedPods = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "affected_pods",
		Help:      "Pods this Execute RepackRun actually disrupted, by target resource.",
		Buckets:   podCountBuckets,
	}, []string{"resource"})

	// EvictedPods counts the Pods this run evicted on its own request.
	EvictedPods = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "evicted_pods",
		Help:      "Pods this Execute RepackRun evicted on its own request, by target resource.",
		Buckets:   podCountBuckets,
	}, []string{"resource"})

	// IndirectlyRemovedPods counts the Pods that disappeared as collateral once a
	// sibling in the same PodGroup was accepted.
	IndirectlyRemovedPods = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "indirectly_removed_pods",
		Help:      "Pods this Execute RepackRun did not evict itself but that disappeared when a sibling in the same PodGroup was accepted, by target resource.",
		Buckets:   podCountBuckets,
	}, []string{"resource"})

	// FragmentationImprovementPercent uses symmetric buckets because the realized
	// improvement can be negative when replacement placement drifts.
	FragmentationImprovementPercent = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "fragmentation_improvement_percent",
		Help:      "Fragmentation improvement realized by one verified Execute RepackRun, in percentage points.",
		Buckets:   []float64{-100, -60, -40, -20, -10, -5, -2, -1, 0, 1, 2, 3, 4, 5, 6, 8, 10, 15, 20, 30, 40, 60, 80, 100},
	}, []string{"resource"})

	// FreedNodes starts at 0 because the --repack-min-nodes-freed gate is calibrated
	// on exactly that 0-vs-1 boundary, and zero is a verified outcome (planned nodes
	// still occupied after placement drift). The tail is a ~1.5x ladder above 8.
	FreedNodes = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: subsystem,
		Name:      "freed_nodes",
		Help:      "Nodes actually freed of the target resource by one verified Execute RepackRun.",
		Buckets:   []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 24, 32, 48, 64, 96, 128},
	}, []string{"resource"})
)

// podCountBuckets is the ladder for per-run Pod counts. It starts at 0 because a run
// that moved nothing is a real outcome, not a missing sample. Integers up to one
// 8-GPU node, whole-node multiples up to 8 nodes, then a ~1.5x tail.
var podCountBuckets = []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1536, 2048}

// ObserveRun records a finished run's mode+outcome.
func ObserveRun(mode, outcome string) { RunsTotal.WithLabelValues(mode, outcome).Inc() }

// ObserveEvictions records eviction results for one Execute commit.
func ObserveEvictions(resource string, evicted, rejected int) {
	if evicted > 0 {
		EvictionsTotal.WithLabelValues(resource, "evicted").Add(float64(evicted))
	}
	if rejected > 0 {
		EvictionsTotal.WithLabelValues(resource, "rejected").Add(float64(rejected))
	}
}

// ObserveIndirectRemovals records planned victims that disappeared after
// another eviction in the same PodGroup was accepted.
func ObserveIndirectRemovals(resource string, count int) {
	if count > 0 {
		EvictionsTotal.WithLabelValues(resource, "indirectly_removed").Add(float64(count))
	}
}

func ObserveEvictionRetryBatch(pods int) {
	if pods <= 0 {
		return
	}
	EvictionRetryBatchesTotal.Inc()
	EvictionRetryPodsTotal.Add(float64(pods))
}

// ObserveCycle records reconcile wall time for a mode.
func ObserveCycle(mode string, seconds float64) {
	CycleDurationSeconds.WithLabelValues(mode).Observe(seconds)
}

// ObserveGateRejection records one gate deferral by reason.
func ObserveGateRejection(reason string) { GateRejectionsTotal.WithLabelValues(reason).Inc() }

// ObservePlanner records search width, expensive feasibility-simulation usage and the bounded
// preflight rejection reasons for one drain planning pass.
func ObservePlanner(mode string, candidatesEvaluated, feasibilitySimulations int, prunedByReason map[string]int) {
	PlannerCandidatesEvaluated.WithLabelValues(mode).Observe(float64(candidatesEvaluated))
	PlannerFeasibilitySimulations.WithLabelValues(mode).Observe(float64(feasibilitySimulations))
	for reason, count := range prunedByReason {
		if count > 0 {
			PlannerCandidatesPrunedTotal.WithLabelValues(mode, reason).Add(float64(count))
		}
	}
}

// ObserveRunBenefit publishes one terminal Run's realized benefit. Cards moved and
// Pods affected hold for any Execute; freed nodes and the fragmentation improvement
// are only meaningful once the result was verified against a scheduler snapshot.
// defaultResource is the configured fallback for Runs that name no goal resource.
func ObserveRunBenefit(run *repackv1alpha1.RepackRun, defaultResource string) {
	result := run.Status.Result
	if result == nil {
		return // DryRun, or an Execute that failed before any eviction
	}
	resource := string(engineconf.ResolveResource(run, defaultResource))
	evicted, indirectlyRemoved := disruptedPods(run)
	addMovedCards(resource, int(result.MovedCardCount))
	observeDisruptions(resource, evicted, indirectlyRemoved)

	// Unverified results have freedNodeCount zeroed and fragAfterPercent reset to the
	// plan's before value, so both observations would be fabricated.
	if !result.MetricsVerified || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return
	}
	improvement := run.Status.Plan.Summary.FragBeforePercent - result.FragAfterPercent
	observeRunVerifiedBenefit(resource, int(result.FreedNodeCount), int(improvement))
}

// addMovedCards accumulates the cards one terminal Execute run relocated. Zero is
// skipped: Add(0) would create a series for a resource that never moved anything.
func addMovedCards(resource string, cards int) {
	if cards <= 0 {
		return
	}
	MovedCardsTotal.WithLabelValues(resource).Add(float64(cards))
}

// observeDisruptions records how many Pods one terminal Execute run disrupted, as a
// total and by cause. The total is summed here rather than passed in, so it cannot
// drift from the parts. Zero is observed, unlike the Counters: "executed, disrupted
// nothing" is a real outcome.
func observeDisruptions(resource string, evicted, indirectlyRemoved int) {
	AffectedPods.WithLabelValues(resource).Observe(float64(evicted + indirectlyRemoved))
	EvictedPods.WithLabelValues(resource).Observe(float64(evicted))
	IndirectlyRemovedPods.WithLabelValues(resource).Observe(float64(indirectlyRemoved))
}

// observeRunVerifiedBenefit records the benefit of one terminal Execute run whose
// result was verified against a scheduler snapshot. Calling it otherwise publishes
// fabricated zeros as real samples.
func observeRunVerifiedBenefit(resource string, freedNodes, improvementPercent int) {
	FreedNodes.WithLabelValues(resource).Observe(float64(freedNodes))
	FragmentationImprovementPercent.WithLabelValues(resource).Observe(float64(improvementPercent))
}

// disruptedPods counts the Pods the Run actually disrupted. Rejected victims are not
// counted: they never moved.
func disruptedPods(run *repackv1alpha1.RepackRun) (evicted, indirectlyRemoved int) {
	for index := range run.Status.Relocations {
		switch run.Status.Relocations[index].Eviction.Phase {
		case repackv1alpha1.PodEvictionAccepted:
			evicted++
		case repackv1alpha1.PodEvictionIndirectlyRemoved:
			indirectlyRemoved++
		}
	}
	return evicted, indirectlyRemoved
}
