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
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	whv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	"volcano.sh/volcano/pkg/controllers/repack/placement"
	"volcano.sh/volcano/pkg/webhooks/router"
	"volcano.sh/volcano/pkg/webhooks/schema"
	"volcano.sh/volcano/pkg/webhooks/util"
)

func init() {
	router.RegisterAdmission(service)
}

var service = &router.AdmissionService{
	Path:   "/podgroups/mutate",
	Func:   PodGroups,
	Config: config,
	MutatingConfig: &whv1.MutatingWebhookConfiguration{
		Webhooks: []whv1.MutatingWebhook{{
			Name: "mutatepodgroup.volcano.sh",
			Rules: []whv1.RuleWithOperations{
				{
					Operations: []whv1.OperationType{whv1.Create},
					Rule: whv1.Rule{
						APIGroups:   []string{schedulingv1beta1.SchemeGroupVersion.Group},
						APIVersions: []string{schedulingv1beta1.SchemeGroupVersion.Version},
						Resources:   []string{"podgroups"},
					},
				},
			},
		}},
	},
}

var config = &router.AdmissionServiceConfig{}

const repackRunLookupTimeout = 2 * time.Second

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// PodGroups mutate podgroups.
func PodGroups(ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	klog.V(3).Infof("Mutating %s podgroup %s.", ar.Request.Operation, ar.Request.Name)

	podgroup, err := schema.DecodePodGroup(ar.Request.Object, ar.Request.Resource)
	if err != nil {
		return util.ToAdmissionResponse(err)
	}

	var patchBytes []byte
	switch ar.Request.Operation {
	case admissionv1.Create:
		patchBytes, err = createPodGroupPatch(podgroup)
	default:
		return util.ToAdmissionResponse(fmt.Errorf("invalid operation `%s`, "+
			"expect operation to be `CREATE`", ar.Request.Operation))
	}

	if err != nil {
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result:  &metav1.Status{Message: err.Error()},
		}
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

func createPodGroupPatch(podgroup *schedulingv1beta1.PodGroup) ([]byte, error) {
	patch := make([]patchOperation, 0, 2)
	if queuePatch := createQueuePatch(podgroup); queuePatch != nil {
		patch = append(patch, *queuePatch)
	}
	if leasePatch := createRepackPlacementLeasePatch(podgroup); leasePatch != nil {
		patch = append(patch, *leasePatch)
	}
	if len(patch) == 0 {
		return nil, nil
	}
	return json.Marshal(patch)
}

func createQueuePatch(podgroup *schedulingv1beta1.PodGroup) *patchOperation {
	if podgroup.Spec.Queue != schedulingv1beta1.DefaultQueue {
		return nil
	}
	ns, err := config.KubeClient.CoreV1().Namespaces().Get(context.TODO(), podgroup.Namespace, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to get namespace", "namespace", podgroup.Namespace)
		return nil
	}

	if val, ok := ns.GetAnnotations()[schedulingv1beta1.QueueNameAnnotationKey]; ok {
		return &patchOperation{
			Op:    "add",
			Path:  "/spec/queue",
			Value: val,
		}
	}
	return nil
}

// createRepackPlacementLeasePatch is the admission-time half of the replacement
// placement barrier. The lease is stored atomically with a recreated PodGroup,
// so Pods created after a successful PodGroup CREATE cannot observe an
// intermediate object that exists without Repack protection.
//
// This function deliberately has no API side effects: it never updates the Run
// or another PodGroup. The nominator persists the concrete old-to-new mapping
// asynchronously after replacement Pods appear.
func createRepackPlacementLeasePatch(podGroup *schedulingv1beta1.PodGroup) *patchOperation {
	if config.VolcanoClient == nil || podGroup == nil {
		return nil
	}
	if existing := podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation]; existing != "" {
		klog.V(4).InfoS("repack PodGroup webhook: preserving existing placement lease",
			"podGroup", podGroup.Namespace+"/"+podGroup.Name, "lease", existing)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), repackRunLookupTimeout)
	defer cancel()
	runs, err := config.VolcanoClient.RepackV1alpha1().RepackRuns().List(ctx, metav1.ListOptions{
		LabelSelector: repackv1alpha1.PlacementActiveLabel + "=true",
	})
	if err != nil {
		// Admission remains available when Repack discovery is temporarily
		// unavailable. The engine periodically repairs missed PodGroup leases for
		// later Pods, and the placement deadline prevents a missed barrier from
		// being reported as silent success.
		klog.ErrorS(err, "repack PodGroup webhook: cannot discover active RepackRun; allowing PodGroup without lease",
			"podGroup", podGroup.Namespace+"/"+podGroup.Name)
		return nil
	}

	var matched *repackv1alpha1.RepackRun
	for index := range runs.Items {
		run := &runs.Items[index]
		if !placement.PlacementAppliesToPodGroup(run, podGroup) {
			klog.V(4).InfoS("repack PodGroup webhook: active Run does not cover PodGroup",
				"run", run.Name, "podGroup", podGroup.Namespace+"/"+podGroup.Name,
				"workload", placement.WorkloadKeyForPodGroup(podGroup))
			continue
		}
		if matched != nil {
			// Execute serialization should make this impossible. Failing open is
			// safer than attaching an ambiguous owner and blocking unrelated Pods.
			klog.ErrorS(fmt.Errorf("multiple active RepackRuns matched the same PodGroup"),
				"repack PodGroup webhook: ambiguous placement owner; lease not injected",
				"podGroup", podGroup.Namespace+"/"+podGroup.Name,
				"firstRun", matched.Name, "secondRun", run.Name)
			return nil
		}
		matched = run
	}
	if matched == nil {
		return nil
	}

	lease := placement.OwnerValue(matched.Name, matched.UID)
	if lease == "" {
		return nil
	}
	klog.V(3).InfoS("repack PodGroup webhook: injecting placement lease into recreated PodGroup",
		"run", matched.Name, "podGroup", podGroup.Namespace+"/"+podGroup.Name,
		"workload", placement.WorkloadKeyForPodGroup(podGroup), "lease", lease)
	if len(podGroup.Annotations) == 0 {
		return &patchOperation{
			Op:   "add",
			Path: "/metadata/annotations",
			Value: map[string]string{
				repackv1alpha1.PlacementLeaseAnnotation: lease,
			},
		}
	}
	return &patchOperation{
		Op:    "add",
		Path:  "/metadata/annotations/repack.volcano.sh~1placement-lease",
		Value: lease,
	}
}
