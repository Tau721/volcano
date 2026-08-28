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

// Package drain contains the reusable incremental drain planner used by the
// repack action. Each step cheaply orders the active drain units against the
// committed moves, then lazily invokes the scheduler-faithful relocation solver
// in that order. The first feasible unit is committed. This preserves complete
// scheduler validation for the selected plan without paying that cost for every
// candidate in a large cluster. Because gang damage is scored over the whole
// prospective plan, a unit that reuses an already-affected gang remains cheap.
// Vacating a unit is atomic and bounded by plugin-provided disruption budgets;
// the calling Action owns final benefit admission.
//
// "Unit" generalizes "node": a node-domain plugin yields one single-node unit per
// node; a hypernode-domain plugin yields multi-node units. With both enabled
// the units are a weighted union and the planner prefers higher-benefit units first.
package drain

import (
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/metrics"
)

const prospectiveReceiver = "<prospective>"

// BuildPlan constructs a plan from the session snapshot. Cross-cutting action
// concerns such as benefit admission and reporting deliberately stay in the
// action, while scenario policy is supplied through session callbacks.
func BuildPlan(ssn *framework.Session) *api.RepackPlan {
	targetResource := ssn.Resource()
	nodes := ssn.Nodes()
	movable := ssn.Movable()
	units := ssn.FreeableUnits()
	if len(units) == 0 || len(nodes) == 0 {
		return nil
	}
	nodesByName := make(map[string]*schedapi.NodeInfo, len(nodes))
	for _, n := range nodes {
		if n != nil {
			nodesByName[n.Name] = n
		}
	}

	return drainGreedy(nodes, nodesByName, units, ssn, movable, targetResource)
}

// candidate is one unit that can be vacated this step, with the moves that vacate
// it and the disruption-budget deltas those moves imply.
type candidate struct {
	unit               api.FreeableUnit
	inUnit             map[string]bool
	victims            []*schedapi.TaskInfo
	scoringMoves       []*api.Move
	prospectivePlan    *api.CandidatePlan
	placed             []*api.Move
	additionalResource int64
	key                string
}

// preparedUnit contains the immutable data collected once for a drain unit.
// active is monotonic: a unit leaves the set after it is proven infeasible,
// exceeds a monotonic budget, is drained, or receives a committed move.
type preparedUnit struct {
	candidate candidate
	active    bool
}

type scoredCandidate struct {
	candidate candidate
	score     framework.CandidatePlanScore
}

// drainState is the running state of one drain pass. Feasibility (which victims
// can relocate where) is delegated to the snapshot's scheduler-faithful feasibility check
// (Snapshot.FeasibleRelocation); this struct only tracks the greedy pass progress
// and the disruption budget.
type drainState struct {
	// Fixed for the whole pass.
	ssn                 *framework.Session
	snapshot            framework.Snapshot
	nodesByName         map[string]*schedapi.NodeInfo
	movable             api.Movable
	resource            v1.ResourceName
	alwaysStaysOccupied map[string]bool // immovable or outside drain scope
	receiverNodes       []*schedapi.NodeInfo
	receiverSlackByNode map[string]int64
	receiverResource    int64 // remaining target-resource slack across eligible receivers
	// Mutated as units are committed.
	drained                map[string]bool  // emptied — no longer a receiver
	filled                 map[string]bool  // received a moved-in pod
	provenStuck            map[string]bool  // proven un-vacatable this pass → prefer as receiver
	stuckUnits             map[string]bool  // monotonic infeasibility cache; never re-simulate
	placedResourceByNode   map[string]int64 // resource already assigned to a receiver by committed moves
	candidatesEvaluated    int
	feasibilitySimulations int
	prunedByReason         map[string]int
	activeUnits            []*preparedUnit
	moves                  []*api.Move
	freedNodes             []string
	freedUnits             []api.FreeableUnit
}

