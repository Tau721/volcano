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

package allocate

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"volcano.sh/apis/pkg/apis/scheduling"
	schedulingv1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/conf"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/plugins/gang"
	"volcano.sh/volcano/pkg/scheduler/plugins/predicates"
	"volcano.sh/volcano/pkg/scheduler/uthelper"
	"volcano.sh/volcano/pkg/scheduler/util"
)

const (
	podGroupAntiAffinityForestTestPlugin = "podgroup-antiaffinity-forest-test"
	forestTestAscendResource             = v1.ResourceName("huawei.com/ascend-1980")
)

type podGroupAntiAffinityForestTestPluginImpl struct{}

func (*podGroupAntiAffinityForestTestPluginImpl) Name() string {
	return podGroupAntiAffinityForestTestPlugin
}

func (*podGroupAntiAffinityForestTestPluginImpl) OnSessionOpen(ssn *framework.Session) {
	// Required PodGroup anti-affinity removed occupied domain A and the
	// coarser shared parent, leaving the exact same-tier roots B and C.
	ssn.AddHyperNodeGradientForJobFn(podGroupAntiAffinityForestTestPlugin, func(
		job *api.JobInfo,
		_ *api.HyperNodeInfo,
		_ api.SearchPurpose,
	) api.HyperNodeGradientResult {
		if !job.ContainsHardPodGroupAntiAffinity() {
			return api.HyperNodeGradientAbstain()
		}
		return api.HyperNodeGradientConstrain(
			[][]*api.HyperNodeInfo{{ssn.HyperNodes["b-t2"], ssn.HyperNodes["c-t2"]}},
		)
	})
	ssn.AddHyperNodeGradientForSubJobFn(podGroupAntiAffinityForestTestPlugin, func(
		subJob *api.SubJobInfo,
		root *api.HyperNodeInfo,
		_ api.SearchPurpose,
	) api.HyperNodeGradientResult {
		job := ssn.Jobs[subJob.Job]
		if job != nil && !job.ContainsHardPodGroupAntiAffinity() && subJob.IsSoftTopologyMode() {
			return api.HyperNodeGradientAbstain()
		}
		var candidates []*api.HyperNodeInfo
		switch root.Name {
		case "shared-t3":
			// The LCA traversal envelope contains excluded A. The allocate
			// candidate-forest filter must remove A after this callback.
			candidates = []*api.HyperNodeInfo{
				ssn.HyperNodes["a-t1-0"], ssn.HyperNodes["a-t1-1"],
				ssn.HyperNodes["b-t1-0"], ssn.HyperNodes["b-t1-1"],
				ssn.HyperNodes["c-t1-0"], ssn.HyperNodes["c-t1-1"],
			}
		case "b-t2":
			candidates = []*api.HyperNodeInfo{ssn.HyperNodes["b-t1-0"], ssn.HyperNodes["b-t1-1"]}
		case "c-t2":
			candidates = []*api.HyperNodeInfo{ssn.HyperNodes["c-t1-0"], ssn.HyperNodes["c-t1-1"]}
		}
		return api.HyperNodeGradientConstrain([][]*api.HyperNodeInfo{candidates})
	})
	ssn.AddHyperNodeOrderFn(podGroupAntiAffinityForestTestPlugin, func(
		_ *api.SubJobInfo,
		candidates map[string][]*api.NodeInfo,
	) (map[string]float64, error) {
		baseScores := map[string]float64{
			// A would always win if the traversal envelope leaked it.
			"a-t1-0": 10000,
			"a-t1-1": 9000,
			"b-t1-0": 400,
			"b-t1-1": 300,
			"c-t1-0": 200,
			"c-t1-1": 100,
		}
		scores := make(map[string]float64, len(candidates))
		for name := range candidates {
			scores[name] = baseScores[name]
		}
		return scores, nil
	})
}

func (*podGroupAntiAffinityForestTestPluginImpl) OnSessionClose(_ *framework.Session) {}

type candidateForestActionTestCase struct {
	name                    string
	podCount                int
	minMember               int
	jobTopologyMode         string
	jobHighestTier          int
	subJobTopologyMode      string
	skipPAA                 bool
	wantJobPlacement        string
	wantNodes               sets.Set[string]
	wantCandidateForestPath bool
}

