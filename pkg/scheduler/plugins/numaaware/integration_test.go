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

package numaaware

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"

	nodeinfov1alpha1 "volcano.sh/apis/pkg/apis/nodeinfo/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/cache"
	"volcano.sh/volcano/pkg/scheduler/conf"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/util"
)

const (
	integrationNamespace = "c1"
	integrationJob       = "pg1"
	integrationNodeName  = "n1"
	integrationPolicy    = "single-numa-node"
)

// integrationEnv is a cache plus an open session with only numa-aware enabled.
type integrationEnv struct {
	cache *cache.SchedulerCache
	ssn   *framework.Session
	close func()
}

func newIntegrationEnv(t *testing.T, pods ...*v1.Pod) *integrationEnv {
	t.Helper()

	framework.RegisterPluginBuilder(PluginName, New)
	t.Cleanup(framework.CleanupPluginBuilders)

	sc := &cache.SchedulerCache{
		Nodes:           make(map[string]*api.NodeInfo),
		Jobs:            make(map[api.JobID]*api.JobInfo),
		Queues:          make(map[api.QueueID]*api.QueueInfo),
		StatusUpdater:   &util.FakeStatusUpdater{},
		HyperNodesInfo:  api.NewHyperNodesInfo(nil),
		Recorder:        record.NewFakeRecorder(100),
		BindFlowChannel: make(chan *cache.BindContext, 100),
	}

	sc.AddOrUpdateNode(util.BuildNode(integrationNodeName,
		api.BuildResourceList("4", "8Gi", []api.ScalarResource{{Name: "pods", Value: "10"}}...),
		map[string]string{}))
	sc.AddNumaInfoV1alpha1(integrationNumatopology(integrationNodeName))
	for _, pod := range pods {
		sc.AddPod(pod)
	}
	sc.AddPodGroupV1beta1(integrationPodGroup())
	sc.AddQueueV1beta1(integrationQueue())

	enabled := true
	ssn := framework.OpenSession(sc, []conf.Tier{{
		Plugins: []conf.PluginOption{{Name: PluginName, EnabledPredicate: &enabled}},
	}}, nil)

	closed := false
	closeSession := func() {
		if closed {
			return
		}
		closed = true
		framework.CloseSession(ssn)
	}
	t.Cleanup(closeSession)

	return &integrationEnv{cache: sc, ssn: ssn, close: closeSession}
}

// allocateIntegrationPod runs the predicate and commits an allocate, which submits the bind.
func allocateIntegrationPod(t *testing.T, env *integrationEnv, pod *v1.Pod) {
	t.Helper()

	task := integrationTask(t, env.ssn, pod)
	node := env.ssn.Nodes[integrationNodeName]
	require.NotNil(t, node, "node %s should be in the session", integrationNodeName)
	require.NoError(t, env.ssn.PredicateForAllocateAction(task, node))

	stmt := env.ssn.Statement()
	require.NoError(t, stmt.Allocate(task, node))
	stmt.Commit()
}

func integrationTask(t *testing.T, ssn *framework.Session, pod *v1.Pod) *api.TaskInfo {
	t.Helper()

	jobID := api.JobID(fmt.Sprintf("%s/%s", pod.Namespace, pod.Annotations[schedulingv1beta1.KubeGroupNameAnnotationKey]))
	job, ok := ssn.Jobs[jobID]
	require.True(t, ok, "job %s should be in the session", jobID)
	task, ok := job.Tasks[api.TaskID(pod.UID)]
	require.True(t, ok, "task for pod %s should be in the session", pod.Name)
	return task
}

func integrationPodMeta(pod *v1.Pod) api.PodMeta {
	return api.PodMeta{UID: pod.UID, Name: pod.Name, Namespace: pod.Namespace}
}

func integrationNumatopology(name string) *nodeinfov1alpha1.Numatopology {
	return &nodeinfov1alpha1.Numatopology{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: nodeinfov1alpha1.NumatopoSpec{
			Policies: map[nodeinfov1alpha1.PolicyName]string{
				nodeinfov1alpha1.CPUManagerPolicy:      "static",
				nodeinfov1alpha1.TopologyManagerPolicy: integrationPolicy,
			},
			NumaResMap: map[string]nodeinfov1alpha1.ResourceInfo{
				string(v1.ResourceCPU): {Allocatable: "0-3", Capacity: 4},
			},
			// CPUs 0,1 on NUMA node 0; CPUs 2,3 on NUMA node 1.
			CPUDetail: map[string]nodeinfov1alpha1.CPUInfo{
				"0": {NUMANodeID: 0},
				"1": {NUMANodeID: 0},
				"2": {NUMANodeID: 1},
				"3": {NUMANodeID: 1},
			},
		},
	}
}

func integrationGuaranteedPod(name string, cpu int64) *v1.Pod {
	requests := api.BuildResourceList(fmt.Sprintf("%d", cpu), "1Gi")
	pod := util.BuildPod(integrationNamespace, name, "", v1.PodPending, requests, integrationJob,
		map[string]string{}, map[string]string{})
	pod.Spec.Containers[0].Resources.Limits = requests.DeepCopy()
	pod.Annotations[schedulingv1beta1.NumaPolicyKey] = integrationPolicy
	return pod
}

func integrationBestEffortPod(name string) *v1.Pod {
	return util.BuildPod(integrationNamespace, name, "", v1.PodPending, nil, integrationJob,
		map[string]string{}, map[string]string{})
}

func integrationPodGroup() *schedulingv1beta1.PodGroup {
	return &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{Name: integrationJob, Namespace: integrationNamespace},
		Spec:       schedulingv1beta1.PodGroupSpec{Queue: integrationNamespace},
	}
}