// drainGreedy is the single dynamic pass. Static unit facts are prepared once.
// Each step orders active units with cheap prospective moves, then runs complete
// scheduler simulation lazily until the first feasible candidate is found.
// Terminates because each commit drains >= 1 node and inactive units never return.
func drainGreedy(
	nodes []*schedapi.NodeInfo,
	nodesByName map[string]*schedapi.NodeInfo,
	units []api.FreeableUnit,
	ssn *framework.Session,
	movable api.Movable,
	targetResource v1.ResourceName,
) *api.RepackPlan {
	s := newDrainState(nodes, nodesByName, ssn, movable, targetResource)
	s.prepareUnits(units)
	planningStartTime := time.Now()
	defer func() {
		metrics.ObservePlanner(string(ssn.Mode()), s.candidatesEvaluated, s.feasibilitySimulations, s.prunedByReason)
		klog.V(4).InfoS("repack drain: planning performance summary", "run", runName(ssn),
			"candidateEvaluations", s.candidatesEvaluated, "feasibilitySimulations", s.feasibilitySimulations,
			"prunedByReason", s.prunedByReason, "duration", time.Since(planningStartTime))
	}()
	for step := 1; ; step++ {
		if ssn.Context().Err() != nil {
			break
		}
		// 1. Order all currently active candidates without constructing receiver
		// lists, cloning scheduler state, or running predicates.
		preliminary := s.preliminaryCandidates()
		if len(preliminary) == 0 {
			break
		}
		ordered := orderScoredCandidates(s.scoreCandidates(preliminary))
		klog.V(4).InfoS("repack drain: step ordered active units", "step", step,
			"activeCandidates", len(ordered), "nodesFreedSoFar", len(s.freedNodes))

		// 2. Lazily run the expensive scheduler simulation in plan score order.
		chosen, selectedPosition, ok := s.firstFeasibleCandidate(ordered)
		if !ok {
			break
		}
		s.logCandidateOrder(step, ordered, selectedPosition)
		// 3. Commit the first scheduler-feasible candidate.
		klog.V(4).InfoS("repack drain: committing unit", "step", step, "unit", chosen.key,
			"freesNodes", chosen.unit.Nodes, "moves", len(chosen.placed), "movedResource", chosen.additionalResource)
		s.commit(chosen)
	}
	// 4. A pass that freed nothing yields no plan.
	return s.plan()
}

// newDrainState caches the per-pass budget caps and initializes progress maps.
func newDrainState(
	nodes []*schedapi.NodeInfo,
	nodesByName map[string]*schedapi.NodeInfo,
	ssn *framework.Session,
	movable api.Movable,
	targetResource v1.ResourceName,
) *drainState {
	state := &drainState{
		ssn:                  ssn,
		snapshot:             ssn.Snapshot(),
		nodesByName:          nodesByName,
		movable:              movable,
		resource:             targetResource,
		alwaysStaysOccupied:  make(map[string]bool, len(nodes)),
		receiverSlackByNode:  make(map[string]int64, len(nodes)),
		drained:              make(map[string]bool),
		filled:               make(map[string]bool),
		provenStuck:          make(map[string]bool),
		stuckUnits:           make(map[string]bool),
		placedResourceByNode: make(map[string]int64),
		prunedByReason:       make(map[string]int),
	}
	// Base receiver eligibility is a planner invariant, not an optional packing
	// policy. Repack never lights up an empty target-resource node, and a fully
	// occupied node has no useful receiver capacity. Give plugins only this
	// prefiltered universe. ReceiverPool enforces filter-only subset semantics;
	// the second eligibility pass remains a defensive boundary for custom code.
	baseReceivers, receiverStats := eligibleReceiverNodes(nodes, targetResource)
	state.receiverNodes, _ = eligibleReceiverNodes(ssn.ReceiverPool(baseReceivers), targetResource)
	klog.V(4).InfoS("repack drain: base receiver pool prepared",
		"eligibleReceivers", len(state.receiverNodes),
		"emptyNodesExcluded", receiverStats.empty,
		"fullNodesExcluded", receiverStats.full,
		"noSlackNodesExcluded", receiverStats.noSlack,
		"unavailableNodesExcluded", receiverStats.unavailable)
	for _, node := range nodes {
		if node == nil {
			continue
		}
		state.alwaysStaysOccupied[node.Name] =
			!api.EvaluateNodeFreeability(node, api.NodeFreeabilityState{}, movable, targetResource).Freeable ||
				!state.snapshot.NodeInScope(node)
	}
	for _, node := range state.receiverNodes {
		slack := receiverSlack(node, targetResource)
		state.receiverSlackByNode[node.Name] = slack
		state.receiverResource += max(slack, 0)
	}
	return state
}

