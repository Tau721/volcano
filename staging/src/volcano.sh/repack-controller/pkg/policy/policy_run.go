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

package policy

import (
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// ConstructRunFromTemplate builds a RepackRun object from the Policy's runTemplate
// at the given triggerTime. The returned object is ready to be created via the API
// client; it is NOT persisted here.
func ConstructRunFromTemplate(policy *repackv1alpha1.RepackPolicy, triggerLabel string, triggerTime time.Time) *repackv1alpha1.RepackRun {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: runName(policy.Name, triggerTime),
		},
	}

	// Deep-copy the template spec to avoid mutating the lister cache.
	run.Spec = *policy.Spec.RunTemplate.Spec.DeepCopy()

	// Set labels: policy ownership + trigger source + template labels.
	labels := make(map[string]string)
	for k, v := range policy.Spec.RunTemplate.ObjectMeta.Labels {
		labels[k] = v
	}
	labels[repackv1alpha1.RepackPolicyLabel] = policy.Name
	labels[repackv1alpha1.RepackTriggerLabel] = triggerLabel
	run.Labels = labels

	// Set annotations from template.
	annotations := make(map[string]string)
	for k, v := range policy.Spec.RunTemplate.ObjectMeta.Annotations {
		annotations[k] = v
	}
	run.Annotations = annotations

	// Set owner reference to the policy.
	trueVal := true
	falseVal := false
	run.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion:         repackv1alpha1.SchemeGroupVersion.String(),
			Kind:               "RepackPolicy",
			Name:               policy.Name,
			UID:                policy.UID,
			Controller:         &trueVal,
			BlockOwnerDeletion: &falseVal,
		},
	}

	return run
}

// MakeObjectReference builds a v1.ObjectReference from a RepackRun for use in
// RepackPolicyStatus.InProgress.
func MakeObjectReference(run *repackv1alpha1.RepackRun) v1.ObjectReference {
	return v1.ObjectReference{
		APIVersion: repackv1alpha1.SchemeGroupVersion.String(),
		Kind:       "RepackRun",
		Name:       run.Name,
		UID:        run.UID,
	}
}