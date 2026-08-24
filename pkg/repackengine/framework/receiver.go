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

// ReceiverRankFn returns a fixed-width lexicographic rank. Larger values are preferred.
type ReceiverRankFn func(ctx *api.PlanContext, candidate *PlanningCandidate, receiver *ReceiverCandidate) ReceiverRank

// ReceiverRank is one plugin's fixed-width, lexicographically compared score.
type ReceiverRank [5]int64

// ReceiverRankPhase defines the stable order between receiver policy groups.
type ReceiverRankPhase int

const (
	// ReceiverRankPhaseStability evaluates placement stability first.
	ReceiverRankPhaseStability ReceiverRankPhase = iota
	// ReceiverRankPhaseDisruption evaluates workload impact after stability.
	ReceiverRankPhaseDisruption
	// ReceiverRankPhasePacking evaluates packing preferences last.
	ReceiverRankPhasePacking
)

type namedReceiverRank struct {
	name  string
	phase ReceiverRankPhase
	order int
	fn    ReceiverRankFn
}

// ReceiverRankTerm records one named plugin's rank for diagnostics.
type ReceiverRankTerm struct {
	Name   string
	Values ReceiverRank
}

// RankedReceiver contains a receiver and all ranks used to order it.
type RankedReceiver struct {
	Receiver *ReceiverCandidate
	Terms    []ReceiverRankTerm
}

func (s *Session) AddReceiverPoolFn(fn ReceiverPoolFn) {
	if fn != nil {
		s.receiverPoolFns = append(s.receiverPoolFns, fn)
	}
}

func (s *Session) AddReceiverRankFn(name string, phase ReceiverRankPhase, fn ReceiverRankFn) {
	if fn == nil {
		return
	}
	s.receiverRankFns = append(s.receiverRankFns, namedReceiverRank{
		name: name, phase: phase, order: len(s.receiverRankFns), fn: fn,
	})
	sort.SliceStable(s.receiverRankFns, func(i, j int) bool {
		if s.receiverRankFns[i].phase != s.receiverRankFns[j].phase {
			return s.receiverRankFns[i].phase < s.receiverRankFns[j].phase
		}
		return s.receiverRankFns[i].order < s.receiverRankFns[j].order
	})
}

// ReceiverPool chains filter-only policies without allowing a plugin to
// reintroduce removed nodes, foreign nodes, or duplicate capacity.
func (s *Session) ReceiverPool(nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo {
	pool := intersectReceiverPool(nodes, nodes)
	ctx := s.PlanContext()
	for _, fn := range s.receiverPoolFns {
		selected := fn(ctx, append([]*schedapi.NodeInfo(nil), pool...))
		pool = intersectReceiverPool(pool, selected)
	}
	return pool
}

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

// OrderReceivers evaluates each rank once, then performs a stable lexicographic sort.
func (s *Session) OrderReceivers(candidate *PlanningCandidate, receivers []*ReceiverCandidate) []RankedReceiver {
	ctx := s.PlanContext()
	ranked := make([]RankedReceiver, 0, len(receivers))
	allTerms := make([]ReceiverRankTerm, len(receivers)*len(s.receiverRankFns))
	for receiverIndex, receiver := range receivers {
		terms := allTerms[receiverIndex*len(s.receiverRankFns) : (receiverIndex+1)*len(s.receiverRankFns)]
		for rankIndex, rankFn := range s.receiverRankFns {
			terms[rankIndex] = ReceiverRankTerm{Name: rankFn.name, Values: rankFn.fn(ctx, candidate, receiver)}
		}
		ranked = append(ranked, RankedReceiver{Receiver: receiver, Terms: terms})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return compareReceiverRanks(ranked[i].Terms, ranked[j].Terms) > 0
	})
	return ranked
}

func compareReceiverRanks(left, right []ReceiverRankTerm) int {
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
