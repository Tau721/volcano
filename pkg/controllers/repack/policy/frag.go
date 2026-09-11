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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/controllers/repack/frag"
)

// FragResult is one fragmentation measurement of a resource.
type FragResult struct {
	Providing int64 // nodes providing the resource
	Occupied  int64 // providing nodes in use
	Optimal   int64 // theoretical-optimal occupied nodes for the demand, clamped to Occupied
	Rate      float64
}

// FragFromInventory computes the cluster-wide fragmentation rate of resource
// from Node allocatable and bound, non-terminal Pod requests. It is the pure
// aggregation measureFrag runs over its lister reads, exported so the engine's
// golden test can drive the controller's exact path over an explicit inventory.
// onFrag is always cluster-wide: the run template's scope does not participate.
func FragFromInventory(nodes []*v1.Node, pods []*v1.Pod, resource v1.ResourceName) FragResult {
	var providing int64
	capacities := make([]int64, 0, len(nodes))
	capacityByNode := make(map[string]int64, len(nodes))
	for _, node := range nodes {
		if node.Status.Allocatable == nil {
			continue
		}
		quantity := node.Status.Allocatable[resource]
		capacity := quantity.MilliValue()
		if capacity <= 0 {
			continue // node does not provide the resource
		}
		providing++
		capacities = append(capacities, capacity)
		capacityByNode[node.Name] = capacity
	}

	// Bound, non-terminal pods on resource-providing nodes only (matches the
	// scheduler cache's node.Tasks view). Pods on non-providing nodes are ignored,
	// like the engine which only walks providing nodes.
	var occupied int64
	occupiedNodes := make(map[string]struct{})
	requests := make([]int64, 0, len(pods))
	for _, pod := range pods {
		if pod.Spec.NodeName == "" || podTerminal(pod.Status.Phase) {
			continue
		}
		if _, provides := capacityByNode[pod.Spec.NodeName]; !provides {
			continue
		}
		var req int64
		for _, container := range pod.Spec.Containers {
			quantity := container.Resources.Requests[resource]
			req += quantity.MilliValue()
		}
		if req <= 0 {
			continue
		}
		requests = append(requests, req)
		if _, seen := occupiedNodes[pod.Spec.NodeName]; !seen {
			occupiedNodes[pod.Spec.NodeName] = struct{}{}
			occupied++
		}
	}

	if providing == 0 {
		return FragResult{}
	}
	optimal, _ := frag.ComputeOptimalNodeCount(requests, capacities)
	if optimal > occupied {
		optimal = occupied // defensive clamp; rate stays in [0,1]
	}
	return FragResult{
		Providing: providing,
		Occupied:  occupied,
		Optimal:   optimal,
		Rate:      float64(occupied-optimal) / float64(providing),
	}
}

// measureFrag measures resource fragmentation cluster-wide from the informer
// listers. A lister failure surfaces as an error so the caller can report a
// degraded state instead of silently treating the cluster as 0% fragmented.
func (c *Controller) measureFrag(resource v1.ResourceName) (FragResult, error) {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "RepackPolicy frag: list nodes", "resource", resource)
		return FragResult{}, err
	}
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "RepackPolicy frag: list pods", "resource", resource)
		return FragResult{}, err
	}
	return FragFromInventory(nodes, pods, resource), nil
}

func podTerminal(phase v1.PodPhase) bool {
	return phase == v1.PodSucceeded || phase == v1.PodFailed
}
