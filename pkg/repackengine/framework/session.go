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
	"context"
	"sort"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
)

// Core callback contracts plugins register into a Session.
type (
	// MovableFn reports whether a task may be moved. Aggregated with AND — any
	// plugin may veto a move (gang breach, PDB, frozen scope).
	MovableFn func(task *schedapi.TaskInfo) bool
	// DomainFn enumerates the freeable units a domain contributes (node units,
	// hypernode units, ...). Aggregated by union; the planner optimizes their combined
	// weighted benefit.
	DomainFn func(snapshot Snapshot) []api.FreeableUnit
)

// SessionConfig is the per-run input the Engine supplies to OpenSession.
type SessionConfig struct {
	Context       context.Context
	Snapshot      Snapshot
	Run           *repackv1alpha1.RepackRun
	Scope         *enginescope.Matcher
	Resource      v1.ResourceName
	Mode          repackv1alpha1.RepackMode
	MinNodesFreed int
	// MinFragImprovementPercent is the run's benefit gate from
	// spec.goals[0].minFragImprovementPercent (percentage points, 0-100): a plan
	// whose fragmentation improvement is below it is not worth it. 0 = no gate.
	MinFragImprovementPercent int
	MaxPodGroups              int
	MaxResource               int64
	LimitPodGroups            bool // distinguishes omitted from explicit zero
	LimitResource             bool // distinguishes omitted from explicit zero
	// PinnedTasks is the set of scheduler task UIDs whose workload template pins
	// spec.nodeName. A pinned pod can never be relocated — the controller
	// recreates an evicted replacement from the same template — so Movable()
	// excludes these tasks. Nil means no pinned tasks.
	PinnedTasks sets.Set[schedapi.TaskID]
}

// Session is one repack pass: a snapshot plus the callbacks plugins register,
// consumed by actions and their planners. Mirrors framework.Session in the scheduler.
type Session struct {
	configuration SessionConfig
	plugins       []Plugin // opened plugins, for OnSessionClose
	capabilities  map[PluginCapability]bool

	movableFns     []MovableFn
	domainFns      []DomainFn
	planScoreTerms []planScoreTerm
	constraintFns  []PlanConstraintFn
	pinnedTasks    sets.Set[schedapi.TaskID]

	candidateFilterFns    []namedCandidateFilter
	receiverPoolFns       []ReceiverPoolFn
	victimOrderFns        []namedVictimOrder
	receiverPreferenceFns []namedReceiverPreference

	// results filled by the action, read by the Engine runtime
	plan                *api.RepackPlan
	report              api.Report
	constraintRejection string // terminal reason of the first failing plan constraint
}

// OpenSession builds a Session and runs each configured plugin's OnSessionOpen
// in canonical name order. Plugin configuration is a set: reordering the YAML
// list cannot change callback composition or planning behavior. Unknown plugin
// names are ignored; the engine validates them before opening a production
// session.
func OpenSession(configuration SessionConfig, pluginOptions []PluginOption) *Session {
	if configuration.Context == nil {
		configuration.Context = context.Background()
	}
	ssn := &Session{
		configuration: configuration,
		capabilities:  make(map[PluginCapability]bool),
		pinnedTasks:   configuration.PinnedTasks,
	}
	ssn.registerBuiltinConstraints()
	canonicalOptions := append([]PluginOption(nil), pluginOptions...)
	sort.SliceStable(canonicalOptions, func(i, j int) bool {
		return canonicalOptions[i].Name < canonicalOptions[j].Name
	})
	for _, option := range canonicalOptions {
		p, ok := GetPlugin(option.Name, option.Arguments)
		if !ok {
			continue
		}
		p.OnSessionOpen(ssn)
		ssn.plugins = append(ssn.plugins, p)
		klog.V(4).InfoS("repack: session opened plugin", "run", runNameOf(configuration), "plugin", option.Name)
	}
	return ssn
}

// runNameOf returns the RepackRun name for log correlation, or "" for
// sessions opened outside a run (engine bootstrap, unit tests).
func runNameOf(configuration SessionConfig) string {
	if configuration.Run != nil {
		return configuration.Run.Name
	}
	return ""
}

// CloseSession runs OnSessionClose on the plugins opened by OpenSession.
func CloseSession(ssn *Session) {
	for _, p := range ssn.plugins {
		p.OnSessionClose(ssn)
	}
	ssn.plugins = nil
}

