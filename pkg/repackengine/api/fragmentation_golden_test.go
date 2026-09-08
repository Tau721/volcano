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

package api

// Golden equivalence between the two fragmentation aggregation paths: the
// RepackPolicy controller measures over Node allocatable and bound, non-terminal
// Pod requests (leaf pkg/policy), the engine over the scheduler cache's
// node.Tasks/Used projection. One synthetic cluster is expressed both ways and the
// two FragRates must match field-for-field; only the input aggregation can drift,
// which is what this pins.

import (
	"math"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"volcano.sh/repack-controller/pkg/policy"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// goldenPod is one bound or unbound pod in the synthetic cluster: device counts of
// gpu per container (whole devices; both sides read them as milli).
type goldenPod struct {
	name  string
	node  string // node name; "" = unbound
	phase v1.PodPhase
	reqs  []int64
}

func buildGolden(providing map[string]int64, nonProviding []string, pods []goldenPod, resourceName v1.ResourceName) ([]*v1.Node, []*v1.Pod) {
	nodes := make([]*v1.Node, 0, len(providing)+len(nonProviding))
	for name, devices := range providing {
		nodes = append(nodes, &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     v1.NodeStatus{Allocatable: v1.ResourceList{resourceName: *resource.NewQuantity(devices, resource.DecimalSI)}},
		})
	}
	for _, name := range nonProviding {
		nodes = append(nodes, &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			// Node exists but does not provide the target resource.
			Status: v1.NodeStatus{Allocatable: v1.ResourceList{v1.ResourceCPU: *resource.NewQuantity(8, resource.DecimalSI)}},
		})
	}
	corePods := make([]*v1.Pod, 0, len(pods))
	for _, p := range pods {
		containers := make([]v1.Container, 0, len(p.reqs))
		for _, devices := range p.reqs {
			containers = append(containers, v1.Container{
				Name: "c",
				Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
					resourceName: *resource.NewQuantity(devices, resource.DecimalSI),
				}},
			})
		}
		corePods = append(corePods, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: p.name},
			Spec:       v1.PodSpec{NodeName: p.node, Containers: containers},
			Status:     v1.PodStatus{Phase: p.phase},
		})
	}
	return nodes, corePods
}

// toEngineNodes rebuilds the scheduler-cache view of the inventory: a task per
// bound, non-terminal pod (finished pods have left the cache), Resreq = summed
// container requests, Used = summed task requests. It derives independently of
// FragFromInventory so the two paths stay independent under test.
func toEngineNodes(nodes []*v1.Node, pods []*v1.Pod, resourceName v1.ResourceName) []*schedapi.NodeInfo {
	out := make([]*schedapi.NodeInfo, 0, len(nodes))
	for _, node := range nodes {
		if node.Status.Allocatable == nil {
			continue
		}
		capacityQty := node.Status.Allocatable[resourceName]
		capacity := capacityQty.MilliValue()
		if capacity <= 0 {
			continue
		}
		ni := &schedapi.NodeInfo{
			Name:        node.Name,
			Allocatable: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resourceName: float64(capacity)}},
			Tasks:       map[schedapi.TaskID]*schedapi.TaskInfo{},
		}
		var used float64
		for _, pod := range pods {
			if pod.Spec.NodeName != node.Name {
				continue
			}
			if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
				continue
			}
			var req float64
			for _, container := range pod.Spec.Containers {
				q := container.Resources.Requests[resourceName]
				req += float64(q.MilliValue())
			}
			if req <= 0 {
				continue
			}
			ni.Tasks[schedapi.TaskID(pod.Name)] = &schedapi.TaskInfo{
				Resreq: &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resourceName: req}},
			}
			used += req
		}
		if used > 0 {
			ni.Used = &schedapi.Resource{ScalarResources: map[v1.ResourceName]float64{resourceName: used}}
		}
		out = append(out, ni)
	}
	return out
}

