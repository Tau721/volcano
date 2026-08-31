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

// Package framework is the repack engine's plugin/action framework — the
// analogue of pkg/scheduler/framework. It defines the engine Session (a plugin-
// populated, action-consumed view of one repack pass), the Plugin and Action
// extension contracts and their registries, plus planning extension points. It
// depends only on pkg/repackengine/api (pure model) and
// pkg/scheduler/api (NodeInfo/TaskInfo) — never on the scheduler framework; the
// live-Session coupling lives in pkg/repackengine/adapter.
package framework

import (
	"context"

	"k8s.io/apimachinery/pkg/util/sets"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// Snapshot is the read-only cluster view a repack pass plans against. Abstracting
// it (instead of the scheduler cache/Session) lets the standalone engine build a
// view from informers; a live scheduler Session is one implementation (see
// pkg/repackengine/adapter), tests use a fake. Implementations must be safe for
// concurrent reads during one pass.
type Snapshot interface {
	// Nodes returns ALL candidate nodes — the receiver universe. scope.nodes
	// gates drain TARGETS (via NodeInScope), not the receiver set: a node the
	// user excludes from draining can still receive relocated pods.
	Nodes() []*schedapi.NodeInfo
	// NodeInScope reports whether a node may be a drain TARGET (scope.nodes
	// include − exclude). Out-of-scope nodes stay as (preferred) receivers.
	// True when no node scope is set.
	NodeInScope(node *schedapi.NodeInfo) bool
	// PodGroupView returns Gang availability facts for a PodGroup (zero if absent).
	PodGroupView(schedapi.JobID) api.PodGroupView
	// FeasibleRelocation simulates evicting victims and greedily relocating them
	// onto receivers using the scheduler's full filter stack, returning the per-pod
	// placements and whether every victim fit. committed are the moves already
	// decided this pass (their pods count as present on their receivers). The
	// implementation must be non-destructive (no shared-state mutation) and stop
	// promptly when ctx is cancelled.
	FeasibleRelocation(ctx context.Context, committed []*api.Move, victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo) ([]*api.Move, bool)
	// HyperNodesSetByTier returns the HyperNode tier topology: tier -> set of
	// HyperNode names at that tier, from down to top. The map and each inner set
	// are deep copies; callers may freely read, index and iterate them without
	// aliasing the snapshot's internal state. HyperNode-aware plugins consume this
	// to build their own node->HyperNode index at the tier they plan against —
	// the node->HyperNode relationship itself lives in RealNodesSet, and building
	// it from the tier maps (instead of a dedicated node->HyperNode accessor) keeps
	// the adapter a thin copy of the scheduler Session fields.
	HyperNodesSetByTier() map[int]sets.Set[string]
	// RealNodesSet returns the membership of every HyperNode: HyperNode name ->
	// set of real node names under it (direct and inherited members). The map and
	// each inner set are deep copies. HyperNode-aware plugins use it together with
	// HyperNodesSetByTier to classify nodes per HyperNode at the target tier.
	RealNodesSet() map[string]sets.Set[string]
	// HyperNodeTierNameMap returns the tier-name index: tierName -> tier number.
	// RepackRun may name the target tier by HyperNode.Spec.TierName instead of the
	// numeric Spec.Tier; plugins resolve that name through this map. The returned
	// map is a copy (the values are immutable integers).
	HyperNodeTierNameMap() map[string]int
}
