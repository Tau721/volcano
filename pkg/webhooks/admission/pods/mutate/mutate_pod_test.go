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
	"encoding/json"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	"volcano.sh/volcano/pkg/controllers/repack/placement"
	webconfig "volcano.sh/volcano/pkg/webhooks/config"
)

func TestPatchPlacementGateOwnerReplacesExistingAnnotation(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "replacement",
		Annotations: map[string]string{
			repackv1alpha1.PlacementGateOwnerAnnotation: "old/uid",
			"example.com/preserved":                     "value",
		},
	}}
	original, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	patchBytes, err := json.Marshal([]patchOperation{patchPlacementGateOwner(pod, "new/uid")})
	if err != nil {
		t.Fatal(err)
	}
	patch, err := jsonpatch.DecodePatch(patchBytes)
	if err != nil {
		t.Fatalf("decode JSON Patch: %v", err)
	}
	updatedBytes, err := patch.Apply(original)
	if err != nil {
		t.Fatalf("apply add operation to existing annotation: %v", err)
	}
	updated := &v1.Pod{}
	if err := json.Unmarshal(updatedBytes, updated); err != nil {
		t.Fatal(err)
	}
	if got := updated.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation]; got != "new/uid" {
		t.Fatalf("placement gate owner = %q, want new/uid", got)
	}
	if got := updated.Annotations["example.com/preserved"]; got != "value" {
		t.Fatalf("unrelated annotation = %q, want value", got)
	}
}

func TestMutatePods(t *testing.T) {
	affinityJSONStr := `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/os","operator":"In","values":["linux"]}]}]}}}`
	var affinity v1.Affinity
	json.Unmarshal([]byte(affinityJSONStr), &affinity)

	admissionConfigData := &webconfig.AdmissionConfiguration{
		ResGroupsConfig: []webconfig.ResGroupConfig{
			{
				ResourceGroup: "management",
				Object: webconfig.Object{
					Key: "namespace",
					Value: []string{
						"mng-ns-1",
						"mng-ns-2",
					},
				},
				SchedulerName: "default-scheduler",
				Tolerations: []v1.Toleration{
					{
						Key:      "mng-taint-1",
						Operator: v1.TolerationOpExists,
						Effect:   v1.TaintEffectNoSchedule,
					},
				},
				Affinity: affinityJSONStr,
				Labels: map[string]string{
					"volcano.sh/nodetype": "management",
				},
			},
			{
				ResourceGroup: "cpu",
				Object: webconfig.Object{
					Key: "annotation",
					Value: []string{
						"volcano.sh/resource-group: cpu",
					},
				},
				SchedulerName: "volcano",
				Labels: map[string]string{
					"volcano.sh/nodetype": "cpu",
				},
			},
			{
				ResourceGroup: "gpu",
				SchedulerName: "volcano",
				Labels: map[string]string{
					"volcano.sh/nodetype": "gpu",
				},
			},
		},
	}

	config.ConfigData = admissionConfigData

	testCases := []struct {
		Name   string
		Pod    *v1.Pod
		expect []patchOperation
	}{
		{
			Name: "test-1",
			Pod: &v1.Pod{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Pod",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "mng-ns-1",
					Name:      "mng-pod",
				},
				Spec: v1.PodSpec{
					SchedulerName: "default-scheduler",
				},
			},
			expect: []patchOperation{
				{
					Op:   "add",
					Path: "/spec/nodeSelector",
					Value: map[string]string{
						"volcano.sh/nodetype": "management",
					},
				},
				{
					Op:    "add",
					Path:  "/spec/affinity",
					Value: affinity,
				},
				{
					Op:   "add",
					Path: "/spec/tolerations",
					Value: []v1.Toleration{
						{
							Key:      "mng-taint-1",
							Operator: v1.TolerationOpExists,
							Effect:   v1.TaintEffectNoSchedule,
						},
					},
				},
				{
					Op:    "add",
					Path:  "/spec/schedulerName",
					Value: "default-scheduler",
				},
			},
		},
		{
			Name: "test-2",
			Pod: &v1.Pod{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Pod",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "cpu-pod",
					Annotations: map[string]string{
						"volcano.sh/resource-group": "cpu",
					},
				},
			},
			expect: []patchOperation{
				{
					Op:   "add",
					Path: "/spec/nodeSelector",
					Value: map[string]string{
						"volcano.sh/nodetype": "cpu",
					},
				},
				{
					Op:    "add",
					Path:  "/spec/schedulerName",
					Value: "volcano",
				},
			},
		},
		{
			Name: "test-3",
			Pod: &v1.Pod{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Pod",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "gpu-pod",
					Annotations: map[string]string{
						"volcano.sh/resource-group": "gpu",
					},
				},
			},
			expect: []patchOperation{
				{
					Op:   "add",
					Path: "/spec/nodeSelector",
					Value: map[string]string{
						"volcano.sh/nodetype": "gpu",
					},
				},
				{
					Op:    "add",
					Path:  "/spec/schedulerName",
					Value: "volcano",
				},
			},
		},
		{
			Name: "test-4",
			Pod: &v1.Pod{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Pod",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "normal-pod",
				},
			},
			expect: nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.Name, func(t *testing.T) {
			patchBytes, _ := createPatch(testCase.Pod)
			expectBytes, _ := json.Marshal(testCase.expect)
			if !equality.Semantic.DeepEqual(patchBytes, expectBytes) {
				t.Errorf("Test case '%s' failed, expect: %v, got: %v", testCase.Name,
					expectBytes, patchBytes)
			}
		})
	}
}