// prepareUnits performs all snapshot-stable drain checks and materializes the
// victims/scoring moves once. Dynamic state and budgets are handled per step.
func (s *drainState) prepareUnits(units []api.FreeableUnit) {
	s.activeUnits = make([]*preparedUnit, 0, len(units))
	for _, unit := range units {
		if prepared, ok := s.prepareUnit(unit); ok {
			s.activeUnits = append(s.activeUnits, &preparedUnit{candidate: prepared, active: true})
		}
	}
}

func (s *drainState) prepareUnit(unit api.FreeableUnit) (candidate, bool) {
	key := unitKey(unit)
	// Domain plugins should only contribute partially occupied nodes, but retain
	// this defensive planner gate so a custom domain cannot move pods off an
	// empty or already compact, fully occupied node.
	for _, nodeName := range unit.Nodes {
		class := api.ClassifyTargetResourceNode(s.nodesByName[nodeName], s.resource)
		if class != api.TargetResourceNodePartial {
			s.recordPruned("source_node_not_partially_occupied")
			klog.V(5).InfoS("repack drain: skip unit — source node is not partially occupied",
				"unit", key, "node", nodeName, "targetResource", s.resource, "classification", class)
			return candidate{}, false
		}
	}
	inUnit, diagnostics, ok := freeableNow(unit, s.nodesByName, nil, nil, s.movable, s.resource)
	if !ok {
		logNonFreeableNodes(key, diagnostics, s.resource)
		return candidate{}, false
	}
	// Skip accelerator-empty units: freeing a node that runs no accelerator pod
	// isn't defrag (its accelerator capacity is already idle).
	accelerated := false
	for _, nodeName := range unit.Nodes {
		if occupiesAccelerator(s.nodesByName[nodeName], s.resource) {
			accelerated = true
			break
		}
	}
	if !accelerated {
		klog.V(5).InfoS("repack drain: skip unit — no accelerator pods on it", "unit", key, "nodes", unit.Nodes)
		return candidate{}, false
	}
	var victims []*schedapi.TaskInfo
	for _, nodeName := range unit.Nodes {
		victims = append(victims, api.VictimsOf(s.nodesByName[nodeName], s.movable, s.resource)...)
	}
	if len(victims) == 0 {
		klog.V(5).InfoS("repack drain: skip unit — no movable accelerator victims", "unit", key, "nodes", unit.Nodes)
		return candidate{}, false
	}
	// Calculate cheap, deterministic prospective deltas before invoking the full
	// scheduler simulation. Under a tight maxPerRun this avoids cloning cycle
	// state and running predicates for candidates that cannot be selected anyway.
	var additionalResource int64
	for _, v := range victims {
		additionalResource += api.Scalar(v.InitResreq, s.resource)
	}
	scoringMoves := make([]*api.Move, 0, len(victims))
	for _, victim := range victims {
		scoringMoves = append(scoringMoves, &api.Move{Task: victim, From: victim.NodeName, To: prospectiveReceiver})
	}
	return candidate{
		unit: unit, inUnit: inUnit, victims: victims, scoringMoves: scoringMoves,
		additionalResource: additionalResource, key: key,
	}, true
}

// preliminaryCandidates applies cheap, monotonic state, budget, and aggregate
// receiver-capacity checks. Candidates rejected here never enter disruption
// scoring and are removed permanently from the active set.
func (s *drainState) preliminaryCandidates() []candidate {
	preliminary := make([]candidate, 0, len(s.activeUnits))
	active := s.activeUnits[:0]
	for _, prepared := range s.activeUnits {
		if !prepared.active {
			continue
		}
		candidate := prepared.candidate
		if s.stuckUnits[candidate.key] {
			prepared.active = false
			continue
		}
		if s.unitConsumed(candidate.unit) {
			prepared.active = false
			continue
		}
		s.candidatesEvaluated++
		availableReceiverResource := s.availableReceiverResource(candidate.inUnit)
		if availableReceiverResource < candidate.additionalResource {
			s.recordPruned("insufficient_receiver_resource")
			klog.V(5).InfoS("repack drain: candidate rejected by aggregate receiver capacity preflight",
				"unit", candidate.key, "required", candidate.additionalResource,
				"available", availableReceiverResource)
			s.markUnitInfeasible(candidate.key, candidate.unit)
			prepared.active = false
			continue
		}
		planningCandidate := s.planningCandidate(candidate)
		if rejected := s.ssn.CandidateAdmissible(planningCandidate); rejected != nil {
			s.recordPruned(rejected.Reason)
			klog.V(5).InfoS("repack drain: candidate rejected by plugin",
				"unit", candidate.key, "reason", rejected.Reason, "message", rejected.Message)
			if rejected.MarkInfeasible {
				s.markUnitInfeasible(candidate.key, candidate.unit)
			}
			prepared.active = false
			continue
		}
		// Reuse the same whole-plan view (and its cached move aggregate) for
		// disruption scoring and receiver ordering in this step.
		candidate.prospectivePlan = planningCandidate.Plan
		preliminary = append(preliminary, candidate)
		active = append(active, prepared)
	}
	s.activeUnits = active
	return preliminary
}

