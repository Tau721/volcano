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

package repackcontroller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
	"volcano.sh/repack-controller/pkg/placement"
)

func nominatorWith(runs ...*repackv1alpha1.RepackRun) *Nominator {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range runs {
		_ = idx.Add(r)
	}
	return &Nominator{
		repackRunLister: repacklisters.NewRepackRunLister(idx),
		now:             func() time.Time { return time.Unix(1000, 0) },
	}
}

func TestNominatorSerializesRepackRunStatusWrites(t *testing.T) {
	if nominationWorkerCount != 1 {
		t.Fatalf("nominationWorkerCount = %d, want 1 to serialize the active Execute RepackRun status", nominationWorkerCount)
	}
}

func matchNomination(n *Nominator, pod *corev1.Pod) (*repackv1alpha1.PodRelocationStatus, string) {
	runs, _ := n.repackRunLister.List(labels.Everything())
	candidateHash := testSchedulingRequirementsHash(pod)
	for _, run := range runs {
		nomination, _ := n.matchNomination(run, pod, candidateHash)
		if nomination != nil {
			return nomination, run.Name
		}
	}
	return nil, ""
}

func testSchedulingRequirementsHash(pod *corev1.Pod) string {
	hash, err := placement.SchedulingRequirementsHash(pod)
	if err != nil {
		panic(err)
	}
	return hash
}

func runWithNoms(name string, noms ...repackv1alpha1.PodRelocationStatus) *repackv1alpha1.RepackRun {
	r := &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: repackv1alpha1.RepackModeExecute},
		Status:     repackv1alpha1.RepackRunStatus{Phase: repackv1alpha1.RepackRunning},
	}
	for index := range noms {
		if noms[index].Eviction.Phase == "" {
			noms[index].Eviction.Phase = repackv1alpha1.PodEvictionAccepted
		}
	}
	r.Status.Relocations = noms
	return r
}

func pendingPod(ns, name, pg string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, Labels: labels,
			Annotations: map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: pg},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func expectRecorderEvent(t *testing.T, recorder *record.FakeRecorder, reason string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, reason) {
				return
			}
		case <-deadline:
			t.Fatalf("did not observe event reason %q", reason)
		}
	}
}

func TestNeedsNomination(t *testing.T) {
	ok := pendingPod("ns", "p", "g", nil)
	if !needsNomination(ok) {
		t.Error("a pending, unscheduled, unnominated pod needs nomination")
	}
	cases := map[string]func(*corev1.Pod){
		"scheduled":  func(p *corev1.Pod) { p.Spec.NodeName = "n0" },
		"nominated":  func(p *corev1.Pod) { p.Status.NominatedNodeName = "n0" },
		"notPending": func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"deleting":   func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
	}
	for name, mutate := range cases {
		p := pendingPod("ns", "p", "g", nil)
		mutate(p)
		if needsNomination(p) {
			t.Errorf("%s pod must NOT need nomination", name)
		}
	}
}

func TestMatchNomination(t *testing.T) {
	t.Run("victimPodName exact wins", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodRelocationStatus{Namespace: "ns", PodGroupName: "g", VictimPodName: "w-0", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{}},
		))
		rec, owner := matchNomination(n, pendingPod("ns", "w-0", "g", nil))
		if rec == nil || owner != "r1" || rec.PlannedNodeName != "n2" {
			t.Fatalf("exact victim match failed: rec=%+v owner=%q", rec, owner)
		}
	})

	t.Run("victimPodName exact remains scoped to PodGroup", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodRelocationStatus{Namespace: "ns", PodGroupName: "source", VictimPodName: "w-0", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{}},
		))
		rec, _ := matchNomination(n, pendingPod("ns", "w-0", "concurrent-scale-out", nil))
		if rec != nil {
			t.Fatalf("exact Pod name must not bypass PodGroup identity: %+v", rec)
		}
	})

	t.Run("scheduling requirements hash match", func(t *testing.T) {
		pod := pendingPod("ns", "renamed-xyz", "g", nil)
		pod.Spec.NodeSelector = map[string]string{"accelerator": "npu"}
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodRelocationStatus{
				Namespace: "ns", PodGroupName: "g",
				SchedulingRequirementsHash: testSchedulingRequirementsHash(pod),
				PlannedNodeName:            "n5", Placement: repackv1alpha1.PodPlacementStatus{},
			},
		))
		rec, owner := matchNomination(n, pod)
		if rec == nil || owner != "r1" || rec.PlannedNodeName != "n5" {
			t.Fatalf("scheduling requirements match failed: rec=%+v", rec)
		}
	})

	t.Run("homogeneous PodGroup when scheduling requirements hash is empty", func(t *testing.T) {
		n := nominatorWith(runWithNoms("r1",
			repackv1alpha1.PodRelocationStatus{Namespace: "ns", PodGroupName: "g", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{}},
		))
		rec, owner := matchNomination(n, pendingPod("ns", "any-pod", "g", nil))
		if rec == nil || owner != "r1" || rec.PlannedNodeName != "n1" {
			t.Fatalf("homogeneous PodGroup match failed: rec=%+v", rec)
		}
	})

	t.Run("different scheduling requirements do not match", func(t *testing.T) {
		victim := pendingPod("ns", "old", "g", nil)
		victim.Spec.NodeSelector = map[string]string{"accelerator": "npu"}
		replacement := pendingPod("ns", "new", "g", nil)
		replacement.Spec.NodeSelector = map[string]string{"accelerator": "gpu"}
		n := nominatorWith(runWithNoms("r1", repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "g", VictimPodName: victim.Name,
			SchedulingRequirementsHash: testSchedulingRequirementsHash(victim),
			PlannedNodeName:            "n5", Placement: repackv1alpha1.PodPlacementStatus{},
		}))
		if rec, _ := matchNomination(n, replacement); rec != nil {
			t.Fatalf("different scheduling requirements must not match: %+v", rec)
		}
	})

	t.Run("no match: wrong PodGroup / namespace / bound", func(t *testing.T) {
		bound := repackv1alpha1.PodRelocationStatus{Namespace: "ns", PodGroupName: "g", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementPlaced}}
		wrongPG := repackv1alpha1.PodRelocationStatus{Namespace: "ns", PodGroupName: "other", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{}}
		wrongNS := repackv1alpha1.PodRelocationStatus{Namespace: "elsewhere", PodGroupName: "g", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{}}
		n := nominatorWith(runWithNoms("r1", bound, wrongPG, wrongNS))
		if rec, _ := matchNomination(n, pendingPod("ns", "p", "g", nil)); rec != nil {
			t.Fatalf("should not match any (bound/wrong pg/ns): got %+v", rec)
		}
	})

	t.Run("execution deadline makes every placement unavailable", func(t *testing.T) {
		run := runWithNoms("r1", repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "g", PlannedNodeName: "n1",
		})
		past := metav1.NewTime(time.Unix(500, 0))
		run.Status.ExecutionDeadline = &past
		if rec, _ := matchNomination(nominatorWith(run), pendingPod("ns", "p", "g", nil)); rec != nil {
			t.Fatalf("expired execution must not nominate a Pod: %+v", rec)
		}
	})
}