func TestPatchRepackPlacementGate(t *testing.T) {
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "pg", PlannedNodeName: "node-b", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "pg", Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid"},
	}}
	previousClient, previousSchedulers := config.VolcanoClient, config.SchedulerNames
	defer func() {
		config.VolcanoClient = previousClient
		config.SchedulerNames = previousSchedulers
	}()
	config.VolcanoClient = vcfake.NewSimpleClientset(run, podGroup)
	config.SchedulerNames = []string{"volcano"}

	patches, err := patchRepackPlacementGate(&v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "replacement", Annotations: map[string]string{"scheduling.k8s.io/group-name": "pg"},
		},
		Spec: v1.PodSpec{SchedulerName: "volcano"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 2 || patches[0].Path != "/spec/schedulingGates" {
		t.Fatalf("placement gate patches = %#v", patches)
	}
	gates, ok := patches[0].Value.([]v1.PodSchedulingGate)
	if !ok || len(gates) != 1 || gates[0].Name != repackv1alpha1.PlacementGateName {
		t.Fatalf("placement gates = %#v", patches[0].Value)
	}
	if patches[1].Path != "/metadata/annotations/repack.volcano.sh~1placement-gate-owner" || patches[1].Value != "run/run-uid" {
		t.Fatalf("placement owner patch = %#v", patches[1])
	}
}

func TestPatchRepackPlacementGateBeforeReplacementPodGroupMapping(t *testing.T) {
	controller := true
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Plan: &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
				Namespace: "ns", PodGroupName: "old",
				Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
			}}},
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "old", PlannedNodeName: "node-b", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	newPodGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "new",
		Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	previousClient, previousSchedulers := config.VolcanoClient, config.SchedulerNames
	defer func() {
		config.VolcanoClient = previousClient
		config.SchedulerNames = previousSchedulers
	}()
	config.VolcanoClient = vcfake.NewSimpleClientset(run, newPodGroup)
	config.SchedulerNames = []string{"volcano"}

	patches, err := patchRepackPlacementGate(&v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "replacement",
			Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: "new"},
		},
		Spec: v1.PodSpec{SchedulerName: "volcano"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 2 || patches[0].Path != "/spec/schedulingGates" {
		t.Fatalf("replacement Pod was not gated before PodGroup mapping became durable: %#v", patches)
	}
}

