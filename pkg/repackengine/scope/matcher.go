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

// Package scope compiles RepackRun node and workload selectors into the
// predicates used by planning. It is independent of the plugin framework.
package scope

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

// GangLookup returns the information needed to match a gang against a
// RepackScope: its PodGroup name in "namespace/name" form (for name selectors)
// and its labels (for label selectors).
// "Everything is a PodGroup": the labels are the PodGroup's own labels. The
// pg-controller inherits stable pod-template labels onto auto-created PodGroups;
// workloads that create their own PodGroup must expose their labels there. This
// keeps scope matching independent of workload-specific pod or owner lookups.
// found=false means the JobID is unknown to the snapshot (treated as out of scope).
// The live-Session implementation is adapter.SessionGangScopeLookup; tests use a func.
type GangLookup func(schedapi.JobID) (podGroupName string, gangLabels labels.Labels, found bool)

type compiledTerm struct {
	includeAll bool
	incSel     labels.Selector
	incNames   map[string]bool
	excSel     labels.Selector
	excNames   map[string]bool
}

// matches applies "included AND NOT excluded", include/exclude each (selector ∪
// names); empty include = all, empty exclude = none.
func (c *compiledTerm) matches(name string, labelSet labels.Labels) bool {
	included := c.includeAll ||
		(c.incNames != nil && c.incNames[name]) ||
		(c.incSel != nil && c.incSel.Matches(labelSet))
	if !included {
		return false
	}
	excluded := (c.excNames != nil && c.excNames[name]) ||
		(c.excSel != nil && c.excSel.Matches(labelSet))
	return !excluded
}

func compileSelector(sel *repackv1alpha1.RepackSelector) (selector labels.Selector, names map[string]bool, empty bool, err error) {
	if sel == nil || (sel.Selector == nil && len(sel.Names) == 0) {
		return nil, nil, true, nil
	}
	if sel.Selector != nil {
		selector, err = metav1.LabelSelectorAsSelector(sel.Selector)
		if err != nil {
			return nil, nil, false, err
		}
	}
	if len(sel.Names) > 0 {
		names = make(map[string]bool, len(sel.Names))
		for _, n := range sel.Names {
			names[n] = true
		}
	}
	return selector, names, false, nil
}

func compileTerm(t *repackv1alpha1.RepackSelectorTerm) (*compiledTerm, error) {
	c := &compiledTerm{includeAll: true}
	if t == nil {
		return c, nil
	}
	incSel, incNames, incEmpty, err := compileSelector(t.Include)
	if err != nil {
		return nil, fmt.Errorf("include: %w", err)
	}
	excSel, excNames, _, err := compileSelector(t.Exclude)
	if err != nil {
		return nil, fmt.Errorf("exclude: %w", err)
	}
	c.includeAll, c.incSel, c.incNames = incEmpty, incSel, incNames
	c.excSel, c.excNames = excSel, excNames
	return c, nil
}

// Matcher decides, for one repack pass, which gangs may move and which nodes
// may be drain targets. It is the compiled form of a RepackScope: NewMatcher
// parses the selectors once, and the two methods are called during planning.
// Passing this one named value (instead of two loose predicate funcs) keeps the
// scope logic readable and discoverable — a reader can jump straight to InScope /
// NodeInScope to see exactly what "in scope" means.
type Matcher struct {
	lookupGang      GangLookup
	podGroupMatcher *compiledTerm // scope.podGroups (include − exclude)
	nodeMatcher     *compiledTerm // scope.nodes (include − exclude)
}

// InScope reports whether the gang (PodGroup) with this JobID is in scope: it is
// looked up to its "namespace/name" and labels, then matched against
// scope.podGroups. An unknown gang is out of scope.
func (m *Matcher) InScope(id schedapi.JobID) bool {
	podGroupName, gangLabels, found := m.lookupGang(id)
	if !found {
		return false
	}
	if gangLabels == nil {
		gangLabels = labels.Set{}
	}
	return m.podGroupMatcher.matches(podGroupName, gangLabels)
}

// NodeInScope reports whether a node may be a drain target, matching its name and
// labels against scope.nodes. A nil node is never in scope.
func (m *Matcher) NodeInScope(n *schedapi.NodeInfo) bool {
	if n == nil {
		return false
	}
	return m.nodeMatcher.matches(n.Name, nodeLabels(n))
}

// NewMatcher compiles a RepackScope into a Matcher. Selectors are parsed
// once; a malformed selector is a compile-time error. A nil scope/axis means
// "whole domain" for that axis; exclude wins.
func NewMatcher(scope *repackv1alpha1.RepackScope, lookupGang GangLookup) (*Matcher, error) {
	var podGroupTerm, nodeTerm *repackv1alpha1.RepackSelectorTerm
	if scope != nil {
		podGroupTerm, nodeTerm = scope.PodGroups, scope.Nodes
	}
	podGroupMatcher, err := compileTerm(podGroupTerm)
	if err != nil {
		return nil, fmt.Errorf("scope.podGroups: %w", err)
	}
	nodeMatcher, err := compileTerm(nodeTerm)
	if err != nil {
		return nil, fmt.Errorf("scope.nodes: %w", err)
	}
	return &Matcher{
		lookupGang:      lookupGang,
		podGroupMatcher: podGroupMatcher,
		nodeMatcher:     nodeMatcher,
	}, nil
}

func nodeLabels(n *schedapi.NodeInfo) labels.Labels {
	if n.Node != nil && n.Node.Labels != nil {
		return labels.Set(n.Node.Labels)
	}
	return labels.Set{}
}