func (s *drainState) unitConsumed(unit api.FreeableUnit) bool {
	for _, nodeName := range unit.Nodes {
		if s.drained[nodeName] || s.filled[nodeName] {
			return true
		}
	}
	return false
}

// firstFeasibleCandidate is the lazy boundary: candidates are already ordered
// by cheap disruption scoring, and complete scheduler simulation stops at the
// first success. selectedPosition is one-based within the preliminary order.
func (s *drainState) firstFeasibleCandidate(ordered []scoredCandidate) (candidate, int, bool) {
	for index := range ordered {
		if s.ssn.Context().Err() != nil {
			return candidate{}, 0, false
		}
		candidate := ordered[index].candidate
		candidate.victims = s.ssn.OrderVictims(candidate.victims)
		receivers := s.receiversInPreferenceOrderWithPlan(candidate.inUnit, candidate.victims, candidate.prospectivePlan)
		if !s.receiversHaveResourceCapacity(receivers, candidate.additionalResource) {
			s.recordPruned("insufficient_receiver_resource")
			klog.V(5).InfoS("repack drain: skip unit — receiver resource capacity is insufficient", "unit", candidate.key,
				"required", candidate.additionalResource, "receivers", len(receivers))
			s.markUnitInfeasible(candidate.key, candidate.unit)
			continue
		}
		klog.V(5).InfoS("repack drain: evaluating unit feasibility", "unit", candidate.key,
			"victims", taskNames(candidate.victims), "victimCount", len(candidate.victims),
			"receivers", nodeNames(receivers), "receiverCount", len(receivers))
		s.feasibilitySimulations++
		placed, feasible := s.snapshot.FeasibleRelocation(s.ssn.Context(), s.moves, candidate.victims, receivers)
		if !feasible {
			klog.V(4).InfoS("repack drain: unit INFEASIBLE — victims cannot all relocate onto receivers",
				"unit", candidate.key, "victims", len(candidate.victims), "receivers", len(receivers))
			s.markUnitInfeasible(candidate.key, candidate.unit)
			s.recordPruned("scheduler_infeasible")
			continue
		}
		candidate.placed = placed
		if klog.V(5).Enabled() {
			for _, move := range placed {
				klog.V(5).InfoS("repack drain: victim placement", "unit", candidate.key,
					"pod", move.Task.Name, "from", move.From, "to", move.To)
			}
		}
		return candidate, index + 1, true
	}
	return candidate{}, 0, false
}

// planningCandidate exposes the complete prospective plan to policy plugins
// before disruption scoring.
func (s *drainState) planningCandidate(candidate candidate) *framework.PlanningCandidate {
	return &framework.PlanningCandidate{
		Unit: candidate.unit,
		Plan: api.NewCandidatePlan(s.moves, candidate.scoringMoves),
	}
}

// availableReceiverResource is the target-resource slack outside the candidate
// unit after accounting for moves committed earlier in this planning pass.
func (s *drainState) availableReceiverResource(inUnit map[string]bool) int64 {
	available := s.receiverResource
	for nodeName := range inUnit {
		available -= max(s.receiverSlackByNode[nodeName]-s.placedResourceByNode[nodeName], 0)
	}
	return available
}

func (s *drainState) recordPruned(reason string) {
	s.prunedByReason[reason]++
}

