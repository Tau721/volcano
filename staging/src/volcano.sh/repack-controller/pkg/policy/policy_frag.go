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
	"sort"

	v1 "k8s.io/api/core/v1"
)

// FragRateResult holds fragmentation information for one resource.
type FragRateResult struct {
	Resource                 v1.ResourceName
	ProvidingNodeCount       int   // nodes with allocatable[resource] > 0
	OccupiedNodeCount        int   // nodes with at least one pod using the resource
	OptimalOccupiedNodeCount int   // minimum nodes to host all requests (compact bin-packing)
	FragRatePercent          int32 // 0-100, (occupied - optimal) / providing * 100
	Exact                    bool  // whether the optimal count is exact (all capacities and requests are powers of two)
}

// ComputeFragRate computes the fragmentation rate for the given resource across all
// nodes and pods. This is equivalent to the engine's MeasureResourceFragmentation
// but uses standard K8s informer objects instead of scheduler-internal types.
//
// Algorithm:
//  1. Filter nodes with Allocatable[resource] > 0 → providingNodeCount
//  2. Collect per-node capacity; check homogeneity
//  3. From placed pods, aggregate resource requests per node → occupiedNodeCount
//  4. Collect individual pod requests for bin-packing optimal
//  5. If homogeneous capacities: use closed-form optimalNodes()
//  6. If heterogeneous: sort capacities descending, greedily count nodes to cover total demand
//  7. Result: (occupied - optimal) / providing * 100
// nodeCap holds a node's name and its capacity for a specific resource.
type nodeCap struct {
	name     string
	capacity int64
}

func ComputeFragRate(nodes []*v1.Node, pods []*v1.Pod, resourceName v1.ResourceName) FragRateResult {
	result := FragRateResult{Resource: resourceName}

	// Step 1: Identify providing nodes and their capacities.
	var providingNodes []nodeCap
	for _, n := range nodes {
		qty, ok := n.Status.Allocatable[resourceName]
		if !ok {
			continue
		}
		cap := qty.MilliValue()
		if cap <= 0 {
			continue
		}
		providingNodes = append(providingNodes, nodeCap{name: n.Name, capacity: cap})
	}
	result.ProvidingNodeCount = len(providingNodes)
	if result.ProvidingNodeCount == 0 {
		return result
	}

	// Check homogeneity: all capacities equal?
	refCap := providingNodes[0].capacity
	homogeneous := true
	for _, nc := range providingNodes[1:] {
		if nc.capacity != refCap {
			homogeneous = false
			break
		}
	}

	// Step 2-3: From placed pods, aggregate requests per node.
	requestsPerNode := make(map[string]int64)
	var allRequests []int64

	for _, pod := range pods {
		// Only consider placed pods (Running or Succeeded with nodeName set).
		if pod.Spec.NodeName == "" {
			continue
		}
		if pod.Status.Phase != v1.PodRunning && pod.Status.Phase != v1.PodSucceeded {
			continue
		}
		podReq := int64(0)
		for _, c := range pod.Spec.Containers {
			qty, ok := c.Resources.Requests[resourceName]
			if !ok {
				continue
			}
			podReq += qty.MilliValue()
		}
		if podReq > 0 {
			requestsPerNode[pod.Spec.NodeName] += podReq
			allRequests = append(allRequests, podReq)
		}
	}

	result.OccupiedNodeCount = len(requestsPerNode)
	if len(allRequests) == 0 {
		// No resource usage at all — zero fragmentation.
		return result
	}

	// Step 4-5-6: Compute optimal occupied nodes.
	var optimal int64
	var exact bool
	if homogeneous && len(providingNodes) > 0 {
		optimal, exact = optimalNodes(allRequests, refCap)
	} else {
		// Heterogeneous: greedy lower bound.
		optimal = greedyOptimalNodes(providingNodes, allRequests)
		exact = false
	}

	result.OptimalOccupiedNodeCount = int(optimal)
	result.Exact = exact

	// Step 7: Fragmentation rate.
	if result.ProvidingNodeCount > 0 {
		num := result.OccupiedNodeCount - result.OptimalOccupiedNodeCount
		if num < 0 {
			num = 0
		}
		result.FragRatePercent = int32(int64(num) * 100 / int64(result.ProvidingNodeCount))
	}

	return result
}

// optimalNodes returns the minimum number of nodes of capacity needed to host
// all resource requests. Duplicated from pkg/repackengine/api/fragmentation.go
// to avoid importing the main volcano module into this independent leaf module.
// exact is true when both node capacity and all requests are powers of two.
func optimalNodes(resourceRequests []int64, nodeCapacity int64) (optimalNodeCount int64, exact bool) {
	if nodeCapacity <= 0 || len(resourceRequests) == 0 {
		return 0, true
	}

	exact = isPowerOfTwo(nodeCapacity)
	totalRequest := int64(0)
	for _, req := range resourceRequests {
		totalRequest += req
		if exact && !isPowerOfTwo(req) {
			exact = false
		}
	}

	optimalNodeCount = ceilDiv(totalRequest, nodeCapacity)
	return
}

// greedyOptimalNodes computes a lower bound for the number of nodes needed to
// host all requests when capacities are heterogeneous. Sorts capacities descending
// and counts how many of the largest nodes are needed to cover the total demand.
func greedyOptimalNodes(nodes []nodeCap, requests []int64) int64 {
	if len(nodes) == 0 || len(requests) == 0 {
		return 0
	}

	// Sort capacities descending.
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].capacity > nodes[j].capacity
	})

	totalRequest := int64(0)
	for _, req := range requests {
		totalRequest += req
	}

	// Count how many largest-capacity nodes cover the total demand.
	covered := int64(0)
	count := int64(0)
	for _, nc := range nodes {
		covered += nc.capacity
		count++
		if covered >= totalRequest {
			return count
		}
	}
	// Total capacity is less than total demand — use all nodes.
	return int64(len(nodes))
}

// scalarResource extracts a resource quantity from a ResourceList as millis.
func scalarResource(rl v1.ResourceList, resource v1.ResourceName) int64 {
	qty, ok := rl[resource]
	if !ok {
		return 0
	}
	return qty.MilliValue()
}

// scalarResourceFromPod extracts the total request for a resource across all containers.
func scalarResourceFromPod(pod *v1.Pod, resource v1.ResourceName) int64 {
	total := int64(0)
	for _, c := range pod.Spec.Containers {
		qty, ok := c.Resources.Requests[resource]
		if !ok {
			continue
		}
		total += qty.MilliValue()
	}
	return total
}

func isPowerOfTwo(v int64) bool {
	return v > 0 && (v&(v-1)) == 0
}

func ceilDiv(numerator, denominator int64) int64 {
	if denominator == 0 {
		return 0
	}
	return (numerator + denominator - 1) / denominator
}