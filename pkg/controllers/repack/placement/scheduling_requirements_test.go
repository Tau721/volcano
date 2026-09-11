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

package placement

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSchedulingRequirementsHashIgnoresObjectIdentityAndPlacementProtocolMetadata(t *testing.T) {
	original := schedulingRequirementsTestPod("old-pod", "old-group")
	replacement := schedulingRequirementsTestPod("new-pod", "new-group")
	replacement.UID = "new-uid"
	replacement.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: "repack.volcano.sh/placement"}}
	replacement.Annotations["repack.volcano.sh/placement-gate-owner"] = "run/run-uid"

	originalHash := mustSchedulingRequirementsHash(t, original)
	replacementHash := mustSchedulingRequirementsHash(t, replacement)
	if originalHash == "" || replacementHash == "" {
		t.Fatal("non-nil Pods must produce a scheduling requirements hash")
	}
	if originalHash != replacementHash {
		t.Fatalf("object recreation changed scheduling requirements hash: old=%q new=%q", originalHash, replacementHash)
	}
	if len(originalHash) != 22 {
		t.Fatalf("hash length = %d, want 22-character base64url-encoded 128-bit digest", len(originalHash))
	}
}

func TestSchedulingRequirementsHashChangesWithSchedulingConstraints(t *testing.T) {
	base := schedulingRequirementsTestPod("pod", "group")

	nodeSelectorChanged := base.DeepCopy()
	nodeSelectorChanged.Spec.NodeSelector["accelerator"] = "gpu"
	if mustSchedulingRequirementsHash(t, base) == mustSchedulingRequirementsHash(t, nodeSelectorChanged) {
		t.Fatal("different node selectors must produce different hashes")
	}

	resourcesChanged := base.DeepCopy()
	resourcesChanged.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("2")
	if mustSchedulingRequirementsHash(t, base) == mustSchedulingRequirementsHash(t, resourcesChanged) {
		t.Fatal("different resource requirements must produce different hashes")
	}

	priorityChanged := base.DeepCopy()
	priority := int32(100)
	priorityChanged.Spec.Priority = &priority
	if mustSchedulingRequirementsHash(t, base) == mustSchedulingRequirementsHash(t, priorityChanged) {
		t.Fatal("different scheduling priorities must produce different hashes")
	}
}

func TestSchedulingRequirementsHashNormalizesSetLikeOrder(t *testing.T) {
	first := schedulingRequirementsTestPod("pod-a", "group")
	first.Spec.Containers = append(first.Spec.Containers,
		corev1.Container{Name: "sidecar", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		}})
	first.Spec.Tolerations = []corev1.Toleration{
		{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "ai"},
		{Key: "accelerator", Operator: corev1.TolerationOpExists},
	}

	second := first.DeepCopy()
	second.Spec.Containers[0], second.Spec.Containers[1] = second.Spec.Containers[1], second.Spec.Containers[0]
	second.Spec.Tolerations[0], second.Spec.Tolerations[1] = second.Spec.Tolerations[1], second.Spec.Tolerations[0]

	if mustSchedulingRequirementsHash(t, first) != mustSchedulingRequirementsHash(t, second) {
		t.Fatal("reordering regular containers or tolerations must not change scheduling equivalence")
	}
}

func TestSchedulingRequirementsHashNilPod(t *testing.T) {
	if _, err := SchedulingRequirementsHash(nil); err == nil {
		t.Fatal("nil Pod must return an error")
	}
}

func mustSchedulingRequirementsHash(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	hash, err := SchedulingRequirementsHash(pod)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func schedulingRequirementsTestPod(name, podGroupName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      name,
			Labels:    map[string]string{"controller-generated": name},
			Annotations: map[string]string{
				"scheduling.k8s.io/group-name": podGroupName,
			},
		},
		Spec: corev1.PodSpec{
			SchedulerName: "volcano",
			NodeSelector:  map[string]string{"accelerator": "npu"},
			Containers: []corev1.Container{{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			}},
		},
	}
}
