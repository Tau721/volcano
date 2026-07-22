/*
Copyright 2025 The Volcano Authors.

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

package allocate

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	"volcano.sh/apis/pkg/apis/scheduling"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/util"
)

func TestCaptureRestoreHyperNodePlacement(t *testing.T) {
	job := &api.JobInfo{
		UID:                "job-1",
		AllocatedHyperNode: "",
		SubJobs: map[api.SubJobID]*api.SubJobInfo{
			"sub-1": {
				UID:                "sub-1",
				AllocatedHyperNode: "",
			},
		},
	}
	subJob := job.SubJobs["sub-1"]
	placement := captureHyperNodePlacement(job, subJob)

	job.AllocatedHyperNode = "sn-a"
	subJob.AllocatedHyperNode = "sn-a"
	restoreHyperNodePlacement(job, subJob, placement)

	if job.AllocatedHyperNode != "" {
		t.Fatalf("job AllocatedHyperNode = %q, want empty", job.AllocatedHyperNode)
	}
	if subJob.AllocatedHyperNode != "" {
		t.Fatalf("subJob AllocatedHyperNode = %q, want empty", subJob.AllocatedHyperNode)
	}
}

func TestUpdateJobAllocatedHyperNodeFromSubJob(t *testing.T) {
	hn := api.HyperNodeInfoMap{
		"root": newPlacementTestHyperNode("root", 3, ""),
		"sn-a": newPlacementTestHyperNode("sn-a", 2, "root"),
		"sn-b": newPlacementTestHyperNode("sn-b", 2, "root"),
	}
	ssn := &framework.Session{
		HyperNodes:                hn,
		HyperNodesReadyToSchedule: true,
		DirtyJobs:                 sets.New[api.JobID](),
	}
	job := &api.JobInfo{UID: "job-1", AllocatedHyperNode: "sn-a"}
	subJob := &api.SubJobInfo{UID: "sub-1"}

	updateJobAllocatedHyperNodeFromSubJob(ssn, job, subJob, "sn-b")
	if job.AllocatedHyperNode != "root" {
		t.Fatalf("job AllocatedHyperNode = %q, want root", job.AllocatedHyperNode)
	}
}

func TestPreferJobSoftTopologyCandidates(t *testing.T) {
	hyperNodes := api.HyperNodeInfoMap{
		"root-a":   newPlacementTestHyperNode("root-a", 3, ""),
		"a-tier2":  newPlacementTestHyperNode("a-tier2", 2, "root-a"),
		"a-tier1":  newPlacementTestHyperNode("a-tier1", 1, "a-tier2"),
		"a-tier1b": newPlacementTestHyperNode("a-tier1b", 1, "a-tier2"),
		"root-b":   newPlacementTestHyperNode("root-b", 3, ""),
		"b-tier2":  newPlacementTestHyperNode("b-tier2", 2, "root-b"),
		"b-tier1":  newPlacementTestHyperNode("b-tier1", 1, "b-tier2"),
	}
	alloc := &Action{session: &framework.Session{HyperNodes: hyperNodes}}
	jobTier := 2

	newJob := func(mode scheduling.NetworkTopologyMode, tier *int, withPeer bool, jobAnchor, currentAnchor string) (*api.JobInfo, *api.SubJobInfo) {
		if jobAnchor == "" {
			jobAnchor = "a-tier1"
		}
		current := &api.SubJobInfo{UID: "current", AllocatedHyperNode: currentAnchor}
		subJobs := map[api.SubJobID]*api.SubJobInfo{"current": current}
		if withPeer {
			subJobs["peer"] = &api.SubJobInfo{UID: "peer", AllocatedHyperNode: "a-tier1"}
		}
		return &api.JobInfo{
			AllocatedHyperNode: jobAnchor,
			PodGroup: &api.PodGroup{PodGroup: scheduling.PodGroup{Spec: scheduling.PodGroupSpec{
				NetworkTopology: &scheduling.NetworkTopologySpec{Mode: mode, HighestTierAllowed: tier},
				SubGroupPolicy:  []scheduling.SubGroupPolicySpec{{Name: "partition"}},
			}}},
			SubJobs: subJobs,
		}, current
	}

	tests := []struct {
		name          string
		mode          scheduling.NetworkTopologyMode
		tier          *int
		withPeer      bool
		jobAnchor     string
		currentAnchor string
		input         []string
		want          []string
	}{
		{
			name:     "prefers candidates under established job tier",
			mode:     scheduling.SoftNetworkTopologyMode,
			tier:     &jobTier,
			withPeer: true,
			input:    []string{"a-tier1b", "b-tier1"},
			want:     []string{"a-tier1b"},
		},
		{
			name:     "falls back when only remote candidate is feasible",
			mode:     scheduling.SoftNetworkTopologyMode,
			tier:     &jobTier,
			withPeer: true,
			input:    []string{"b-tier1"},
			want:     []string{"b-tier1"},
		},
		{
			name:     "does not change hard topology candidates",
			mode:     scheduling.HardNetworkTopologyMode,
			tier:     &jobTier,
			withPeer: true,
			input:    []string{"a-tier1b", "b-tier1"},
			want:     []string{"a-tier1b", "b-tier1"},
		},
		{
			name:     "does not constrain first subjob",
			mode:     scheduling.SoftNetworkTopologyMode,
			tier:     &jobTier,
			withPeer: false,
			input:    []string{"a-tier1b", "b-tier1"},
			want:     []string{"a-tier1b", "b-tier1"},
		},
		{
			name:     "does not constrain without a job tier",
			mode:     scheduling.SoftNetworkTopologyMode,
			withPeer: true,
			input:    []string{"a-tier1b", "b-tier1"},
			want:     []string{"a-tier1b", "b-tier1"},
		},
		{
			name:          "keeps preference for partially allocated current subjob",
			mode:          scheduling.SoftNetworkTopologyMode,
			tier:          &jobTier,
			withPeer:      true,
			jobAnchor:     "a-tier2",
			currentAnchor: "a-tier1b",
			input:         []string{"a-tier1b", "b-tier1"},
			want:          []string{"a-tier1b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, current := newJob(tt.mode, tt.tier, tt.withPeer, tt.jobAnchor, tt.currentAnchor)
			candidates := make(map[string]*framework.Statement, len(tt.input))
			for _, hyperNode := range tt.input {
				candidates[hyperNode] = nil
			}

			got := alloc.preferJobSoftTopologyCandidates(job, current, candidates)
			if len(got) != len(tt.want) {
				t.Fatalf("candidate count = %d, want %d: %#v", len(got), len(tt.want), got)
			}
			for _, hyperNode := range tt.want {
				if _, found := got[hyperNode]; !found {
					t.Fatalf("candidate %q missing from %#v", hyperNode, got)
				}
			}
		})
	}
}

func TestFilterGradientsByMinResourceTierStats(t *testing.T) {
	nodeInfo := api.NewNodeInfo(util.BuildNode(
		"node-a",
		api.BuildResourceList("4", "8Gi", []api.ScalarResource{{Name: "pods", Value: "110"}}...),
		nil,
	))

	ssn := &framework.Session{
		Nodes: map[string]*api.NodeInfo{"node-a": nodeInfo},
		RealNodesSet: map[string]sets.Set[string]{
			"sn-a": sets.New("node-a"),
			"sn-b": sets.New("node-a"),
		},
		HyperNodes: api.HyperNodeInfoMap{
			"sn-a": newPlacementTestHyperNode("sn-a", 2, "root"),
			"sn-b": newPlacementTestHyperNode("sn-b", 2, "root"),
		},
		HyperNodeTierNameMap: api.HyperNodeTierNameMap{"supernode": 2},
	}

	gradients := [][]*api.HyperNodeInfo{
		{ssn.HyperNodes["sn-a"], ssn.HyperNodes["sn-b"]},
	}
	minResource := &api.Resource{MilliCPU: 20000, Memory: 40 * 1024 * 1024 * 1024}

	filtered, stats := FilterGradientsByMinResource(ssn, gradients, minResource, "")
	if len(filtered) != 0 {
		t.Fatalf("expected empty filtered gradients, got %#v", filtered)
	}
	if stats == nil {
		t.Fatal("expected resource filter stats")
	}
	if stats.ExcludedByTier[2] != 2 {
		t.Fatalf("expected 2 resource exclusions at supernode tier, got %#v", stats.ExcludedByTier)
	}
	if stats.FinalByTier[2] != 0 {
		t.Fatalf("expected 0 final hyperNodes, got %#v", stats.FinalByTier)
	}
}

func newPlacementTestHyperNode(name string, tier int, parent string) *api.HyperNodeInfo {
	hn := &topologyv1alpha1.HyperNode{}
	hn.Name = name
	hn.Spec.Tier = tier
	hni := api.NewHyperNodeInfo(hn)
	hni.Parent = parent
	return hni
}