func TestHomogeneousNominationWaitsForVictimDeletion(t *testing.T) {
	n := nominatorWith(runWithNoms("r1", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "g", VictimPodName: "old", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{},
	}))
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	victim := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "old"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if err := pods.Add(victim); err != nil {
		t.Fatal(err)
	}
	n.podLister = corelisters.NewPodLister(pods)
	replacement := pendingPod("ns", "new-random-name", "g", nil)

	if rec, _ := matchNomination(n, replacement); rec != nil {
		t.Fatalf("prepared nomination must not be consumed while victim exists: %+v", rec)
	}
	if err := pods.Delete(victim); err != nil {
		t.Fatal(err)
	}
	if rec, _ := matchNomination(n, replacement); rec == nil || rec.PlannedNodeName != "n2" {
		t.Fatalf("nomination should activate after victim deletion: %+v", rec)
	}
}

func TestVictimGoneDistinguishesRecreatedPodByUID(t *testing.T) {
	victimUID := types.UID("old-uid")
	current := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "worker", UID: victimUID},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(current); err != nil {
		t.Fatal(err)
	}
	nominator := nominatorWith()
	nominator.podLister = corelisters.NewPodLister(pods)
	relocation := &repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", VictimPodName: current.Name, VictimPodUID: victimUID,
	}

	if nominator.victimGone(relocation) {
		t.Fatal("the original victim UID is still running")
	}

	recreated := current.DeepCopy()
	recreated.UID = "new-uid"
	if err := pods.Update(recreated); err != nil {
		t.Fatal(err)
	}
	if !nominator.victimGone(relocation) {
		t.Fatal("a same-name Pod with a different UID must not keep the original victim alive")
	}

	if err := pods.Update(current); err != nil {
		t.Fatal(err)
	}
	terminating := current.DeepCopy()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	if err := pods.Update(terminating); err != nil {
		t.Fatal(err)
	}
	if !nominator.victimGone(relocation) {
		t.Fatal("the original victim is no longer placement-blocking after termination starts")
	}

	relocation.VictimPodUID = ""
	if nominator.victimGone(relocation) {
		t.Fatal("without a durable victim UID, preserve the conservative name-based behavior")
	}
}

