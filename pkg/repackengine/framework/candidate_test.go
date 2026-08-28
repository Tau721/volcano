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
	"cmp"
	"fmt"
	"testing"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

func TestCandidateAdmissibleStopsAtFirstPluginVeto(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	called := []string{}
	ssn.AddCandidateFilterFn("allow", func(*api.PlanContext, *PlanningCandidate) *CandidateFilterResult {
		called = append(called, "allow")
		return nil
	})
	ssn.AddCandidateFilterFn("repackbudget", func(*api.PlanContext, *PlanningCandidate) *CandidateFilterResult {
		called = append(called, "repackbudget")
		return &CandidateFilterResult{Reason: "max_resource"}
	})
	ssn.AddCandidateFilterFn("must-not-run", func(*api.PlanContext, *PlanningCandidate) *CandidateFilterResult {
		called = append(called, "must-not-run")
		return nil
	})

	result := ssn.CandidateAdmissible(&PlanningCandidate{})
	if result == nil || result.Reason != "max_resource" {
		t.Fatalf("result=%+v, want max_resource veto", result)
	}
	if got := fmt.Sprint(called); got != "[allow repackbudget]" {
		t.Fatalf("callbacks=%s, want configured order with first-veto stop", got)
	}
}

func TestOrderVictimsComposesComparators(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddVictimOrderFn("resource", func(left, right *schedapi.TaskInfo) int {
		return cmp.Compare(api.Scalar(right.InitResreq, gpu), api.Scalar(left.InitResreq, gpu))
	})
	ssn.AddVictimOrderFn("name", func(left, right *schedapi.TaskInfo) int {
		return cmp.Compare(left.Name, right.Name)
	})

	ordered := ssn.OrderVictims([]*schedapi.TaskInfo{
		task("b", "g", 1), task("c", "g", 2), task("a", "g", 1),
	})
	if got := fmt.Sprint([]string{ordered[0].Name, ordered[1].Name, ordered[2].Name}); got != "[c a b]" {
		t.Fatalf("victim order=%s, want [c a b]", got)
	}
}

func TestSession_PlanScoresExplainNormalizationAndWeighting(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	// Disruption is a cost; the score fn negates it so higher = more preferred.
	ssn.AddPlanScoreFn("movedPods", 1, func(ctx *api.PlanContext, plan *api.CandidatePlan) int64 {
		return -plan.MoveAggregate(ctx).MovedPods
	})
	ssn.AddPlanScoreFn("constantRisk", 2, func(*api.PlanContext, *api.CandidatePlan) int64 {
		return 7
	})
	cheap := api.NewCandidatePlan(nil, []*api.Move{
		move(task("a", "ga", 1), "n0", "n1"),
	})
	costly := api.NewCandidatePlan(nil, []*api.Move{
		move(task("b", "gb", 1), "n0", "n1"),
		move(task("c", "gc", 1), "n0", "n1"),
	})

	scores := ssn.PlanScores([]*api.CandidatePlan{costly, cheap})
	if len(scores) != 2 || len(scores[0].Terms) != 2 || len(scores[1].Terms) != 2 {
		t.Fatalf("scores=%+v, want two candidates with two term explanations each", scores)
	}
	if scores[0].Total != 200 || scores[1].Total != 300 {
		t.Fatalf("totals=(%v,%v), want (200,300)", scores[0].Total, scores[1].Total)
	}
	movedPods := scores[0].Terms[0]
	if movedPods.Name != "movedPods" || movedPods.Raw != -2 || movedPods.Weight != 1 ||
		movedPods.Score != 0 || movedPods.Contribution != 0 {
		t.Errorf("costly movedPods explanation=%+v", movedPods)
	}
	constantRisk := scores[0].Terms[1]
	if constantRisk.Name != "constantRisk" || constantRisk.Raw != 7 ||
		constantRisk.Score != 100 || constantRisk.Contribution != 200 {
		t.Errorf("constant term explanation=%+v; tied terms must receive the same maximum score", constantRisk)
	}
	if cheapMovedPods := scores[1].Terms[0]; cheapMovedPods.Score != 100 || cheapMovedPods.Contribution != 100 {
		t.Errorf("cheap movedPods explanation=%+v, want score=100 contribution=100", cheapMovedPods)
	}
}

func TestSession_PlanScoresSkipZeroWeight(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	calls := 0
	ssn.AddPlanScoreFn("disabled", 0, func(*api.PlanContext, *api.CandidatePlan) int64 {
		calls++
		return 100
	})

	scores := ssn.PlanScores([]*api.CandidatePlan{{}, {}})
	if calls != 0 {
		t.Fatalf("disabled score function called %d times, want 0", calls)
	}
	for index, score := range scores {
		if score.Total != 0 || len(score.Terms) != 0 {
			t.Errorf("score[%d]=%+v, want no contribution or explanation for disabled term", index, score)
		}
	}
}

func TestSession_PlanScoresUseIntegerRange(t *testing.T) {
	ssn := newSession(&fakeSnap{})
	ssn.AddPlanScoreFn("cost", 3, func(ctx *api.PlanContext, plan *api.CandidatePlan) int64 {
		return -plan.MoveAggregate(ctx).MovedPods
	})

	candidates := []*api.CandidatePlan{
		api.NewCandidatePlan(nil, candidateMoves(1)),
		api.NewCandidatePlan(nil, candidateMoves(2)),
		api.NewCandidatePlan(nil, candidateMoves(4)),
	}
	scores := ssn.PlanScores(candidates)
	wantStrategyScores := []int64{100, 66, 0}
	for index, want := range wantStrategyScores {
		if got := scores[index].Terms[0].Score; got != want {
			t.Errorf("candidate[%d] strategy score=%d, want %d", index, got, want)
		}
		if got := scores[index].Total; got != want*3 {
			t.Errorf("candidate[%d] total=%d, want %d", index, got, want*3)
		}
	}
}

func candidateMoves(count int) []*api.Move {
	moves := make([]*api.Move, 0, count)
	for index := 0; index < count; index++ {
		moves = append(moves, move(task(fmt.Sprintf("task-%d", index), "g", 1), "n0", "n1"))
	}
	return moves
}
