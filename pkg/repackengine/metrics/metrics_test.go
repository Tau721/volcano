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

package metrics

import (
	"math"
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// probeResource is a sentinel accelerator resource name: a labeled vector exports
// nothing until it owns a child, so each family needs one series to become visible.
const probeResource = "probe.example.com/accelerator"

// Each benefit test uses its own resource name: the metrics live on the default registry
// for the whole test binary, so a shared name would leak between tests.
const (
	noResultResource           = "noresult.example.com/gpu"
	unverifiedResource         = "unverified.example.com/gpu"
	positiveResource           = "positive.example.com/gpu"
	negativeResource           = "negative.example.com/gpu"
	totalCheckResource         = "totalcheck.example.com/gpu"
	noopResource               = "noop.example.com/gpu"
	noPlanResource             = "noplan.example.com/gpu"
	planWithoutSummaryResource = "nosummary.example.com/gpu"
)

// defaultTestResource stands in for the engine's configured fallback resource: every
// fixture below names its own goal, so this value is never the one observed.
const defaultTestResource = "default.example.com/gpu"

var (
	movedCardsName        = "volcano_repack_moved_cards_total"
	affectedPodsName      = "volcano_repack_affected_pods"
	evictedPodsName       = "volcano_repack_evicted_pods"
	indirectlyRemovedName = "volcano_repack_indirectly_removed_pods"
	freedNodesName        = "volcano_repack_freed_nodes"
	improvementName       = "volcano_repack_fragmentation_improvement_percent"
	benefitMetricNames    = []string{movedCardsName, affectedPodsName, evictedPodsName, indirectlyRemovedName, freedNodesName, improvementName}
)

func goalResource(resource string) []repackv1alpha1.RepackGoal {
	return []repackv1alpha1.RepackGoal{{Resource: v1.ResourceName(resource)}}
}

func relocationInPhase(phase repackv1alpha1.PodEvictionPhase) repackv1alpha1.PodRelocationStatus {
	return repackv1alpha1.PodRelocationStatus{
		Namespace:       "ns",
		VictimPodName:   "victim-" + string(phase),
		PlannedNodeName: "n1",
		Eviction:        repackv1alpha1.PodEvictionStatus{Phase: phase},
	}
}

// TestMetricSurfaceSnapshot pins every exported metric's name, label set, kind and
// histogram buckets — a rename, a type swap and a bucket change are each silently
// breaking for existing dashboards and queries. Ladders are written out rather than
// read from the package vars, which would keep passing through any change.
func TestMetricSurfaceSnapshot(t *testing.T) {
	podCountLiteralBuckets := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1536, 2048}

	tests := []struct {
		name    string
		labels  []string
		buckets []float64
		touch   func()
	}{
		{name: "volcano_repack_runs_total", labels: []string{"mode", "outcome"},
			touch: func() { RunsTotal.WithLabelValues("test-mode", "test-outcome") }},
		{name: "volcano_repack_evictions_total", labels: []string{"resource", "result"},
			touch: func() { EvictionsTotal.WithLabelValues(probeResource, "test-result") }},
		{name: "volcano_repack_eviction_retry_batches_total"},
		{name: "volcano_repack_eviction_retry_pods_total"},
		{name: "volcano_repack_cycle_duration_seconds", labels: []string{"mode"},
			buckets: prometheus.DefBuckets, touch: func() { CycleDurationSeconds.WithLabelValues("test-mode") }},
		{name: "volcano_repack_gate_rejections_total", labels: []string{"reason"},
			touch: func() { GateRejectionsTotal.WithLabelValues("test-reason") }},
		{name: "volcano_repack_planner_candidates_evaluated", labels: []string{"mode"},
			buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000},
			touch:   func() { PlannerCandidatesEvaluated.WithLabelValues("test-mode") }},
		{name: "volcano_repack_planner_feasibility_simulations", labels: []string{"mode"},
			buckets: []float64{0, 1, 5, 10, 25, 50, 100, 250, 500, 1000},
			touch:   func() { PlannerFeasibilitySimulations.WithLabelValues("test-mode") }},
		{name: "volcano_repack_planner_candidates_pruned_total", labels: []string{"mode", "reason"},
			touch: func() { PlannerCandidatesPrunedTotal.WithLabelValues("test-mode", "test-reason") }},
		{name: "volcano_repack_moved_cards_total", labels: []string{"resource"},
			touch: func() { MovedCardsTotal.WithLabelValues(probeResource) }},
		{name: "volcano_repack_affected_pods", labels: []string{"resource"},
			buckets: podCountLiteralBuckets, touch: func() { AffectedPods.WithLabelValues(probeResource) }},
		{name: "volcano_repack_evicted_pods", labels: []string{"resource"},
			buckets: podCountLiteralBuckets, touch: func() { EvictedPods.WithLabelValues(probeResource) }},
		{name: "volcano_repack_indirectly_removed_pods", labels: []string{"resource"},
			buckets: podCountLiteralBuckets, touch: func() { IndirectlyRemovedPods.WithLabelValues(probeResource) }},
		{name: "volcano_repack_fragmentation_improvement_percent", labels: []string{"resource"},
			buckets: []float64{-100, -60, -40, -20, -10, -5, -2, -1, 0, 1, 2, 3, 4, 5, 6, 8, 10,
				15, 20, 30, 40, 60, 80, 100},
			touch: func() { FragmentationImprovementPercent.WithLabelValues(probeResource) }},
		{name: "volcano_repack_freed_nodes", labels: []string{"resource"},
			buckets: []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 24, 32, 48, 64, 96, 128},
			touch:   func() { FreedNodes.WithLabelValues(probeResource) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.touch != nil {
				tc.touch()
			}
			metric := gatherFirstSample(t, tc.name)
			assertLabelNames(t, tc.name, metric, tc.labels)
			assertMetricKind(t, tc.name, metric, tc.buckets != nil)
			if tc.buckets != nil {
				assertBuckets(t, tc.name, metric, tc.buckets)
			}
		})
	}
}

