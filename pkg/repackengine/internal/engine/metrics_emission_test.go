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

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	state "volcano.sh/volcano/pkg/controllers/repack/state"
)

// Sentinel resources, one per test: series live on the default registry for the whole
// test binary, so a shared resource name would be observed by more than one test.
const (
	wiringResource   = "wiring.example.com/gpu"
	identityResource = "identity.example.com/gpu"
)

// TestUpdateStatusTerminalPublishesRunMetrics pins the only place the engine's metrics
// leave it — the terminal status write. No other test reads the registry, so without
// this a dropped Observe call would keep the whole suite green.
func TestUpdateStatusTerminalPublishesRunMetrics(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal-metrics"},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode:  repackv1alpha1.RepackModeExecute,
			Goals: []repackv1alpha1.RepackGoal{{Resource: v1.ResourceName(wiringResource)}},
		},
		Status: repackv1alpha1.RepackRunStatus{
			// fragBefore 55 -> fragAfter 43, and 3 nodes freed.
			Plan:   &repackv1alpha1.RepackPlan{Summary: &repackv1alpha1.RepackSummary{FragBeforePercent: 55}},
			Result: &repackv1alpha1.RepackResult{MovedCardCount: 20, FreedNodeCount: 3, FragAfterPercent: 43, MetricsVerified: true},
			Relocations: []repackv1alpha1.PodRelocationStatus{
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}},
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}},
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionIndirectlyRemoved}},
			},
			Conditions: []metav1.Condition{{
				Type: state.CondComplete, Status: metav1.ConditionTrue, Reason: state.ReasonExecutionCompleted,
			}},
		},
	}
	engine := &Engine{volcanoClient: vcfake.NewSimpleClientset(run.DeepCopy())}

	resource := map[string]string{"resource": wiringResource}
	outcome := map[string]string{"mode": "Execute", "outcome": state.ReasonExecutionCompleted}
	histograms := []struct {
		name    string
		wantSum float64
	}{
		{"volcano_repack_affected_pods", 3},
		{"volcano_repack_evicted_pods", 2},
		{"volcano_repack_indirectly_removed_pods", 1},
		{"volcano_repack_freed_nodes", 3},
		{"volcano_repack_fragmentation_improvement_percent", 12},
	}
	type histogramBefore struct {
		count uint64
		sum   float64
	}
	before := make([]histogramBefore, len(histograms))
	for index, want := range histograms {
		before[index].count, before[index].sum = histogramTotals(t, want.name, resource)
	}
	runsBefore := counterValue(t, "volcano_repack_runs_total", outcome)
	cardsBefore := counterValue(t, "volcano_repack_moved_cards_total", resource)

	if err := engine.updateStatusTerminal(context.Background(), run); err != nil {
		t.Fatalf("updateStatusTerminal() error = %v", err)
	}

	if got := counterValue(t, "volcano_repack_runs_total", outcome); got != runsBefore+1 {
		t.Errorf("runs_total%v = %v, want %v", outcome, got, runsBefore+1)
	}
	if got := counterValue(t, "volcano_repack_moved_cards_total", resource); got != cardsBefore+20 {
		t.Errorf("moved_cards_total = %v, want %v", got, cardsBefore+20)
	}
	for index, want := range histograms {
		count, sum := histogramTotals(t, want.name, resource)
		if count != before[index].count+1 || sum != before[index].sum+want.wantSum {
			t.Errorf("%s = (%d, %v), want (%d, %v)",
				want.name, count, sum, before[index].count+1, before[index].sum+want.wantSum)
		}
	}
}