func TestReconcileOpensGateAfterVictimNameIsReused(t *testing.T) {
	replacementPod2 := pendingPod("ns", "pod-2", "group", nil)
	replacementPod2.UID = "new-pod-2-uid"
	replacementPod3 := pendingPod("ns", "pod-3", "group", nil)
	replacementPod3.UID = "new-pod-3-uid"
	replacementPod3.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	replacementPod3.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"

	run := runWithNoms("run",
		repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "group", VictimPodName: "pod-1", VictimPodUID: "old-pod-1-uid",
			PlannedNodeName: "node-a",
			Placement: repackv1alpha1.PodPlacementStatus{
				ReplacementPodName: replacementPod2.Name,
				ReplacementPodUID:  replacementPod2.UID,
				Phase:              repackv1alpha1.PodPlacementWaitingForNodeSelection,
			},
		},
		repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "group", VictimPodName: "pod-2", VictimPodUID: "old-pod-2-uid",
			PlannedNodeName: "node-b",
			Placement:       repackv1alpha1.PodPlacementStatus{},
		},
	)
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, pod := range []*corev1.Pod{replacementPod2, replacementPod3} {
		if err := pods.Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	kubernetesClient := k8sfake.NewSimpleClientset(replacementPod2.DeepCopy(), replacementPod3.DeepCopy())
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		recorder:         record.NewFakeRecorder(10),
		now:              func() time.Time { return time.Unix(1000, 0) },
	}

	if err := nominator.reconcile(context.Background(), "ns/pod-3"); err != nil {
		t.Fatalf("associate pod-3 with the remaining relocation: %v", err)
	}
	updatedRun, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(
		context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	remaining := &updatedRun.Status.Relocations[1]
	if remaining.Placement.ReplacementPodName != replacementPod3.Name ||
		remaining.Placement.ReplacementPodUID != replacementPod3.UID ||
		remaining.Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection {
		t.Fatalf("pod-3 did not durably claim the remaining relocation: %+v", remaining)
	}

	remaining.Placement.SelectedNodeName = remaining.PlannedNodeName
	if _, err := volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(
		context.Background(), updatedRun, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := nominator.reconcile(context.Background(), "ns/pod-3"); err != nil {
		t.Fatalf("nominate pod-3 and open its gate: %v", err)
	}

	updatedPod, err := kubernetesClient.CoreV1().Pods(replacementPod3.Namespace).Get(
		context.Background(), replacementPod3.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedPod.Status.NominatedNodeName != "node-b" {
		t.Fatalf("pod-3 nominatedNodeName = %q, want node-b", updatedPod.Status.NominatedNodeName)
	}
	if hasPlacementGate(updatedPod) {
		t.Fatal("pod-3 placement gate remained closed after its receiver became durable")
	}
}

func TestMatchNominationUsesRecordedReplacementPodGroupForEveryMatchingStrategy(t *testing.T) {
	hashMatchedPod := pendingPod("ns", "new-worker", "new", nil)
	hashMatchedPod.Spec.NodeSelector = map[string]string{"accelerator": "npu"}
	tests := []struct {
		name       string
		nomination repackv1alpha1.PodRelocationStatus
		pod        *corev1.Pod
	}{
		{
			name: "exact Pod name",
			nomination: repackv1alpha1.PodRelocationStatus{
				Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "new",
				VictimPodName: "worker-0", PlannedNodeName: "node-a", Placement: repackv1alpha1.PodPlacementStatus{},
			},
			pod: pendingPod("ns", "worker-0", "new", nil),
		},
		{
			name: "scheduling requirements hash",
			nomination: repackv1alpha1.PodRelocationStatus{
				Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "new",
				VictimPodName: "old-worker", PlannedNodeName: "node-a",
				SchedulingRequirementsHash: testSchedulingRequirementsHash(hashMatchedPod), Placement: repackv1alpha1.PodPlacementStatus{},
			},
			pod: hashMatchedPod,
		},
		{
			name: "homogeneous PodGroup",
			nomination: repackv1alpha1.PodRelocationStatus{
				Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "new",
				VictimPodName: "old-random", PlannedNodeName: "node-a", Placement: repackv1alpha1.PodPlacementStatus{},
			},
			pod: pendingPod("ns", "new-random", "new", nil),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nominator := nominatorWith(runWithNoms("run", test.nomination))
			nomination, owner := matchNomination(nominator, test.pod)
			if nomination == nil || owner != "run" {
				t.Fatalf("recorded replacement PodGroup did not match %s: nomination=%+v owner=%q",
					test.name, nomination, owner)
			}
		})
	}
}

func TestRemovePlacementGateRemovesOwnerMarker(t *testing.T) {
	pod := pendingPod("ns", "scale-out", "group", nil)
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/uid"
	client := k8sfake.NewSimpleClientset(pod)
	recorder := record.NewFakeRecorder(10)
	n := &Nominator{kubernetesClient: client, recorder: recorder}

	if err := n.clearPlacementGate(context.Background(), pod); err != nil {
		t.Fatalf("remove placement gate: %v", err)
	}
	for _, action := range client.Actions() {
		patch, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetResource().Resource != "pods" {
			continue
		}
		body := string(patch.GetPatch())
		if !strings.Contains(body, "/spec/schedulingGates/0") || !strings.Contains(body, "placement-gate-owner") {
			t.Fatalf("patch must remove gate and owner marker, got %s", body)
		}
		expectRecorderEvent(t, recorder, eventReasonPlacementReleased)
		return
	}
	t.Fatal("expected Pod patch")
}

func TestHasClaimableNominationForPodGroup(t *testing.T) {
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
	})
	if !hasClaimableNominationForPodGroup(run, "ns", "group", "", time.Unix(1000, 0)) {
		t.Fatal("homogeneous pending placement in the same PodGroup must remain a potential match")
	}
	run.Status.Relocations[0].SchedulingRequirementsHash = "hash-a"
	if hasClaimableNominationForPodGroup(run, "ns", "group", "hash-b", time.Unix(1000, 0)) {
		t.Fatal("a different scheduling requirements hash must not hold an unrelated Pod")
	}
	if !hasClaimableNominationForPodGroup(run, "ns", "group", "hash-a", time.Unix(1000, 0)) {
		t.Fatal("the same scheduling requirements hash must remain a potential match")
	}
	run.Status.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementPlaced
	if hasClaimableNominationForPodGroup(run, "ns", "group", "hash-a", time.Unix(1000, 0)) {
		t.Fatal("placed nomination must not hold an unrelated Pod")
	}

	run.Status.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementWaitingForNodeSelection
	run.Status.Relocations[0].Placement.ReplacementPodName = "claimed"
	run.Status.Relocations[0].Placement.ReplacementPodUID = "claimed-uid"
	if hasClaimableNominationForPodGroup(run, "ns", "group", "hash-a", time.Unix(1000, 0)) {
		t.Fatal("a placement claimed by another gated Pod must not hold an unrelated Pod")
	}
}