// ---- registration (called by plugins in OnSessionOpen) ----

func (s *Session) AddMovableFn(fn MovableFn) {
	if fn != nil {
		s.movableFns = append(s.movableFns, fn)
	}
}
func (s *Session) AddDomainFn(fn DomainFn) {
	if fn != nil {
		s.domainFns = append(s.domainFns, fn)
		if s.capabilities == nil {
			s.capabilities = make(map[PluginCapability]bool)
		}
		s.capabilities[CapabilityDomain] = true
	}
}

// ProvidesCapability reports capabilities backed by callbacks actually
// registered in this Session. It complements the static registration metadata:
// metadata validates composition before opening a Session, while this runtime
// view prevents a plugin that only declares a capability from silently running
// an Action without the callback that implements it.
func (s *Session) ProvidesCapability(capability PluginCapability) bool {
	return s != nil && s.capabilities[capability]
}

// ---- config accessors ----

func (s *Session) Snapshot() Snapshot              { return s.configuration.Snapshot }
func (s *Session) Context() context.Context        { return s.configuration.Context }
func (s *Session) Run() *repackv1alpha1.RepackRun  { return s.configuration.Run }
func (s *Session) Scope() *enginescope.Matcher     { return s.configuration.Scope }
func (s *Session) Resource() v1.ResourceName       { return s.configuration.Resource }
func (s *Session) Mode() repackv1alpha1.RepackMode { return s.configuration.Mode }
func (s *Session) MinNodesFreed() int              { return s.configuration.MinNodesFreed }
func (s *Session) MinFragImprovementPercent() int  { return s.configuration.MinFragImprovementPercent }
func (s *Session) MaxPodGroups() int               { return s.configuration.MaxPodGroups }
func (s *Session) MaxResource() int64              { return s.configuration.MaxResource }
func (s *Session) LimitPodGroups() bool            { return s.configuration.LimitPodGroups }
func (s *Session) LimitResource() bool             { return s.configuration.LimitResource }

// ---- aggregate consumption (called by actions/planners) ----

// Nodes returns the snapshot's candidate nodes.
func (s *Session) Nodes() []*schedapi.NodeInfo { return s.configuration.Snapshot.Nodes() }

// Movable returns an api.Movable that first enforces Repack's non-optional
// PodGroup ownership boundary, then applies the AND of all registered policy
// callbacks. With no callbacks, every valid PodGroup task is movable.
//
// A pod pinned via spec.nodeName is immovable: the scheduler honors the pin
// outside its filter stack, so Repack can never relocate it to another node.
// Treating it as immovable excludes it from VictimsOf and marks its node
// unfreeable, matching what Execute can actually achieve.
func (s *Session) Movable() api.Movable {
	fns := s.movableFns
	return func(t *schedapi.TaskInfo) bool {
		if _, _, valid := api.PodGroupIdentity(t); !valid {
			return false
		}
		// A live bound pod always carries spec.nodeName, so only the workload
		// template distinguishes a true pin (the Engine pre-resolves those task
		// UIDs into PinnedTasks). Treat a pinned pod as immovable, or it would be
		// drained and its replacement glued back onto the same node.
		if s.pinnedTasks != nil && s.pinnedTasks.Has(t.UID) {
			return false
		}
		for _, fn := range fns {
			if !fn(t) {
				return false
			}
		}
		return true
	}
}

// FreeableUnits is the union of every domain plugin's units. With both node and
// hypernode domains enabled this carries both levels; the planner orders them in one
// active candidate set.
func (s *Session) FreeableUnits() []api.FreeableUnit {
	var out []api.FreeableUnit
	for _, fn := range s.domainFns {
		out = append(out, fn(s.configuration.Snapshot)...)
	}
	return out
}

// PlanContext builds the scoring context from the snapshot and target resource.
func (s *Session) PlanContext() *api.PlanContext {
	return &api.PlanContext{TargetResource: s.configuration.Resource, PodGroupViews: s.configuration.Snapshot}
}

// ---- result (set by the action, read by the Engine runtime) ----

func (s *Session) SetPlan(p *api.RepackPlan) { s.plan = p }
func (s *Session) Plan() *api.RepackPlan     { return s.plan }
func (s *Session) SetReport(r api.Report)    { s.report = r }
func (s *Session) Report() api.Report        { return s.report }
