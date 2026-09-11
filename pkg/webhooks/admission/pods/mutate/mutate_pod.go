/*
Copyright 2021 The Volcano Authors.

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

package mutate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	whv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	"volcano.sh/volcano/pkg/controllers/repack/placement"
	"volcano.sh/volcano/pkg/features"
	"volcano.sh/volcano/pkg/scheduler/api"
	wkconfig "volcano.sh/volcano/pkg/webhooks/config"
	"volcano.sh/volcano/pkg/webhooks/router"
	"volcano.sh/volcano/pkg/webhooks/schema"
	"volcano.sh/volcano/pkg/webhooks/util"
)

// patchOperation define the patch operation structure
type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// init register mutate pod
func init() {
	router.RegisterAdmission(service)
}

var service = &router.AdmissionService{
	Path:   "/pods/mutate",
	Func:   Pods,
	Config: config,
	MutatingConfig: &whv1.MutatingWebhookConfiguration{
		Webhooks: []whv1.MutatingWebhook{{
			Name: "mutatepod.volcano.sh",
			Rules: []whv1.RuleWithOperations{
				{
					Operations: []whv1.OperationType{whv1.Create},
					Rule: whv1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"pods"},
					},
				},
			},
		}},
	},
}

var config = &router.AdmissionServiceConfig{}

const repackPlacementLookupTimeout = 2 * time.Second

// Pods mutate pods.
func Pods(ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	klog.V(4).InfoS("mutating Pod admission request",
		"operation", ar.Request.Operation, "namespace", ar.Request.Namespace, "name", ar.Request.Name)
	pod, err := schema.DecodePod(ar.Request.Object, ar.Request.Resource)
	if err != nil {
		return util.ToAdmissionResponse(err)
	}

	if pod.Namespace == "" {
		pod.Namespace = ar.Request.Namespace
	}

	var patchBytes []byte
	switch ar.Request.Operation {
	case admissionv1.Create:
		patchBytes, err = createPatch(pod)
		if err != nil {
			return util.ToAdmissionResponse(err)
		}
	default:
		err = fmt.Errorf("expect operation to be 'CREATE' ")
		return util.ToAdmissionResponse(err)
	}

	reviewResponse := admissionv1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
	}
	if len(patchBytes) > 0 {
		pt := admissionv1.PatchTypeJSONPatch
		reviewResponse.PatchType = &pt
	}

	return &reviewResponse
}

// createPatch patch pod
func createPatch(pod *v1.Pod) ([]byte, error) {
	var patch []patchOperation
	placementPatches, err := patchRepackPlacementGate(pod)
	if err != nil {
		return nil, err
	}
	patch = append(patch, placementPatches...)
	if config.ConfigData == nil {
		klog.V(5).Infof("admission configuration is empty.")
		return json.Marshal(patch)
	}
	config.ConfigData.Lock()
	defer config.ConfigData.Unlock()

	// Add scheduling gates if opted-in
	patchGates := patchSchedulingGates(pod)
	if patchGates != nil {
		patch = append(patch, *patchGates)
	}
	for _, resourceGroup := range config.ConfigData.ResGroupsConfig {
		klog.V(3).Infof("resourceGroup %s", resourceGroup.ResourceGroup)
		group := GetResGroup(resourceGroup)
		if !group.IsBelongResGroup(pod, resourceGroup) {
			continue
		}

		patchLabel := patchLabels(pod, resourceGroup)
		if patchLabel != nil {
			patch = append(patch, *patchLabel)
		}

		patchAffinity := patchAffinity(pod, resourceGroup)
		if patchAffinity != nil {
			patch = append(patch, *patchAffinity)
		}

		patchToleration := patchTaintToleration(pod, resourceGroup)
		if patchToleration != nil {
			patch = append(patch, *patchToleration)
		}
		patchScheduler := patchSchedulerName(resourceGroup)
		if patchScheduler != nil {
			patch = append(patch, *patchScheduler)
		}

		klog.V(5).Infof("pod patch %v", patch)
		return json.Marshal(patch)
	}

	return json.Marshal(patch)
}

// patchRepackPlacementGate holds only replacement Pods belonging to a PodGroup
// with a live, engine-owned placement lease. The PodGroup lookup keeps the
// admission path generic: no workload kind needs to be recognized here.
func patchRepackPlacementGate(pod *v1.Pod) ([]patchOperation, error) {
	if config.VolcanoClient == nil || pod == nil {
		return nil, nil
	}
	if !slices.Contains(config.SchedulerNames, pod.Spec.SchedulerName) {
		return nil, nil
	}
	podGroupName := placement.PodGroupName(pod)
	if podGroupName == "" {
		return nil, nil
	}
	klog.V(4).InfoS("repack webhook: checking PodGroup placement lease",
		"pod", pod.Namespace+"/"+pod.Name, "podGroup", pod.Namespace+"/"+podGroupName)
	ctx, cancel := context.WithTimeout(context.Background(), repackPlacementLookupTimeout)
	defer cancel()
	podGroup, err := config.VolcanoClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(ctx, podGroupName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.V(4).InfoS("repack webhook: PodGroup not found; placement gate not added",
			"pod", pod.Namespace+"/"+pod.Name, "podGroup", pod.Namespace+"/"+podGroupName)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get PodGroup %s/%s for repack placement: %w", pod.Namespace, podGroupName, err)
	}
	lease := podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation]
	if lease == "" {
		klog.V(4).InfoS("repack webhook: PodGroup has no placement lease",
			"pod", pod.Namespace+"/"+pod.Name, "podGroup", pod.Namespace+"/"+podGroupName)
		return nil, nil
	}
	runName, runUID, ok := placement.ParseOwner(lease)
	if !ok {
		klog.V(3).InfoS("Ignoring malformed Repack placement lease", "podGroup", pod.Namespace+"/"+podGroupName, "lease", lease)
		return nil, nil
	}
	run, err := config.VolcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.V(4).InfoS("repack webhook: placement lease owner Run no longer exists",
			"pod", pod.Namespace+"/"+pod.Name, "podGroup", pod.Namespace+"/"+podGroupName, "run", runName)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get RepackRun %q for PodGroup %s/%s placement: %w", runName, pod.Namespace, podGroupName, err)
	}
	if run.UID != runUID || !placement.PlacementAppliesToPodGroup(run, podGroup) {
		klog.V(4).InfoS("repack webhook: placement lease is not active for PodGroup",
			"pod", pod.Namespace+"/"+pod.Name, "podGroup", pod.Namespace+"/"+podGroupName,
			"run", runName, "runPhase", run.Status.Phase, "uidMatches", run.UID == runUID,
			"workload", placement.WorkloadKeyForPodGroup(podGroup))
		return nil, nil
	}
	patches := make([]patchOperation, 0, 2)
	gate := appendSchedulingGate(pod, v1.PodSchedulingGate{Name: repackv1alpha1.PlacementGateName})
	if gate != nil {
		patches = append(patches, *gate)
	}
	if pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != lease {
		patches = append(patches, patchPlacementGateOwner(pod, lease))
	}
	if len(patches) > 0 {
		klog.V(3).InfoS("repack webhook: adding placement scheduling gate",
			"pod", pod.Namespace+"/"+pod.Name, "podGroup", pod.Namespace+"/"+podGroupName,
			"run", run.Name, "lease", lease, "gateAlreadyPresent", gate == nil)
	}
	return patches, nil
}

// patchPlacementGateOwner records which Run added the gate. A Pod-level marker
// is necessary because a PodGroup lease is intentionally shared by all Pods in
// the scheduling unit, including concurrent scale-out Pods.
func patchPlacementGateOwner(pod *v1.Pod, lease string) patchOperation {
	if len(pod.Annotations) == 0 {
		return patchOperation{Op: "add", Path: "/metadata/annotations", Value: map[string]string{
			repackv1alpha1.PlacementGateOwnerAnnotation: lease,
		}}
	}
	return patchOperation{
		Op:    "add",
		Path:  "/metadata/annotations/repack.volcano.sh~1placement-gate-owner",
		Value: lease,
	}
}

// patchSchedulingGates adds a scheduling gate for Volcano-managed pods.
// The gate prevents cluster autoscalers from seeing the pod until Volcano
// determines it's ready (queue admission + gang scheduling satisfied).
func patchSchedulingGates(pod *v1.Pod) *patchOperation {
	// Skip if SchedulingGatesQueueAdmission feature gate is not enabled
	if !utilfeature.DefaultFeatureGate.Enabled(features.SchedulingGatesQueueAdmission) {
		return nil
	}

	// Check if opt-in annotation is present
	if !api.HasQueueAllocationGateAnnotation(pod) {
		klog.V(4).Infof("Pod %s/%s does not have opt-in annotation, skipping gate",
			pod.Namespace, pod.Name)
		return nil
	}

	gate := v1.PodSchedulingGate{Name: schedulingv1beta1.QueueAllocationGateKey}
	return appendSchedulingGate(pod, gate)
}

// appendSchedulingGate appends a gate without clobbering gates injected by
// another mutating webhook in the same admission chain.
func appendSchedulingGate(pod *v1.Pod, gate v1.PodSchedulingGate) *patchOperation {

	// Idempotent: do not add a duplicate Volcano gate.
	// This prevents appending the same gate multiple times if the mutation is retried.
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == gate.Name {
			return nil
		}
	}

	// Parent missing: The schedulingGates slice hasn't been initialized yet.
	// We must use "add" on the base path with an array containing our gate.
	if pod.Spec.SchedulingGates == nil {
		return &patchOperation{
			Op:    "add",
			Path:  "/spec/schedulingGates",
			Value: []v1.PodSchedulingGate{gate},
		}
	}

	// Parent exists: We can safely append to the existing array.
	// Using the "-" path operator tells JSON Patch to append to the end of the array,
	// preventing us from overwriting gates added by parallel webhooks.
	return &patchOperation{
		Op:    "add",
		Path:  "/spec/schedulingGates/-",
		Value: gate,
	}
}

// patchLabels patch label
func patchLabels(pod *v1.Pod, resGroupConfig wkconfig.ResGroupConfig) *patchOperation {
	if len(resGroupConfig.Labels) == 0 {
		return nil
	}

	nodeSelector := make(map[string]string)
	for key, label := range pod.Spec.NodeSelector {
		nodeSelector[key] = label
	}

	for key, label := range resGroupConfig.Labels {
		nodeSelector[key] = label
	}

	return &patchOperation{Op: "add", Path: "/spec/nodeSelector", Value: nodeSelector}
}

// patchAffinity patch affinity
func patchAffinity(pod *v1.Pod, resGroupConfig wkconfig.ResGroupConfig) *patchOperation {
	if resGroupConfig.Affinity == "" {
		return nil
	}

	if pod.Spec.Affinity != nil {
		klog.V(5).Infof("pod affinity exist: %s", pod.Name)
		return nil
	}

	var affinity v1.Affinity
	err := json.Unmarshal([]byte(resGroupConfig.Affinity), &affinity)
	if err != nil {
		fmt.Println("Failed to unmarshal JSON:", err)
		klog.V(3).Infof("Failed to unmarshal JSON: %s", err)
		return nil
	}

	return &patchOperation{Op: "add", Path: "/spec/affinity", Value: affinity}
}

// patchTaintToleration patch taint toleration
func patchTaintToleration(pod *v1.Pod, resGroupConfig wkconfig.ResGroupConfig) *patchOperation {
	if len(resGroupConfig.Tolerations) == 0 {
		return nil
	}

	var dst []v1.Toleration
	dst = append(dst, pod.Spec.Tolerations...)
	dst = append(dst, resGroupConfig.Tolerations...)

	return &patchOperation{Op: "add", Path: "/spec/tolerations", Value: dst}
}

// patchSchedulerName patch scheduler
func patchSchedulerName(resGroupConfig wkconfig.ResGroupConfig) *patchOperation {
	if resGroupConfig.SchedulerName == "" {
		return nil
	}

	return &patchOperation{Op: "add", Path: "/spec/schedulerName", Value: resGroupConfig.SchedulerName}
}
