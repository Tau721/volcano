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

// PlanningCandidate is the stable, read-only candidate view exposed to plugins.
type PlanningCandidate struct {
	Unit api.FreeableUnit
	Plan *api.CandidatePlan
}

// CandidateFilterResult rejects a candidate before disruption scoring. Filters
// must be monotonic for one planning pass because rejected units are not revisited.
type CandidateFilterResult struct {
	Reason         string
	Message        string
	MarkInfeasible bool
}

// CandidateFilterFn evaluates a drain candidate before expensive scoring and
// relocation simulation. Returning nil admits the candidate.
type CandidateFilterFn func(ctx *api.PlanContext, candidate *PlanningCandidate) *CandidateFilterResult

type namedCandidateFilter struct {
	name string
	fn   CandidateFilterFn
}

func (s *Session) AddCandidateFilterFn(name string, fn CandidateFilterFn) {
	if fn != nil {
		s.candidateFilterFns = append(s.candidateFilterFns, namedCandidateFilter{name: name, fn: fn})
	}
}

// CandidateAdmissible applies every cheap plugin veto before candidate scoring.
func (s *Session) CandidateAdmissible(candidate *PlanningCandidate) *CandidateFilterResult {
	ctx := s.PlanContext()
	for _, filter := range s.candidateFilterFns {
		if result := filter.fn(ctx, candidate); result != nil {
			return result
		}
	}
	return nil
}

// VictimOrderFn returns a negative value when left should be simulated first,
// positive when right should be first, and zero to defer to the next plugin.
type VictimOrderFn func(left, right *schedapi.TaskInfo) int

type namedVictimOrder struct {
	name string
	fn   VictimOrderFn
}

func (s *Session) AddVictimOrderFn(name string, fn VictimOrderFn) {
	if fn != nil {
		s.victimOrderFns = append(s.victimOrderFns, namedVictimOrder{name: name, fn: fn})
	}
}

// OrderVictims applies configured lexicographic comparators and preserves the
// original order when every plugin abstains.
func (s *Session) OrderVictims(victims []*schedapi.TaskInfo) []*schedapi.TaskInfo {
	ordered := append([]*schedapi.TaskInfo(nil), victims...)
	sort.SliceStable(ordered, func(i, j int) bool {
		for _, term := range s.victimOrderFns {
			if comparison := term.fn(ordered[i], ordered[j]); comparison != 0 {
				return comparison < 0
			}
		}
		return false
	})
	return ordered
}

// DisruptionScoreFn measures one candidate-plan disruption dimension. Higher
// raw values are more disruptive; Session converts them to [0,100] preferences.
type DisruptionScoreFn func(ctx *api.PlanContext, plan *api.CandidatePlan) int64

type scoreTerm struct {
	name   string
	weight int64
	fn     DisruptionScoreFn
}

// DisruptionScoreTerm explains one enabled scoring dimension for one candidate.
type DisruptionScoreTerm struct {
	Name         string
	Weight       int64
	Raw          int64
	Score        int64
	Contribution int64
}

// CandidateDisruptionScore is the complete score used to rank one candidate.
// Higher Total is preferred, matching scheduler node-score semantics.
type CandidateDisruptionScore struct {
	Total int64
	Terms []DisruptionScoreTerm
}

const (
	MinCandidateScore int64 = 0
	MaxCandidateScore int64 = 100
)

func (s *Session) AddDisruptionScoreFn(name string, weight int64, fn DisruptionScoreFn) {
	if fn != nil {
		s.scoreTerms = append(s.scoreTerms, scoreTerm{name: name, weight: weight, fn: fn})
	}
}

// DisruptionScores reverse-normalizes every enabled term across the candidate
// batch, then applies its configured integer weight.
func (s *Session) DisruptionScores(candidates []*api.CandidatePlan) []CandidateDisruptionScore {
	scores := make([]CandidateDisruptionScore, len(candidates))
	if len(candidates) == 0 {
		return scores
	}
	ctx := s.PlanContext()
	for _, term := range s.scoreTerms {
		if term.weight <= 0 {
			continue
		}
		rawValues := make([]int64, len(candidates))
		var minimum, maximum int64
		for index, candidate := range candidates {
			rawValues[index] = term.fn(ctx, candidate)
			if index == 0 || rawValues[index] < minimum {
				minimum = rawValues[index]
			}
			if index == 0 || rawValues[index] > maximum {
				maximum = rawValues[index]
			}
		}
		span := maximum - minimum
		for index := range candidates {
			preferenceScore := MaxCandidateScore
			if span > 0 {
				normalizedCost := int64(float64(rawValues[index]-minimum) * float64(MaxCandidateScore) / float64(span))
				preferenceScore = MaxCandidateScore - normalizedCost
			}
			preferenceScore = max(MinCandidateScore, min(MaxCandidateScore, preferenceScore))
			contribution := term.weight * preferenceScore
			scores[index].Total += contribution
			scores[index].Terms = append(scores[index].Terms, DisruptionScoreTerm{
				Name: term.name, Weight: term.weight, Raw: rawValues[index],
				Score: preferenceScore, Contribution: contribution,
			})
		}
	}
	return scores
}