// TestObserveRunBenefitIgnoresRunsWithoutResult covers DryRun and an Execute that
// failed before any eviction: both have no status.result, so nothing is observed
// rather than a fabricated zero.
func TestObserveRunBenefitIgnoresRunsWithoutResult(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		Spec:   repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeDryRun, Goals: goalResource(noResultResource)},
		Status: repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackSucceeded},
	}
	ObserveRunBenefit(run, defaultTestResource)

	for _, name := range benefitMetricNames {
		assertNoSeries(t, name, noResultResource)
	}
}

// TestObserveRunBenefitSkipsUnverifiedBenefit pins the asymmetry: cards and Pods
// are measured, freed nodes and the improvement are not, because an unverified
// result has freedNodeCount zeroed and fragAfterPercent reset to the plan's
// before value.
func TestObserveRunBenefitSkipsUnverifiedBenefit(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute, Goals: goalResource(unverifiedResource)},
		Status: repackv1alpha1.RepackRunStatus{
			Plan: &repackv1alpha1.RepackPlan{Summary: &repackv1alpha1.RepackSummary{FragBeforePercent: 60}},
			Result: &repackv1alpha1.RepackResult{
				MovedCardCount: 12, FreedNodeCount: 0, FragAfterPercent: 60, MetricsVerified: false,
			},
			Relocations: []repackv1alpha1.PodRelocationStatus{
				relocationInPhase(repackv1alpha1.PodEvictionAccepted),
				relocationInPhase(repackv1alpha1.PodEvictionIndirectlyRemoved),
				relocationInPhase(repackv1alpha1.PodEvictionRejected),
			},
		},
	}
	ObserveRunBenefit(run, defaultTestResource)

	assertCounterValue(t, movedCardsName, unverifiedResource, 12)
	assertHistogramObserved(t, affectedPodsName, unverifiedResource, 1, 2)
	assertHistogramObserved(t, evictedPodsName, unverifiedResource, 1, 1)
	assertHistogramObserved(t, indirectlyRemovedName, unverifiedResource, 1, 1)
	assertNoSeries(t, freedNodesName, unverifiedResource)
	assertNoSeries(t, improvementName, unverifiedResource)
}

// TestObserveRunBenefitCountsDisruptionTotal uses asymmetric counts: with equally
// many evicted and indirectly removed Pods, a total that was really "twice one of the
// parts" would read the same as the correct sum. The rejected relocation must stay
// out of all three — a rejected victim never moved.
func TestObserveRunBenefitCountsDisruptionTotal(t *testing.T) {
	relocations := []repackv1alpha1.PodRelocationStatus{
		relocationInPhase(repackv1alpha1.PodEvictionAccepted),
		relocationInPhase(repackv1alpha1.PodEvictionAccepted),
		relocationInPhase(repackv1alpha1.PodEvictionAccepted),
		relocationInPhase(repackv1alpha1.PodEvictionAccepted),
		relocationInPhase(repackv1alpha1.PodEvictionIndirectlyRemoved),
		relocationInPhase(repackv1alpha1.PodEvictionRejected),
		relocationInPhase(repackv1alpha1.PodEvictionRejected),
	}
	run := &repackv1alpha1.RepackRun{
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute, Goals: goalResource(totalCheckResource)},
		Status: repackv1alpha1.RepackRunStatus{
			Result:      &repackv1alpha1.RepackResult{MovedCardCount: 19},
			Relocations: relocations,
		},
	}
	ObserveRunBenefit(run, defaultTestResource)

	assertHistogramObserved(t, affectedPodsName, totalCheckResource, 1, 5)
	assertHistogramObserved(t, evictedPodsName, totalCheckResource, 1, 4)
	assertHistogramObserved(t, indirectlyRemovedName, totalCheckResource, 1, 1)
}

