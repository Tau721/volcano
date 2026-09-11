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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// normalizedSchedulingRequirements contains stable Pod attributes that can
// affect scheduler placement. It deliberately excludes object identity,
// PodGroup association, scheduling gates, and controller-generated metadata so
// the same logical workload revision produces the same value after recreation.
//
// The hash is an equivalence hint rather than a feasibility proof. Volume and
// device state are intentionally left to the scheduler, which validates
// nominatedNodeName against the current cluster before binding.
type normalizedSchedulingRequirements struct {
	ContainerResources        []corev1.ResourceRequirements     `json:"containerResources,omitempty"`
	InitContainerRequirements []normalizedInitContainer         `json:"initContainerRequirements,omitempty"`
	Overhead                  corev1.ResourceList               `json:"overhead,omitempty"`
	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	Tolerations               []corev1.Toleration               `json:"tolerations,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	HostPorts                 []normalizedHostPort              `json:"hostPorts,omitempty"`
	SchedulerName             string                            `json:"schedulerName,omitempty"`
	PriorityClassName         string                            `json:"priorityClassName,omitempty"`
	Priority                  *int32                            `json:"priority,omitempty"`
	PreemptionPolicy          *corev1.PreemptionPolicy          `json:"preemptionPolicy,omitempty"`
	RuntimeClassName          *string                           `json:"runtimeClassName,omitempty"`
	ResourceClaims            []corev1.PodResourceClaim         `json:"resourceClaims,omitempty"`
	HostNetwork               bool                              `json:"hostNetwork,omitempty"`
	OS                        *corev1.PodOS                     `json:"os,omitempty"`
}

type normalizedInitContainer struct {
	Resources     corev1.ResourceRequirements    `json:"resources,omitempty"`
	RestartPolicy *corev1.ContainerRestartPolicy `json:"restartPolicy,omitempty"`
}

type normalizedHostPort struct {
	HostIP   string          `json:"hostIP,omitempty"`
	HostPort int32           `json:"hostPort"`
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// SchedulingRequirementsHash returns an opaque, deterministic digest of the
// Pod's normalized scheduling requirements. Repack persists it only for
// PodGroups with explicit SubGroup policies.
func SchedulingRequirementsHash(pod *corev1.Pod) (string, error) {
	if pod == nil {
		return "", fmt.Errorf("Pod is required")
	}

	requirements := normalizedSchedulingRequirements{
		Overhead:                  pod.Spec.Overhead,
		NodeSelector:              pod.Spec.NodeSelector,
		Affinity:                  pod.Spec.Affinity,
		Tolerations:               append([]corev1.Toleration(nil), pod.Spec.Tolerations...),
		TopologySpreadConstraints: append([]corev1.TopologySpreadConstraint(nil), pod.Spec.TopologySpreadConstraints...),
		SchedulerName:             pod.Spec.SchedulerName,
		PriorityClassName:         pod.Spec.PriorityClassName,
		Priority:                  pod.Spec.Priority,
		PreemptionPolicy:          pod.Spec.PreemptionPolicy,
		RuntimeClassName:          pod.Spec.RuntimeClassName,
		ResourceClaims:            append([]corev1.PodResourceClaim(nil), pod.Spec.ResourceClaims...),
		HostNetwork:               pod.Spec.HostNetwork,
		OS:                        pod.Spec.OS,
	}
	for index := range pod.Spec.Containers {
		container := &pod.Spec.Containers[index]
		requirements.ContainerResources = append(requirements.ContainerResources, container.Resources)
		requirements.HostPorts = appendHostPorts(requirements.HostPorts, container.Ports)
	}
	for index := range pod.Spec.InitContainers {
		container := &pod.Spec.InitContainers[index]
		requirements.InitContainerRequirements = append(requirements.InitContainerRequirements, normalizedInitContainer{
			Resources:     container.Resources,
			RestartPolicy: container.RestartPolicy,
		})
		requirements.HostPorts = appendHostPorts(requirements.HostPorts, container.Ports)
	}

	// Regular container order and the order of set-like scheduling constraints
	// do not change feasibility. Sorting their canonical JSON prevents harmless
	// controller reorderings from changing the digest.
	if err := sortByCanonicalJSON(requirements.ContainerResources); err != nil {
		return "", fmt.Errorf("sort container resources: %w", err)
	}
	if err := sortByCanonicalJSON(requirements.Tolerations); err != nil {
		return "", fmt.Errorf("sort tolerations: %w", err)
	}
	if err := sortByCanonicalJSON(requirements.TopologySpreadConstraints); err != nil {
		return "", fmt.Errorf("sort topology spread constraints: %w", err)
	}
	if err := sortByCanonicalJSON(requirements.ResourceClaims); err != nil {
		return "", fmt.Errorf("sort resource claims: %w", err)
	}
	if err := sortByCanonicalJSON(requirements.HostPorts); err != nil {
		return "", fmt.Errorf("sort host ports: %w", err)
	}

	serialized, err := json.Marshal(requirements)
	if err != nil {
		return "", fmt.Errorf("serialize normalized scheduling requirements: %w", err)
	}
	sum := sha256.Sum256(serialized)
	return base64.RawURLEncoding.EncodeToString(sum[:16]), nil
}

func appendHostPorts(existing []normalizedHostPort, ports []corev1.ContainerPort) []normalizedHostPort {
	for index := range ports {
		port := &ports[index]
		if port.HostPort == 0 {
			continue
		}
		existing = append(existing, normalizedHostPort{
			HostIP:   port.HostIP,
			HostPort: port.HostPort,
			Protocol: port.Protocol,
		})
	}
	return existing
}

func sortByCanonicalJSON[T any](values []T) error {
	type sortableValue struct {
		value T
		key   string
	}
	sorted := make([]sortableValue, 0, len(values))
	for index := range values {
		serialized, err := json.Marshal(values[index])
		if err != nil {
			return err
		}
		sorted = append(sorted, sortableValue{value: values[index], key: string(serialized)})
	}
	sort.SliceStable(sorted, func(left, right int) bool {
		return sorted[left].key < sorted[right].key
	})
	for index := range sorted {
		values[index] = sorted[index].value
	}
	return nil
}
