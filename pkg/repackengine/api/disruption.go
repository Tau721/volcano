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

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// PodGroupView exposes the Gang availability facts a disruption strategy needs about
// one PodGroup. The session fills it from api.JobInfo; tests fake it.
type PodGroupView struct {
	MinAvailable int32 // gang floor: this many pods must stay running
	Running      int32 // pods currently running for the PodGroup
	Footprint    int64 // gang's total accelerator cards (whole-gang blast radius)
}

// PodGroupViewer supplies plan-scoring facts.
type PodGroupViewer interface {
	PodGroupView(id api.JobID) PodGroupView
}

// PlanContext carries lookups shared by every scoring strategy in a comparison.
// The engine Session builds it and passes it to registered plan score functions.
type PlanContext struct {
	TargetResource v1.ResourceName // accelerator resource being defragmented
	PodGroupViews  PodGroupViewer  // per-PodGroup facts; nil-safe via PodGroupView()
}

// PodGroupView returns the PodGroup facts, zero-valued when unknown.
func (context *PlanContext) PodGroupView(podGroupID api.JobID) PodGroupView {
	if context == nil || context.PodGroupViews == nil {
		return PodGroupView{}
	}
	return context.PodGroupViews.PodGroupView(podGroupID)
}

// Resource returns the target resource selected by the RepackRun. Configuration
// validation, not the policy-neutral model, is responsible for requiring it.
func (context *PlanContext) Resource() v1.ResourceName {
	if context == nil {
		return ""
	}
	return context.TargetResource
}

// CandidatePlan is one rearrangement under comparison: committedMoves are the
// moves already selected in this pass, moves the candidate's incremental moves.
// Keeping them separate avoids copying the committed prefix for every candidate.
type CandidatePlan struct {
	committedMoves    []*Move
	moves             []*Move
	aggregateResource v1.ResourceName
	aggregate         *PlanMoveAggregate
	// incrementalFromNodes / freedNodes lazily cache the deduplicated source-node
	// views; nil means "not computed yet".
	incrementalFromNodes []string
	freedNodes           []string
}

// NewCandidatePlan creates an immutable candidate view for plugins. Full slice
// expressions freeze the visible lengths without copying; callers must treat
// existing Move records as immutable afterwards.
func NewCandidatePlan(committedMoves, moves []*Move) *CandidatePlan {
	return &CandidatePlan{
		committedMoves: committedMoves[:len(committedMoves):len(committedMoves)],
		moves:          moves[:len(moves):len(moves)],
	}
}

// PodGroupMoveAggregate is the per-PodGroup move aggregate.
type PodGroupMoveAggregate struct {
	MovedPods     int64
	MovedResource int64
}

// PlanMoveAggregate is the whole-plan move aggregate, with a per-PodGroup breakdown.
type PlanMoveAggregate struct {
	AffectedPodGroups int64
	MovedResource     int64
	MovedPods         int64
	ByPodGroup        map[api.JobID]*PodGroupMoveAggregate
}

// MoveAggregate computes (and caches) the move aggregate for the given context.
// Exported so plan score functions in plugin packages can build on it.
func (plan *CandidatePlan) MoveAggregate(context *PlanContext) *PlanMoveAggregate {
	if plan == nil {
		return &PlanMoveAggregate{ByPodGroup: map[api.JobID]*PodGroupMoveAggregate{}}
	}
	resource := context.Resource()
	if plan.aggregate != nil && plan.aggregateResource == resource {
		return plan.aggregate
	}
	moveAggregate := &PlanMoveAggregate{ByPodGroup: map[api.JobID]*PodGroupMoveAggregate{}}
	for _, move := range plan.committedMoves {
		moveAggregate.addMove(context, move)
	}
	for _, move := range plan.moves {
		moveAggregate.addMove(context, move)
	}
	moveAggregate.AffectedPodGroups = int64(len(moveAggregate.ByPodGroup))
	plan.aggregateResource = resource
	plan.aggregate = moveAggregate
	return moveAggregate
}

// IncrementalFromNodes returns the distinct source nodes freed by THIS
// candidate's own incremental moves (excluding committedMoves). It is the
// per-candidate anchor of the HyperNode block scores: the consolidation unit is a
// single node, so these moves share exactly one From. FreedNodes must NOT be used
// here — it also covers earlier committed moves, which would make the anchor
// ambiguous. Excludes To==From moves; deduplicated; nil when nothing freed.
func (plan *CandidatePlan) IncrementalFromNodes() []string {
	if plan == nil {
		return nil
	}
	if plan.incrementalFromNodes != nil {
		return plan.incrementalFromNodes
	}
	plan.incrementalFromNodes = plan.distinctFromNodes(plan.moves)
	return plan.incrementalFromNodes
}

// FreedNodes returns the distinct source nodes the whole prospective plan frees —
// committedMoves plus this candidate's moves. This is the view the block scores
// use (freed so far, including this candidate) and matches RepackPlan.FreedNodes.
// Superset of IncrementalFromNodes; excludes To==From moves; nil when nothing freed.
func (plan *CandidatePlan) FreedNodes() []string {
	if plan == nil {
		return nil
	}
	if plan.freedNodes != nil {
		return plan.freedNodes
	}
	plan.freedNodes = plan.distinctFromNodes(plan.committedMoves, plan.moves)
	return plan.freedNodes
}

// distinctFromNodes deduplicates the From of every relocation across the move
// sets, sorted for determinism. Empty From and To==From moves are not freed-node
// evidence (the To==From exclusion is defensive: drain unit moves never produce it).
func (plan *CandidatePlan) distinctFromNodes(moveSets ...[]*Move) []string {
	set := sets.New[string]()
	for _, moves := range moveSets {
		for _, move := range moves {
			if move != nil && move.From != "" && move.To != move.From {
				set.Insert(move.From)
			}
		}
	}
	return sets.List(set)
}

func (moveAggregate *PlanMoveAggregate) addMove(context *PlanContext, move *Move) {
	if move == nil || move.Task == nil || move.To == move.From {
		return // not actually relocated
	}
	requestedResource := Scalar(move.Task.InitResreq, context.Resource())
	moveAggregate.MovedPods++
	moveAggregate.MovedResource += requestedResource
	if move.Task.Job == "" {
		return
	}
	podGroupAggregate := moveAggregate.ByPodGroup[move.Task.Job]
	if podGroupAggregate == nil {
		podGroupAggregate = &PodGroupMoveAggregate{}
		moveAggregate.ByPodGroup[move.Task.Job] = podGroupAggregate
	}
	podGroupAggregate.MovedPods++
	podGroupAggregate.MovedResource += requestedResource
}

// DisruptionCost is a flat summary of a plan's default dimensions, for
// status/report output — not the comparison mechanism.
type DisruptionCost struct {
	AffectedPodGroups int64
	MovedResource     int64
	MovedPods         int64
}

// CalculateDisruptionCost summarizes a move set's default dimensions for targetResource.
func CalculateDisruptionCost(moves []*Move, targetResource v1.ResourceName) DisruptionCost {
	plan := NewCandidatePlan(nil, moves)
	moveAggregate := plan.MoveAggregate(&PlanContext{TargetResource: targetResource})
	return DisruptionCost{
		AffectedPodGroups: moveAggregate.AffectedPodGroups,
		MovedResource:     moveAggregate.MovedResource,
		MovedPods:         moveAggregate.MovedPods,
	}
}