// TestGoldenControllerFragMatchesEngine feeds each synthetic cluster to both
// aggregation paths and asserts the controller (policy.FragFromInventory) and the
// engine (MeasureResourceFragmentation) agree, anchored to hand-computed values.
func TestGoldenControllerFragMatchesEngine(t *testing.T) {
	cases := []struct {
		name          string
		providing     map[string]int64
		nonProviding  []string
		pods          []goldenPod
		wantProviding int64
		wantOccupied  int64
		wantOptimal   int64
		wantRate      float64
	}{
		{name: "halfFragCluster",
			providing: map[string]int64{"n1": 8, "n2": 8},
			pods: []goldenPod{
				{name: "p1", node: "n1", phase: v1.PodRunning, reqs: []int64{4}},
				{name: "p2", node: "n2", phase: v1.PodRunning, reqs: []int64{4}},
			},
			wantProviding: 2, wantOccupied: 2, wantOptimal: 1, wantRate: 0.5},
		{name: "terminalAndNonProviderExcluded",
			providing:    map[string]int64{"n1": 8, "n2": 8},
			nonProviding: []string{"nCPU"},
			pods: []goldenPod{
				{name: "live", node: "n1", phase: v1.PodRunning, reqs: []int64{4}},
				{name: "done", node: "n2", phase: v1.PodSucceeded, reqs: []int64{4}}, // finished: no demand
				{name: "cpu", node: "nCPU", phase: v1.PodRunning, reqs: []int64{4}},  // node provides no gpu
				{name: "unbound", node: "", phase: v1.PodPending, reqs: []int64{4}},  // not scheduled
			},
			wantProviding: 2, wantOccupied: 1, wantOptimal: 1, wantRate: 0},
		{name: "multiContainerAndEmptyNode",
			providing: map[string]int64{"n1": 8, "n2": 8},
			pods: []goldenPod{
				{name: "wide", node: "n1", phase: v1.PodRunning, reqs: []int64{2, 2}}, // two containers sum to 4
				{name: "s1", node: "n1", phase: v1.PodRunning, reqs: []int64{1}},
				{name: "s2", node: "n1", phase: v1.PodRunning, reqs: []int64{1}},
				// n2 stays a providing but empty node.
			},
			wantProviding: 2, wantOccupied: 1, wantOptimal: 1, wantRate: 0},
		{name: "heterogeneousCapacities",
			providing: map[string]int64{"big": 8, "small": 4},
			pods: []goldenPod{
				{name: "pb", node: "big", phase: v1.PodRunning, reqs: []int64{3}},
				{name: "ps", node: "small", phase: v1.PodRunning, reqs: []int64{3}},
			},
			// Non-power-of-two demand on unequal capacities: volume lower bound,
			// still order-independent and clamped into [0,1].
			wantProviding: 2, wantOccupied: 2, wantOptimal: 1, wantRate: 0.5},
		{name: "emptyCluster",
			providing:     map[string]int64{},
			pods:          nil,
			wantProviding: 0, wantOccupied: 0, wantOptimal: 0, wantRate: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, corePods := buildGolden(tc.providing, tc.nonProviding, tc.pods, gpu)

			ctrl := policy.FragFromInventory(nodes, corePods, gpu)
			engine := MeasureResourceFragmentation(toEngineNodes(nodes, corePods, gpu), gpu)

			if ctrl.Providing != engine.ProvidingNodeCount ||
				ctrl.Occupied != engine.OccupiedNodeCount ||
				ctrl.Optimal != engine.OptimalOccupiedNodeCount {
				t.Errorf("controller/engine disagree: controller=%+v engine providing=%d occupied=%d optimal=%d",
					ctrl, engine.ProvidingNodeCount, engine.OccupiedNodeCount, engine.OptimalOccupiedNodeCount)
			}
			if math.Abs(ctrl.Rate-engine.FragmentationRate()) > 1e-9 {
				t.Errorf("rate drift: controller=%v engine=%v", ctrl.Rate, engine.FragmentationRate())
			}
			if ctrl.Providing != tc.wantProviding || ctrl.Occupied != tc.wantOccupied || ctrl.Optimal != tc.wantOptimal {
				t.Errorf("controller = %+v, want providing=%d occupied=%d optimal=%d",
					ctrl, tc.wantProviding, tc.wantOccupied, tc.wantOptimal)
			}
			if math.Abs(ctrl.Rate-tc.wantRate) > 1e-9 {
				t.Errorf("controller rate = %v, want %v", ctrl.Rate, tc.wantRate)
			}
		})
	}
}
