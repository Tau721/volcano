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

// Package frag is the pure fragmentation model shared by the repack engine and
// the RepackPolicy controller. It depends only on the standard library so both
// consumers — the engine's MeasureResourceFragmentation and the controller's
// cluster-wide onFrag measurement — call one copy and cannot drift.
package frag

import "sort"

// OptimalNodes returns the minimum number of nodes (each of the given capacity)
// needed to host all requests, plus whether the result is exact.
//
// A request g >= capacity occupies ceil(g/capacity) whole nodes; smaller requests
// are packed into shared nodes. Because every request and the capacity are powers
// of two, the volume bound is tight and the closed form below is exactly the
// optimum (validated by brute force in TestOptimalNodes_MatchesBruteForce_Pow2).
func OptimalNodes(resourceRequests []int64, nodeCapacity int64) (optimalNodeCount int64, exact bool) {
	if nodeCapacity <= 0 {
		return 0, false
	}
	var wholeNodeDemand, sharedNodeDemand int64
	exact = isPowerOfTwo(nodeCapacity)
	for _, requestedResource := range resourceRequests {
		if requestedResource <= 0 {
			continue
		}
		if !isPowerOfTwo(requestedResource) {
			exact = false
		}
		if requestedResource >= nodeCapacity {
			wholeNodeDemand += ceilDiv(requestedResource, nodeCapacity) // whole nodes for multi-node tasks
		} else {
			sharedNodeDemand += requestedResource // sub-node tasks share via volume packing
		}
	}
	return wholeNodeDemand + ceilDiv(sharedNodeDemand, nodeCapacity), exact
}

// ComputeOptimalNodeCount returns the minimum number of nodes whose aggregate
// capacity hosts all requests: the homogeneous closed form (OptimalNodes) when
// every capacity is equal, else a deterministic capacity-descending greedy cover
// of total demand (a volume lower bound, Exact=false). Callers clamp the result
// to their observed occupied-node count.
func ComputeOptimalNodeCount(resourceRequests, nodeCapacities []int64) (optimalNodeCount int64, exact bool) {
	capacities := make([]int64, 0, len(nodeCapacities))
	var firstCapacity int64
	homogeneous := true
	for _, c := range nodeCapacities {
		if c <= 0 {
			continue
		}
		capacities = append(capacities, c)
		if firstCapacity == 0 {
			firstCapacity = c
		} else if c != firstCapacity {
			homogeneous = false
		}
	}
	var totalResourceDemand int64
	for _, r := range resourceRequests {
		if r > 0 {
			totalResourceDemand += r
		}
	}
	if len(capacities) == 0 {
		return 0, false
	}
	if homogeneous {
		return OptimalNodes(resourceRequests, firstCapacity)
	}
	// Heterogeneous pools cannot use an arbitrary first-node capacity: that made
	// the metric depend on map iteration order and could produce an optimal count
	// above the occupied count. Cover total demand with the largest capacities.
	sort.Slice(capacities, func(i, j int) bool { return capacities[i] > capacities[j] })
	var coveredCapacity int64
	for _, capacity := range capacities {
		if coveredCapacity >= totalResourceDemand {
			break
		}
		coveredCapacity += capacity
		optimalNodeCount++
	}
	return optimalNodeCount, false
}

func ceilDiv(numerator, denominator int64) int64 {
	if denominator <= 0 {
		return 0
	}
	return (numerator + denominator - 1) / denominator
}

func isPowerOfTwo(value int64) bool { return value > 0 && (value&(value-1)) == 0 }
