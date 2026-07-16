/*
Copyright 2025 The Volcano Authors.

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

package validate

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	fakeclient "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	informers "volcano.sh/apis/pkg/client/informers/externalversions"
)

func TestValidatePodGroup(t *testing.T) {
	highestTierAllowed := 1
	tests := []struct {
		name        string
		podGroup    *schedulingv1beta1.PodGroup
		queue       *schedulingv1beta1.Queue
		expectError bool
	}{
		{
			name: "valid podgroup with open queue",
			podGroup: &schedulingv1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-podgroup",
				},
				Spec: schedulingv1beta1.PodGroupSpec{
					Queue: "test-queue",
				},
			},
			queue: &schedulingv1beta1.Queue{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-queue",
				},
				Status: schedulingv1beta1.QueueStatus{
					State: schedulingv1beta1.QueueStateOpen,
				},
			},
			expectError: false,
		},
		{
			name: "invalid podgroup with closed queue",
			podGroup: &schedulingv1beta1.PodGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodGroup",
					APIVersion: "scheduling.volcano.sh/v1beta1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-podgroup",
				},
				Spec: schedulingv1beta1.PodGroupSpec{
					Queue: "test-queue",
				},
			},
			queue: &schedulingv1beta1.Queue{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-queue",
				},
				Status: schedulingv1beta1.QueueStatus{
					State: schedulingv1beta1.QueueStateClosed,
				},
			},
			expectError: true,
		},
		{
			name: "valid podgroup with empty queue",
			podGroup: &schedulingv1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-podgroup",
				},
				Spec: schedulingv1beta1.PodGroupSpec{
					Queue: "",
				},
			},
			queue:       &schedulingv1beta1.Queue{},
			expectError: false,
		},
		{
			name: "invalid podgroup with a queue that does not exist",
			podGroup: &schedulingv1beta1.PodGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodGroup",
					APIVersion: "scheduling.volcano.sh/v1beta1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-podgroup",
				},
				Spec: schedulingv1beta1.PodGroupSpec{
					Queue: "test-queue",
				},
			},
			queue:       &schedulingv1beta1.Queue{},
			expectError: true,
		},
		{
			name: "valid podgroup configured with SubGroupPolicy containing HighestTierName",
			podGroup: &schedulingv1beta1.PodGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodGroup",
					APIVersion: "scheduling.volcano.sh/v1beta1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-podgroup",
				},
				Spec: schedulingv1beta1.PodGroupSpec{
					SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{
						{
							Name: "test-policy",
							NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
								Mode:            schedulingv1beta1.HardNetworkTopologyMode,
								HighestTierName: "volcano.sh/hypernode",
							},
						},
					},
				},
			},
			queue:       &schedulingv1beta1.Queue{},
			expectError: false,
		},
		{
			name: "valid podgroup configured with SubGroupPolicy containing HighestTierAllowed",
			podGroup: &schedulingv1beta1.PodGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodGroup",
					APIVersion: "scheduling.volcano.sh/v1beta1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-podgroup",
				},
				Spec: schedulingv1beta1.PodGroupSpec{
					SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{
						{
							Name: "test-policy",
							NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
								Mode:               schedulingv1beta1.HardNetworkTopologyMode,
								HighestTierAllowed: &highestTierAllowed,
							},
						},
					},
				},
			},
			queue:       &schedulingv1beta1.Queue{},
			expectError: false,
		},
		{
			name: "invalid podgroup configured with SubGroupPolicy containing HighestTierAllowed and HighestTierName",
			podGroup: &schedulingv1beta1.PodGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodGroup",
					APIVersion: "scheduling.volcano.sh/v1beta1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-podgroup",
				},
				Spec: schedulingv1beta1.PodGroupSpec{
					SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{
						{
							Name: "test-policy",
							NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
								Mode:               schedulingv1beta1.HardNetworkTopologyMode,
								HighestTierAllowed: &highestTierAllowed,
								HighestTierName:    "volcano.sh/hypernode",
							},
						},
					},
				},
			},
			queue:       &schedulingv1beta1.Queue{},
			expectError: true,
		},
		{
			name: "invalid podgroup configured with NetworkTopology containing HighestTierAllowed and HighestTierName",
			podGroup: &schedulingv1beta1.PodGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PodGroup",
					APIVersion: "scheduling.volcano.sh/v1beta1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-podgroup",
				},
				Spec: schedulingv1beta1.PodGroupSpec{
					NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
						Mode:               schedulingv1beta1.HardNetworkTopologyMode,
						HighestTierAllowed: &highestTierAllowed,
						HighestTierName:    "volcano.sh/hypernode",
					},
				},
			},
			queue:       &schedulingv1beta1.Queue{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config.VolcanoClient = fakeclient.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(config.VolcanoClient, 0)
			queueInformer := informerFactory.Scheduling().V1beta1().Queues()
			config.QueueLister = queueInformer.Lister()
			err := queueInformer.Informer().GetIndexer().Add(tt.queue)
			assert.Nil(t, err)

			pgJson, _ := json.Marshal(tt.podGroup)
			// Create an AdmissionReview object
			ar := admissionv1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{
					Kind:       "AdmissionReview",
					APIVersion: "admission.k8s.io/v1beta1",
				},
				Request: &admissionv1.AdmissionRequest{
					Kind: metav1.GroupVersionKind{
						Group:   "scheduling.volcano.sh",
						Version: schedulingv1beta1.SchemeGroupVersion.Version,
						Kind:    "PodGroup",
					},
					Operation: admissionv1.Create,
					Name:      tt.podGroup.Name,
					Object:    runtime.RawExtension{Raw: pgJson},
					Resource: metav1.GroupVersionResource{
						Group:    schedulingv1beta1.SchemeGroupVersion.Group,
						Version:  schedulingv1beta1.SchemeGroupVersion.Version,
						Resource: "podgroups",
					},
				},
			}

			response := Validate(ar)
			if tt.expectError && response.Allowed {
				t.Errorf("Expected error but got allowed response")
			} else if !tt.expectError && !response.Allowed {
				t.Errorf("Expected allowed response but got error: %v", response.Result.Message)
			}
		})
	}
}

func TestValidateTopologyAffinityPreferredWeight(t *testing.T) {
	selector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"topology.volcano.sh/group": "prod"},
	}
	validPreferred := schedulingv1beta1.PodGroupAffinityTerm{
		PodGroupSelector: selector,
		TopologyTierName: "supernode",
		Weight:           50,
	}

	tests := []struct {
		name    string
		spec    *schedulingv1beta1.TopologyAffinitySpec
		wantErr bool
		errMsg  string
	}{
		{
			name: "preferred term without weight is rejected",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				PodGroupAntiAffinity: &schedulingv1beta1.PodGroupAntiAffinity{
					Preferred: []schedulingv1beta1.PodGroupAffinityTerm{
						{
							PodGroupSelector: selector,
							TopologyTierName: "supernode",
						},
					},
				},
			},
			wantErr: true,
			errMsg:  "weight must be an integer in the range 1-100",
		},
		{
			name: "preferred term with zero weight is rejected",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				PodGroupAntiAffinity: &schedulingv1beta1.PodGroupAntiAffinity{
					Preferred: []schedulingv1beta1.PodGroupAffinityTerm{
						{
							PodGroupSelector: selector,
							TopologyTierName: "supernode",
							Weight:           0,
						},
					},
				},
			},
			wantErr: true,
			errMsg:  "weight must be an integer in the range 1-100",
		},
		{
			name: "preferred term with weight above 100 is rejected",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				PodGroupAntiAffinity: &schedulingv1beta1.PodGroupAntiAffinity{
					Preferred: []schedulingv1beta1.PodGroupAffinityTerm{
						{
							PodGroupSelector: selector,
							TopologyTierName: "supernode",
							Weight:           101,
						},
					},
				},
			},
			wantErr: true,
			errMsg:  "weight must be an integer in the range 1-100",
		},
		{
			name: "preferred term with valid weight is accepted",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				PodGroupAntiAffinity: &schedulingv1beta1.PodGroupAntiAffinity{
					Preferred: []schedulingv1beta1.PodGroupAffinityTerm{validPreferred},
				},
			},
			wantErr: false,
		},
		{
			name: "required term must not set weight",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				PodGroupAntiAffinity: &schedulingv1beta1.PodGroupAntiAffinity{
					Required: []schedulingv1beta1.PodGroupAffinityTerm{
						{
							PodGroupSelector: selector,
							TopologyTierName: "supernode",
							Weight:           50,
						},
					},
				},
			},
			wantErr: true,
			errMsg:  "weight must not be set on required terms",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errMsg := validateTopologyAffinity(tt.spec, nil)
			if tt.wantErr {
				assert.NotEmpty(t, errMsg)
				if tt.errMsg != "" {
					assert.Contains(t, errMsg, tt.errMsg)
				}
				return
			}
			assert.Empty(t, errMsg)
		})
	}
}

func TestValidateSubGroupTopologyAffinity(t *testing.T) {
	tier1 := int32(1)
	tier2 := int32(2)
	minSubGroups := int32(2)
	policies := []schedulingv1beta1.SubGroupPolicySpec{
		{Name: "prefill"},
		{Name: "decode"},
		{Name: "worker", MinSubGroups: &minSubGroups},
	}

	tests := []struct {
		name    string
		spec    *schedulingv1beta1.TopologyAffinitySpec
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid subGroupAffinity",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				SubGroupAffinity: &schedulingv1beta1.SubGroupAffinity{
					Required: []schedulingv1beta1.SubGroupAffinityTerm{{SubGroups: []string{"prefill", "decode"}, TopologyTier: &tier2}},
				},
			},
		},
		{
			name: "subGroupAffinity requires two policies",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				SubGroupAffinity: &schedulingv1beta1.SubGroupAffinity{
					Required: []schedulingv1beta1.SubGroupAffinityTerm{{SubGroups: []string{"prefill"}, TopologyTier: &tier2}},
				},
			},
			wantErr: true,
			errMsg:  "subGroupAffinity requires at least two subGroups",
		},
		{
			name: "unknown policy is rejected",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				SubGroupAntiAffinity: &schedulingv1beta1.SubGroupAntiAffinity{
					Required: []schedulingv1beta1.SubGroupAffinityTerm{{SubGroups: []string{"missing"}, TopologyTier: &tier2}},
				},
			},
			wantErr: true,
			errMsg:  "is not defined",
		},
		{
			name: "single policy anti-affinity accepts splittable policy",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				SubGroupAntiAffinity: &schedulingv1beta1.SubGroupAntiAffinity{
					Required: []schedulingv1beta1.SubGroupAffinityTerm{{SubGroups: []string{"worker"}, TopologyTier: &tier2}},
				},
			},
		},
		{
			name: "single policy anti-affinity rejects non-splittable policy",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				SubGroupAntiAffinity: &schedulingv1beta1.SubGroupAntiAffinity{
					Required: []schedulingv1beta1.SubGroupAffinityTerm{{SubGroups: []string{"prefill"}, TopologyTier: &tier2}},
				},
			},
			wantErr: true,
			errMsg:  "single-policy subGroupAntiAffinity requires",
		},
		{
			name: "hard affinity cannot be finer than hard anti-affinity",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				SubGroupAffinity: &schedulingv1beta1.SubGroupAffinity{
					Required: []schedulingv1beta1.SubGroupAffinityTerm{{SubGroups: []string{"prefill", "decode"}, TopologyTier: &tier1}},
				},
				SubGroupAntiAffinity: &schedulingv1beta1.SubGroupAntiAffinity{
					Required: []schedulingv1beta1.SubGroupAffinityTerm{{SubGroups: []string{"prefill", "decode"}, TopologyTier: &tier2}},
				},
			},
			wantErr: true,
			errMsg:  "tier must be greater than or equal",
		},
		{
			name: "preferred subGroupAffinity requires weight",
			spec: &schedulingv1beta1.TopologyAffinitySpec{
				SubGroupAffinity: &schedulingv1beta1.SubGroupAffinity{
					Preferred: []schedulingv1beta1.SubGroupAffinityTerm{{SubGroups: []string{"prefill", "decode"}, TopologyTier: &tier2}},
				},
			},
			wantErr: true,
			errMsg:  "weight must be an integer in the range 1-100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errMsg := validateTopologyAffinity(tt.spec, policies)
			if tt.wantErr {
				assert.NotEmpty(t, errMsg)
				assert.Contains(t, errMsg, tt.errMsg)
				return
			}
			assert.Empty(t, errMsg)
		})
	}
}
