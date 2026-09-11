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

package victimorder

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const testGPU = v1.ResourceName("nvidia.com/gpu")

type victimOrderSnapshot struct{ nodes []*schedapi.NodeInfo }

func (s victimOrderSnapshot) Nodes() []*schedapi.NodeInfo       { return s.nodes }
func (victimOrderSnapshot) NodeInScope(*schedapi.NodeInfo) bool { return true }
func (victimOrderSnapshot) PodGroupView(schedapi.JobID) api.PodGroupView {
	return api.PodGroupView{}
}
func (victimOrderSnapshot) FeasibleRelocation(context.Context, []*api.Move, []*schedapi.TaskInfo, []*schedapi.NodeInfo) ([]*api.Move, bool) {
	return nil, false
}
func (victimOrderSnapshot) HyperNodesSetByTier() map[int]sets.Set[string] {
	return map[int]sets.Set[string]{}
}
func (victimOrderSnapshot) RealNodesSet() map[string]sets.Set[string] {
	return map[string]sets.Set[string]{}
}
func (victimOrderSnapshot) HyperNodeTierNameMap() map[string]int {
	return map[string]int{}
}

func zonedNode(name, zone string) *schedapi.NodeInfo {
	return &schedapi.NodeInfo{
		Name: name,
		Node: &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"zone": zone}},
		},
	}
}

func plainNode(name string) *schedapi.NodeInfo {
	return &schedapi.NodeInfo{Name: name, Node: &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}}
}

func cordonedNode(name string) *schedapi.NodeInfo {
	node := plainNode(name)
	node.Node.Spec.Unschedulable = true
	return node
}

func reservedNode(name, key, value string) *schedapi.NodeInfo {
	node := plainNode(name)
	node.Node.Spec.Taints = []v1.Taint{{Key: key, Value: value, Effect: v1.TaintEffectNoSchedule}}
	return node
}

func gpuTask(name, nodeName string, requested int64, pod *v1.Pod) *schedapi.TaskInfo {
	if pod == nil {
		pod = &v1.Pod{}
	}
	pod.Name = name
	return &schedapi.TaskInfo{
		UID: schedapi.TaskID(name), Name: name, Pod: pod,
		TransactionContext: schedapi.TransactionContext{NodeName: nodeName},
		InitResreq: &schedapi.Resource{
			ScalarResources: map[v1.ResourceName]float64{testGPU: float64(requested)},
		},
	}
}

func newTestPlugin(nodes []*schedapi.NodeInfo) *victimOrderPlugin {
	return &victimOrderPlugin{
		countNodeAffinity:       true,
		countTaints:             true,
		countCordon:             true,
		orderByResourceRequests: true,
		resource:                testGPU,
		nodes:                   nodes,
		counts:                  map[*schedapi.TaskInfo]allowedCount{},
	}
}

func countFor(t *testing.T, plugin *victimOrderPlugin, task *schedapi.TaskInfo) int {
	t.Helper()
	result := plugin.evaluate(task)
	if !result.known {
		t.Fatalf("evaluate(%s) reported unknown, want a count", task.Name)
	}
	return result.count
}

func selectorPod(key, value string) *v1.Pod {
	return &v1.Pod{Spec: v1.PodSpec{NodeSelector: map[string]string{key: value}}}
}

func affinityPod(operator v1.NodeSelectorOperator, key string, values ...string) *v1.Pod {
	return &v1.Pod{Spec: v1.PodSpec{Affinity: &v1.Affinity{
		NodeAffinity: &v1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{{
					MatchExpressions: []v1.NodeSelectorRequirement{{Key: key, Operator: operator, Values: values}},
				}},
			},
		},
	}}}
}

// The two-node sets below each isolate one factor so a failure names its cause.