func TestPotentialNominationProtectsOnlyCompatiblePodInUnmappedPodGroup(t *testing.T) {
	controller := true
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "old", VictimPodName: "old-0", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
	})
	run.Status.Plan = &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
		Namespace: "ns", PodGroupName: "old",
		Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
	}}}
	podGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "candidate",
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: placement.OwnerValue(run.Name, run.UID),
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	nominator := &Nominator{
		volcanoClient: vcfake.NewSimpleClientset(podGroup),
		now:           func() time.Time { return time.Unix(1000, 0) },
	}
	pending, err := nominator.hasPotentialNominationForPod(
		context.Background(), run, pendingPod("ns", "candidate-0", "candidate", nil), "")
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("unmapped leased PodGroup must remain gated while its workload has pending placements")
	}

	run.Status.Relocations[0].Placement.Phase = repackv1alpha1.PodPlacementPlaced
	pending, err = nominator.hasPotentialNominationForPod(
		context.Background(), run, pendingPod("ns", "candidate-0", "candidate", nil), "")
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("workload candidate must be released after all placements finish")
	}
}

func TestReconcilePatchesNominatedNodeAndRecordsPlacementNominated(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	pod.UID = "replacement-uid"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "replacement", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID, Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection},
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	runs := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := runs.Add(run); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
	var nominatedNodePatch string
	kubernetesClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetSubresource() != "status" {
			return false, nil, nil
		}
		nominatedNodePatch = string(patchAction.GetPatch())
		return true, pod, nil
	})
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		repackRunLister:  repacklisters.NewRepackRunLister(runs),
		recorder:         record.NewFakeRecorder(10),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	if !strings.Contains(nominatedNodePatch, `"nominatedNodeName":"n2"`) {
		t.Errorf("pod status patch = %s, want nominatedNodeName n2", nominatedNodePatch)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated RepackRun: %v", err)
	}
	if updated.Status.Relocations[0].Placement.Phase != repackv1alpha1.PodPlacementNominated {
		t.Errorf("nomination phase = %q, want %q", updated.Status.Relocations[0].Placement.Phase, repackv1alpha1.PodPlacementNominated)
	}
	if updated.Status.Relocations[0].Placement.ReplacementPodName != "replacement" {
		t.Errorf("replacement pod = %q, want replacement", updated.Status.Relocations[0].Placement.ReplacementPodName)
	}
	opened, err := kubernetesClient.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hasPlacementGate(opened) {
		t.Fatal("selected replacement gate was not opened")
	}
	if opened.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "run/run-uid" {
		t.Fatal("gate owner must remain until actual binding is observed")
	}
	expectRecorderEvent(t, nominator.recorder.(*record.FakeRecorder), eventReasonPlacementNominated)
}

func TestReconcileResumesInterruptedNominationAndOpensGate(t *testing.T) {
	for _, initialPhase := range []repackv1alpha1.PodPlacementPhase{
		repackv1alpha1.PodPlacementWaitingForNodeSelection,
		repackv1alpha1.PodPlacementNominated,
	} {
		t.Run(string(initialPhase), func(t *testing.T) {
			pod := pendingPod("ns", "replacement", "group", nil)
			pod.UID = "replacement-uid"
			pod.Status.NominatedNodeName = "n2"
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
			pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
			run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID, Phase: initialPhase},
			})
			pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if err := pods.Add(pod); err != nil {
				t.Fatal(err)
			}
			kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
			volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
			nominator := &Nominator{
				kubernetesClient: kubernetesClient,
				volcanoClient:    volcanoClient,
				podLister:        corelisters.NewPodLister(pods),
				now:              time.Now,
			}

			if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
				t.Fatalf("reconcile() error = %v", err)
			}
			updatedRun, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if phase := updatedRun.Status.Relocations[0].Placement.Phase; phase != repackv1alpha1.PodPlacementNominated {
				t.Fatalf("nomination phase = %q, want Nominated", phase)
			}
			updatedPod, err := kubernetesClient.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if hasPlacementGate(updatedPod) {
				t.Fatal("an interrupted Nominated transition must eventually open the placement gate")
			}
			if updatedPod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == "" {
				t.Fatal("gate owner must remain until binding is observed")
			}
		})
	}
}

func TestReconcileDoesNotMutatePodBeforeNominationStatusIsDurable(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	pod.UID = "replacement-uid"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID,
			Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection},
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	volcanoClient.PrependReactor("update", "repackruns", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: repackv1alpha1.GroupName, Resource: "repackruns"},
			run.Name, errors.New("forced status conflict"))
	})
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); !apierrors.IsConflict(err) {
		t.Fatalf("reconcile() error = %v, want conflict", err)
	}
	for _, action := range kubernetesClient.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "pods" {
			t.Fatalf("Pod was patched before RepackRun status became durable: %#v", action)
		}
	}
}

