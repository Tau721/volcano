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

package adapter

import (
	"k8s.io/apimachinery/pkg/labels"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"

	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
)

// SessionGangScopeLookup adapts a live Session into a scope.GangLookup:
// it reads each gang's namespace/name and labels off the JobInfo's PodGroup.
// Production source for scope.NewMatcher; tests use a plain func.
func SessionGangScopeLookup(ssn *schedframework.Session) enginescope.GangLookup {
	// Tasks on nodes may reference Job IDs omitted from ssn.Jobs: the scheduler
	// snapshot skips jobs whose PodGroup is not linked yet or whose queue is not
	// in the cache, even though the pods are already running. Repack still needs
	// to treat those gangs as scope-visible when they host accelerator workloads.
	onCluster := jobIDsOnNodes(ssn)
	return func(id schedapi.JobID) (string, labels.Labels, bool) {
		if id == "" {
			return "", nil, false
		}
		if ji, ok := ssn.Jobs[id]; ok && ji != nil {
			if ji.PodGroup != nil {
				podGroupName := ji.PodGroup.Namespace + "/" + ji.PodGroup.Name
				return podGroupName, labels.Set(ji.PodGroup.Labels), true
			}
			// Job is indexed but its PodGroup has not arrived yet — still treat the
			// gang as in default scope using the JobID (namespace/name).
			return string(id), labels.Set{}, true
		}
		if onCluster[id] {
			return string(id), labels.Set{}, true
		}
		return "", nil, false
	}
}

func jobIDsOnNodes(ssn *schedframework.Session) map[schedapi.JobID]bool {
	out := make(map[schedapi.JobID]bool)
	for _, n := range ssn.Nodes {
		if n == nil {
			continue
		}
		for _, t := range n.Tasks {
			if t != nil && t.Job != "" {
				out[t.Job] = true
			}
		}
	}
	return out
}