func integrationQueue() *schedulingv1beta1.Queue {
	return &schedulingv1beta1.Queue{
		ObjectMeta: metav1.ObjectMeta{Name: integrationNamespace},
		Spec:       schedulingv1beta1.QueueSpec{Weight: 1},
	}
}

// Covers predicate -> bind context extension -> AddBindTask -> UnassignedNumaPods.
func TestIntegration_AllocateWritesNumaReservation(t *testing.T) {
	pod := integrationGuaranteedPod("p1", 2)
	env := newIntegrationEnv(t, pod)
	allocateIntegrationPod(t, env, pod)

	resSets, ok := env.cache.Nodes[integrationNodeName].UnassignedNumaPods[integrationPodMeta(pod)]
	require.True(t, ok, "a reservation must be written when the bind is submitted")

	cpus := resSets[string(v1.ResourceCPU)]
	require.Equal(t, 2, cpus.Size(), "the pod's full CPU request must be reserved")

	numaNodes := sets.New[int]()
	for _, cpuID := range cpus.List() {
		numaNodes.Insert(env.cache.Nodes[integrationNodeName].NumaInfo.CPUDetail[cpuID].NUMANodeID)
	}
	assert.Len(t, numaNodes, 1, "single-numa-node must allocate CPUs from one NUMA node")
}

// A pipelined task never enters the bind flow, so it must not be recorded.
func TestIntegration_PipelineDoesNotWriteNumaReservation(t *testing.T) {
	pod := integrationGuaranteedPod("p2", 2)
	env := newIntegrationEnv(t, pod)

	task := integrationTask(t, env.ssn, pod)
	node := env.ssn.Nodes[integrationNodeName]
	require.NoError(t, env.ssn.PredicateForAllocateAction(task, node))

	stmt := env.ssn.Statement()
	require.NoError(t, stmt.Pipeline(task, integrationNodeName, false))
	stmt.Commit()

	_, ok := env.cache.Nodes[integrationNodeName].UnassignedNumaPods[integrationPodMeta(pod)]
	assert.False(t, ok, "pipeline must not record a reservation")

	env.close()

	_, ok = env.cache.Nodes[integrationNodeName].UnassignedNumaPods[integrationPodMeta(pod)]
	assert.False(t, ok, "session close must not write pipelined decisions")
}

// Non-Guaranteed pods are dispatched but outside the numa-aware scope.
func TestIntegration_NonGuaranteedPodDoesNotWriteNumaReservation(t *testing.T) {
	pod := integrationBestEffortPod("p3")
	env := newIntegrationEnv(t, pod)
	allocateIntegrationPod(t, env, pod)

	_, ok := env.cache.Nodes[integrationNodeName].UnassignedNumaPods[integrationPodMeta(pod)]
	assert.False(t, ok, "non-Guaranteed pods are not managed by numa-aware")
}

// DeletePod is the event-driven cleanup path.
func TestIntegration_DeletePodClearsNumaReservation(t *testing.T) {
	pod := integrationGuaranteedPod("p4", 2)
	env := newIntegrationEnv(t, pod)
	allocateIntegrationPod(t, env, pod)

	meta := integrationPodMeta(pod)
	_, ok := env.cache.Nodes[integrationNodeName].UnassignedNumaPods[meta]
	require.True(t, ok, "precondition: reservation must exist")

	env.cache.DeletePod(pod)

	_, ok = env.cache.Nodes[integrationNodeName].UnassignedNumaPods[meta]
	assert.False(t, ok, "pod deletion must release the reservation")
}

// Snapshot keeps reservations for tasks that still exist on the entry node and drops the rest.
func TestIntegration_SnapshotPrunesByTaskState(t *testing.T) {
	tests := []struct {
		name     string
		status   api.TaskStatus
		taskNode string
		dropTask bool
		wantKept bool
	}{
		{name: "in-flight binding reservation is kept", status: api.Binding, wantKept: true},
		{name: "releasing reservation is kept", status: api.Releasing, wantKept: true},
		{name: "unknown reservation is kept", status: api.Unknown, wantKept: true},
		{name: "reservation of a task bound to another node is pruned", status: api.Binding, taskNode: "n2"},
		{name: "pending reservation is pruned", status: api.Pending},
		{name: "pipelined reservation is pruned", status: api.Pipelined},
		{name: "succeeded reservation is pruned", status: api.Succeeded},
		{name: "failed reservation is pruned", status: api.Failed},
		{name: "reservation without a cached task is pruned", dropTask: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := integrationGuaranteedPod("p1", 2)
			env := newIntegrationEnv(t, pod)
			allocateIntegrationPod(t, env, pod)

			meta := integrationPodMeta(pod)
			_, ok := env.cache.Nodes[integrationNodeName].UnassignedNumaPods[meta]
			require.True(t, ok, "precondition: reservation must exist")

			jobID := api.JobID(fmt.Sprintf("%s/%s", pod.Namespace,
				pod.Annotations[schedulingv1beta1.KubeGroupNameAnnotationKey]))
			if tt.dropTask {
				delete(env.cache.Jobs, jobID)
			} else {
				task, ok := env.cache.Jobs[jobID].Tasks[api.TaskID(pod.UID)]
				require.True(t, ok, "precondition: task must be in the cache")
				task.Status = tt.status
				if tt.taskNode != "" {
					task.NodeName = tt.taskNode
				}
			}

			env.cache.Snapshot()

			_, kept := env.cache.Nodes[integrationNodeName].UnassignedNumaPods[meta]
			assert.Equal(t, tt.wantKept, kept)
		})
	}
}