func TestObserveRunBenefitPublishesVerifiedBenefit(t *testing.T) {
	tests := []struct {
		name        string
		resource    string
		fragBefore  int32
		fragAfter   int32
		freedNodes  int32
		wantImprove float64
	}{
		{name: "improved", resource: positiveResource, fragBefore: 55, fragAfter: 43, freedNodes: 2, wantImprove: 12},
		{name: "regressed", resource: negativeResource, fragBefore: 50, fragAfter: 57, freedNodes: 0, wantImprove: -7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := &repackv1alpha1.RepackRun{
				Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute, Goals: goalResource(tc.resource)},
				Status: repackv1alpha1.RepackRunStatus{
					Plan: &repackv1alpha1.RepackPlan{Summary: &repackv1alpha1.RepackSummary{FragBeforePercent: tc.fragBefore}},
					Result: &repackv1alpha1.RepackResult{
						MovedCardCount: 5, FreedNodeCount: tc.freedNodes,
						FragAfterPercent: tc.fragAfter, MetricsVerified: true,
					},
					Relocations: []repackv1alpha1.PodRelocationStatus{
						relocationInPhase(repackv1alpha1.PodEvictionAccepted),
						relocationInPhase(repackv1alpha1.PodEvictionIndirectlyRemoved),
						// Neither of these was disrupted by this run.
						relocationInPhase(repackv1alpha1.PodEvictionRejected),
						relocationInPhase(repackv1alpha1.PodEvictionInProgress),
					},
				},
			}
			ObserveRunBenefit(run, defaultTestResource)

			assertCounterValue(t, movedCardsName, tc.resource, 5)
			assertHistogramObserved(t, affectedPodsName, tc.resource, 1, 2)
			assertHistogramObserved(t, evictedPodsName, tc.resource, 1, 1)
			assertHistogramObserved(t, indirectlyRemovedName, tc.resource, 1, 1)
			assertHistogramObserved(t, freedNodesName, tc.resource, 1, float64(tc.freedNodes))
			assertHistogramObserved(t, improvementName, tc.resource, 1, tc.wantImprove)
		})
	}
}

// TestObserveRunBenefitObservesZeroForVerifiedNoopExecute pins why the Pod families are
// histograms: "executed and disrupted nothing" is a real outcome that must be counted,
// so a verified zero is observed — while moved_cards_total, a Counter, must not gain a
// series at all for a run that relocated nothing.
func TestObserveRunBenefitObservesZeroForVerifiedNoopExecute(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute, Goals: goalResource(noopResource)},
		Status: repackv1alpha1.RepackRunStatus{
			Plan: &repackv1alpha1.RepackPlan{Summary: &repackv1alpha1.RepackSummary{FragBeforePercent: 40}},
			Result: &repackv1alpha1.RepackResult{
				MovedCardCount: 0, FreedNodeCount: 0, FragAfterPercent: 40, MetricsVerified: true,
			},
		},
	}
	ObserveRunBenefit(run, defaultTestResource)

	assertNoSeries(t, movedCardsName, noopResource)
	assertHistogramObserved(t, affectedPodsName, noopResource, 1, 0)
	assertHistogramObserved(t, evictedPodsName, noopResource, 1, 0)
	assertHistogramObserved(t, indirectlyRemovedName, noopResource, 1, 0)
	assertHistogramObserved(t, freedNodesName, noopResource, 1, 0)
	assertHistogramObserved(t, improvementName, noopResource, 1, 0)
}

// TestObserveRunBenefitSkipsVerifiedBenefitWithoutPlanSummary covers the nil guard: the
// improvement is read off the plan's before value, and a verified result can outlive a
// stripped plan. The run still reports what it measured.
func TestObserveRunBenefitSkipsVerifiedBenefitWithoutPlanSummary(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		plan     *repackv1alpha1.RepackPlan
	}{
		{name: "no plan", resource: noPlanResource},
		{name: "plan without summary", resource: planWithoutSummaryResource, plan: &repackv1alpha1.RepackPlan{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := &repackv1alpha1.RepackRun{
				Spec: repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute, Goals: goalResource(tc.resource)},
				Status: repackv1alpha1.RepackRunStatus{
					Plan: tc.plan,
					Result: &repackv1alpha1.RepackResult{
						MovedCardCount: 7, FreedNodeCount: 8, FragAfterPercent: 30, MetricsVerified: true,
					},
					Relocations: []repackv1alpha1.PodRelocationStatus{
						relocationInPhase(repackv1alpha1.PodEvictionAccepted),
					},
				},
			}
			ObserveRunBenefit(run, defaultTestResource)

			assertCounterValue(t, movedCardsName, tc.resource, 7)
			assertHistogramObserved(t, affectedPodsName, tc.resource, 1, 1)
			assertNoSeries(t, freedNodesName, tc.resource)
			assertNoSeries(t, improvementName, tc.resource)
		})
	}
}