func TestEvaluateCountsEachStaticFactor(t *testing.T) {
	t.Run("nodeSelector", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("other", "z0"), zonedNode("target", "z1")})
		task := gpuTask("victim", "source", 1, selectorPod("zone", "z1"))
		if got := countFor(t, plugin, task); got != 1 {
			t.Fatalf("count=%d, want 1 (only the z1 node)", got)
		}
	})

	t.Run("nodeAffinity required In", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("other", "z0"), zonedNode("target", "z1")})
		task := gpuTask("victim", "source", 1, affinityPod(v1.NodeSelectorOpIn, "zone", "z1"))
		if got := countFor(t, plugin, task); got != 1 {
			t.Fatalf("count=%d, want 1 (only the z1 node)", got)
		}
	})

	t.Run("nodeAffinity required NotIn", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("other", "z0"), zonedNode("target", "z1")})
		task := gpuTask("victim", "source", 1, affinityPod(v1.NodeSelectorOpNotIn, "zone", "z1"))
		if got := countFor(t, plugin, task); got != 1 {
			t.Fatalf("count=%d, want 1 (only the node outside z1)", got)
		}
	})

	t.Run("taints", func(t *testing.T) {
		nodes := []*schedapi.NodeInfo{plainNode("source"), plainNode("free"), reservedNode("reserved", "dedicated", "gpu")}
		plugin := newTestPlugin(nodes)
		if got := countFor(t, plugin, gpuTask("victim", "source", 1, nil)); got != 1 {
			t.Fatalf("count=%d, want 1 (the NoSchedule node rejects an untolerating pod)", got)
		}
		tolerating := gpuTask("victim", "source", 1, nil)
		tolerating.Pod.Spec.Tolerations = []v1.Toleration{{Key: "dedicated", Operator: v1.TolerationOpEqual, Value: "gpu", Effect: v1.TaintEffectNoSchedule}}
		if got := countFor(t, plugin, tolerating); got != 2 {
			t.Fatalf("count=%d, want 2 (the tolerating pod admits both free nodes)", got)
		}
	})

	t.Run("cordon", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{plainNode("source"), plainNode("open"), cordonedNode("cordoned")})
		if got := countFor(t, plugin, gpuTask("victim", "source", 1, nil)); got != 1 {
			t.Fatalf("count=%d, want 1 (a cordoned node is not a candidate)", got)
		}
	})

	t.Run("source node is never a candidate", func(t *testing.T) {
		plugin := newTestPlugin([]*schedapi.NodeInfo{plainNode("source"), plainNode("other")})
		if got := countFor(t, plugin, gpuTask("victim", "source", 1, nil)); got != 1 {
			t.Fatalf("count=%d, want 1 (the drained node itself must not be counted)", got)
		}
	})
}

func TestEvaluateSkipsDisabledFactors(t *testing.T) {
	nodes := []*schedapi.NodeInfo{plainNode("source"), reservedNode("reserved", "dedicated", "gpu"), cordonedNode("cordoned")}

	all := newTestPlugin(nodes)
	if got := countFor(t, all, gpuTask("victim", "source", 1, nil)); got != 0 {
		t.Fatalf("all factors on: count=%d, want 0", got)
	}

	none := newTestPlugin(nodes)
	none.countTaints, none.countCordon = false, false
	if got := countFor(t, none, gpuTask("victim", "source", 1, nil)); got != 2 {
		t.Fatalf("taints and cordon off: count=%d, want 2", got)
	}
}

func TestEvaluateReportsUnknownWithoutPodSpec(t *testing.T) {
	plugin := newTestPlugin([]*schedapi.NodeInfo{plainNode("source"), plainNode("other")})
	specless := gpuTask("victim", "source", 1, nil)
	specless.Pod = nil
	if result := plugin.evaluate(specless); result.known {
		t.Fatal("a task without a pod spec must report unknown, not a count")
	}
}

// The memo key is the task object, not its UID: a plan-state clone keeps the UID
// while carrying a different source node, and the source node is excluded from
// the count.
func TestCountMemoizesPerTaskObjectNotUID(t *testing.T) {
	plugin := newTestPlugin([]*schedapi.NodeInfo{zonedNode("source", "z1"), zonedNode("other", "z0")})
	onSource := gpuTask("victim", "source", 1, selectorPod("zone", "z1"))
	onOther := gpuTask("victim", "other", 1, selectorPod("zone", "z1"))
	if onSource == onOther || onSource.UID != onOther.UID {
		t.Fatalf("test setup: want distinct objects sharing a UID, got %q vs %q", onSource.UID, onOther.UID)
	}

	if got := plugin.count(onSource); !got.known || got.count != 0 {
		t.Fatalf("count on source=%v, want a known 0: the z1 node is the source", got)
	}
	if got := plugin.count(onOther); !got.known || got.count != 1 {
		t.Fatalf("count on other=%v, want a known 1: the memo must not reuse the source-node answer", got)
	}
}

