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

// Package metrics defines the volcano-repack-engine Prometheus metrics. They are
// registered on the default registry (via promauto), which the engine's /metrics
// endpoint serves. Emit points live in the internal Engine runtime.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

	// EvictionsTotal counts planned Pod disruption outcomes during Execute.
	EvictionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Subsystem: subsystem,
		Name:      "evictions_total",
		Help:      "Number of planned Pod disruption outcomes during Execute, by result (evicted/rejected/indirectly_removed).",
	}, []string{"result"})

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
)

// ObserveRun records a finished run's mode+outcome.
func ObserveRun(mode, outcome string) { RunsTotal.WithLabelValues(mode, outcome).Inc() }

// ObserveEvictions records eviction results for one Execute commit.
func ObserveEvictions(evicted, rejected int) {
	if evicted > 0 {
		EvictionsTotal.WithLabelValues("evicted").Add(float64(evicted))
	}
	if rejected > 0 {
		EvictionsTotal.WithLabelValues("rejected").Add(float64(rejected))
	}
}

// ObserveIndirectRemovals records planned victims that disappeared after
// another eviction in the same PodGroup was accepted.
func ObserveIndirectRemovals(count int) {
	if count > 0 {
		EvictionsTotal.WithLabelValues("indirectly_removed").Add(float64(count))
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
