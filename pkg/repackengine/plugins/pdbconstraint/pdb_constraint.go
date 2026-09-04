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

// Package pdbconstraint prevents Repack from planning movement of accelerator
// Pods protected by a fresh, deterministic zero-disruption PDB. Dynamic PDB
// allowance remains authoritative at the Kubernetes Eviction API.
package pdbconstraint

import (
	"sort"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"

	schedapi "volcano.sh/volcano/pkg/scheduler/api"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

const (
	Name                 = "pdbconstraint"
	zeroDisruptionReason = "pdb_zero_disruption"
)

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory: func(framework.Arguments) framework.Plugin { return &pdbConstraintPlugin{} },
		Validator: func(arguments framework.Arguments) error {
			return arguments.ValidateKeys()
		},
	})
}

type pdbConstraintPlugin struct{}

type compiledPDBConstraint struct {
	namespace      string
	name           string
	selector       labels.Selector
	policy         *policyv1.UnhealthyPodEvictionPolicyType
	currentHealthy int32
	desiredHealthy int32
	disrupted      map[string]metav1.Time
}

type blockedTaskInfo struct {
	PDBNamespace string
	PDBName      string
}

func (*pdbConstraintPlugin) Name() string { return Name }

func (*pdbConstraintPlugin) OnSessionOpen(ssn *framework.Session) {
	if ssn == nil || ssn.Snapshot() == nil {
		klog.Warningf("repack: PDB constraints unavailable; planning will continue without static PDB filtering: snapshot is unavailable")
		return
	}
	reader, ok := ssn.Snapshot().(framework.PodDisruptionBudgetReader)
	if !ok {
		klog.Warningf("repack: PDB constraints unavailable; planning will continue without static PDB filtering: snapshot does not implement PodDisruptionBudgetReader")
		return
	}
	pdbs, err := reader.ListPodDisruptionBudgets()
	if err != nil {
		klog.Warningf("repack: PDB constraints unavailable; planning will continue without static PDB filtering: %v", err)
		return
	}

	constraintsByNamespace, zeroDisruptionPDBCount := compilePDBConstraints(pdbs)
	blockedTasks := make(map[schedapi.TaskID]blockedTaskInfo)
	blockedPodGroups := make(map[schedapi.JobID]struct{})
	targetTaskCount := 0
	seenTasks := make(map[schedapi.TaskID]struct{})
	for _, node := range ssn.Snapshot().Nodes() {
		if node == nil {
			continue
		}
		for _, task := range node.Tasks {
			key := taskKey(task)
			if task == nil || key == "" {
				continue
			}
			if _, seen := seenTasks[key]; seen {
				continue
			}
			seenTasks[key] = struct{}{}
			if api.Scalar(task.InitResreq, ssn.Resource()) <= 0 {
				continue
			}
			targetTaskCount++
			if info, blocked := blockingPDB(task, constraintsByNamespace); blocked {
				blockedTasks[key] = info
				if task.Job != "" {
					blockedPodGroups[task.Job] = struct{}{}
				}
				klog.V(5).InfoS("repack: task excluded by zero-disruption PDB constraint",
					"reason", zeroDisruptionReason,
					"pod", task.Pod.Namespace+"/"+task.Pod.Name,
					"podGroup", task.Job,
					"pdb", info.PDBNamespace+"/"+info.PDBName)
			}
		}
	}

	klog.V(4).InfoS("repack: PDB constraints prepared",
		"run", runName(ssn),
		"pdbCount", len(pdbs),
		"zeroDisruptionPDBCount", zeroDisruptionPDBCount,
		"targetTaskCount", targetTaskCount,
		"blockedTaskCount", len(blockedTasks),
		"blockedPodGroupCount", len(blockedPodGroups))
	if len(blockedTasks) == 0 {
		return
	}
	ssn.AddMovableFn(func(task *schedapi.TaskInfo) bool {
		if task == nil {
			return true
		}
		_, blocked := blockedTasks[taskKey(task)]
		return !blocked
	})
}

func (*pdbConstraintPlugin) OnSessionClose(*framework.Session) {}