func TestAllocatePodGroupAntiAffinityAcrossCandidateForest(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		t.Run(fmt.Sprintf("iteration-%02d", iteration), func(t *testing.T) {
			runCandidateForestActionTest(t, candidateForestActionTestCase{
				name:                    "required PodGroup anti-affinity allocates four SubJobs across B plus C",
				podCount:                4,
				minMember:               4,
				wantJobPlacement:        "shared-t3",
				wantCandidateForestPath: true,
				wantNodes: sets.New[string](
					"b-node-0", "b-node-1", "c-node-0", "c-node-1",
				),
			})
		})
	}
}

func TestCandidateForestUsesActualPlacementInsteadOfEnvelope(t *testing.T) {
	runCandidateForestActionTest(t, candidateForestActionTestCase{
		name:                    "candidate forest records B when the solution only uses B",
		podCount:                2,
		minMember:               2,
		wantJobPlacement:        "b-t2",
		wantCandidateForestPath: true,
		wantNodes:               sets.New[string]("b-node-0", "b-node-1"),
	})
}

func TestCandidateForestPreservesNativeSoftJobTopologyFallback(t *testing.T) {
	runCandidateForestActionTest(t, candidateForestActionTestCase{
		name:                    "soft Job topology does not narrow required B plus C forest",
		podCount:                4,
		minMember:               4,
		jobTopologyMode:         string(schedulingv1.SoftNetworkTopologyMode),
		jobHighestTier:          2,
		wantJobPlacement:        "shared-t3",
		wantCandidateForestPath: true,
		wantNodes: sets.New[string](
			"b-node-0", "b-node-1", "c-node-0", "c-node-1",
		),
	})
}

func TestCandidateForestSupportsNativeSoftSubGroups(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		t.Run(fmt.Sprintf("iteration-%02d", iteration), func(t *testing.T) {
			runCandidateForestActionTest(t, candidateForestActionTestCase{
				name:                    "required PodGroup anti-affinity spans B plus C with native-soft SubGroups",
				podCount:                4,
				minMember:               4,
				subJobTopologyMode:      string(schedulingv1.SoftNetworkTopologyMode),
				wantJobPlacement:        "shared-t3",
				wantCandidateForestPath: true,
				wantNodes: sets.New[string](
					"b-node-0", "b-node-1", "c-node-0", "c-node-1",
				),
			})
		})
	}
}

func TestNativeSoftSubGroupsWithoutPAAKeepCoarseFallback(t *testing.T) {
	for iteration := 0; iteration < 10; iteration++ {
		t.Run(fmt.Sprintf("iteration-%02d", iteration), func(t *testing.T) {
			runCandidateForestActionTest(t, candidateForestActionTestCase{
				name:                    "native-soft SubGroups without required PAA keep the existing coarse fallback",
				podCount:                4,
				minMember:               4,
				subJobTopologyMode:      string(schedulingv1.SoftNetworkTopologyMode),
				skipPAA:                 true,
				wantJobPlacement:        "shared-t3",
				wantCandidateForestPath: false,
			})
		})
	}
}