func TestRepackPlacementPodGroupName(t *testing.T) {
	controller := true
	deploymentReplicaSetOwner := metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "workload-7f8d9c", UID: types.UID("replicaset-uid"), Controller: &controller,
	}
	tests := []struct {
		name string
		pod  *v1.Pod
		want string
	}{
		{
			name: "explicit PodGroup wins over automatic name",
			pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations:     map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: "explicit-pg"},
				OwnerReferences: []metav1.OwnerReference{deploymentReplicaSetOwner},
			}},
			want: "explicit-pg",
		},
		{
			name: "Deployment Pod derives its ReplicaSet PodGroup",
			pod:  &v1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{deploymentReplicaSetOwner}}},
			want: "podgroup-replicaset-uid",
		},
		{
			name: "ownerless Pod is not associated by its Pod UID fallback",
			pod:  &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("pod-uid")}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := placement.PodGroupName(tt.pod); got != tt.want {
				t.Fatalf("repack placement PodGroup = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPatchRepackPlacementGateDerivesNormalPodGroup(t *testing.T) {
	controller := true
	podGroupName := "podgroup-replicaset-uid"
	run := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: podGroupName, PlannedNodeName: "node-b", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: podGroupName, Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid"},
	}}
	previousClient, previousSchedulers := config.VolcanoClient, config.SchedulerNames
	defer func() {
		config.VolcanoClient = previousClient
		config.SchedulerNames = previousSchedulers
	}()
	config.VolcanoClient = vcfake.NewSimpleClientset(run, podGroup)
	config.SchedulerNames = []string{"volcano"}

	patches, err := patchRepackPlacementGate(&v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "deployment-replacement", OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "workload-7f8d9c", UID: types.UID("replicaset-uid"), Controller: &controller,
			}},
		},
		Spec: v1.PodSpec{SchedulerName: "volcano"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(patches) != 2 || patches[0].Path != "/spec/schedulingGates" {
		t.Fatalf("derived placement gate patches = %#v", patches)
	}
}

func TestPatchRepackPlacementGateSafetyBoundaries(t *testing.T) {
	activeRun := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run", UID: types.UID("run-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status: repackv1alpha1.RepackRunStatus{
			Phase: repackv1alpha1.RepackRunning,
			Relocations: []repackv1alpha1.PodRelocationStatus{{
				Namespace: "ns", PodGroupName: "pg", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
			}},
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "pg", Annotations: map[string]string{repackv1alpha1.PlacementLeaseAnnotation: "run/run-uid"},
	}}
	previousClient, previousSchedulers := config.VolcanoClient, config.SchedulerNames
	defer func() {
		config.VolcanoClient = previousClient
		config.SchedulerNames = previousSchedulers
	}()
	config.SchedulerNames = []string{"volcano"}

	t.Run("non Volcano scheduler is never intercepted", func(t *testing.T) {
		config.VolcanoClient = vcfake.NewSimpleClientset(activeRun, podGroup)
		patches, err := patchRepackPlacementGate(&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: "pg"}},
			Spec:       v1.PodSpec{SchedulerName: "default-scheduler"},
		})
		if err != nil || len(patches) != 0 {
			t.Fatalf("patches = %#v, err = %v", patches, err)
		}
	})

	t.Run("terminal owner does not gate", func(t *testing.T) {
		terminal := activeRun.DeepCopy()
		terminal.Status.Phase = repackv1alpha1.RepackFailed
		config.VolcanoClient = vcfake.NewSimpleClientset(terminal, podGroup)
		patches, err := patchRepackPlacementGate(&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: "pg"}},
			Spec:       v1.PodSpec{SchedulerName: "volcano"},
		})
		if err != nil || len(patches) != 0 {
			t.Fatalf("patches = %#v, err = %v", patches, err)
		}
	})

	t.Run("malformed lease fails open", func(t *testing.T) {
		malformed := podGroup.DeepCopy()
		malformed.Annotations[repackv1alpha1.PlacementLeaseAnnotation] = "malformed"
		config.VolcanoClient = vcfake.NewSimpleClientset(malformed)
		patches, err := patchRepackPlacementGate(&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: "pg"}},
			Spec:       v1.PodSpec{SchedulerName: "volcano"},
		})
		if err != nil || len(patches) != 0 {
			t.Fatalf("patches = %#v, err = %v", patches, err)
		}
	})
}
