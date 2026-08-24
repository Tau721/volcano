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
	LimitPodGroups            bool                                        // distinguishes omitted from explicit zero
	LimitResource             bool                                        // distinguishes omitted from explicit zero
	Free                      func(*schedapi.NodeInfo) *schedapi.Resource // nil = FutureIdle
}

// Session is one repack pass: a snapshot plus the callbacks plugins register,
// consumed by actions and their planners. Mirrors framework.Session in the scheduler.
type Session struct {
	configuration SessionConfig
	plugins       []Plugin // opened plugins, for OnSessionClose
	capabilities  map[PluginCapability]bool

	movableFns    []MovableFn
	domainFns     []DomainFn
	scoreTerms    []scoreTerm
	constraintFns []PlanConstraintFn

	candidateFilterFns []namedCandidateFilter
	receiverPoolFns    []ReceiverPoolFn
	victimOrderFns     []namedVictimOrder
	receiverRankFns    []namedReceiverRank

	// results filled by the action, read by the Engine runtime
	plan   *api.RepackPlan
	report api.Report
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
	}
	return ssn
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

// Free returns the node free-capacity basis (default NodeInfo.FutureIdle).
func (s *Session) Free() func(*schedapi.NodeInfo) *schedapi.Resource {
	if s.configuration.Free != nil {
		return s.configuration.Free
	}
	return func(n *schedapi.NodeInfo) *schedapi.Resource { return n.FutureIdle() }
}

// ---- aggregate consumption (called by actions/planners) ----

// Nodes returns the snapshot's candidate nodes.
func (s *Session) Nodes() []*schedapi.NodeInfo { return s.configuration.Snapshot.Nodes() }

// FeasibleRelocation delegates to the snapshot's scheduler-faithful relocation
// feasibility check: simulate evicting victims and greedily place them onto receivers with the
// full scheduler filter stack. See Snapshot.FeasibleRelocation.
func (s *Session) FeasibleRelocation(committed []*api.Move, victims []*schedapi.TaskInfo, receivers []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return s.configuration.Snapshot.FeasibleRelocation(s.configuration.Context, committed, victims, receivers)
}

// Movable returns an api.Movable that is the AND of all registered MovableFns
// (no plugins → everything movable).
func (s *Session) Movable() api.Movable {
	fns := s.movableFns
	return func(t *schedapi.TaskInfo) bool {
		if t == nil {
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
// hypernode domains enabled this carries both levels; the planner ranks them in one
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
