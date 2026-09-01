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

package framework

import (
	"sort"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// ReceiverCandidate is the planner-computed, policy-neutral receiver view.
type ReceiverCandidate struct {
	Node              *schedapi.NodeInfo
	StaysOccupied     bool
	AvailableResource int64
}

// ReceiverPoolFn applies a snapshot-stable receiver-universe policy once per pass.
type ReceiverPoolFn func(ctx *api.PlanContext, nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo

// ReceiverPreferenceFn returns a fixed-width lexicographic preference. Larger
// values are preferred.
type ReceiverPreferenceFn func(ctx *api.PlanContext, candidate *PlanningCandidate, receiver *ReceiverCandidate) ReceiverPreference

// ReceiverPreference is one plugin's fixed-width, lexicographically compared
// preference vector.
type ReceiverPreference [5]int64

// ReceiverPreferencePhase defines the stable order between receiver policy groups.
type ReceiverPreferencePhase int

const (
	// ReceiverPreferencePhaseStability evaluates placement stability first.
	ReceiverPreferencePhaseStability ReceiverPreferencePhase = iota
	// ReceiverPreferencePhaseTopology evaluates HyperNode-block placement after
	// stability: it may only ever lose to stability policies (which prefer
	// sacrificial, non-drainable receivers), never to disruption or packing.
	ReceiverPreferencePhaseTopology
	// ReceiverPreferencePhaseDisruption evaluates workload impact after topology.
	ReceiverPreferencePhaseDisruption
	// ReceiverPreferencePhasePacking evaluates packing preferences last.
	ReceiverPreferencePhasePacking
)

type namedReceiverPreference struct {
	name  string
	phase ReceiverPreferencePhase
	order int
	fn    ReceiverPreferenceFn
}

// ReceiverPreferenceTerm records one named plugin's preference for diagnostics.
type ReceiverPreferenceTerm struct {
	Name   string
	Values ReceiverPreference
}

// OrderedReceiver contains a receiver and all preferences used to order it.
type OrderedReceiver struct {
	Receiver *ReceiverCandidate
	Terms    []ReceiverPreferenceTerm
}

func (s *Session) AddReceiverPoolFn(fn ReceiverPoolFn) {
	if fn != nil {
		s.receiverPoolFns = append(s.receiverPoolFns, fn)
	}
}

func (s *Session) AddReceiverPreferenceFn(name string, phase ReceiverPreferencePhase, fn ReceiverPreferenceFn) {
	if fn == nil {
		return
	}
	s.receiverPreferenceFns = append(s.receiverPreferenceFns, namedReceiverPreference{
		name: name, phase: phase, order: len(s.receiverPreferenceFns), fn: fn,
	})
	sort.SliceStable(s.receiverPreferenceFns, func(i, j int) bool {
		if s.receiverPreferenceFns[i].phase != s.receiverPreferenceFns[j].phase {
			return s.receiverPreferenceFns[i].phase < s.receiverPreferenceFns[j].phase
		}
		return s.receiverPreferenceFns[i].order < s.receiverPreferenceFns[j].order
	})
}

// ReceiverPool chains filter-only policies without allowing a plugin to
// reintroduce removed nodes, foreign nodes, or duplicate capacity.
func (s *Session) ReceiverPool(nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo {
	pool := normalizeReceiverPool(nodes)
	ctx := s.PlanContext()
	for _, fn := range s.receiverPoolFns {
		selected := fn(ctx, append([]*schedapi.NodeInfo(nil), pool...))
		pool = intersectReceiverPool(pool, selected)
	}
	return pool
}

// normalizeReceiverPool establishes the initial receiver universe. It removes
// unusable identities and duplicate capacity while preserving the caller's
// stable node order.
func normalizeReceiverPool(nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo {
	retained := make([]*schedapi.NodeInfo, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if node == nil || node.Name == "" || seen[node.Name] {
			continue
		}
		seen[node.Name] = true
		retained = append(retained, node)
	}
	return retained
}

// intersectReceiverPool retains the current pool's order and node objects,
// using selected only as a membership set. A policy therefore cannot expand
// the receiver universe or duplicate its capacity.
func intersectReceiverPool(current, selected []*schedapi.NodeInfo) []*schedapi.NodeInfo {
	selectedNames := make(map[string]bool, len(selected))
	for _, node := range selected {
		if node != nil && node.Name != "" {
			selectedNames[node.Name] = true
		}
	}
	retained := make([]*schedapi.NodeInfo, 0, min(len(current), len(selectedNames)))
	seen := make(map[string]bool, len(current))
	for _, node := range current {
		if node == nil || node.Name == "" || seen[node.Name] || !selectedNames[node.Name] {
			continue
		}
		seen[node.Name] = true
		retained = append(retained, node)
	}
	return retained
}

// OrderReceivers evaluates each preference once, then performs a stable
// lexicographic sort.
func (s *Session) OrderReceivers(candidate *PlanningCandidate, receivers []*ReceiverCandidate) []OrderedReceiver {
	ctx := s.PlanContext()
	ordered := make([]OrderedReceiver, 0, len(receivers))
	allTerms := make([]ReceiverPreferenceTerm, len(receivers)*len(s.receiverPreferenceFns))
	for receiverIndex, receiver := range receivers {
		terms := allTerms[receiverIndex*len(s.receiverPreferenceFns) : (receiverIndex+1)*len(s.receiverPreferenceFns)]
		for preferenceIndex, preferenceFn := range s.receiverPreferenceFns {
			terms[preferenceIndex] = ReceiverPreferenceTerm{
				Name: preferenceFn.name, Values: preferenceFn.fn(ctx, candidate, receiver),
			}
		}
		ordered = append(ordered, OrderedReceiver{Receiver: receiver, Terms: terms})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return compareReceiverPreferences(ordered[i].Terms, ordered[j].Terms) > 0
	})
	return ordered
}

func compareReceiverPreferences(left, right []ReceiverPreferenceTerm) int {
	for termIndex := 0; termIndex < len(left) && termIndex < len(right); termIndex++ {
		leftValues, rightValues := left[termIndex].Values, right[termIndex].Values
		for valueIndex := range leftValues {
			switch {
			case leftValues[valueIndex] > rightValues[valueIndex]:
				return 1
			case leftValues[valueIndex] < rightValues[valueIndex]:
				return -1
			}
		}
	}
	return 0
}
