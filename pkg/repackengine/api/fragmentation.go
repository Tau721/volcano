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

// Package api implements the fragmentation index and pure model/contracts for
// the repack (defragmentation) engine.
//
// Per accelerator resource R (e.g. nvidia.com/gpu, huawei.com/Ascend910):
//
//	FragmentationRate(R) = (occupied nodes - optimal occupied nodes) / providing nodes
//	  providing nodes = nodes with Allocatable[R] > 0
//	  occupied nodes = providing nodes with Used[R] > 0
//	  optimal occupied nodes = theoretical minimum for R's demand (see frag.OptimalNodes)
//
// The optimal-node-count math itself lives in the leaf module
// volcano.sh/repack-controller/pkg/frag, shared with the RepackPolicy
// controller's onFrag measurement so the two cannot drift.
package api

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"volcano.sh/repack-controller/pkg/frag"
	"volcano.sh/volcano/pkg/scheduler/api"
)

// ResourceFragmentation is the fragmentation measurement for one accelerator resource.
type ResourceFragmentation struct {
	Resource                 v1.ResourceName
	ProvidingNodeCount       int64 // nodes providing this resource (in scope)
	OccupiedNodeCount        int64 // nodes currently occupied by this resource
	OptimalOccupiedNodeCount int64 // theoretical-optimal occupied nodes for the demand
	// Exact is true when OptimalOccupiedNodeCount is exact (power-of-two requests
	// on a homogeneous power-of-two capacity). When false it is a volume lower
	// bound and FragmentationRate may over-estimate.
	Exact bool
}

// FragmentationRate returns (occupiedNodes-optimalNodes)/providingNodes.
func (fragmentation ResourceFragmentation) FragmentationRate() float64 {
	if fragmentation.ProvidingNodeCount == 0 {
		return 0
	}
	return float64(fragmentation.OccupiedNodeCount-fragmentation.OptimalOccupiedNodeCount) / float64(fragmentation.ProvidingNodeCount)
}

// MeasureResourceFragmentation computes the fragmentation of an accelerator
// resource over the given nodes. Callers pass a cluster-wide snapshot (the engine
// passes the whole session) so it matches the RepackPolicy controller's onFrag
// trigger. Demand comes from the tasks placed on resource-providing nodes; the
// optimal count is exact on homogeneous capacities, else a volume lower bound.
func MeasureResourceFragmentation(nodes []*api.NodeInfo, targetResource v1.ResourceName) ResourceFragmentation {
	fragmentation := ResourceFragmentation{Resource: targetResource}

	nodeCapacities := make([]int64, 0, len(nodes))
	resourceRequests := make([]int64, 0, 64)

	for _, node := range nodes {
		if node == nil || node.Allocatable == nil {
			continue
		}
		capacity := Scalar(node.Allocatable, targetResource)
		if capacity <= 0 {
			continue // node does not provide this resource
		}
		fragmentation.ProvidingNodeCount++
		nodeCapacities = append(nodeCapacities, capacity)
		resourceUsage := int64(0)
		if node.Used != nil {
			resourceUsage = Scalar(node.Used, targetResource)
		}
		if resourceUsage > 0 {
			fragmentation.OccupiedNodeCount++
		}
		klog.V(5).InfoS("repack frag: node accelerator usage", "node", node.Name,
			"resource", targetResource, "capacity", capacity, "used", resourceUsage)
		for _, task := range node.Tasks {
			if task == nil || task.Resreq == nil {
				continue
			}
			if requestedResource := Scalar(task.Resreq, targetResource); requestedResource > 0 {
				resourceRequests = append(resourceRequests, requestedResource)
			}
		}
	}

	if fragmentation.ProvidingNodeCount == 0 {
		klog.V(5).InfoS("repack frag: no node provides this resource", "resource", targetResource)
		return fragmentation
	}
	fragmentation.OptimalOccupiedNodeCount, fragmentation.Exact = frag.ComputeOptimalNodeCount(resourceRequests, nodeCapacities)
	// The current placement itself proves an optimum cannot require more than the
	// current number of occupied nodes. Clamp defensive lower-bound approximations
	// so FragmentationRate always stays in its documented [0,1] range.
	if fragmentation.OptimalOccupiedNodeCount > fragmentation.OccupiedNodeCount {
		fragmentation.OptimalOccupiedNodeCount = fragmentation.OccupiedNodeCount
	}
	return fragmentation
}

// scalar reads an accelerator (scalar) resource amount as a rounded int64.
// Accelerator cards are whole units; CPU/memory would keep Quantity.
func Scalar(resource *api.Resource, resourceName v1.ResourceName) int64 {
	if resource == nil || resource.ScalarResources == nil {
		return 0
	}
	// Volcano stores scalar/extended resources in MILLI-units (1 device = 1000),
	// see scheduler/api NewResource. This returns that raw milli value, rounded;
	// it is the internal unit the fragmentation math and drain budget both use.
	// For a human/user-facing whole-device count use Cards.
	return int64(resource.ScalarResources[resourceName] + 0.5)
}

// Cards returns the whole number of accelerator devices in r for the given
// resource (Scalar / 1000). This is the unit users deal in — status.plan card
// counts and spec.maxPerRun.resources — as opposed to the internal milli Scalar.
func Cards(resource *api.Resource, resourceName v1.ResourceName) int64 {
	return Scalar(resource, resourceName) / 1000
}
