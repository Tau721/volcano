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

package api

import (
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/api"
)

// Movable reports whether a running task may be moved by repack (design §4.5
// scope.podGroups include/exclude, gang/PDB rules). Tasks of frozen PodGroups
// return false and are never evicted. A nil Movable means everything is movable.
// The engine Session aggregates the MovableFns registered by plugins (AND: any
// plugin may veto a move) into a single Movable for planners to consume.
type Movable func(task *api.TaskInfo) bool

// PodGroupIdentity returns the canonical namespace/name identity of the
// PodGroup which owns task. Repack execution is PodGroup based: a task without
// this identity cannot participate in a plan, independently of optional policy
// plugins such as workloadscope.
func PodGroupIdentity(task *api.TaskInfo) (namespace, name string, valid bool) {
	if task == nil {
		return "", "", false
	}
	jobID := string(task.Job)
	separator := strings.IndexByte(jobID, '/')
	if separator <= 0 || separator == len(jobID)-1 || strings.IndexByte(jobID[separator+1:], '/') >= 0 {
		return "", "", false
	}
	namespace, name = jobID[:separator], jobID[separator+1:]
	if task.Namespace != "" && task.Namespace != namespace {
		return "", "", false
	}
	return namespace, name, true
}

// NodeFreeabilityReason explains why a node cannot be drained in the current
// planning pass. An empty value means the node is freeable.
type NodeFreeabilityReason string

const (
	NodeNotFoundReason                  NodeFreeabilityReason = "node_not_found"
	AlreadyDrainedReason                NodeFreeabilityReason = "already_drained"
	SelectedAsReceiverReason            NodeFreeabilityReason = "selected_as_receiver"
	NoTargetResourcePodReason           NodeFreeabilityReason = "no_target_resource_pod"
	HasImmovableTargetResourcePodReason NodeFreeabilityReason = "has_immovable_target_resource_pod"
)

// NodeFreeabilityState carries the per-pass state which affects whether a node
// may still be selected as a drain target.
type NodeFreeabilityState struct {
	Drained bool
	Filled  bool
}

// NodeFreeability is the single source of truth for a node's drain eligibility.
// ImmovableTasks contains only target-resource tasks that veto draining.
type NodeFreeability struct {
	Freeable       bool
	Reason         NodeFreeabilityReason
	ImmovableTasks []*api.TaskInfo
}

// EvaluateNodeFreeability reports whether a node's target-resource (res)
// capacity can be vacated, together with the exact reason if it cannot.
//
// Only tasks requesting res count. The scheduler cache tracks every pod on a node
// (system DaemonSets — kube-proxy, CNI — included), and those have no PodGroup so
// they are never movable; requiring all pods movable would make every real
// accelerator node unfreeable. "Freeing" a node for defrag means vacating its
// accelerator, not removing pinned pods — the node keeps running its DaemonSets,
// its accelerator just goes idle. A node with a frozen accelerator task can never
// be freed (its card stays pinned).
func EvaluateNodeFreeability(node *api.NodeInfo, state NodeFreeabilityState, movable Movable, res v1.ResourceName) NodeFreeability {
	if node == nil {
		return NodeFreeability{Reason: NodeNotFoundReason}
	}
	if state.Drained {
		return NodeFreeability{Reason: AlreadyDrainedReason}
	}
	if state.Filled {
		return NodeFreeability{Reason: SelectedAsReceiverReason}
	}
	hasTargetResourcePod := false
	var immovableTasks []*api.TaskInfo
	for _, t := range node.Tasks {
		if t == nil || Scalar(t.InitResreq, res) <= 0 {
			continue // non-accelerator pod (DaemonSet/CPU-only): irrelevant to freeing res
		}
		hasTargetResourcePod = true
		if movable != nil && !movable(t) {
			immovableTasks = append(immovableTasks, t)
		}
	}
	if !hasTargetResourcePod {
		return NodeFreeability{Reason: NoTargetResourcePodReason}
	}
	if len(immovableTasks) > 0 {
		return NodeFreeability{Reason: HasImmovableTargetResourcePodReason, ImmovableTasks: immovableTasks}
	}
	return NodeFreeability{Freeable: true}
}

// VictimsOf returns the movable target-resource (res) tasks that vacating node
// would displace. Non-accelerator pods (DaemonSets/CPU-only) stay on the node and
// are not returned — only the accelerator pods need relocating.
func VictimsOf(node *api.NodeInfo, movable Movable, res v1.ResourceName) []*api.TaskInfo {
	if node == nil {
		return nil
	}
	out := make([]*api.TaskInfo, 0, len(node.Tasks))
	for _, t := range node.Tasks {
		if t == nil || Scalar(t.InitResreq, res) <= 0 {
			continue
		}
		if movable == nil || movable(t) {
			out = append(out, t)
		} else {
			klog.V(5).InfoS("repack: accelerator pod is NOT movable under the active constraints, stays on node",
				"pod", t.Name, "node", node.Name, "podGroup", t.Job)
		}
	}
	return out
}