func TestReconcileRetriesEveryPodMutationAfterNominationIsDurable(t *testing.T) {
	for _, failurePoint := range []string{"pod-status", "placement-gate"} {
		t.Run(failurePoint, func(t *testing.T) {
			pod := pendingPod("ns", "replacement", "group", nil)
			pod.UID = "replacement-uid"
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
			pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
			run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID,
					Phase: repackv1alpha1.PodPlacementNominated},
			})
			pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if err := pods.Add(pod); err != nil {
				t.Fatal(err)
			}
			kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
			failureInjected := false
			kubernetesClient.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if failureInjected {
					return false, nil, nil
				}
				isStatusPatch := action.GetSubresource() == "status"
				if (failurePoint == "pod-status" && isStatusPatch) || (failurePoint == "placement-gate" && !isStatusPatch) {
					failureInjected = true
					return true, nil, errors.New("forced Pod patch failure")
				}
				return false, nil, nil
			})
			nominator := &Nominator{
				kubernetesClient: kubernetesClient,
				volcanoClient:    vcfake.NewSimpleClientset(run.DeepCopy()),
				podLister:        corelisters.NewPodLister(pods),
				now:              time.Now,
			}

			if err := nominator.reconcile(context.Background(), "ns/replacement"); err == nil {
				t.Fatalf("first reconcile() unexpectedly succeeded at failure point %s", failurePoint)
			}
			partiallyUpdatedPod, err := kubernetesClient.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !hasPlacementGate(partiallyUpdatedPod) {
				t.Fatal("placement gate must remain until every preceding mutation succeeds")
			}

			if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
				t.Fatalf("retry reconcile() error = %v", err)
			}
			updatedPod, err := kubernetesClient.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if updatedPod.Status.NominatedNodeName != "n2" {
				t.Fatalf("nominatedNodeName = %q, want n2", updatedPod.Status.NominatedNodeName)
			}
			if hasPlacementGate(updatedPod) {
				t.Fatal("retry did not open the placement gate")
			}
			if updatedPod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == "" {
				t.Fatal("gate owner must remain until binding is observed")
			}
		})
	}
}

func TestReconcileExpiredNominationClearsGateFromAlreadyNominatedPod(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	pod.UID = "replacement-uid"
	pod.Status.NominatedNodeName = "n2"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{SelectedNodeName: "n2", ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID,
			Phase: repackv1alpha1.PodPlacementTimedOut},
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    vcfake.NewSimpleClientset(run.DeepCopy()),
		podLister:        corelisters.NewPodLister(pods),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
		t.Fatal(err)
	}
	updatedPod, err := kubernetesClient.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hasPlacementGate(updatedPod) || updatedPod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
		t.Fatalf("expired placement metadata was not cleared: gates=%v annotations=%v", updatedPod.Spec.SchedulingGates, updatedPod.Annotations)
	}
}

func TestReconcileGatedReplacementReportsIdentityWithoutOpeningGate(t *testing.T) {
	pod := pendingPod("ns", "replacement", "group", nil)
	pod.UID = "replacement-uid"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "replacement", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	runs := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := runs.Add(run); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset()
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		repackRunLister:  repacklisters.NewRepackRunLister(runs),
		recorder:         record.NewFakeRecorder(10),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/replacement"); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated RepackRun: %v", err)
	}
	nomination := updated.Status.Relocations[0]
	if nomination.Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection {
		t.Errorf("phase = %q, want %q", nomination.Placement.Phase, repackv1alpha1.PodPlacementWaitingForNodeSelection)
	}
	if nomination.Placement.ReplacementPodName != pod.Name || nomination.Placement.ReplacementPodUID != pod.UID {
		t.Errorf("replacement identity = %q/%q, want %q/%q", nomination.Placement.ReplacementPodName, nomination.Placement.ReplacementPodUID, pod.Name, pod.UID)
	}
	for _, action := range kubernetesClient.Actions() {
		if action.GetVerb() == "patch" {
			t.Errorf("gated replacement must not be nominated or opened before engine selection: %#v", action)
		}
	}
	expectRecorderEvent(t, nominator.recorder.(*record.FakeRecorder), eventReasonReplacementGated)
}

func TestObservePlacementRecordsSuccessAndDrift(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		actualNode     string
		expectedPhase  repackv1alpha1.PodPlacementPhase
		expectedReason string
	}{
		{name: "selected node", actualNode: "n2", expectedPhase: repackv1alpha1.PodPlacementPlaced, expectedReason: eventReasonPlacementSucceeded},
		{name: "different node", actualNode: "n3", expectedPhase: repackv1alpha1.PodPlacementPlaced, expectedReason: eventReasonAlternativePlacement},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pod := pendingPod("ns", "replacement", "group", nil)
			pod.UID = "replacement-uid"
			pod.Spec.NodeName = testCase.actualNode
			pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
			run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
				Namespace: "ns", PodGroupName: "group", VictimPodName: "victim",
				PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{SelectedNodeName: "n2",
					ReplacementPodName: pod.Name, ReplacementPodUID: pod.UID,
					Phase: repackv1alpha1.PodPlacementNominated},
			})
			recorder := record.NewFakeRecorder(10)
			volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
			nominator := &Nominator{
				volcanoClient: volcanoClient,
				recorder:      recorder,
			}

			if err := nominator.observePlacement(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := updated.Status.Relocations[0]; got.Placement.Phase != testCase.expectedPhase || got.Placement.ActualNodeName != testCase.actualNode {
				t.Fatalf("observed nomination = %+v, want phase %s on %s", got, testCase.expectedPhase, testCase.actualNode)
			}
			expectRecorderEvent(t, recorder, testCase.expectedReason)
		})
	}
}