func runName(ssn *framework.Session) string {
	if run := ssn.Run(); run != nil {
		return run.Name
	}
	return ""
}

// receiversHaveResourceCapacity is a necessary, intentionally predicate-free
// preflight. It accounts for moves already committed to a receiver, then lets
// FeasibleRelocation perform the authoritative multi-dimensional filter check.
func (s *drainState) receiversHaveResourceCapacity(receivers []*schedapi.NodeInfo, requiredResource int64) bool {
	var availableResource int64
	for _, receiver := range receivers {
		availableResource += s.receiverSlackByNode[receiver.Name] - s.placedResourceByNode[receiver.Name]
		if availableResource >= requiredResource {
			return true
		}
	}
	return false
}

func (s *drainState) markUnitInfeasible(key string, unit api.FreeableUnit) {
	for _, nodeName := range unit.Nodes {
		s.provenStuck[nodeName] = true
	}
	s.stuckUnits[key] = true
}

// receiversInPreferenceOrder returns the nodes that may receive relocated pods,
// in the order composed by receiver policy plugins. The planner supplies dynamic
// facts and enforces state/capacity invariants; it does not understand Gang or
// bin-packing semantics.
func (s *drainState) receiversInPreferenceOrder(
	inUnit map[string]bool,
	candidateVictims []*schedapi.TaskInfo,
) []*schedapi.NodeInfo {
	return s.receiversInPreferenceOrderWithPlan(inUnit, candidateVictims, nil)
}

func (s *drainState) receiversInPreferenceOrderWithPlan(
	inUnit map[string]bool,
	candidateVictims []*schedapi.TaskInfo,
	prospectivePlan *api.CandidatePlan,
) []*schedapi.NodeInfo {
	receivers := make([]*framework.ReceiverCandidate, 0, len(s.receiverNodes))
	minimumVictimResource := minTaskResource(candidateVictims, s.resource)
	for _, n := range s.receiverNodes {
		if inUnit[n.Name] || s.drained[n.Name] {
			continue
		}
		availableResource := s.receiverSlackByNode[n.Name] - s.placedResourceByNode[n.Name]
		// Every victim requests the target resource. A receiver that cannot fit
		// even the smallest victim can never be selected by the full solver.
		if availableResource < minimumVictimResource {
			continue
		}
		receivers = append(receivers, &framework.ReceiverCandidate{
			Node:              n,
			StaysOccupied:     s.staysOccupied(n),
			AvailableResource: availableResource,
		})
	}
	if prospectivePlan == nil {
		prospectivePlan = api.NewCandidatePlan(s.moves, prospectiveMoves(candidateVictims))
	}
	planningCandidate := &framework.PlanningCandidate{Plan: prospectivePlan}
	ordered := s.ssn.OrderReceivers(planningCandidate, receivers)
	if klog.V(5).Enabled() && len(ordered) > 0 {
		preferred := ordered[0]
		klog.V(5).InfoS("repack drain: preferred receiver calculated",
			"candidateVictims", taskNames(candidateVictims),
			"receiverCount", len(ordered),
			"preferredReceiver", preferred.Receiver.Node.Name,
			"staysOccupied", preferred.Receiver.StaysOccupied,
			"availableResource", preferred.Receiver.AvailableResource,
			"pluginPreferences", preferred.Terms)
	}

	orderedNodes := make([]*schedapi.NodeInfo, 0, len(ordered))
	for _, receiver := range ordered {
		orderedNodes = append(orderedNodes, receiver.Receiver.Node)
	}
	return orderedNodes
}

func prospectiveMoves(victims []*schedapi.TaskInfo) []*api.Move {
	moves := make([]*api.Move, 0, len(victims))
	for _, victim := range victims {
		moves = append(moves, &api.Move{Task: victim, From: victim.NodeName, To: prospectiveReceiver})
	}
	return moves
}

func minTaskResource(tasks []*schedapi.TaskInfo, targetResource v1.ResourceName) int64 {
	var minimum int64
	for _, task := range tasks {
		requested := api.Scalar(task.InitResreq, targetResource)
		if requested > 0 && (minimum == 0 || requested < minimum) {
			minimum = requested
		}
	}
	return minimum
}

