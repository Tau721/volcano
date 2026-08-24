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
	"fmt"
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

func TestOrderReceiversUsesPriorityAndEvaluatesEachRankOnce(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	stabilityCalls, fitCalls := 0, 0
	ssn.AddReceiverRankFn("bestFit", ReceiverRankPhasePacking, func(_ *api.PlanContext, _ *PlanningCandidate, receiver *ReceiverCandidate) ReceiverRank {
		fitCalls++
		return ReceiverRank{-receiver.AvailableResource}
	})
	ssn.AddReceiverRankFn("stability", ReceiverRankPhaseStability, func(_ *api.PlanContext, _ *PlanningCandidate, receiver *ReceiverCandidate) ReceiverRank {
		stabilityCalls++
		if receiver.StaysOccupied {
			return ReceiverRank{1}
		}
		return ReceiverRank{}
	})

	receivers := []*ReceiverCandidate{
		{Node: node("free-small", nil), AvailableResource: 1},
		{Node: node("stays-large", nil), StaysOccupied: true, AvailableResource: 6},
		{Node: node("stays-small", nil), StaysOccupied: true, AvailableResource: 2},
	}
	ordered := ssn.OrderReceivers(&PlanningCandidate{}, receivers)
	got := []string{ordered[0].Receiver.Node.Name, ordered[1].Receiver.Node.Name, ordered[2].Receiver.Node.Name}
	if fmt.Sprint(got) != "[stays-small stays-large free-small]" {
		t.Fatalf("receiver order=%v, want stability before best-fit", got)
	}
	if stabilityCalls != len(receivers) || fitCalls != len(receivers) {
		t.Fatalf("rank calls stability=%d fit=%d, want one call per receiver", stabilityCalls, fitCalls)
	}
}

func TestReceiverPoolChainsWithoutMutatingCallerSlice(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddReceiverPoolFn(func(_ *api.PlanContext, nodes []*schedapi.NodeInfo) []*schedapi.NodeInfo {
		nodes[0] = nil
		return nodes[1:]
	})
	original := []*schedapi.NodeInfo{node("a", nil), node("b", nil)}
	pool := ssn.ReceiverPool(original)
	if original[0] == nil || original[0].Name != "a" {
		t.Fatalf("caller slice was mutated: %v", original)
	}
	if len(pool) != 1 || pool[0].Name != "b" {
		t.Fatalf("pool=%v, want [b]", pool)
	}
}

func TestReceiverPoolCannotReintroduceOrDuplicateNodes(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	a, b, c := node("a", nil), node("b", nil), node("c", nil)
	ssn.AddReceiverPoolFn(func(_ *api.PlanContext, _ []*schedapi.NodeInfo) []*schedapi.NodeInfo {
		return []*schedapi.NodeInfo{a, c}
	})
	ssn.AddReceiverPoolFn(func(_ *api.PlanContext, current []*schedapi.NodeInfo) []*schedapi.NodeInfo {
		// b was removed by the previous filter; repeated and foreign nodes must
		// not be able to expand the pool or its capacity again.
		return append(current, b, b, node("foreign", nil))
	})

	pool := ssn.ReceiverPool([]*schedapi.NodeInfo{a, b, b, c})
	if len(pool) != 2 {
		t.Fatalf("pool=%v, want two unique retained nodes", pool)
	}
	if got := fmt.Sprint([]string{pool[0].Name, pool[1].Name}); got != "[a c]" {
		t.Fatalf("pool=%v, want the unique retained subset [a c]", got)
	}
}
