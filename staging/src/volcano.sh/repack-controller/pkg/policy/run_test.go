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
	"context"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"
)

// Startup self-sync: Run gates worker start behind cache.WaitForCacheSync on all
// four informers. Until the stores report synced not even a queued policy is
// reconciled; once they do, the startup Add replay is processed and derives the
// run. Overriding the *Synced predicates keeps the gate observable and
// deterministic without racing real informers.
func TestRunWaitsForSyncThenProcessesPolicyReplay(t *testing.T) {
	due := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute) // cron slot long past
	pol := withCron(policy("pol", "u1", due), everyMin)
	vcClient := vcfake.NewSimpleClientset(pol)
	kubeClient := kubefake.NewSimpleClientset()
	coreFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	vcFactory := vcinformer.NewSharedInformerFactory(vcClient, 0)

	ctrl := New(vcClient, coreFactory, vcFactory, Options{Workers: 1})

	var ready atomic.Bool
	gate := func() bool { return ready.Load() }
	ctrl.policySynced = gate
	ctrl.runSynced = gate
	ctrl.nodeSynced = gate
	ctrl.podSynced = gate

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); ctrl.Run(ctx) }()

	// Stores unsynced: the informers may already have enqueued the policy (Add
	// replay), but no worker runs, so nothing is reconciled.
	time.Sleep(100 * time.Millisecond)
	assertRunCount(t, vcClient, 0)

	// Stores report synced: WaitForCacheSync returns, workers start, the queued
	// replay is reconciled into a derived run.
	ready.Store(true)
	waitForRunCount(t, ctx, vcClient, 1)

	runs, err := vcClient.RepackV1alpha1().RepackRuns().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if got := runs.Items[0].Labels[repackv1alpha1.RepackPolicyLabel]; got != "pol" {
		t.Errorf("derived run policy label = %q, want pol", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
}

func assertRunCount(t *testing.T, vcClient *vcfake.Clientset, want int) {
	t.Helper()
	if got := countRuns(t, vcClient); got != want {
		t.Fatalf("runs = %d, want %d before store sync", got, want)
	}
}

func waitForRunCount(t *testing.T, ctx context.Context, vcClient *vcfake.Clientset, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countRuns(t, vcClient) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runs never reached %d after sync", want)
}

func countRuns(t *testing.T, vcClient *vcfake.Clientset) int {
	t.Helper()
	items, err := vcClient.RepackV1alpha1().RepackRuns().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	return len(items.Items)
}