// nodes gives the constrained victim one admissible receiver and every other
// victim two, so the count key must outrank the size key in both directions.
func constrainedAndLooseVictims(t *testing.T) (plugin *victimOrderPlugin, constrained, loose *schedapi.TaskInfo) {
	t.Helper()
	nodes := []*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("plain", "z0"), zonedNode("target", "z1")}
	return newTestPlugin(nodes),
		gpuTask("constrained", "source", 1, selectorPod("zone", "z1")),
		gpuTask("loose", "source", 8, nil)
}

func TestCompareOrdersFewestAllowedReceiversFirst(t *testing.T) {
	plugin, constrained, loose := constrainedAndLooseVictims(t)

	if got := plugin.compare(constrained, loose); got >= 0 {
		t.Fatalf("compare(constrained, loose)=%d, want <0: fewer allowed receivers sorts first", got)
	}
	if got := plugin.compare(loose, constrained); got <= 0 {
		t.Fatalf("compare(loose, constrained)=%d, want >0", got)
	}
}

// The count key must win outright, not merely act as a tie-break: the loose
// victim requests eight times the cards and still sorts second.
func TestComparePrefersFewerReceiversOverLargerRequest(t *testing.T) {
	plugin, constrained, loose := constrainedAndLooseVictims(t)
	if got := api.Scalar(loose.InitResreq, testGPU); got <= api.Scalar(constrained.InitResreq, testGPU) {
		t.Fatalf("test setup: loose requests %v, want more than constrained", got)
	}
	if got := plugin.compare(constrained, loose); got >= 0 {
		t.Fatalf("compare=%d, want <0: the constrained victim wins despite its smaller request", got)
	}
}

// Ties on the count key fall back to first-fit decreasing: the larger request is
// simulated first so an infeasible layout fails fast.
func TestCompareFallsBackToLargestRequestFirst(t *testing.T) {
	plugin, _, loose := constrainedAndLooseVictims(t)
	small := gpuTask("small", "source", 1, nil) // unconstrained too, so the counts tie
	medium := gpuTask("medium", "source", 4, nil)

	if got := plugin.compare(medium, small); got >= 0 {
		t.Fatalf("compare(medium, small)=%d, want <0: the larger request sorts first", got)
	}
	if got := plugin.compare(loose, medium); got >= 0 {
		t.Fatalf("compare(loose, medium)=%d, want <0: 8 cards before 4", got)
	}
	if got := plugin.compare(medium, loose); got <= 0 {
		t.Fatalf("compare(medium, loose)=%d, want >0", got)
	}
}

func TestCompareSkipsCountKeyWhenEveryFactorIsDisabled(t *testing.T) {
	plugin, constrained, loose := constrainedAndLooseVictims(t)
	plugin.countNodeAffinity, plugin.countTaints, plugin.countCordon = false, false, false

	if got := plugin.compare(constrained, loose); got <= 0 {
		t.Fatalf("compare=%d, want >0: with no factors the size key decides and loose is larger", got)
	}
}

// With the size key off, equal counts abstain in both directions rather than
// falling back to first-fit decreasing.
func TestCompareAbstainsWithoutResourceRequests(t *testing.T) {
	plugin, _, loose := constrainedAndLooseVictims(t)
	plugin.orderByResourceRequests = false
	small := gpuTask("small", "source", 1, nil) // unconstrained too, so the counts tie

	if got := plugin.compare(loose, small); got != 0 {
		t.Fatalf("compare(loose, small)=%d, want 0: the size key is disabled", got)
	}
	if got := plugin.compare(small, loose); got != 0 {
		t.Fatalf("compare(small, loose)=%d, want 0: the size key is disabled", got)
	}
}

// Disabling one key must not disturb the other: the count key still ranks a
// constrained victim ahead of a loose one.
func TestCountKeyDecidesWithoutResourceRequests(t *testing.T) {
	plugin, constrained, loose := constrainedAndLooseVictims(t)
	plugin.orderByResourceRequests = false

	if got := plugin.compare(constrained, loose); got >= 0 {
		t.Fatalf("compare(constrained, loose)=%d, want <0: fewer receivers still sorts first", got)
	}
}

