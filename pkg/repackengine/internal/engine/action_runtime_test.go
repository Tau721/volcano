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

package engine

import (
	"context"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	k8stesting "k8s.io/client-go/testing"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	fake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	enginescope "volcano.sh/volcano/pkg/repackengine/scope"
	schedapi "volcano.sh/volcano/pkg/scheduler/api"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
)

// pinnedSessionJob builds a scheduler JobInfo whose tasks carry the pod labels
// (volcano.sh/job-name / volcano.sh/job-namespace / volcano.sh/task-spec) that
// map them back to their vcjob task specs, plus a matching vcjob in the fake
// client. taskPinned names which task-spec templates pin spec.nodeName.
func pinnedSessionJob(t *testing.T, jobName, namespace string, taskPinned map[string]bool) (*schedframework.Session, *fake.Clientset, schedapi.JobID) {
	t.Helper()
	jobID := schedapi.JobID(namespace + "/" + jobName)
	tasks := make(map[schedapi.TaskID]*schedapi.TaskInfo)
	for taskSpec := range taskPinned {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Labels: map[string]string{
					batchv1alpha1.JobNameKey:      jobName,
					batchv1alpha1.JobNamespaceKey: namespace,
					batchv1alpha1.TaskSpecKey:     taskSpec,
				},
			},
		}
		taskID := schedapi.TaskID(namespace + "/" + jobName + "/" + taskSpec)
		tasks[taskID] = &schedapi.TaskInfo{
			UID:       taskID,
			Job:       jobID,
			Name:      taskSpec,
			Namespace: namespace,
			TaskRole:  taskSpec,
			Pod:       pod,
		}
	}
	vcjob := &batchv1alpha1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: jobName},
		Spec: batchv1alpha1.JobSpec{Tasks: []batchv1alpha1.TaskSpec{
			{Name: "pinned", Template: v1.PodTemplateSpec{Spec: v1.PodSpec{NodeName: "integration-worker"}}},
			{Name: "free", Template: v1.PodTemplateSpec{Spec: v1.PodSpec{}}},
		}},
	}
	client := fake.NewSimpleClientset(vcjob)
	ssn := &schedframework.Session{
		Jobs: map[schedapi.JobID]*schedapi.JobInfo{
			jobID: {UID: jobID, Name: jobName, Namespace: namespace, Tasks: tasks},
		},
	}
	return ssn, client, jobID
}

// collectPinnedTasks must resolve the vcjob task templates (not the live pods)
// into task UIDs: only the template distinguishes a spec.nodeName pin, because
// the scheduler writes nodeName into every bound pod. The pinned task of the
// in-scope job is returned; an out-of-scope job is ignored entirely.
func TestCollectPinnedTasks(t *testing.T) {
	ssn, client, jobID := pinnedSessionJob(t, "myjob", "ns", map[string]bool{"pinned": true, "free": false})

	t.Run("resolves pinned task-specs to task UIDs", func(t *testing.T) {
		pinned, err := collectPinnedTasks(context.Background(), client, ssn, nil)
		if err != nil {
			t.Fatalf("collectPinnedTasks: %v", err)
		}
		want := sets.New[schedapi.TaskID](schedapi.TaskID(string(jobID) + "/pinned"))
		if !pinned.Equal(want) {
			t.Errorf("pinned=%v, want %v", sets.List(pinned), sets.List(want))
		}
	})

	t.Run("out-of-scope job yields nothing", func(t *testing.T) {
		outOfScope, _ := enginescope.NewMatcher(nil, func(schedapi.JobID) (string, labels.Labels, bool) {
			return "", nil, false // unknown gang: never in scope
		})
		pinned, err := collectPinnedTasks(context.Background(), client, ssn, outOfScope)
		if err != nil {
			t.Fatalf("collectPinnedTasks: %v", err)
		}
		if len(pinned) != 0 {
			t.Errorf("pinned=%v, want empty for out-of-scope job", sets.List(pinned))
		}
	})

	t.Run("in-scope job with no pinned templates", func(t *testing.T) {
		ssnFree, clientFree, _ := pinnedSessionJob(t, "freejob", "ns", map[string]bool{"free": false})
		pinned, err := collectPinnedTasks(context.Background(), clientFree, ssnFree, nil)
		if err != nil {
			t.Fatalf("collectPinnedTasks: %v", err)
		}
		if len(pinned) != 0 {
			t.Errorf("pinned=%v, want empty for a job whose template never pins", sets.List(pinned))
		}
	})

	t.Run("missing vcjob is skipped, not fatal", func(t *testing.T) {
		// Client holds no vcjob for this job: the Get fails and the helper must
		// skip the job (nothing recreates its pods glued to a node) without error.
		orphan := &schedframework.Session{
			Jobs: map[schedapi.JobID]*schedapi.JobInfo{
				"ns/ghost": {
					UID:       "ns/ghost",
					Name:      "ghost",
					Namespace: "ns",
					Tasks: map[schedapi.TaskID]*schedapi.TaskInfo{
						"ns/ghost/t": {
							UID: "ns/ghost/t",
							Job: "ns/ghost",
							Pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: map[string]string{batchv1alpha1.JobNameKey: "ghost"}}},
						},
					},
				},
			},
		}
		pinned, err := collectPinnedTasks(context.Background(), client, orphan, nil)
		if err != nil {
			t.Fatalf("collectPinnedTasks: %v", err)
		}
		if len(pinned) != 0 {
			t.Errorf("pinned=%v, want empty for a job with no vcjob", sets.List(pinned))
		}
	})

	t.Run("a non-NotFound template read error fails the cycle closed", func(t *testing.T) {
		// The F2 defect: any read failure other than NotFound silently made every
		// pod movable, so the planner could drain pinned Pods. The error must
		// propagate so the planning cycle fails with ReasonScopeResolutionFailed.
		ssnErr, clientErr, _ := pinnedSessionJob(t, "errjob", "ns", map[string]bool{"pinned": true})
		clientErr.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("simulated template read failure")
		})
		_, err := collectPinnedTasks(context.Background(), clientErr, ssnErr, nil)
		if err == nil {
			t.Fatal("collectPinnedTasks must propagate a non-NotFound template read error (fail-closed), got nil")
		}
	})
}