// receiverSlack is the immediately available target-resource capacity used to
// best-fit sort receivers. Allocatable-Used remains valid when a newly added
// extended resource has not yet been reflected in NodeInfo.Idle.
func receiverSlack(n *schedapi.NodeInfo, targetResource v1.ResourceName) int64 {
	return api.Scalar(api.NodeFreeCapacity(n), targetResource)
}

// eligibleReceiverNodes performs the snapshot-stable receiver prefilter before
// any candidate scoring. Only partially occupied nodes with positive scheduler-
// visible slack may receive moved pods; empty nodes remain idle and full nodes
// never enter plugin ordering or scheduler simulation.
type receiverEligibilityStats struct {
	unavailable int
	empty       int
	full        int
	noSlack     int
}

func eligibleReceiverNodes(
	nodes []*schedapi.NodeInfo,
	targetResource v1.ResourceName,
) ([]*schedapi.NodeInfo, receiverEligibilityStats) {
	eligible := make([]*schedapi.NodeInfo, 0, len(nodes))
	var stats receiverEligibilityStats
	for _, node := range nodes {
		class := api.ClassifyTargetResourceNode(node, targetResource)
		switch class {
		case api.TargetResourceNodeUnavailable:
			stats.unavailable++
			continue
		case api.TargetResourceNodeEmpty:
			stats.empty++
			continue
		case api.TargetResourceNodeFull:
			stats.full++
			continue
		case api.TargetResourceNodePartial:
			// Continue with scheduler-visible slack validation below.
		default:
			stats.unavailable++
			continue
		}
		if receiverSlack(node, targetResource) <= 0 {
			stats.noSlack++
			continue
		}
		eligible = append(eligible, node)
	}
	return eligible, stats
}

// staysOccupied reports whether a node will remain occupied regardless of this pass,
// so it is a preferred receiver. A node stays if it already received a committed
// move, has an immovable accelerator pod, is receiver-only (excluded from
// draining by scope.nodes), or was already proven un-vacatable this pass.
func (s *drainState) staysOccupied(n *schedapi.NodeInfo) bool {
	return s.filled[n.Name] ||
		s.alwaysStaysOccupied[n.Name] ||
		s.provenStuck[n.Name]
}

// scoreCandidates orders active candidates by deterministic tie-breakers, then
// evaluates their preliminary plan scores. Stable score ordering preserves
// higher unit benefit and lexical unit key when scores tie.
func (s *drainState) scoreCandidates(activeCandidates []candidate) []scoredCandidate {
	orderedCandidates := append([]candidate(nil), activeCandidates...)
	sort.SliceStable(orderedCandidates, func(i, j int) bool {
		if orderedCandidates[i].unit.Weight != orderedCandidates[j].unit.Weight {
			return orderedCandidates[i].unit.Weight > orderedCandidates[j].unit.Weight
		}
		return orderedCandidates[i].key < orderedCandidates[j].key
	})
	candidatePlans := make([]*api.CandidatePlan, len(orderedCandidates))
	for index, candidate := range orderedCandidates {
		moves := candidate.scoringMoves
		if len(moves) == 0 {
			moves = candidate.placed // direct unit tests and already-realized candidates
		}
		candidatePlans[index] = orderedCandidates[index].prospectivePlan
		if candidatePlans[index] == nil {
			candidatePlans[index] = api.NewCandidatePlan(s.moves, moves)
		}
	}
	scores := s.ssn.PlanScores(candidatePlans)
	scored := make([]scoredCandidate, len(orderedCandidates))
	for index := range orderedCandidates {
		scored[index] = scoredCandidate{candidate: orderedCandidates[index], score: scores[index]}
	}
	return scored
}

func orderScoredCandidates(scored []scoredCandidate) []scoredCandidate {
	ordered := append([]scoredCandidate(nil), scored...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].score.Total > ordered[j].score.Total
	})
	return ordered
}

// commit applies the chosen candidate to the pass state: mark receivers filled and
// the unit's nodes drained, record moved PodGroups/cards, and append the moves.
func (s *drainState) commit(chosen candidate) {
	for _, m := range chosen.placed {
		s.filled[m.To] = true
		if m.Task != nil {
			movedResource := api.Scalar(m.Task.InitResreq, s.resource)
			s.placedResourceByNode[m.To] += movedResource
			s.receiverResource -= movedResource
		}
	}
	for _, nodeName := range chosen.unit.Nodes {
		s.drained[nodeName] = true
		s.receiverResource -= max(s.receiverSlackByNode[nodeName]-s.placedResourceByNode[nodeName], 0)
		s.freedNodes = append(s.freedNodes, nodeName)
	}
	s.freedUnits = append(s.freedUnits, chosen.unit)
	s.moves = append(s.moves, chosen.placed...)
}