// TestEvictionsTotalMatchesDisruptedPodHistograms pins the identity the design doc
// states between the two families. Each side is counted by its own reader —
// summarizeEvictions over the eviction journal at the barrier, disruptedPods over
// status.relocations at the terminal write — so nothing but this test keeps them equal.
func TestEvictionsTotalMatchesDisruptedPodHistograms(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "identity-metrics"},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode:  repackv1alpha1.RepackModeExecute,
			Goals: []repackv1alpha1.RepackGoal{{Resource: v1.ResourceName(identityResource)}},
		},
		Status: repackv1alpha1.RepackRunStatus{
			Result: &repackv1alpha1.RepackResult{MovedCardCount: 5},
			Relocations: []repackv1alpha1.PodRelocationStatus{
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}},
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}},
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionAccepted}},
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionIndirectlyRemoved}},
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionIndirectlyRemoved}},
				{Eviction: repackv1alpha1.PodEvictionStatus{Phase: repackv1alpha1.PodEvictionRejected}},
			},
		},
	}
	engine := &Engine{volcanoClient: vcfake.NewSimpleClientset(run.DeepCopy())}

	resource := map[string]string{"resource": identityResource}
	evictedBefore := counterValue(t, "volcano_repack_evictions_total", map[string]string{"resource": identityResource, "result": "evicted"})
	rejectedBefore := counterValue(t, "volcano_repack_evictions_total", map[string]string{"resource": identityResource, "result": "rejected"})
	indirectBefore := counterValue(t, "volcano_repack_evictions_total", map[string]string{"resource": identityResource, "result": "indirectly_removed"})
	_, evictedSumBefore := histogramTotals(t, "volcano_repack_evicted_pods", resource)
	_, indirectSumBefore := histogramTotals(t, "volcano_repack_indirectly_removed_pods", resource)

	engine.observeEvictionSummary(run, summarizeEvictions(run.Status.Relocations))
	if err := engine.updateStatusTerminal(context.Background(), run); err != nil {
		t.Fatalf("updateStatusTerminal() error = %v", err)
	}

	evictedDelta := counterValue(t, "volcano_repack_evictions_total", map[string]string{"resource": identityResource, "result": "evicted"}) - evictedBefore
	rejectedDelta := counterValue(t, "volcano_repack_evictions_total", map[string]string{"resource": identityResource, "result": "rejected"}) - rejectedBefore
	indirectDelta := counterValue(t, "volcano_repack_evictions_total", map[string]string{"resource": identityResource, "result": "indirectly_removed"}) - indirectBefore
	_, evictedSum := histogramTotals(t, "volcano_repack_evicted_pods", resource)
	_, indirectSum := histogramTotals(t, "volcano_repack_indirectly_removed_pods", resource)

	if want := 3.0; evictedDelta != want {
		t.Errorf("evictions_total{result=evicted} = %v, want %v", evictedDelta, want)
	}
	if want := 2.0; indirectDelta != want {
		t.Errorf("evictions_total{result=indirectly_removed} = %v, want %v", indirectDelta, want)
	}
	// The rejected victim never moved, so it is on the counter and in no histogram.
	if want := 1.0; rejectedDelta != want {
		t.Errorf("evictions_total{result=rejected} = %v, want %v", rejectedDelta, want)
	}
	if want := evictedDelta; evictedSum-evictedSumBefore != want {
		t.Errorf("evicted_pods sum = %v, want evictions_total{result=evicted} %v", evictedSum-evictedSumBefore, want)
	}
	if want := indirectDelta; indirectSum-indirectSumBefore != want {
		t.Errorf("indirectly_removed_pods sum = %v, want evictions_total{result=indirectly_removed} %v", indirectSum-indirectSumBefore, want)
	}
}

// counterValue returns a counter series' value, or 0 when the series does not exist yet:
// a labeled vector exports nothing until it owns a child, and these tests compare against
// a value read before their own call.
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	metric := findSeries(t, name, labels)
	if metric == nil {
		return 0
	}
	if metric.GetCounter() == nil {
		t.Fatalf("series %s%v is not a counter", name, labels)
	}
	return metric.GetCounter().GetValue()
}

// histogramTotals returns a histogram series' sample count and sum, zeroed when absent.
func histogramTotals(t *testing.T, name string, labels map[string]string) (uint64, float64) {
	t.Helper()
	metric := findSeries(t, name, labels)
	if metric == nil {
		return 0, 0
	}
	if metric.GetHistogram() == nil {
		t.Fatalf("series %s%v is not a histogram", name, labels)
	}
	return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum()
}

func findSeries(t *testing.T, name string, labels map[string]string) *dto.Metric {
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
			if labelsMatch(metric, labels) {
				return metric
			}
		}
	}
	return nil
}

func labelsMatch(metric *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		got[label.GetName()] = label.GetValue()
	}
	for name, value := range want {
		if got[name] != value {
			return false
		}
	}
	return true
}