func runCandidateForestActionTest(t *testing.T, tc candidateForestActionTestCase) {
	t.Helper()
	tier1, tier2, tier3, hyperNodes, realNodes, nodes := buildPodGroupAntiAffinityForestTopology()

	trueValue := true
	tiers := []conf.Tier{{Plugins: []conf.PluginOption{
		{
			Name:                gang.PluginName,
			EnabledJobOrder:     &trueValue,
			EnabledJobReady:     &trueValue,
			EnabledJobPipelined: &trueValue,
			EnabledJobStarving:  &trueValue,
			EnabledSubJobReady:  &trueValue,
			EnabledSubJobOrder:  &trueValue,
		},
		{
			Name:             predicates.PluginName,
			EnabledPredicate: &trueValue,
		},
		{
			Name:                     podGroupAntiAffinityForestTestPlugin,
			EnabledHyperNodeGradient: &trueValue,
			EnabledHyperNodeOrder:    &trueValue,
		},
	}}}

	subJobTopologyMode := tc.subJobTopologyMode
	if subJobTopologyMode == "" {
		subJobTopologyMode = string(schedulingv1.HardNetworkTopologyMode)
	}
	pg := util.BuildPodGroupWithSubGroupPolicy(
		"target", "default", "", "q1", int32(tc.minMember), nil, schedulingv1.PodGroupInqueue,
		tc.jobTopologyMode, tc.jobHighestTier,
		[]schedulingv1.SubGroupPolicySpec{
			util.BuildSubGroupPolicyWithMinSubGroups(
				"worker", []string{"volcano.sh/shard-id"}, subJobTopologyMode, 1, 1, int32(tc.podCount),
			),
		},
	)
	if !tc.skipPAA {
		tierTwo := int32(2)
		pg.Spec.TopologyAffinity = &schedulingv1.TopologyAffinitySpec{
			PodGroupAntiAffinity: &schedulingv1.PodGroupAntiAffinity{
				Required: []schedulingv1.PodGroupAffinityTerm{{
					PodGroupSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"topology.volcano.sh/group": "llama-70b-prod"},
					},
					TopologyTier: &tierTwo,
				}},
			},
		}
	}

	pods := make([]*v1.Pod, 0, tc.podCount)
	for shard := 0; shard < tc.podCount; shard++ {
		pods = append(pods, util.BuildPod(
			"default",
			fmt.Sprintf("target-%d", shard),
			"",
			v1.PodPending,
			api.BuildResourceList(
				"100m", "128Mi",
				[]api.ScalarResource{{Name: string(forestTestAscendResource), Value: "6"}}...,
			),
			"target",
			map[string]string{
				"volcano.sh/task-spec": "worker",
				"volcano.sh/shard-id":  fmt.Sprintf("%d", shard),
			},
			nil,
		))
	}

	test := uthelper.TestCommonStruct{
		Name: tc.name,
		Plugins: map[string]framework.PluginBuilder{
			gang.PluginName:       gang.New,
			predicates.PluginName: predicates.New,
			podGroupAntiAffinityForestTestPlugin: func(_ framework.Arguments) framework.Plugin {
				return &podGroupAntiAffinityForestTestPluginImpl{}
			},
		},
		PodGroups:           []*schedulingv1.PodGroup{pg},
		Pods:                pods,
		Nodes:               nodes,
		HyperNodesSetByTier: map[int]sets.Set[string]{1: tier1, 2: tier2, 3: tier3},
		HyperNodesMap:       hyperNodes,
		HyperNodes:          realNodes,
		Queues:              []*schedulingv1.Queue{util.BuildQueue("q1", 1, nil)},
		ExpectBindsNum:      tc.podCount,
		MinimalBindCheck:    true,
	}

	ssn := test.RegisterSession(tiers, nil)
	defer test.Close()
	action := New()
	test.Run([]framework.Action{action})
	if err := test.CheckAll(0); err != nil {
		t.Fatal(err)
	}

	job := ssn.Jobs[api.JobID("default/target")]
	if job == nil {
		t.Fatal("scheduled job default/target not found")
	}
	if got := shouldUsePodGroupAntiAffinityCandidateForest(job); got != tc.wantCandidateForestPath {
		t.Fatalf("shouldUsePodGroupAntiAffinityCandidateForest = %t, want %t", got, tc.wantCandidateForestPath)
	}
	if job.AllocatedHyperNode != tc.wantJobPlacement {
		t.Fatalf("job AllocatedHyperNode = %q, want %q", job.AllocatedHyperNode, tc.wantJobPlacement)
	}
	if !tc.skipPAA {
		if decision := action.recorder.jobDecisions[job.UID]; decision != tc.wantJobPlacement {
			t.Fatalf("recorded job decision = %q, want actual placement %q", decision, tc.wantJobPlacement)
		}
		for decisionKey := range action.recorder.subJobDecisions[job.UID] {
			if decisionKey != tc.wantJobPlacement {
				t.Fatalf("recorded SubJob decision key = %q, want only actual placement %q", decisionKey, tc.wantJobPlacement)
			}
		}
	}

	gotNodes := sets.New[string]()
	for _, task := range job.Tasks {
		if task.NodeName == "" {
			t.Fatalf("task %s was not allocated", task.UID)
		}
		if !tc.skipPAA && (task.NodeName == "a-node-0" || task.NodeName == "a-node-1") {
			t.Fatalf("task %s leaked into excluded domain A node %s", task.UID, task.NodeName)
		}
		gotNodes.Insert(task.NodeName)
	}
	if gotNodes.Len() != tc.podCount {
		t.Fatalf("allocated node count = %d, want %d distinct nodes: %v", gotNodes.Len(), tc.podCount, gotNodes.UnsortedList())
	}
	if tc.wantNodes != nil && !gotNodes.Equal(tc.wantNodes) {
		t.Fatalf("allocated nodes = %v, want %v", gotNodes.UnsortedList(), tc.wantNodes.UnsortedList())
	}
}

