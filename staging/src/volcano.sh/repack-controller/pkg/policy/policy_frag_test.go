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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// makeNode creates a Node with the given allocatable resource quantity.
func makeNode(name string, resourceName corev1.ResourceName, quantity string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				resourceName: resource.MustParse(quantity),
			},
		},
	}
}

// makePod creates a Pod on a given node, requesting the specified resource quantity.
func makePod(name, nodeName string, resourceName corev1.ResourceName, quantity string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							resourceName: resource.MustParse(quantity),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestComputeFragRate(t *testing.T) {
	gpu := corev1.ResourceName("nvidia.com/gpu")

	tests := []struct {
		name           string
		nodes          []*corev1.Node
		pods           []*corev1.Pod
		resource       corev1.ResourceName
		wantProviding  int
		wantOccupied   int
		wantOptimal    int
		wantRate       int32
	}{
		{
			name:    "empty cluster",
			nodes:   nil,
			pods:    nil,
			resource: gpu,
		},
		{
			name:    "no resource on nodes",
			nodes: []*corev1.Node{
				makeNode("node-1", corev1.ResourceCPU, "4"),
			},
			pods:       nil,
			resource:   gpu,
		},
		{
			name: "all nodes fully occupied — zero fragmentation",
			nodes: []*corev1.Node{
				makeNode("node-1", gpu, "8"),
				makeNode("node-2", gpu, "8"),
				makeNode("node-3", gpu, "8"),
			},
			pods: []*corev1.Pod{
				makePod("pod-1", "node-1", gpu, "8", corev1.PodRunning),
				makePod("pod-2", "node-2", gpu, "8", corev1.PodRunning),
				makePod("pod-3", "node-3", gpu, "8", corev1.PodRunning),
			},
			resource:      gpu,
			wantProviding: 3,
			wantOccupied:  3,
			wantOptimal:   3,
			wantRate:      0,
		},
		{
			name: "fragmented: 5 nodes occupied, 10 providing, only 1 needed",
			nodes: func() []*corev1.Node {
				var nodes []*corev1.Node
				for i := 1; i <= 10; i++ {
					nodes = append(nodes, makeNode(fmt.Sprintf("node-%d", i), gpu, "8"))
				}
				return nodes
			}(),
			pods: func() []*corev1.Pod {
				var pods []*corev1.Pod
				for i := 1; i <= 5; i++ {
					pods = append(pods, makePod(fmt.Sprintf("pod-%d", i), fmt.Sprintf("node-%d", i), gpu, "1", corev1.PodRunning))
				}
				return pods
			}(),
			resource:      gpu,
			wantProviding: 10,
			wantOccupied:  5,
			// Optimal: 5 GPUs requested on 8-GPU nodes = ceil(5000/8000) = 1 node
			// Frag: (5-1)/10 * 100 = 40
			wantOptimal: 1,
			wantRate:    40,
		},
		{
			name: "single node partially filled — no fragmentation (1 node is optimal too)",
			nodes: []*corev1.Node{
				makeNode("node-1", gpu, "8"),
			},
			pods: []*corev1.Pod{
				makePod("pod-1", "node-1", gpu, "1", corev1.PodRunning),
			},
			resource:      gpu,
			wantProviding: 1,
			wantOccupied:  1,
			wantOptimal:   1,
			wantRate:      0,
		},
		{
			name: "heterogeneous capacities",
			nodes: []*corev1.Node{
				makeNode("big-node", gpu, "8"),
				makeNode("med-node-1", gpu, "4"),
				makeNode("med-node-2", gpu, "4"),
				makeNode("small-node", gpu, "2"),
			},
			pods: []*corev1.Pod{
				makePod("pod-1", "big-node", gpu, "1", corev1.PodRunning),
				makePod("pod-2", "med-node-1", gpu, "1", corev1.PodRunning),
				makePod("pod-3", "med-node-2", gpu, "1", corev1.PodRunning),
				makePod("pod-4", "small-node", gpu, "1", corev1.PodRunning),
			},
			resource:      gpu,
			wantProviding: 4,
			wantOccupied:  4,
			// Greedy: big(8) + med-1(4) = 12 covers total request 16k → 2 nodes
			// Frag: (4-2)/4 * 100 = 50
			wantOptimal: 2,
			wantRate:    50,
		},
		{
			name: "unplaced pods excluded",
			nodes: []*corev1.Node{
				makeNode("node-1", gpu, "8"),
			},
			pods: []*corev1.Pod{
				makePod("pending-pod", "", gpu, "4", corev1.PodPending),
				makePod("placed-pod", "node-1", gpu, "2", corev1.PodRunning),
			},
			resource:      gpu,
			wantProviding: 1,
			wantOccupied:  1,
			wantOptimal:   1,
			wantRate:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeFragRate(tt.nodes, tt.pods, tt.resource)
			if got.ProvidingNodeCount != tt.wantProviding {
				t.Errorf("ProvidingNodeCount = %d, want %d", got.ProvidingNodeCount, tt.wantProviding)
			}
			if got.OccupiedNodeCount != tt.wantOccupied {
				t.Errorf("OccupiedNodeCount = %d, want %d", got.OccupiedNodeCount, tt.wantOccupied)
			}
			if got.OptimalOccupiedNodeCount != tt.wantOptimal {
				t.Errorf("OptimalOccupiedNodeCount = %d, want %d", got.OptimalOccupiedNodeCount, tt.wantOptimal)
			}
			if got.FragRatePercent != tt.wantRate {
				t.Errorf("FragRatePercent = %d, want %d", got.FragRatePercent, tt.wantRate)
			}
		})
	}
}

