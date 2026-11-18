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

package numaaware

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/cpuset"

	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/cache"
	"volcano.sh/volcano/pkg/scheduler/util"
)

// Only decisions for the node the task is bound to are carried into the bind context.
func TestNuma_SetupBindContextExtension(t *testing.T) {
	tests := []struct {
		name         string
		taskNode     string
		decisionNode string
		wantSets     api.ResNumaSets
	}{
		{
			name:         "decision on the target node is carried in the bind context",
			taskNode:     "n2",
			decisionNode: "n2",
			wantSets:     api.ResNumaSets{string(v1.ResourceCPU): cpuset.New(0, 1)},
		},
		{
			name:     "no decision produces no extension",
			taskNode: "n2",
		},
		{
			name:         "decision for a different node produces no extension",
			taskNode:     "n2",
			decisionNode: "n1",
		},
		{
			name:         "task without a node produces no extension",
			taskNode:     "",
			decisionNode: "n2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := util.BuildPod("c1", "p1", tt.taskNode, v1.PodPending,
				api.BuildResourceList("2", "1Gi"), "pg1", map[string]string{}, map[string]string{})
			task := api.NewTaskInfo(pod)
			plugin := New(nil).(*numaPlugin)
			if tt.decisionNode != "" {
				sets := tt.wantSets
				if sets == nil {
					sets = api.ResNumaSets{string(v1.ResourceCPU): cpuset.New(2, 3)}
				}
				plugin.assignRes[task.UID] = map[string]api.ResNumaSets{tt.decisionNode: sets}
			}

			bindCtx := &cache.BindContext{
				TaskInfo:   task,
				Extensions: make(map[string]cache.BindContextExtension),
			}
			plugin.SetupBindContextExtension(nil, bindCtx)

			ext, ok := bindCtx.Extensions[PluginName]
			if tt.wantSets == nil {
				if ok {
					t.Fatalf("expected no extension, got %v", ext)
				}
				return
			}
			if !ok {
				t.Fatalf("expected an extension in the bind context")
			}
			reservation, ok := ext.(*numaReservation)
			if !ok {
				t.Fatalf("unexpected extension type %T", ext)
			}
			got := reservation.ReservedNumaSets()
			if !got[string(v1.ResourceCPU)].Equals(tt.wantSets[string(v1.ResourceCPU)]) {
				t.Errorf("reserved cpuset = %v, want %v", got, tt.wantSets)
			}
		})
	}
}

// The bind context carries a copy, so later decisions cannot mutate data handed to the cache.
func TestNuma_SetupBindContextExtension_ClonesReservation(t *testing.T) {
	pod := util.BuildPod("c1", "p1", "n2", v1.PodPending,
		api.BuildResourceList("2", "1Gi"), "pg1", map[string]string{}, map[string]string{})
	task := api.NewTaskInfo(pod)
	plugin := New(nil).(*numaPlugin)
	plugin.assignRes[task.UID] = map[string]api.ResNumaSets{
		"n2": {string(v1.ResourceCPU): cpuset.New(0, 1)},
	}

	bindCtx := &cache.BindContext{
		TaskInfo:   task,
		Extensions: make(map[string]cache.BindContextExtension),
	}
	plugin.SetupBindContextExtension(nil, bindCtx)

	plugin.assignRes[task.UID]["n2"][string(v1.ResourceCPU)] = cpuset.New(9)

	reservation := bindCtx.Extensions[PluginName].(*numaReservation)
	got := reservation.ReservedNumaSets()[string(v1.ResourceCPU)]
	if !got.Equals(cpuset.New(0, 1)) {
		t.Errorf("reservation should be a copy, got %v", got)
	}
}