// An unevaluable victim must not be read as "admits no nodes", which would sort
// it first. Only the count key abstains; the size key still has an opinion.
func TestCountKeyAbstainsWithoutPodSpec(t *testing.T) {
	plugin, constrained, _ := constrainedAndLooseVictims(t)
	specless := gpuTask("specless", "source", 1, nil)
	specless.Pod = nil

	if got := plugin.compareAllowedCounts(constrained, specless); got != 0 {
		t.Fatalf("count key=%d, want 0 (abstain)", got)
	}
	// Both request one card, so the whole comparator abstains and the sort keeps
	// them in the order the caller supplied.
	if got := plugin.compare(constrained, specless); got != 0 {
		t.Fatalf("compare=%d, want 0 with equal requests", got)
	}
	if got := plugin.compare(specless, constrained); got != 0 {
		t.Fatalf("compare=%d, want 0 with equal requests", got)
	}
}

// The plugin is the sole victim-order registrar, so binpack no longer competes
// with it and its presence cannot perturb the order.
func TestVictimOrderIsUnaffectedByBinpack(t *testing.T) {
	nodes := []*schedapi.NodeInfo{zonedNode("source", "z0"), zonedNode("plain", "z0"), zonedNode("target", "z1")}
	snapshot := victimOrderSnapshot{nodes: nodes}
	constrained := gpuTask("constrained", "source", 1, selectorPod("zone", "z1"))
	loose := gpuTask("loose", "source", 8, nil)

	order := func(options []framework.PluginOption) []string {
		ssn := framework.OpenSession(framework.SessionConfig{Snapshot: snapshot, Resource: testGPU}, options)
		defer framework.CloseSession(ssn)
		names := []string{}
		for _, victim := range ssn.OrderVictims([]*schedapi.TaskInfo{loose, constrained}) {
			names = append(names, victim.Name)
		}
		return names
	}

	alone := order(framework.PluginOptions(Name))
	withBinpack := order(framework.PluginOptions(Name, "binpack"))
	if len(alone) != 2 || alone[0] != "constrained" {
		t.Fatalf("victim order=%v, want constrained first", alone)
	}
	if len(withBinpack) != 2 || withBinpack[0] != alone[0] || withBinpack[1] != alone[1] {
		t.Fatalf("victim order with binpack=%v, want unchanged %v", withBinpack, alone)
	}
}

// Victims are collected by ranging a map, so the framework owes a total order
// even when no plugin has an opinion.
func TestOrderVictimsIsDeterministicWithoutPlugins(t *testing.T) {
	snapshot := victimOrderSnapshot{}
	tasks := []*schedapi.TaskInfo{
		gpuTask("c", "source", 1, nil), gpuTask("a", "source", 1, nil), gpuTask("b", "source", 1, nil),
	}
	for _, permutation := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}} {
		ssn := framework.OpenSession(framework.SessionConfig{Snapshot: snapshot, Resource: testGPU}, nil)
		shuffled := []*schedapi.TaskInfo{tasks[permutation[0]], tasks[permutation[1]], tasks[permutation[2]]}
		ordered := ssn.OrderVictims(shuffled)
		framework.CloseSession(ssn)

		for index, want := range []string{"a", "b", "c"} {
			if ordered[index].Name != want {
				t.Fatalf("permutation %v: order=[%s %s %s], want [a b c]",
					permutation, ordered[0].Name, ordered[1].Name, ordered[2].Name)
			}
		}
	}
}

func TestValidateArguments(t *testing.T) {
	if err := validateArguments(nil); err != nil {
		t.Fatalf("empty arguments rejected: %v", err)
	}
	if err := validateArguments(framework.Arguments{argTaints: false}); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
	if err := validateArguments(framework.Arguments{argResourceRequests: false}); err != nil {
		t.Fatalf("resourceRequests rejected: %v", err)
	}
	if err := validateArguments(framework.Arguments{"nodeAffinities": true}); err == nil {
		t.Fatal("a misspelled argument key should be rejected")
	}
	if err := validateArguments(framework.Arguments{argCordon: "true"}); err == nil {
		t.Fatal("a non-boolean argument value should be rejected")
	}
}
