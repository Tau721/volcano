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

// Package victimorder decides the order in which a drained node's victims are
// simulated: the pod admitting the fewest nodes first, ties broken by the larger
// request. Only static pod-to-node constraints are counted; constraints that
// depend on other pods (inter-pod affinity, topology spread, hostPorts) stay out
// of scope because they are only known during simulation.
//
// The count ignores leftover node capacity and therefore undercounts, which is
// the safe direction: an overcount can make a feasible unit look infeasible, and
// the planner caches that as stuck for the rest of the pass.
package victimorder

import (
	"cmp"

	v1 "k8s.io/api/core/v1"
	corev1helper "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "victimorder"

// Each node factor decides whether one node admits a pod. All default to true;
// disabling one only raises every victim's count.
const (
	argNodeAffinity = "nodeAffinity"
	argTaints       = "taints"
	argCordon       = "cordon"
)

// argResourceRequests is the second ordering key: equal counts, larger request first.
// Disabling it leaves ties to the next plugin and the framework's UID fallback.
const argResourceRequests = "resourceRequests"

// argKeys is every accepted argument, so key validation and type validation
// cannot drift apart.
var argKeys = []string{argNodeAffinity, argTaints, argCordon, argResourceRequests}

// TaintTolerationComparisonOperators is alpha and off upstream. Hardcoding the
// default is safe: if a cluster enables it we only undercount, and the reverse
// would be the harmful direction.
const taintComparisonOperatorsEnabled = false

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory: newPlugin, Validator: validateArguments,
	})
}

type victimOrderPlugin struct {
	countNodeAffinity       bool
	countTaints             bool
	countCordon             bool
	orderByResourceRequests bool

	resource v1.ResourceName
	nodes    []*schedapi.NodeInfo
	// counts memoizes per victim object, not per UID: a clone keeps its UID but may
	// carry a different source node. Ordering is single-goroutine, so no lock.
	counts map[*schedapi.TaskInfo]allowedCount
}

// allowedCount is the memoized per-victim result. known is false when the task
// carries nothing to evaluate, so the count key abstains rather than reading "no
// information" as "most constrained".
type allowedCount struct {
	count int
	known bool
}

func newPlugin(arguments framework.Arguments) framework.Plugin {
	return &victimOrderPlugin{
		countNodeAffinity:       configuredBool(arguments, argNodeAffinity),
		countTaints:             configuredBool(arguments, argTaints),
		countCordon:             configuredBool(arguments, argCordon),
		orderByResourceRequests: configuredBool(arguments, argResourceRequests),
		counts:                  map[*schedapi.TaskInfo]allowedCount{},
	}
}

func configuredBool(arguments framework.Arguments, key string) bool {
	value, err := arguments.Bool(key, true)
	if err != nil {
		return true
	}
	return value
}

func validateArguments(arguments framework.Arguments) error {
	if err := arguments.ValidateKeys(argKeys...); err != nil {
		return err
	}
	for _, key := range argKeys {
		if _, err := arguments.Bool(key, true); err != nil {
			return err
		}
	}
	return nil
}

func (*victimOrderPlugin) Name() string { return Name }

func (p *victimOrderPlugin) OnSessionOpen(ssn *framework.Session) {
	p.resource = ssn.Resource()
	p.nodes = ssn.Nodes()
	ssn.AddVictimOrderFn(Name, p.compare)
}

func (*victimOrderPlugin) OnSessionClose(*framework.Session) {}

// compare orders victims by allowed-receiver count ascending, then by requested
// target resource descending.
func (p *victimOrderPlugin) compare(left, right *schedapi.TaskInfo) int {
	if comparison := p.compareAllowedCounts(left, right); comparison != 0 {
		return comparison
	}
	if !p.orderByResourceRequests {
		return 0
	}
	return cmp.Compare(api.Scalar(right.InitResreq, p.resource), api.Scalar(left.InitResreq, p.resource))
}

// compareAllowedCounts returns 0 when either side is unknown or when the node
// factors are all disabled, deferring to the size key.
func (p *victimOrderPlugin) compareAllowedCounts(left, right *schedapi.TaskInfo) int {
	if !p.countNodeAffinity && !p.countTaints && !p.countCordon {
		return 0
	}
	leftCount := p.count(left)
	rightCount := p.count(right)
	if !leftCount.known || !rightCount.known {
		return 0
	}
	return cmp.Compare(leftCount.count, rightCount.count)
}

// count returns the memoized allowed-receiver count for a task.
func (p *victimOrderPlugin) count(task *schedapi.TaskInfo) allowedCount {
	if task == nil {
		return allowedCount{}
	}
	if cached, ok := p.counts[task]; ok {
		return cached
	}
	result := p.evaluate(task)
	p.counts[task] = result
	return result
}

// evaluate counts the nodes outside the task's current node that admit its pod;
// the drained node is never a receiver.
func (p *victimOrderPlugin) evaluate(task *schedapi.TaskInfo) allowedCount {
	if task.Pod == nil || len(p.nodes) == 0 {
		return allowedCount{}
	}
	required := nodeaffinity.GetRequiredNodeAffinity(task.Pod)
	count := 0
	for _, node := range p.nodes {
		if node == nil || node.Node == nil || node.Name == task.NodeName {
			continue
		}
		if p.nodeAdmits(task.Pod, node.Node, required) {
			count++
		}
	}
	return allowedCount{count: count, known: true}
}

// nodeAdmits applies the enabled static factors through the scheduler's own
// helpers, so the count agrees with the filter stack that decides admissibility.
func (p *victimOrderPlugin) nodeAdmits(pod *v1.Pod, node *v1.Node, required nodeaffinity.RequiredNodeAffinity) bool {
	if p.countCordon && node.Spec.Unschedulable {
		return false
	}
	if p.countTaints && !toleratesTaints(pod, node) {
		return false
	}
	if !p.countNodeAffinity {
		return true
	}
	// Folds spec.nodeSelector and nodeAffinity.required into one match; a
	// malformed selector surfaces as an error and is treated as not admitted.
	matches, err := required.Match(node)
	if err != nil {
		klog.V(5).InfoS("repack victimorder: unparsable nodeSelector/nodeAffinity, treating node as not admitted",
			"pod", pod.Namespace+"/"+pod.Name, "node", node.Name, "err", err)
		return false
	}
	return matches
}

// toleratesTaints mirrors the scheduler's TaintToleration filter: only
// NoSchedule and NoExecute taints reject a node.
func toleratesTaints(pod *v1.Pod, node *v1.Node) bool {
	if len(node.Spec.Taints) == 0 {
		return true
	}
	_, untolerated := corev1helper.FindMatchingUntoleratedTaint(klog.Background(), node.Spec.Taints,
		pod.Spec.Tolerations, doNotScheduleTaint, taintComparisonOperatorsEnabled)
	return !untolerated
}

func doNotScheduleTaint(taint *v1.Taint) bool {
	return taint.Effect == v1.TaintEffectNoSchedule || taint.Effect == v1.TaintEffectNoExecute
}