func TestCandidateForestActivationBoundary(t *testing.T) {
	tierTwo := 2
	job := &api.JobInfo{
		UID: "default/target",
		PodGroup: &api.PodGroup{PodGroup: scheduling.PodGroup{Spec: scheduling.PodGroupSpec{
			TopologyAffinity: &scheduling.TopologyAffinitySpec{
				PodGroupAntiAffinity: &scheduling.PodGroupAntiAffinity{
					Required: []scheduling.PodGroupAffinityTerm{{
						PodGroupSelector: &metav1.LabelSelector{},
					}},
				},
			},
		}}},
		NetworkTopology: &scheduling.NetworkTopologySpec{
			Mode:               scheduling.HardNetworkTopologyMode,
			HighestTierAllowed: &tierTwo,
		},
	}
	if shouldUsePodGroupAntiAffinityCandidateForest(job) {
		t.Fatal("real hard Job network topology must keep the legacy single-root path")
	}

	job.NetworkTopology.Mode = scheduling.SoftNetworkTopologyMode
	if !shouldUsePodGroupAntiAffinityCandidateForest(job) {
		t.Fatal("native soft Job network topology should use the candidate-forest path")
	}

	job.NetworkTopology = nil
	job.SubJobs = map[api.SubJobID]*api.SubJobInfo{
		"soft": {
			UID: "soft",
			NetworkTopology: &scheduling.NetworkTopologySpec{
				Mode:               scheduling.SoftNetworkTopologyMode,
				HighestTierAllowed: &tierTwo,
			},
		},
	}
	if !shouldUsePodGroupAntiAffinityCandidateForest(job) {
		t.Fatal("required PAA with native-soft SubGroups should use the candidate-forest path")
	}

	job.PodGroup.Spec.TopologyAffinity = nil
	if shouldUsePodGroupAntiAffinityCandidateForest(job) {
		t.Fatal("native-soft SubGroups without required PAA must keep the existing coarse fallback")
	}
}

func TestFilterGradientsByCandidateForestExcludesEnvelopeSibling(t *testing.T) {
	_, _, _, hyperNodes, _, _ := buildPodGroupAntiAffinityForestTopology()
	stats := &api.HyperNodeGradientStats{
		IntersectedByTier: map[int]int{1: 6},
		ExcludedByReason:  make(map[string]string),
	}
	got := filterGradientsByCandidateForest(
		hyperNodes,
		[][]*api.HyperNodeInfo{{
			hyperNodes["a-t1-0"], hyperNodes["a-t1-1"],
			hyperNodes["b-t1-0"], hyperNodes["b-t1-1"],
			hyperNodes["c-t1-0"], hyperNodes["c-t1-1"],
		}},
		[]*api.HyperNodeInfo{hyperNodes["b-t2"], hyperNodes["c-t2"]},
		stats,
	)
	gotNames := api.HyperNodeNamesInGradients(got)
	wantNames := sets.New[string]("b-t1-0", "b-t1-1", "c-t1-0", "c-t1-1")
	if !gotNames.Equal(wantNames) {
		t.Fatalf("filtered candidates = %v, want exact B+C leaves %v", gotNames.UnsortedList(), wantNames.UnsortedList())
	}
	if stats.ExcludedByReason["a-t1-0"] != "podGroupAntiAffinityCandidateForest" ||
		stats.ExcludedByReason["a-t1-1"] != "podGroupAntiAffinityCandidateForest" {
		t.Fatalf("excluded A diagnostics = %v", stats.ExcludedByReason)
	}
	if stats.IntersectedByTier[1] != 4 {
		t.Fatalf("final tier stats = %v, want tier1=4", stats.IntersectedByTier)
	}
}

