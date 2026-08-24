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

package workloadscope

import (
	"testing"

	"k8s.io/apimachinery/pkg/labels"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/framework"
	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
)

func TestPluginRegistersScopeAsMovableBoundary(t *testing.T) {
	matcher, err := enginescope.NewMatcher(&repackv1alpha1.RepackScope{
		PodGroups: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Names: []string{"ns/allowed"}},
		},
	}, func(id schedapi.JobID) (string, labels.Labels, bool) {
		return string(id), nil, id == "ns/allowed" || id == "ns/denied"
	})
	if err != nil {
		t.Fatal(err)
	}
	ssn := framework.OpenSession(framework.SessionConfig{Scope: matcher}, framework.PluginOptions(Name))
	defer framework.CloseSession(ssn)

	movable := ssn.Movable()
	if !movable(&schedapi.TaskInfo{Job: "ns/allowed"}) {
		t.Fatal("included workload should be movable")
	}
	if movable(&schedapi.TaskInfo{Job: "ns/denied"}) {
		t.Fatal("workload outside scope should be immovable")
	}
}

func TestWorkloadScopeIsOptional(t *testing.T) {
	matcher, err := enginescope.NewMatcher(&repackv1alpha1.RepackScope{
		PodGroups: &repackv1alpha1.RepackSelectorTerm{
			Include: &repackv1alpha1.RepackSelector{Names: []string{"ns/allowed"}},
		},
	}, func(id schedapi.JobID) (string, labels.Labels, bool) {
		return string(id), nil, true
	})
	if err != nil {
		t.Fatal(err)
	}
	ssn := framework.OpenSession(framework.SessionConfig{Scope: matcher}, nil)
	defer framework.CloseSession(ssn)

	if !ssn.Movable()(&schedapi.TaskInfo{Job: "ns/outside"}) {
		t.Fatal("workload scope must not apply when plugin is disabled")
	}
}
