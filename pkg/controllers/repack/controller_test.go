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

package repack

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
)

func controllerForRun(run *repackv1alpha1.RepackRun, now time.Time, cooldown time.Duration) (*Controller, *vcfake.Clientset) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	_ = indexer.Add(run)
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	return &Controller{
		volcanoClient:   volcanoClient,
		repackRunLister: repacklisters.NewRepackRunLister(indexer),
		workQueue:       workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		executeCooldown: cooldown,
		now:             func() time.Time { return now },
	}, volcanoClient
}

func terminalRun(name string, mode repackv1alpha1.RepackMode, completionTime time.Time, ttlSeconds int64) *repackv1alpha1.RepackRun {
	ttl := ttlSeconds
	completed := metav1.NewTime(completionTime)
	return &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: repackv1alpha1.RepackRunSpec{
			Mode:                    mode,
			TTLSecondsAfterFinished: &ttl,
		},
		Status: repackv1alpha1.RepackRunStatus{
			Phase:          repackv1alpha1.RepackSucceeded,
			CompletionTime: &completed,
		},
	}
}

func TestControllerReconcileDeletesExpiredRun(t *testing.T) {
	now := time.Unix(10_000, 0)
	run := terminalRun("expired", repackv1alpha1.RepackModeDryRun, now.Add(-2*time.Minute), 1)
	controller, volcanoClient := controllerForRun(run, now, 0)
	defer controller.workQueue.ShutDown()

	if err := controller.reconcile(context.Background(), run.Name); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	_, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expired run should be deleted, get error = %v", err)
	}
}

func TestControllerReconcileRetainsExecuteRunForCooldown(t *testing.T) {
	now := time.Unix(10_000, 0)
	run := terminalRun("cooldown", repackv1alpha1.RepackModeExecute, now.Add(-time.Minute), 1)
	controller, volcanoClient := controllerForRun(run, now, 10*time.Minute)
	defer controller.workQueue.ShutDown()

	if err := controller.reconcile(context.Background(), run.Name); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	if _, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("run must be retained as cooldown anchor, get error = %v", err)
	}
}