func TestReconcileDerivesAutomaticPodGroupBeforeAnnotation(t *testing.T) {
	controller := true
	pod := pendingPod("ns", "deployment-new", "", nil)
	pod.UID = "replacement-uid"
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "deployment-rs",
		UID: types.UID("replicaset-uid"), Controller: &controller,
	}}
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = "run/run-uid"
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "podgroup-replicaset-uid", VictimPodName: "deployment-old",
		PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
	})
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := k8sfake.NewSimpleClientset(pod.DeepCopy())
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    volcanoClient,
		podLister:        corelisters.NewPodLister(pods),
		now:              time.Now,
	}

	if err := nominator.reconcile(context.Background(), "ns/deployment-new"); err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), "run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if nomination := updated.Status.Relocations[0]; nomination.Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection ||
		nomination.Placement.ReplacementPodName != pod.Name || nomination.Placement.ReplacementPodUID != pod.UID {
		t.Fatalf("derived automatic PodGroup did not claim replacement: %+v", nomination)
	}
}

func TestMarkPlacementGatedClaimsFreshUnassignedNomination(t *testing.T) {
	run := runWithNoms("run",
		repackv1alpha1.PodRelocationStatus{Namespace: "ns", PodGroupName: "group", VictimPodName: "old-0", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}},
		repackv1alpha1.PodRelocationStatus{Namespace: "ns", PodGroupName: "group", VictimPodName: "old-1", PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}},
	)
	first := pendingPod("ns", "new-a", "group", nil)
	first.UID = "new-a-uid"
	second := pendingPod("ns", "new-b", "group", nil)
	second.UID = "new-b-uid"
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy())
	nominator := &Nominator{volcanoClient: volcanoClient, now: time.Now}

	if err := nominator.markPlacementGated(context.Background(), run.Name, first, ""); err != nil {
		t.Fatal(err)
	}
	if err := nominator.markPlacementGated(context.Background(), run.Name, second, ""); err != nil {
		t.Fatal(err)
	}
	updated, err := volcanoClient.RepackV1alpha1().RepackRuns().Get(context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for i := range updated.Status.Relocations {
		nomination := &updated.Status.Relocations[i]
		if nomination.Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection {
			t.Fatalf("nomination was not claimed: %+v", nomination)
		}
		got[nomination.Placement.ReplacementPodName] = true
	}
	if !got[first.Name] || !got[second.Name] {
		t.Fatalf("replacement claims = %v, want both Pods", got)
	}
}

func TestReconcileRecoversDeletedReplacementInSamePodGroup(t *testing.T) {
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{ReplacementPodName: "deleted-replacement", ReplacementPodUID: "deleted-uid",
			Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection},
	})
	replacement := pendingPod("ns", "new-replacement", "group", nil)
	replacement.UID = "new-uid"
	replacement.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	replacement.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] =
		placement.OwnerValue(run.Name, run.UID)

	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := pods.Add(replacement); err != nil {
		t.Fatal(err)
	}
	recorder := record.NewFakeRecorder(10)
	nominator := &Nominator{
		kubernetesClient: k8sfake.NewSimpleClientset(replacement.DeepCopy()),
		volcanoClient:    vcfake.NewSimpleClientset(run.DeepCopy()),
		podLister:        corelisters.NewPodLister(pods),
		recorder:         recorder,
		now:              func() time.Time { return time.Unix(1000, 0) },
	}

	if err := nominator.reconcile(context.Background(), "ns/new-replacement"); err != nil {
		t.Fatal(err)
	}
	updated, err := nominator.volcanoClient.RepackV1alpha1().RepackRuns().Get(
		context.Background(), run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nomination := updated.Status.Relocations[0]
	if nomination.Placement.Phase != repackv1alpha1.PodPlacementWaitingForNodeSelection ||
		nomination.Placement.ReplacementPodName != replacement.Name ||
		nomination.Placement.ReplacementPodUID != replacement.UID {
		t.Fatalf("replacement did not take over the recovered placement: %+v", nomination)
	}
	expectRecorderEvent(t, recorder, eventReasonPlacementRecovered)
}

