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

package allocate

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
)

func TestFilterCandidateForestGradientsByEmptyMinResource(t *testing.T) {
	roots := map[string]*api.HyperNodeInfo{
		"a": {Name: "a"},
		"b": {Name: "b"},
		"c": {Name: "c"},
		"d": {Name: "d"},
	}
	gradients := [][]*api.HyperNodeInfo{
		{roots["c"], nil, roots["b"], roots["b"], roots["a"]},
		{roots["d"]},
	}

	for _, tc := range []struct {
		name        string
		minResource *api.Resource
	}{
		{name: "nil", minResource: nil},
		{name: "empty", minResource: api.EmptyResource()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filtered, stats := FilterCandidateForestGradientsByMinResource(
				&framework.Session{}, gradients, tc.minResource, "",
			)
			if got, want := candidateForestGradientNames(filtered), [][]string{{"a", "b", "c"}, {"d"}}; !equalStringLayers(got, want) {
				t.Fatalf("filtered gradients = %v, want %v", got, want)
			}
			if stats == nil {
				t.Fatal("expected normalized final-by-tier stats")
			}
			if got := stats.FinalByTier[0]; got != 4 {
				t.Fatalf("final tier-0 roots = %d, want 4", got)
			}
			if len(stats.ExcludedByTier) != 0 || len(stats.ExcludedByReason) != 0 {
				t.Fatalf("empty minResource unexpectedly excluded roots: %#v", stats)
			}
		})
	}
}

func TestCandidateForestMinResourceHandlesDuplicateNilAndMissingNodes(t *testing.T) {
	a := &api.HyperNodeInfo{Name: "a"}
	b := &api.HyperNodeInfo{Name: "b"}
	ssn := &framework.Session{
		Nodes: map[string]*api.NodeInfo{
			"node-a": candidateForestMemoryNode(1),
			"node-b": candidateForestMemoryNode(1),
		},
		RealNodesSet: map[string]sets.Set[string]{
			"a": sets.New[string]("node-a"),
			"b": sets.New[string]("node-b", "missing-node"),
		},
	}
	gradients := [][]*api.HyperNodeInfo{{b, nil, a, b, a}}

	filtered, stats := FilterCandidateForestGradientsByMinResource(
		ssn, gradients, &api.Resource{MilliCPU: 2}, "",
	)
	if got, want := candidateForestGradientNames(filtered), [][]string{{"a", "b"}}; !equalStringLayers(got, want) {
		t.Fatalf("filtered gradients = %v, want %v, stats=%#v", got, want, stats)
	}

	filtered, stats = FilterCandidateForestGradientsByMinResource(
		ssn, gradients, &api.Resource{MilliCPU: 3}, "",
	)
	if len(filtered) != 0 {
		t.Fatalf("forest with two available CPUs must not satisfy three CPUs: %v", candidateForestGradientNames(filtered))
	}
	if len(stats.ExcludedByReason) != 2 {
		t.Fatalf("excluded roots = %v, want a and b", stats.ExcludedByReason)
	}
}

func TestCandidateForestMinResourceFailureDiagnosticsAreBounded(t *testing.T) {
	const rootCount = 16
	ssn, gradients := buildCandidateForestMemoryFixture(rootCount)
	filtered, stats := FilterCandidateForestGradientsByMinResource(
		ssn, gradients, &api.Resource{MilliCPU: rootCount + 1}, "",
	)
	if len(filtered) != 0 {
		t.Fatalf("insufficient forest unexpectedly survived: %v", candidateForestGradientNames(filtered))
	}
	if stats == nil {
		t.Fatal("expected minResource failure stats")
	}
	if len(stats.ExcludedByReason) != rootCount {
		t.Fatalf("excluded reason count = %d, want %d", len(stats.ExcludedByReason), rootCount)
	}

	var commonReason string
	for rootName, reason := range stats.ExcludedByReason {
		if commonReason == "" {
			commonReason = reason
		}
		if reason != commonReason {
			t.Fatalf("root %s reason differs: %q != %q", rootName, reason, commonReason)
		}
	}
	if len(commonReason) >= 1024 {
		t.Fatalf("reason length = %d, want less than 1KiB", len(commonReason))
	}
	if !strings.Contains(commonReason, "minResource") || !strings.Contains(commonReason, "roots=16") {
		t.Fatalf("bounded reason lacks resource/root count: %q", commonReason)
	}
	if strings.Contains(commonReason, "candidate-forest-root-") {
		t.Fatalf("bounded reason contains the full root listing: %q", commonReason)
	}
}

func buildCandidateForestMemoryFixture(rootCount int) (*framework.Session, [][]*api.HyperNodeInfo) {
	ssn := &framework.Session{
		Nodes:        make(map[string]*api.NodeInfo, rootCount),
		RealNodesSet: make(map[string]sets.Set[string], rootCount),
	}
	roots := make([]*api.HyperNodeInfo, rootCount)
	for index := range roots {
		rootName := fmt.Sprintf("candidate-forest-root-%06d", index)
		nodeName := fmt.Sprintf("candidate-forest-node-%06d", index)
		roots[index] = &api.HyperNodeInfo{Name: rootName}
		ssn.RealNodesSet[rootName] = sets.New[string](nodeName)
		ssn.Nodes[nodeName] = candidateForestMemoryNode(1)
	}
	return ssn, [][]*api.HyperNodeInfo{roots}
}

func candidateForestMemoryNode(milliCPU float64) *api.NodeInfo {
	return &api.NodeInfo{
		Idle:      &api.Resource{MilliCPU: milliCPU},
		Releasing: api.EmptyResource(),
		Pipelined: api.EmptyResource(),
	}
}

func candidateForestGradientNames(gradients [][]*api.HyperNodeInfo) [][]string {
	result := make([][]string, 0, len(gradients))
	for _, layer := range gradients {
		names := make([]string, 0, len(layer))
		for _, root := range layer {
			if root != nil {
				names = append(names, root.Name)
			}
		}
		result = append(result, names)
	}
	return result
}

func equalStringLayers(left, right [][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if len(left[index]) != len(right[index]) {
			return false
		}
		for item := range left[index] {
			if left[index][item] != right[index][item] {
				return false
			}
		}
	}
	return true
}
