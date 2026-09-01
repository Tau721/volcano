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

package status_test

import (
	"strings"
	"testing"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	state "volcano.sh/repack-controller/pkg/state"

	enginestatus "volcano.sh/volcano/pkg/repackengine/status"
)

func TestCompletionStatusMessageIncludesOperationalResult(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		Status: repackv1alpha1.RepackRunStatus{Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{
				FragBeforePercent: 42,
				FragAfterPercent:  28,
				FreedNodeCount:    2,
				MovedCardCount:    12,
			},
			Moves: []repackv1alpha1.RepackMove{{}, {}, {}},
		}},
	}
	message := enginestatus.CompletionMessage(run, gpuResource, state.ReasonRepackRecommended)
	for _, want := range []string{"nvidia.com/gpu", "3 PodGroups", "12 cards", "2 nodes", "42% to 28%"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not contain %q", message, want)
		}
	}
}

// The node-block gate rejection must read the block target from the run's
// networkTopology and describe the block infeasibility, not the fragmentation
// improvement.
func TestCompletionMessageRequiredNodeBlocksNotMet(t *testing.T) {
	ptr := func(v int) *int { return &v }
	run := &repackv1alpha1.RepackRun{
		Spec: repackv1alpha1.RepackRunSpec{NetworkTopology: &repackv1alpha1.NetworkTopology{
			HyperNodeTier:      ptr(1),
			NodeBlockSize:      ptr(2),
			RequiredNodeBlocks: 3,
		}},
		Status: repackv1alpha1.RepackRunStatus{Plan: &repackv1alpha1.RepackPlan{
			Summary: &repackv1alpha1.RepackSummary{FragBeforePercent: 63},
		}},
	}
	message := enginestatus.CompletionMessage(run, gpuResource, state.ReasonRequiredNodeBlocksNotMet)
	for _, want := range []string{"nvidia.com/gpu", "3 HyperNode node-block(s)", "size 2", "tier 1", "63%"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, "percentage-point improvement") {
		t.Errorf("block-gate message %q must not claim a fragmentation-improvement shortfall", message)
	}
}