func TestReconcileReleasesScaleOutPodWhenPlacementAlreadyClaimed(t *testing.T) {
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "ns", PodGroupName: "group", VictimPodName: "victim", PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{ReplacementPodName: "claimed", ReplacementPodUID: "claimed-uid",
			Phase: repackv1alpha1.PodPlacementWaitingForNodeSelection},
	})
	claimed := pendingPod("ns", "claimed", "group", nil)
	claimed.UID = "claimed-uid"
	scaleOut := pendingPod("ns", "scale-out", "group", nil)
	scaleOut.UID = "scale-out-uid"
	scaleOut.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	scaleOut.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] =
		placement.OwnerValue(run.Name, run.UID)

	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, pod := range []*corev1.Pod{claimed, scaleOut} {
		if err := pods.Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	recorder := record.NewFakeRecorder(10)
	kubernetesClient := k8sfake.NewSimpleClientset(claimed.DeepCopy(), scaleOut.DeepCopy())
	nominator := &Nominator{
		kubernetesClient: kubernetesClient,
		volcanoClient:    vcfake.NewSimpleClientset(run.DeepCopy()),
		podLister:        corelisters.NewPodLister(pods),
		recorder:         recorder,
		now:              func() time.Time { return time.Unix(1000, 0) },
	}

	if err := nominator.reconcile(context.Background(), "ns/scale-out"); err != nil {
		t.Fatal(err)
	}
	updatedScaleOut, err := kubernetesClient.CoreV1().Pods("ns").Get(
		context.Background(), scaleOut.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hasPlacementGate(updatedScaleOut) ||
		updatedScaleOut.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
		t.Fatalf("unrelated scale-out Pod remained gated: %+v", updatedScaleOut)
	}
	expectRecorderEvent(t, recorder, eventReasonPlacementNotMatched)
	select {
	case event := <-recorder.Events:
		if strings.Contains(event, eventReasonPlacementReleased) {
			t.Fatalf("gate release emitted a redundant event: %s", event)
		}
	default:
	}
}

func TestEnsureReplacementPodGroupRecordsWorkloadRecreation(t *testing.T) {
	controller := true
	run := runWithNoms("run",
		repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "old", VictimPodName: "old-0",
			PlannedNodeName: "n1", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
		},
		repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "old", VictimPodName: "old-1",
			PlannedNodeName: "n2", Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement},
		},
	)
	run.Status.Plan = &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
		Namespace: "ns", PodGroupName: "old",
		Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
	}}}
	newPodGroup := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "new",
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: placement.OwnerValue(run.Name, run.UID),
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	pod := pendingPod("ns", "new-0", "new", nil)
	volcanoClient := vcfake.NewSimpleClientset(run.DeepCopy(), newPodGroup)
	nominator := &Nominator{
		volcanoClient: volcanoClient,
		recorder:      record.NewFakeRecorder(10),
	}

	updated, err := nominator.ensureReplacementPodGroup(context.Background(), run.DeepCopy(), pod)
	if err != nil {
		t.Fatal(err)
	}
	for index := range updated.Status.Relocations {
		if got := updated.Status.Relocations[index].ReplacementPodGroupName; got != "new" {
			t.Fatalf("nomination[%d].replacementPodGroupName = %q, want new", index, got)
		}
	}
	expectRecorderEvent(t, nominator.recorder.(*record.FakeRecorder), eventReasonPodGroupRecreated)

	// Reconciliation is idempotent: the durable mapping is reused and no second
	// status mutation is required.
	again, err := nominator.ensureReplacementPodGroup(context.Background(), updated.DeepCopy(), pod)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status.Relocations[0].ReplacementPodGroupName != "new" {
		t.Fatalf("durable mapping was not retained: %+v", again.Status.Relocations[0])
	}
}

func TestSourcePodGroupForReplacementIncludesNamespace(t *testing.T) {
	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{
		Namespace: "namespace-a", PodGroupName: "old", ReplacementPodGroupName: "new",
	})
	if source, found := sourcePodGroupForReplacement(run, "namespace-a", "new"); !found || source != "old" {
		t.Fatalf("namespace-a/new mapping = %q,%t; want old,true", source, found)
	}
	if source, found := sourcePodGroupForReplacement(run, "namespace-b", "new"); found || source != "" {
		t.Fatalf("namespace-b/new must not reuse namespace-a mapping: %q,%t", source, found)
	}
}

func TestEnsureReplacementPodGroupAdvancesAfterRepeatedRecreation(t *testing.T) {
	controller := true
	run := runWithNoms("run",
		repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "replacement-v1",
			VictimPodName: "worker-0", PlannedNodeName: "node-a", Placement: repackv1alpha1.PodPlacementStatus{SelectedNodeName: "node-b",
				ReplacementPodName: "replacement-v1-0", ReplacementPodUID: types.UID("replacement-v1-uid"),
				Phase: repackv1alpha1.PodPlacementNominated},
		},
		repackv1alpha1.PodRelocationStatus{
			Namespace: "ns", PodGroupName: "old", ReplacementPodGroupName: "replacement-v1",
			VictimPodName: "worker-1", PlannedNodeName: "node-a", Placement: repackv1alpha1.PodPlacementStatus{SelectedNodeName: "node-b",
				ReplacementPodName: "replacement-v1-1", ReplacementPodUID: types.UID("replacement-v1-uid-1"),
				ActualNodeName: "node-b", Phase: repackv1alpha1.PodPlacementPlaced},
		},
	)
	run.Status.Plan = &repackv1alpha1.RepackPlan{Moves: []repackv1alpha1.RepackMove{{
		Namespace: "ns", PodGroupName: "old",
		Owner: &repackv1alpha1.WorkloadRef{APIVersion: "serving.example/v1", Kind: "Serving", Name: "model"},
	}}}
	replacementV2 := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "replacement-v2",
		Annotations: map[string]string{
			repackv1alpha1.PlacementLeaseAnnotation: placement.OwnerValue(run.Name, run.UID),
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example/v1", Kind: "Serving", Name: "model", Controller: &controller,
		}},
	}}
	pod := pendingPod("ns", "worker-0", "replacement-v2", nil)
	nominator := &Nominator{
		volcanoClient: vcfake.NewSimpleClientset(run.DeepCopy(), replacementV2),
		recorder:      record.NewFakeRecorder(10),
		now:           time.Now,
	}

	updated, err := nominator.ensureReplacementPodGroup(context.Background(), run.DeepCopy(), pod)
	if err != nil {
		t.Fatal(err)
	}
	for index := range updated.Status.Relocations {
		nomination := &updated.Status.Relocations[index]
		if nomination.ReplacementPodGroupName != "replacement-v2" {
			t.Fatalf("nomination[%d] replacement PodGroup = %q, want replacement-v2", index, nomination.ReplacementPodGroupName)
		}
		if nomination.Placement.Phase != repackv1alpha1.PodPlacementWaitingForReplacement ||
			nomination.Placement.ReplacementPodName != "" || nomination.Placement.ReplacementPodUID != "" ||
			nomination.Placement.SelectedNodeName != "" || nomination.Placement.ActualNodeName != "" {
			t.Fatalf("nomination[%d] was not reset for the next PodGroup generation: %+v", index, nomination)
		}
	}
	if nomination, _ := nominator.matchNomination(
		updated, pod, testSchedulingRequirementsHash(pod)); nomination == nil {
		t.Fatal("same-name Pod in replacement-v2 did not match the advanced PodGroup mapping")
	}
}