// plan is the pass result: nil when nothing was freed.
func (s *drainState) plan() *api.RepackPlan {
	if len(s.freedNodes) == 0 {
		return nil
	}
	return &api.RepackPlan{Moves: s.moves, FreedNodes: s.freedNodes, FreedUnits: s.freedUnits}
}

type nodeFreeability struct {
	nodeName string
	result   api.NodeFreeability
}

// freeableNow evaluates every node in a unit exactly once. It returns the
// blocking diagnostics alongside the eligibility result, so logging cannot
// diverge from the decision or require a second task scan.
func freeableNow(unit api.FreeableUnit, nodesByName map[string]*schedapi.NodeInfo, drained, filled map[string]bool, movable api.Movable, targetResource v1.ResourceName) (map[string]bool, []nodeFreeability, bool) {
	inUnit := make(map[string]bool, len(unit.Nodes))
	var diagnostics []nodeFreeability
	for _, nodeName := range unit.Nodes {
		result := api.EvaluateNodeFreeability(nodesByName[nodeName], api.NodeFreeabilityState{
			Drained: drained[nodeName],
			Filled:  filled[nodeName],
		}, movable, targetResource)
		if !result.Freeable {
			diagnostics = append(diagnostics, nodeFreeability{nodeName: nodeName, result: result})
			continue
		}
		inUnit[nodeName] = true
	}
	return inUnit, diagnostics, len(diagnostics) == 0 && len(inUnit) > 0
}

// logNonFreeableNodes writes one V(4) record for every node that prevents a
// freeable unit from being drained. The diagnostics are produced by the same
// EvaluateNodeFreeability call that rejected the unit.
func logNonFreeableNodes(unitKey string, diagnostics []nodeFreeability, targetResource v1.ResourceName) {
	for _, diagnostic := range diagnostics {
		immovablePods := make([]string, 0, len(diagnostic.result.ImmovableTasks))
		for _, task := range diagnostic.result.ImmovableTasks {
			pod := task.Namespace + "/" + task.Name
			if task.Job != "" {
				pod += " (podGroup=" + string(task.Job) + ")"
			} else {
				pod += " (podGroup=<none>)"
			}
			immovablePods = append(immovablePods, pod)
		}
		sort.Strings(immovablePods)
		klog.V(4).InfoS("repack drain: node cannot be freed", "unit", unitKey, "node", diagnostic.nodeName,
			"targetResource", targetResource, "reason", diagnostic.result.Reason, "immovablePods", immovablePods)
	}
}

// occupiesAccelerator reports whether the node uses any of the target accelerator
// resource — the SAME criterion MeasureResourceFragmentation uses to count a node as occupied
// (B), so "the drain treats this node as empty" ⟺ "the fragmentation metric does
// not count it". A node with only CPU/memory pods is empty for defrag: its
// accelerator capacity is idle, so filling it just lights up a fresh accelerator
// node (net-zero), and freeing it is not a consolidation. Everything (empty/full/
// fragmentation) is judged by the resource being defragmented (goals[0].resource).
func occupiesAccelerator(n *schedapi.NodeInfo, targetResource v1.ResourceName) bool {
	return n != nil && n.Used != nil && api.Scalar(n.Used, targetResource) > 0
}

// unitKey is a deterministic identifier for a unit (sorted member node names).
// taskNames and nodeNames render slices as plain name lists for readable logs.
func taskNames(tasks []*schedapi.TaskInfo) []string {
	names := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t != nil {
			names = append(names, t.Name)
		}
	}
	return names
}

func nodeNames(nodes []*schedapi.NodeInfo) []string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			names = append(names, n.Name)
		}
	}
	return names
}

func unitKey(u api.FreeableUnit) string {
	ns := append([]string(nil), u.Nodes...)
	sort.Strings(ns)
	return strings.Join(ns, ",")
}