func TestOptimalNodes(t *testing.T) {
	tests := []struct {
		name      string
		requests  []int64
		capacity  int64
		wantOpt   int64
		wantExact bool
	}{
		{"exact fit", []int64{8000, 8000}, 8000, 2, true},
		{"single request fits one node", []int64{4000}, 8000, 1, true},
		{"tight fit powers of two", []int64{1000, 1000, 1000, 1000}, 4000, 1, true},
		{"not powers of two", []int64{3000, 3000}, 8000, 1, false},
		{"overflow", []int64{9000}, 8000, 2, false},
		{"zero capacity", []int64{1000}, 0, 0, true},
		{"zero requests", nil, 8000, 0, true},
		{"uneven distribution", []int64{1000, 1000, 1000, 1000, 1000}, 4000, 2, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotExact := optimalNodes(tt.requests, tt.capacity)
			if got != tt.wantOpt {
				t.Errorf("optimalNodes() = %d, want %d", got, tt.wantOpt)
			}
			if gotExact != tt.wantExact {
				t.Errorf("optimalNodes() exact = %v, want %v", gotExact, tt.wantExact)
			}
		})
	}
}

func TestScalarResource(t *testing.T) {
	gpu := corev1.ResourceName("nvidia.com/gpu")

	rl := corev1.ResourceList{
		gpu:             resource.MustParse("8"),
		corev1.ResourceCPU: resource.MustParse("4"),
	}

	tests := []struct {
		name     string
		rl       corev1.ResourceList
		resource corev1.ResourceName
		want     int64
	}{
		{"gpu 8", rl, gpu, 8000},
		{"missing resource", rl, corev1.ResourceName("memory"), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scalarResource(tt.rl, tt.resource)
			if got != tt.want {
				t.Errorf("scalarResource() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCeilDiv(t *testing.T) {
	tests := []struct {
		numer int64
		denom int64
		want  int64
	}{
		{10, 3, 4},
		{9, 3, 3},
		{0, 5, 0},
		{5, 0, 0},
		{1, 2, 1},
	}

	for _, tt := range tests {
		got := ceilDiv(tt.numer, tt.denom)
		if got != tt.want {
			t.Errorf("ceilDiv(%d, %d) = %d, want %d", tt.numer, tt.denom, got, tt.want)
		}
	}
}

func TestIsPowerOfTwo(t *testing.T) {
	tests := []struct {
		v    int64
		want bool
	}{
		{1, true},
		{2, true},
		{4, true},
		{8, true},
		{1024, true},
		{0, false},
		{3, false},
		{5, false},
		{100, false},
		{-1, false},
	}

	for _, tt := range tests {
		got := isPowerOfTwo(tt.v)
		if got != tt.want {
			t.Errorf("isPowerOfTwo(%d) = %v, want %v", tt.v, got, tt.want)
		}
	}
}