func TestFilterCandidateForestGradientsByMinResource(t *testing.T) {
	buildNodeInfo := func(name string) *api.NodeInfo {
		return api.NewNodeInfo(util.BuildNode(
			name,
			api.BuildResourceList(
				"1", "1Gi",
				[]api.ScalarResource{{Name: string(forestTestAscendResource), Value: "8"}}...,
			),
			nil,
		))
	}
	hyperNodes := api.HyperNodeInfoMap{
		"b-t2": newPlacementTestHyperNode("b-t2", 2, "shared-t3"),
		"c-t2": newPlacementTestHyperNode("c-t2", 2, "shared-t3"),
		"d-t2": newPlacementTestHyperNode("d-t2", 2, "shared-t3"),
	}
	ssn := &framework.Session{
		Nodes: map[string]*api.NodeInfo{
			"a0": buildNodeInfo("a0"),
			"a1": buildNodeInfo("a1"),
			"b0": buildNodeInfo("b0"),
			"b1": buildNodeInfo("b1"),
			"c0": buildNodeInfo("c0"),
			"c1": buildNodeInfo("c1"),
		},
		RealNodesSet: map[string]sets.Set[string]{
			"shared-t3": sets.New[string]("a0", "a1", "b0", "b1", "c0", "c1"),
			"b-t2":      sets.New[string]("b0", "b1"),
			"c-t2":      sets.New[string]("c0", "c1"),
			"d-t2":      sets.New[string]("b0", "b1"),
		},
	}
	min24 := api.NewResource(v1.ResourceList{forestTestAscendResource: resource.MustParse("24")})
	forest := [][]*api.HyperNodeInfo{{hyperNodes["b-t2"], hyperNodes["c-t2"]}}
	filtered, stats := FilterCandidateForestGradientsByMinResource(ssn, forest, min24, "")
	if len(filtered) != 1 || len(filtered[0]) != 2 {
		t.Fatalf("B+C forest should satisfy 24 cards, got %#v, stats=%#v", filtered, stats)
	}

	min40 := api.NewResource(v1.ResourceList{forestTestAscendResource: resource.MustParse("40")})
	filtered, _ = FilterCandidateForestGradientsByMinResource(ssn, forest, min40, "")
	if len(filtered) != 0 {
		t.Fatalf("B+C must not count A resources through the shared envelope, got %#v", filtered)
	}

	overlap := [][]*api.HyperNodeInfo{{hyperNodes["b-t2"], hyperNodes["d-t2"]}}
	filtered, _ = FilterCandidateForestGradientsByMinResource(ssn, overlap, min24, "")
	if len(filtered) != 0 {
		t.Fatalf("overlapping roots must not double-count nodes, got %#v", filtered)
	}
}

func buildPodGroupAntiAffinityForestTopology() (
	sets.Set[string],
	sets.Set[string],
	sets.Set[string],
	map[string]*api.HyperNodeInfo,
	map[string]sets.Set[string],
	[]*v1.Node,
) {
	tier1 := sets.New[string]()
	tier2 := sets.New[string]()
	tier3 := sets.New[string]("shared-t3")
	hyperNodes := make(map[string]*api.HyperNodeInfo)
	realNodes := make(map[string]sets.Set[string])
	nodes := make([]*v1.Node, 0, 6)
	rootMembers := make([]api.MemberConfig, 0, 3)
	allNodes := sets.New[string]()

	for _, branch := range []string{"a", "b", "c"} {
		tier2Name := branch + "-t2"
		tier2Members := make([]api.MemberConfig, 0, 2)
		branchNodes := sets.New[string]()
		for leaf := 0; leaf < 2; leaf++ {
			tier1Name := fmt.Sprintf("%s-t1-%d", branch, leaf)
			nodeName := fmt.Sprintf("%s-node-%d", branch, leaf)
			nodes = append(nodes, util.BuildNode(
				nodeName,
				api.BuildResourceList(
					"1", "1Gi",
					[]api.ScalarResource{
						{Name: string(forestTestAscendResource), Value: "8"},
						{Name: "pods", Value: "10"},
					}...,
				),
				nil,
			))
			hyperNodes[tier1Name] = api.NewHyperNodeInfo(api.BuildHyperNode(
				tier1Name,
				1,
				[]api.MemberConfig{{
					Name:     nodeName,
					Type:     topologyv1alpha1.MemberTypeNode,
					Selector: "exact",
				}},
			))
			realNodes[tier1Name] = sets.New[string](nodeName)
			tier1.Insert(tier1Name)
			branchNodes.Insert(nodeName)
			allNodes.Insert(nodeName)
			tier2Members = append(tier2Members, api.MemberConfig{
				Name:     tier1Name,
				Type:     topologyv1alpha1.MemberTypeHyperNode,
				Selector: "exact",
			})
		}
		hyperNodes[tier2Name] = api.NewHyperNodeInfo(api.BuildHyperNode(tier2Name, 2, tier2Members))
		realNodes[tier2Name] = branchNodes
		tier2.Insert(tier2Name)
		rootMembers = append(rootMembers, api.MemberConfig{
			Name:     tier2Name,
			Type:     topologyv1alpha1.MemberTypeHyperNode,
			Selector: "exact",
		})
	}
	hyperNodes["shared-t3"] = api.NewHyperNodeInfo(api.BuildHyperNode("shared-t3", 3, rootMembers))
	realNodes["shared-t3"] = allNodes
	return tier1, tier2, tier3, hyperNodes, realNodes, nodes
}