func isZeroDisruptionPDB(pdb *policyv1.PodDisruptionBudget) bool {
	if pdb == nil || pdb.Status.ObservedGeneration != pdb.Generation || pdb.Status.ExpectedPods <= 0 {
		return false
	}
	condition := apiMeta.FindStatusCondition(pdb.Status.Conditions, policyv1.DisruptionAllowedCondition)
	if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == policyv1.SyncFailedReason {
		return false
	}
	return pdb.Status.DesiredHealthy >= pdb.Status.ExpectedPods
}

func compilePDBConstraints(pdbs []*policyv1.PodDisruptionBudget) (map[string][]compiledPDBConstraint, int) {
	sorted := append([]*policyv1.PodDisruptionBudget(nil), pdbs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i] == nil {
			return false
		}
		if sorted[j] == nil {
			return true
		}
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return sorted[i].Name < sorted[j].Name
	})

	byNamespace := make(map[string][]compiledPDBConstraint)
	zeroDisruptionPDBCount := 0
	for _, pdb := range sorted {
		if !isZeroDisruptionPDB(pdb) {
			continue
		}
		// policy/v1 defines a nil selector as matching no Pods, while an
		// explicitly empty selector matches every Pod in the namespace.
		if pdb.Spec.Selector == nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			klog.Warningf("repack: skipping zero-disruption PDB with invalid selector: pdb=%s/%s: %v",
				pdb.Namespace, pdb.Name, err)
			continue
		}
		byNamespace[pdb.Namespace] = append(byNamespace[pdb.Namespace], compiledPDBConstraint{
			namespace:      pdb.Namespace,
			name:           pdb.Name,
			selector:       selector,
			policy:         pdb.Spec.UnhealthyPodEvictionPolicy,
			currentHealthy: pdb.Status.CurrentHealthy,
			desiredHealthy: pdb.Status.DesiredHealthy,
			disrupted:      pdb.Status.DisruptedPods,
		})
		zeroDisruptionPDBCount++
	}
	return byNamespace, zeroDisruptionPDBCount
}

func blockingPDB(task *schedapi.TaskInfo, constraintsByNamespace map[string][]compiledPDBConstraint) (blockedTaskInfo, bool) {
	if task == nil || task.Pod == nil {
		return blockedTaskInfo{}, false
	}
	pod := task.Pod
	if canIgnorePDB(pod) {
		return blockedTaskInfo{}, false
	}
	for _, constraint := range constraintsByNamespace[pod.Namespace] {
		if !constraint.selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		if _, alreadyDisrupted := constraint.disrupted[pod.Name]; alreadyDisrupted {
			continue
		}
		if !podutil.IsPodReady(pod) && constraint.allowsUnhealthyPodEviction() {
			continue
		}
		return blockedTaskInfo{PDBNamespace: constraint.namespace, PDBName: constraint.name}, true
	}
	return blockedTaskInfo{}, false
}

// canIgnorePDB mirrors Pod states for which the Kubernetes Eviction API skips
// PDB evaluation entirely.
func canIgnorePDB(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodSucceeded ||
		pod.Status.Phase == v1.PodFailed ||
		pod.Status.Phase == v1.PodPending ||
		pod.DeletionTimestamp != nil
}

// allowsUnhealthyPodEviction mirrors the Eviction API's treatment of an
// unready Pod. Unknown future policies remain conservative.
func (c compiledPDBConstraint) allowsUnhealthyPodEviction() bool {
	if c.policy != nil {
		switch *c.policy {
		case policyv1.AlwaysAllow:
			return true
		case policyv1.IfHealthyBudget:
		default:
			return false
		}
	}
	return c.currentHealthy >= c.desiredHealthy && c.desiredHealthy > 0
}

func taskKey(task *schedapi.TaskInfo) schedapi.TaskID {
	if task == nil {
		return ""
	}
	if task.UID != "" {
		return schedapi.TaskID("uid/" + string(task.UID))
	}
	namespace, name := task.Namespace, task.Name
	if task.Pod != nil {
		if namespace == "" {
			namespace = task.Pod.Namespace
		}
		if name == "" {
			name = task.Pod.Name
		}
	}
	if namespace == "" || name == "" {
		return ""
	}
	return schedapi.TaskID("pod/" + namespace + "/" + name)
}

func runName(ssn *framework.Session) string {
	if ssn == nil || ssn.Run() == nil {
		return ""
	}
	return ssn.Run().Name
}
