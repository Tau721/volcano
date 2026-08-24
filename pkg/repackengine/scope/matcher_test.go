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

package scope

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
)

type gangFixture struct {
	podGroupName string
	gangLabels   map[string]string
}

func gangScopeLookupFrom(m map[schedapi.JobID]gangFixture) GangLookup {
	return func(id schedapi.JobID) (string, labels.Labels, bool) {
		g, ok := m[id]
		if !ok {
			return "", nil, false
		}
		return g.podGroupName, labels.Set(g.gangLabels), true
	}
}

func sel(ml map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: ml}
}

func node(name string, nodeLabels map[string]string) *schedapi.NodeInfo {
	return &schedapi.NodeInfo{Name: name, Node: &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels}}}
}

// nil scope = whole domain: every known gang and every node is in scope.
func TestMatcher_NilIsAll(t *testing.T) {
	lookup := gangScopeLookupFrom(map[schedapi.JobID]gangFixture{"ns/a": {"ns/a", nil}})
	matcher, err := NewMatcher(nil, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.InScope("ns/a") {
		t.Error("nil scope should admit any known gang")
	}
	if matcher.InScope("ns/unknown") {
		t.Error("unknown gang must be out of scope")
	}
	if !matcher.NodeInScope(node("n0", nil)) {
		t.Error("nil scope should admit any node")
	}
}

// Node include selector restricts to matching nodes; exclude-by-name wins.
func TestMatcher_NodeSelectorAndExclude(t *testing.T) {
	scope := &repackv1alpha1.RepackScope{
		Nodes: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Selector: sel(map[string]string{"pool": "a100"})},
			Exclude: &repackv1alpha1.RepackSelector{Names: []string{"n-guard"}},
		},
	}
	matcher, err := NewMatcher(scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.NodeInScope(node("n1", map[string]string{"pool": "a100"})) {
		t.Error("a100 node should be in scope")
	}
	if matcher.NodeInScope(node("n2", map[string]string{"pool": "v100"})) {
		t.Error("non-a100 node should be out of scope")
	}
	if matcher.NodeInScope(node("n-guard", map[string]string{"pool": "a100"})) {
		t.Error("excluded-by-name node must be out even if it matches include")
	}
}

// PodGroup include by names restricts the gang set.
func TestMatcher_PodGroupNames(t *testing.T) {
	lookup := gangScopeLookupFrom(map[schedapi.JobID]gangFixture{
		"ns/keep": {"ns/keep", map[string]string{"team": "ml"}},
		"ns/drop": {"ns/drop", map[string]string{"team": "ml"}},
	})
	scope := &repackv1alpha1.RepackScope{
		PodGroups: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Names: []string{"ns/keep"}},
		},
	}
	matcher, err := NewMatcher(scope, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.InScope("ns/keep") {
		t.Error("named gang should be in scope")
	}
	if matcher.InScope("ns/drop") {
		t.Error("unnamed gang should be out of scope")
	}
}

// PodGroup include-by-selector matches PG labels; exclude-by-label wins.
// This is the "everything is a PodGroup" path: for Deployment/StatefulSet/custom
// workloads the pg-controller inherits pod template labels onto the PodGroup
// (§5.2.1), so a PG label selector addresses them uniformly.
func TestMatcher_PodGroupSelector(t *testing.T) {
	lookup := gangScopeLookupFrom(map[schedapi.JobID]gangFixture{
		"ns/dep-x": {"ns/dep-x", map[string]string{"app": "recommender"}},
		"ns/dep-y": {"ns/dep-y", map[string]string{"app": "ranking"}},
		"ns/prot":  {"ns/prot", map[string]string{"app": "recommender", "repack.volcano.sh/protected": "true"}},
	})
	scope := &repackv1alpha1.RepackScope{
		PodGroups: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Selector: sel(map[string]string{"app": "recommender"})},
			Exclude: &repackv1alpha1.RepackSelector{Selector: sel(map[string]string{"repack.volcano.sh/protected": "true"})},
		},
	}
	matcher, err := NewMatcher(scope, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.InScope("ns/dep-x") {
		t.Error("gang matching include selector should be in scope")
	}
	if matcher.InScope("ns/dep-y") {
		t.Error("gang not matching include selector should be out of scope")
	}
	if matcher.InScope("ns/prot") {
		t.Error("gang matching exclude selector must be out even if it matches include")
	}
}

// A malformed selector is a resolve-time error.
func TestMatcher_BadSelectorErrors(t *testing.T) {
	scope := &repackv1alpha1.RepackScope{
		Nodes: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{
				Selector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: "BadOperator"}},
				},
			},
		},
	}
	if _, err := NewMatcher(scope, nil); err == nil {
		t.Error("malformed label selector should be a resolve-time error")
	}
}