func TestEnqueuePendingForRunUsesPodGroupIndex(t *testing.T) {
	matchingPod := pendingPod("ns", "matching", "target", nil)
	unrelatedPod := pendingPod("ns", "unrelated", "other", nil)
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{podGroupIndexName: podGroupIndex})
	if err := podIndexer.Add(matchingPod); err != nil {
		t.Fatal(err)
	}
	if err := podIndexer.Add(unrelatedPod); err != nil {
		t.Fatal(err)
	}
	nominator := &Nominator{
		podLister:              corelisters.NewPodLister(podIndexer),
		podIndexer:             podIndexer,
		podGroupIndexAvailable: true,
		workQueue:              workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
	defer nominator.workQueue.ShutDown()

	run := runWithNoms("run", repackv1alpha1.PodRelocationStatus{Namespace: "ns", PodGroupName: "target", VictimPodName: "gone", PlannedNodeName: "n1"})
	nominator.enqueuePendingForRun(run)
	if nominator.workQueue.Len() != 1 {
		t.Fatalf("queued pods = %d, want only the indexed matching PodGroup", nominator.workQueue.Len())
	}
	key, _ := nominator.workQueue.Get()
	defer nominator.workQueue.Done(key)
	if key != "ns/matching" {
		t.Errorf("queued key = %q, want ns/matching", key)
	}
}

func TestEnqueueTerminalRunUsesGateOwnerIndex(t *testing.T) {
	run := runWithNoms("run")
	run.Status.Phase = repackv1alpha1.RepackFailed
	pod := pendingPod("ns", "held-scale-out", "group", nil)
	pod.Status.NominatedNodeName = "n2"
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: repackv1alpha1.PlacementGateName}}
	pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] = placement.OwnerValue(run.Name, run.UID)
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		podGroupIndexName:           podGroupIndex,
		placementGateOwnerIndexName: placementGateOwnerIndex,
	})
	if err := podIndexer.Add(pod); err != nil {
		t.Fatal(err)
	}
	nominator := &Nominator{
		podLister:               corelisters.NewPodLister(podIndexer),
		podIndexer:              podIndexer,
		podGroupIndexAvailable:  true,
		gateOwnerIndexAvailable: true,
		workQueue:               workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
	defer nominator.workQueue.ShutDown()

	nominator.enqueuePendingForRun(run)
	if nominator.workQueue.Len() != 1 {
		t.Fatalf("queued pods = %d, want owner-marked Pod", nominator.workQueue.Len())
	}
	key, _ := nominator.workQueue.Get()
	defer nominator.workQueue.Done(key)
	if key != "ns/held-scale-out" {
		t.Fatalf("queued key = %q", key)
	}
}

func TestNominationUnavailableUntilEvictionSucceeds(t *testing.T) {
	for _, phase := range []repackv1alpha1.PodEvictionPhase{
		"",
		repackv1alpha1.PodEvictionPending,
		repackv1alpha1.PodEvictionInProgress,
		repackv1alpha1.PodEvictionRejected,
	} {
		if !nominationUnavailableForClaim(&repackv1alpha1.PodRelocationStatus{Eviction: repackv1alpha1.PodEvictionStatus{Phase: phase}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}}) {
			t.Fatalf("eviction phase %q unexpectedly allowed a replacement claim", phase)
		}
	}
	for _, phase := range []repackv1alpha1.PodEvictionPhase{
		repackv1alpha1.PodEvictionAccepted,
		repackv1alpha1.PodEvictionIndirectlyRemoved,
	} {
		if nominationUnavailableForClaim(&repackv1alpha1.PodRelocationStatus{Eviction: repackv1alpha1.PodEvictionStatus{Phase: phase}, Placement: repackv1alpha1.PodPlacementStatus{Phase: repackv1alpha1.PodPlacementWaitingForReplacement}}) {
			t.Fatalf("eviction phase %q unexpectedly blocked a replacement claim", phase)
		}
	}
}