// gatherFirstSample returns the family's first series, failing when the family is not
// exported at all: the surface snapshot asserts against declarations, which some families
// only publish once they own a child.
func gatherFirstSample(t *testing.T, name string) *dto.Metric {
	t.Helper()
	metric := gatherSeries(t, name, nil)
	if metric == nil {
		t.Fatalf("family %q is not exported at all", name)
	}
	return metric
}

// gatherResourceSeries returns the series carrying the given resource label, or nil when
// it was never observed.
func gatherResourceSeries(t *testing.T, name, resource string) *dto.Metric {
	t.Helper()
	return gatherSeries(t, name, map[string]string{"resource": resource})
}

// gatherSeries returns the family's first series carrying the given label values, or nil
// when the family has no such series — a labeled vector exports nothing until it owns a
// child, which is what the absence assertions read.
func gatherSeries(t *testing.T, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if carriesLabels(metric, labels) {
				return metric
			}
		}
	}
	return nil
}

func carriesLabels(metric *dto.Metric, want map[string]string) bool {
	for name, value := range want {
		if labelValue(metric, name) != value {
			return false
		}
	}
	return true
}

func labelValue(metric *dto.Metric, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}

func assertLabelNames(t *testing.T, name string, metric *dto.Metric, want []string) {
	t.Helper()
	got := make([]string, 0, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		got = append(got, label.GetName())
	}
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("family %q labels = %v, want %v", name, got, want)
	}
}

// assertMetricKind pins histogram vs. counter: a type swap keeps the name and labels
// plausible to every other check here, and the _total suffix is a convention, not a
// guarantee.
func assertMetricKind(t *testing.T, name string, metric *dto.Metric, wantHistogram bool) {
	t.Helper()
	if wantHistogram {
		if metric.GetHistogram() == nil {
			t.Fatalf("family %q is not a histogram", name)
		}
		return
	}
	if metric.GetCounter() == nil {
		t.Fatalf("family %q is not a counter", name)
	}
}

func assertBuckets(t *testing.T, name string, metric *dto.Metric, want []float64) {
	t.Helper()
	histogram := metric.GetHistogram()
	if histogram == nil {
		t.Fatalf("family %q is not a histogram", name)
	}
	got := make([]float64, 0, len(histogram.GetBucket()))
	for _, bucket := range histogram.GetBucket() {
		if math.IsInf(bucket.GetUpperBound(), 1) {
			continue // implicit +Inf, not part of the declared ladder
		}
		got = append(got, bucket.GetUpperBound())
	}
	if len(got) != len(want) {
		t.Fatalf("family %q has %d buckets, want %d\ngot:  %v\nwant: %v", name, len(got), len(want), got, want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("family %q buckets = %v, want %v", name, got, want)
	}
}

func assertCounterValue(t *testing.T, name, resource string, want float64) {
	t.Helper()
	series := gatherResourceSeries(t, name, resource)
	if series == nil {
		t.Fatalf("%s has no series for resource %q", name, resource)
	}
	if series.GetCounter() == nil {
		t.Fatalf("%s series for resource %q is not a counter", name, resource)
	}
	if got := series.GetCounter().GetValue(); got != want {
		t.Errorf("%s for resource %q = %v, want %v", name, resource, got, want)
	}
}

func assertHistogramObserved(t *testing.T, name, resource string, wantCount uint64, wantSum float64) {
	t.Helper()
	series := gatherResourceSeries(t, name, resource)
	if series == nil {
		t.Fatalf("%s has no series for resource %q", name, resource)
	}
	if series.GetHistogram() == nil {
		t.Fatalf("%s series for resource %q is not a histogram", name, resource)
	}
	if got := series.GetHistogram().GetSampleCount(); got != wantCount {
		t.Errorf("%s sample count = %d, want %d", name, got, wantCount)
	}
	if got := series.GetHistogram().GetSampleSum(); got != wantSum {
		t.Errorf("%s sample sum = %v, want %v", name, got, wantSum)
	}
}

func assertNoSeries(t *testing.T, name, resource string) {
	t.Helper()
	if series := gatherResourceSeries(t, name, resource); series != nil {
		t.Errorf("%s observed a series for resource %q", name, resource)
	}
}
