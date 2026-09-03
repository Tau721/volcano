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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	repackinformers "volcano.sh/apis/pkg/client/informers/externalversions/repack/v1alpha1"
	repacklisters "volcano.sh/apis/pkg/client/listers/repack/v1alpha1"
	"volcano.sh/repack-controller/pkg/placement"
)

const (
	podGroupIndexName           = "repack.volcano.sh/pod-group"
	placementGateOwnerIndexName = "repack.volcano.sh/placement-gate-owner"
	nominationVictimIndexName   = "repack.volcano.sh/nomination-victim"
)

// Nominator is the placement-steering reconciler: it watches Pods and, for a not-
// yet-scheduled replacement pod, looks up a matching PodRelocationStatus in some
// RepackRun.status.relocations[] and writes pod.status.nominatedNodeName so the
// scheduler prefers the repack-recommended node. It is best-effort and soft — a
// nomination is only a hint; if the node has since filled, the scheduler is free
// to place the pod elsewhere (no reservation; §4.7.1.2).
//
// Why here and not the PodGroup controller: the injector must cover every gang
// kind (vcjob/Deployment/StatefulSet/RawPod), must own the status.relocations[]
// lifecycle, and is conceptually part of the repack control loop — so it lives
// with the RepackRun controller, decoupled from any single workload controller.
type Nominator struct {
	kubernetesClient        kubernetes.Interface
	volcanoClient           vcclientset.Interface
	podLister               corelisters.PodLister
	repackRunLister         repacklisters.RepackRunLister
	podIndexer              cache.Indexer
	repackRunIndexer        cache.Indexer
	podGroupIndexAvailable  bool
	gateOwnerIndexAvailable bool
	victimIndexAvailable    bool
	informerSyncs           []cache.InformerSynced
	workQueue               workqueue.TypedRateLimitingInterface[string]
	recorder                record.EventRecorder
	now                     func() time.Time
}

// nominationWorkerCount is deliberately one. Execute admission already permits
// only one active RepackRun, and every replacement transition updates that same
// RepackRun status object. Parallel Pod workers therefore create resourceVersion
// conflicts without providing useful status-write parallelism.
const nominationWorkerCount = 1

const (
	eventReasonReplacementGated     = "RepackReplacementGated"
	eventReasonPlacementNominated   = "RepackPlacementNominated"
	eventReasonPlacementSucceeded   = "RepackPlacementSucceeded"
	eventReasonAlternativePlacement = "RepackAlternativePlacement"
	eventReasonPlacementGateOpened  = "RepackPlacementGateOpened"
	eventReasonPlacementReleased    = "RepackPlacementReleased"
	eventReasonPlacementNotMatched  = "RepackPlacementNotMatched"
	eventReasonPlacementRecovered   = "RepackPlacementRecovered"
	eventReasonPodGroupRecreated    = "RepackPodGroupRecreated"
)

type nominationMatchMethod string

const (
	nominationMatchedByReplacementPodUID      nominationMatchMethod = "ReplacementPodUID"
	nominationMatchedByVictimName             nominationMatchMethod = "VictimPodName"
	nominationMatchedBySchedulingRequirements nominationMatchMethod = "SchedulingRequirementsHash"
	nominationMatchedByHomogeneousPodGroup    nominationMatchMethod = "HomogeneousPodGroup"
)

// NewEventRecorder creates a Pod event recorder for the placement protocol.
// Pod events complement RepackRun events: operators can diagnose why a concrete
// replacement Pod is gated, nominated, placed on an alternative node, or
// released from kubectl describe.
func NewEventRecorder(kubernetesClient kubernetes.Interface, component string) record.EventRecorder {
	if kubernetesClient == nil {
		return nil
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubernetesClient.CoreV1().Events("")})
	return broadcaster.NewRecorder(kubescheme.Scheme, corev1.EventSource{Component: component})
}

// SetEventRecorder enables Kubernetes events. It is optional so unit tests and
// embedders that do not need events retain a lightweight constructor.
func (n *Nominator) SetEventRecorder(recorder record.EventRecorder) {
	n.recorder = recorder
}

func (n *Nominator) recordPodEvent(pod *corev1.Pod, eventType, reason, message string) {
	if n == nil || n.recorder == nil || pod == nil {
		return
	}
	n.recorder.Event(pod, eventType, reason, message)
}

// NewNominator wires the reconciler to Pod and RepackRun informers. Watching
// both sides is important: a replacement Pod can be observed before the
// prepared nomination status reaches this controller's informer. A later
// RepackRun update must therefore wake the already-existing Pending Pod.
func NewNominator(kubernetesClient kubernetes.Interface, volcanoClient vcclientset.Interface, podInformer coreinformers.PodInformer, repackRunInformer repackinformers.RepackRunInformer) *Nominator {
	podGroupIndexAvailable := true
	gateOwnerIndexAvailable := true
	if err := podInformer.Informer().AddIndexers(cache.Indexers{
		podGroupIndexName:           podGroupIndex,
		placementGateOwnerIndexName: placementGateOwnerIndex,
	}); err != nil {
		klog.ErrorS(err, "repack nominator: add Pod indexes; falling back to namespace scan")
		podGroupIndexAvailable = false
		gateOwnerIndexAvailable = false
	}
	victimIndexAvailable := true
	if err := repackRunInformer.Informer().AddIndexers(cache.Indexers{nominationVictimIndexName: nominationVictimIndex}); err != nil {
		klog.ErrorS(err, "repack nominator: add nomination victim index; falling back to RepackRun scan")
		victimIndexAvailable = false
	}
	n := &Nominator{
		kubernetesClient:        kubernetesClient,
		volcanoClient:           volcanoClient,
		podLister:               podInformer.Lister(),
		repackRunLister:         repackRunInformer.Lister(),
		podIndexer:              podInformer.Informer().GetIndexer(),
		repackRunIndexer:        repackRunInformer.Informer().GetIndexer(),
		podGroupIndexAvailable:  podGroupIndexAvailable,
		gateOwnerIndexAvailable: gateOwnerIndexAvailable,
		victimIndexAvailable:    victimIndexAvailable,
		informerSyncs:           []cache.InformerSynced{podInformer.Informer().HasSynced, repackRunInformer.Informer().HasSynced},
		workQueue:               workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		now:                     time.Now,
	}
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    n.enqueue,
		UpdateFunc: func(_, newObj interface{}) { n.enqueue(newObj) },
		DeleteFunc: n.enqueueAfterVictimDeleted,
	})
	repackRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    n.enqueuePendingForRun,
		UpdateFunc: func(_, newObj interface{}) { n.enqueuePendingForRun(newObj) },
		DeleteFunc: n.enqueueGatedPodsForDeletedRun,
	})
	return n
}

func (n *Nominator) enqueue(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	if pod.Spec.NodeName != "" {
		if pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
			key, err := cache.MetaNamespaceKeyFunc(pod)
			if err == nil {
				n.workQueue.Add(key)
			}
		}
		return
	}
	// A Repack-owned gate/owner marker represents an in-flight protocol that must
	// be reconciled to completion even after nominatedNodeName has been written.
	// In particular, a conflict or restart during status persistence, Pod status
	// patching, or gate removal must not make the Pod disappear from this controller.
	if hasPlacementGate(pod) || pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
		key, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		n.workQueue.Add(key)
		return
	}
	if !needsNomination(pod) {
		return // already nominated, terminating, or not Pending
	}
	key, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	n.workQueue.Add(key)
}

func podGroupIndexKey(namespace, podGroupName string) string {
	return (types.NamespacedName{Namespace: namespace, Name: podGroupName}).String()
}

func nominationVictimIndexKey(namespace, victimPodName string) string {
	return (types.NamespacedName{Namespace: namespace, Name: victimPodName}).String()
}

func podGroupIndex(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	podGroupName := placement.PodGroupName(pod)
	if podGroupName == "" {
		return nil, nil
	}
	return []string{podGroupIndexKey(pod.Namespace, podGroupName)}, nil
}

func placementGateOwnerIndex(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	owner := pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation]
	if owner == "" {
		return nil, nil
	}
	return []string{owner}, nil
}

func nominationVictimIndex(obj interface{}) ([]string, error) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok {
		return nil, nil
	}
	keys := make([]string, 0, len(run.Status.Relocations))
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if nominationUnavailableForClaim(nomination) || nomination.VictimPodName == "" {
			continue
		}
		keys = append(keys, nominationVictimIndexKey(nomination.Namespace, nomination.VictimPodName))
	}
	return keys, nil
}

func (n *Nominator) enqueuePodByName(namespace, podName string) {
	if namespace == "" || podName == "" {
		return
	}
	pod, err := n.podLister.Pods(namespace).Get(podName)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	n.enqueue(pod)
}

// enqueuePendingForRun closes the informer-ordering race: when nomination
// intents become visible, revisit Pending Pods that may already have emitted
// their Add event. The PodGroup index limits this to pods that can actually
// match an active nomination, rather than scanning an entire namespace on every
// status update.
func (n *Nominator) enqueuePendingForRun(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok {
		return
	}
	n.enqueueGatedPodsForOwner(placement.OwnerValue(run.Name, run.UID))
	if !placementRunActive(run) {
		return
	}
	podGroups := map[string]bool{}
	namespaces := map[string]bool{}
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if !nominationUnavailableForClaim(nomination) {
			namespaces[nomination.Namespace] = true
			if nomination.PodGroupName != "" {
				podGroups[podGroupIndexKey(nomination.Namespace, nomination.PodGroupName)] = true
			}
			if nomination.ReplacementPodGroupName != "" {
				podGroups[podGroupIndexKey(nomination.Namespace, nomination.ReplacementPodGroupName)] = true
			}
			n.enqueuePodByName(nomination.Namespace, nomination.VictimPodName)
		}
	}
	for podGroupKey := range podGroups {
		if !n.podGroupIndexAvailable || n.podIndexer == nil {
			continue
		}
		objects, err := n.podIndexer.ByIndex(podGroupIndexName, podGroupKey)
		if err != nil {
			utilruntime.HandleError(err)
			continue
		}
		for _, object := range objects {
			n.enqueue(object)
		}
	}
	klog.V(4).InfoS("repack nominator: nomination update enqueued candidate Pods", "run", run.Name,
		"activeNominationPodGroups", len(podGroups), "podGroupIndexEnabled", n.podGroupIndexAvailable,
		"namespaceFallbackCount", len(namespaces))
	if !n.podGroupIndexAvailable {
		for namespace := range namespaces {
			pods, err := n.podLister.Pods(namespace).List(labels.Everything())
			if err != nil {
				utilruntime.HandleError(err)
				continue
			}
			for _, pod := range pods {
				n.enqueue(pod)
			}
		}
	}
}

func (n *Nominator) enqueueGatedPodsForDeletedRun(obj interface{}) {
	run, ok := obj.(*repackv1alpha1.RepackRun)
	if !ok {
		if tombstone, tombstoneOK := obj.(cache.DeletedFinalStateUnknown); tombstoneOK {
			run, ok = tombstone.Obj.(*repackv1alpha1.RepackRun)
		}
	}
	if !ok || run == nil {
		return
	}
	n.enqueueGatedPodsForOwner(placement.OwnerValue(run.Name, run.UID))
}

func (n *Nominator) enqueueGatedPodsForOwner(owner string) {
	if owner == "" {
		return
	}
	if n.gateOwnerIndexAvailable && n.podIndexer != nil {
		objects, err := n.podIndexer.ByIndex(placementGateOwnerIndexName, owner)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		for _, object := range objects {
			n.enqueue(object)
		}
		return
	}
	if n.podLister == nil {
		return
	}
	pods, err := n.podLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	for _, pod := range pods {
		if pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == owner {
			n.enqueue(pod)
		}
	}
}

// enqueueAfterVictimDeleted wakes a renamed replacement that was held back
// while the original victim still existed. Exact-name replacements are
// naturally handled by their own Add event.
func (n *Nominator) enqueueAfterVictimDeleted(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		if tombstone, tombstoneOK := obj.(cache.DeletedFinalStateUnknown); tombstoneOK {
			pod, ok = tombstone.Obj.(*corev1.Pod)
		}
	}
	if !ok || pod == nil {
		return
	}
	if n.victimIndexAvailable && n.repackRunIndexer != nil {
		objects, err := n.repackRunIndexer.ByIndex(nominationVictimIndexName, nominationVictimIndexKey(pod.Namespace, pod.Name))
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		for _, object := range objects {
			n.enqueuePendingForRun(object)
		}
		klog.V(4).InfoS("repack nominator: victim deletion matched nomination runs by index",
			"namespace", pod.Namespace, "victimPod", pod.Name, "matchedRunCount", len(objects))
		return
	}
	runs, err := n.repackRunLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	for _, run := range runs {
		for index := range run.Status.Relocations {
			nomination := &run.Status.Relocations[index]
			if nomination.Namespace == pod.Namespace && nomination.VictimPodName == pod.Name &&
				!nominationUnavailableForClaim(nomination) {
				n.enqueuePendingForRun(run)
				return
			}
		}
	}
}

// Run launches the reconciler until ctx is cancelled. The caller starts the
// informer factories.
func (n *Nominator) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	defer n.workQueue.ShutDown()
	if !cache.WaitForCacheSync(ctx.Done(), n.informerSyncs...) {
		return fmt.Errorf("nominator: cache failed to sync")
	}
	klog.V(3).InfoS("Starting repack nominator", "workers", nominationWorkerCount,
		"podGroupIndexEnabled", n.podGroupIndexAvailable, "gateOwnerIndexEnabled", n.gateOwnerIndexAvailable,
		"victimIndexEnabled", n.victimIndexAvailable)
	for i := 0; i < nominationWorkerCount; i++ {
		go func() {
			for n.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	return nil
}

func (n *Nominator) processNext(ctx context.Context) bool {
	key, shutdown := n.workQueue.Get()
	if shutdown {
		return false
	}
	defer n.workQueue.Done(key)
	if err := n.reconcile(ctx, key); err != nil {
		utilruntime.HandleError(fmt.Errorf("nominate pod %q: %w", key, err))
		klog.V(4).InfoS("repack nominator: reconcile failed; requeueing with rate limit",
			"pod", key, "retryCount", n.workQueue.NumRequeues(key)+1, "error", err)
		n.workQueue.AddRateLimited(key)
		return true
	}
	n.workQueue.Forget(key)
	return true
}

func (n *Nominator) reconcile(ctx context.Context, key string) error {
	namespace, podName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}
	pod, err := n.podLister.Pods(namespace).Get(podName)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	klog.V(4).InfoS("repack nominator: reconciling Pod",
		"pod", key, "phase", pod.Status.Phase, "nodeName", pod.Spec.NodeName,
		"nominatedNodeName", pod.Status.NominatedNodeName,
		"podGroup", placement.PodGroupName(pod),
		"placementGate", hasPlacementGate(pod),
		"gateOwner", pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation])
	if pod.Spec.NodeName != "" {
		if err := n.observePlacement(ctx, pod); err != nil {
			return err
		}
		return n.clearPlacementGate(ctx, pod)
	}
	hasGate := hasPlacementGate(pod)
	gateOwner := pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation]
	if !hasGate && gateOwner == "" {
		// Pods outside the placement protocol have no durable owner from which to
		// recover a nomination. Ordinary already-nominated Pods are not ours.
		return nil
	}

	run, err := n.placementRunForPod(ctx, pod)
	if err != nil {
		return err
	}
	if run == nil || !placementRunActive(run) {
		klog.V(4).InfoS("repack nominator: releasing placement metadata because owning Run is absent or inactive",
			"pod", key, "gateOwner", gateOwner)
		return n.clearPlacementGate(ctx, pod)
	}
	run, err = n.ensureReplacementPodGroup(ctx, run, pod)
	if err != nil {
		return err
	}

	// Prefer the durable concrete association even for Nominated records. Generic
	// matching deliberately treats Nominated as consumed, but an interrupted
	// Nominated -> gate-open transition must be resumed rather than forgotten.
	nomination := nominationForReplacement(run, pod)
	if nomination != nil {
		switch nomination.Placement.Phase {
		case repackv1alpha1.PodPlacementPlaced,
			repackv1alpha1.PodPlacementTimedOut:
			return n.clearPlacementGate(ctx, pod)
		}
	}

	if !hasGate {
		if placementAwaitingBinding(run, pod) {
			return nil
		}
		return n.clearPlacementGate(ctx, pod)
	}

	candidateSchedulingRequirementsHash, err := placement.SchedulingRequirementsHash(pod)
	if err != nil {
		return fmt.Errorf("derive scheduling requirements for candidate Pod %s: %w", key, err)
	}
	run, staleReplacementPodName, err := n.recoverStalePlacementClaim(
		ctx, run, pod, candidateSchedulingRequirementsHash)
	if err != nil {
		return err
	}
	if staleReplacementPodName != "" {
		klog.V(3).InfoS("repack nominator: recovered placement from a stale replacement Pod claim",
			"run", run.Name, "staleReplacementPod", pod.Namespace+"/"+staleReplacementPodName,
			"newReplacementPod", key, "podGroup", placement.PodGroupName(pod))
		n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPlacementRecovered,
			fmt.Sprintf("RepackRun %s released the stale claim previously held by replacement Pod %s and will retry placement with this Pod.",
				run.Name, staleReplacementPodName))
	}

	// Re-read the concrete association after stale-claim recovery. A repeated
	// reconcile of the same Pod resumes its durable state; a newly-created Pod
	// proceeds through normal matching below.
	nomination = nominationForReplacement(run, pod)
	matchMethod := nominationMatchMethod("")
	if nomination != nil {
		matchMethod = nominationMatchedByReplacementPodUID
	}
	if nomination == nil {
		nomination, matchMethod = n.matchNomination(run, pod, candidateSchedulingRequirementsHash)
	}
	if nomination == nil {
		potentialMatch, err := n.hasPotentialNominationForPod(
			ctx, run, pod, candidateSchedulingRequirementsHash)
		if err != nil {
			return err
		}
		if potentialMatch {
			// The candidate has the same scheduling requirements as unfinished
			// replacement work, but may still be waiting for victim deletion or
			// durable PodGroup recreation mapping. Keep only this potentially
			// matching Pod gated; an unrelated scale-out Pod is released below.
			klog.V(4).InfoS("repack nominator: retaining placement gate for a potential replacement Pod",
				"pod", key, "run", run.Name, "podGroup", placement.PodGroupName(pod),
				"schedulingRequirementsHash", candidateSchedulingRequirementsHash)
			return nil
		}
		klog.V(3).InfoS("repack nominator: releasing unmatched placement gate from concurrent or unrelated Pod",
			"pod", key, "run", run.Name, "podGroup", placement.PodGroupName(pod),
			"schedulingRequirementsHash", candidateSchedulingRequirementsHash)
		return n.clearPlacementGateWithReason(ctx, pod, eventReasonPlacementNotMatched,
			fmt.Sprintf("No unfinished placement in RepackRun %s matched this Pod; its placement gate was removed.", run.Name))
	}
	klog.V(4).InfoS("repack nominator: matched replacement Pod to placement intent",
		"pod", key, "run", run.Name, "podGroup", nomination.PodGroupName,
		"victimPod", nomination.VictimPodName, "plannedNode", nomination.PlannedNodeName,
		"selectedNode", nomination.Placement.SelectedNodeName, "matchMethod", matchMethod,
		"schedulingRequirementsHash", nomination.SchedulingRequirementsHash)
	if nomination.Placement.SelectedNodeName == "" {
		// The admission webhook has stopped the scheduler. Hand the placement
		// decision to the engine, which owns the scheduler session and can choose
		// a current receiver without duplicating predicate logic here.
		return n.markPlacementGated(ctx, run.Name, pod, candidateSchedulingRequirementsHash)
	}

	selectedNode := nomination.Placement.SelectedNodeName
	// Persist the concrete association and selected receiver before mutating the
	// Pod. Every following operation is then recoverable from Run status after a
	// conflict, process restart, or partial API failure.
	if err := n.markPlacementNominated(ctx, run.Name, nomination, pod, selectedNode); err != nil {
		return err
	}
	if pod.Status.NominatedNodeName != selectedNode {
		if err := n.patchNominatedNode(ctx, pod, selectedNode); err != nil {
			return err
		}
	}
	if err := n.openPlacementGate(ctx, pod); err != nil {
		return err
	}
	klog.V(3).InfoS("repack placement nominated replacement pod", "pod", key, "node", selectedNode, "repackRun", run.Name)
	n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPlacementNominated,
		fmt.Sprintf("RepackRun %s selected node %s for this replacement Pod.", run.Name, selectedNode))
	return nil
}

// nominationForReplacement returns the durable placement record already claimed
// by this concrete replacement. Unlike generic matching, it intentionally sees
// Nominated records so an interrupted status-patch/gate-open sequence can resume.
func nominationForReplacement(run *repackv1alpha1.RepackRun, pod *corev1.Pod) *repackv1alpha1.PodRelocationStatus {
	if run == nil || pod == nil || pod.UID == "" {
		return nil
	}
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if nomination.Namespace == pod.Namespace && nomination.Placement.ReplacementPodName == pod.Name &&
			nomination.Placement.ReplacementPodUID == pod.UID {
			return nomination
		}
	}
	return nil
}

// recoverStalePlacementClaim releases one non-terminal placement whose
// previously-associated replacement Pod no longer exists. This covers a
// controller recreating a Pod inside the same PodGroup, where PodGroup
// generation recovery cannot reset the concrete Pod identity for us.
//
// The reset is persisted before the candidate can claim it, so retries after a
// conflict or process restart always resume from a durable state.
func (n *Nominator) recoverStalePlacementClaim(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	candidate *corev1.Pod,
	candidateSchedulingRequirementsHash string,
) (*repackv1alpha1.RepackRun, string, error) {
	if n.volcanoClient == nil || n.kubernetesClient == nil || run == nil || candidate == nil {
		return run, "", nil
	}
	if !hasRecoverablePlacementClaimForPod(
		run, candidate, candidateSchedulingRequirementsHash, n.now()) {
		return run, "", nil
	}
	var updatedRun *repackv1alpha1.RepackRun
	recoveredReplacementPodName := ""
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(
			ctx, run.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		updatedRun = latest
		if !placementRunActive(latest) {
			return nil
		}
		now := n.now()
		for index := range latest.Status.Relocations {
			nomination := &latest.Status.Relocations[index]
			if !placementClaimCanBeRecoveredForPod(
				latest, nomination, candidate, candidateSchedulingRequirementsHash, now) {
				continue
			}
			replacement, getErr := n.kubernetesClient.CoreV1().Pods(nomination.Namespace).Get(
				ctx, nomination.Placement.ReplacementPodName, metav1.GetOptions{})
			switch {
			case apierrors.IsNotFound(getErr):
				// The claimed replacement disappeared before placement completed.
			case getErr != nil:
				return getErr
			case replacement.UID == nomination.Placement.ReplacementPodUID &&
				replacement.DeletionTimestamp == nil:
				// The claim is still held by a live Pod.
				continue
			}

			recoveredReplacementPodName = nomination.Placement.ReplacementPodName
			resetReplacementPlacement(nomination)
			updatedRun, err = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(
				ctx, latest, metav1.UpdateOptions{})
			return err
		}
		return nil
	})
	return updatedRun, recoveredReplacementPodName, err
}

func hasRecoverablePlacementClaimForPod(
	run *repackv1alpha1.RepackRun,
	candidate *corev1.Pod,
	candidateSchedulingRequirementsHash string,
	now time.Time,
) bool {
	if !placementRunActive(run) {
		return false
	}
	for index := range run.Status.Relocations {
		if placementClaimCanBeRecoveredForPod(
			run, &run.Status.Relocations[index], candidate, candidateSchedulingRequirementsHash, now) {
			return true
		}
	}
	return false
}

func placementClaimCanBeRecoveredForPod(
	run *repackv1alpha1.RepackRun,
	nomination *repackv1alpha1.PodRelocationStatus,
	candidate *corev1.Pod,
	candidateSchedulingRequirementsHash string,
	now time.Time,
) bool {
	if nomination == nil || candidate == nil ||
		!placement.EvictionAllowsPlacement(nomination) ||
		nomination.Placement.ReplacementPodName == "" || nomination.Placement.ReplacementPodUID == "" ||
		nomination.Namespace != candidate.Namespace ||
		!placement.RelocationUsesPodGroup(nomination, placement.PodGroupName(candidate)) ||
		executionDeadlinePassed(run, now) {
		return false
	}
	switch nomination.Placement.Phase {
	case repackv1alpha1.PodPlacementWaitingForNodeSelection,
		repackv1alpha1.PodPlacementNominated:
	default:
		return false
	}
	if nomination.Placement.ReplacementPodName == candidate.Name &&
		nomination.Placement.ReplacementPodUID == candidate.UID {
		return false
	}
	return nomination.SchedulingRequirementsHash == "" ||
		nomination.SchedulingRequirementsHash == candidateSchedulingRequirementsHash
}

// hasPotentialNominationForPod reports whether a currently unmatched gated Pod
// may become claimable after victim deletion or PodGroup recreation mapping.
// Hash-bearing relocations require exact scheduling-requirements equality;
// hashless relocations retain the documented homogeneous-PodGroup fallback.
// This narrow test prevents an unrelated concurrent scale-out Pod from waiting
// behind every unfinished placement in the workload.
func (n *Nominator) hasPotentialNominationForPod(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	pod *corev1.Pod,
	candidateSchedulingRequirementsHash string,
) (bool, error) {
	if n.volcanoClient == nil || run == nil || pod == nil {
		return false, nil
	}
	podGroupName := placement.PodGroupName(pod)
	if podGroupName == "" {
		return false, nil
	}
	now := n.now()
	if hasClaimableNominationForPodGroup(
		run, pod.Namespace, podGroupName, candidateSchedulingRequirementsHash, now) {
		return true, nil
	}

	podGroup, err := n.volcanoClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(
		ctx, podGroupName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] !=
		placement.OwnerValue(run.Name, run.UID) {
		return false, nil
	}
	if !placement.PlacementAppliesToPodGroup(run, podGroup) {
		return false, nil
	}

	workload := placement.WorkloadKeyForPodGroup(podGroup)
	sourcePodGroups := placement.SourcePodGroupsByWorkload(run)[workload]
	if len(sourcePodGroups) == 0 {
		return false, nil
	}
	return hasClaimableNominationForSourcePodGroups(
		run, pod.Namespace, sourcePodGroups, candidateSchedulingRequirementsHash, now), nil
}

func hasClaimableNominationForPodGroup(
	run *repackv1alpha1.RepackRun,
	namespace, podGroupName string,
	candidateSchedulingRequirementsHash string,
	now time.Time,
) bool {
	return hasClaimableNomination(run, candidateSchedulingRequirementsHash, now,
		func(nomination *repackv1alpha1.PodRelocationStatus) bool {
			return nomination.Namespace == namespace &&
				placement.RelocationUsesPodGroup(nomination, podGroupName)
		})
}

func hasClaimableNominationForSourcePodGroups(
	run *repackv1alpha1.RepackRun,
	namespace string,
	sourcePodGroups []string,
	candidateSchedulingRequirementsHash string,
	now time.Time,
) bool {
	names := make(map[string]struct{}, len(sourcePodGroups))
	for _, name := range sourcePodGroups {
		names[name] = struct{}{}
	}
	return hasClaimableNomination(run, candidateSchedulingRequirementsHash, now,
		func(nomination *repackv1alpha1.PodRelocationStatus) bool {
			_, found := names[nomination.PodGroupName]
			return nomination.Namespace == namespace && found
		})
}

func hasClaimableNomination(
	run *repackv1alpha1.RepackRun,
	candidateSchedulingRequirementsHash string,
	now time.Time,
	inCandidateGroup func(*repackv1alpha1.PodRelocationStatus) bool,
) bool {
	if !placementRunActive(run) || inCandidateGroup == nil {
		return false
	}
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if nominationUnavailableForClaim(nomination) || executionDeadlinePassed(run, now) {
			continue
		}
		if inCandidateGroup(nomination) && (nomination.SchedulingRequirementsHash == "" ||
			nomination.SchedulingRequirementsHash == candidateSchedulingRequirementsHash) {
			return true
		}
	}
	return false
}

func placementRunActive(run *repackv1alpha1.RepackRun) bool {
	return run != nil && run.Spec.Mode == repackv1alpha1.RepackModeExecute &&
		run.Status.Phase == repackv1alpha1.RepackRunning
}

// needsNomination is true for a pod that is unscheduled, not yet nominated, and
// still Pending — the only pods a nomination can usefully steer.
func needsNomination(pod *corev1.Pod) bool {
	return pod.Spec.NodeName == "" &&
		pod.Status.NominatedNodeName == "" &&
		pod.Status.Phase == corev1.PodPending &&
		pod.DeletionTimestamp == nil
}

// Match precedence — the replacement matching contract (§5.2.2):
//  1. victimPodName exact: nomination.VictimPodName equals the pod's name in the
//     original or durably-recorded replacement PodGroup (same-name rebuild —
//     vcjob/StatefulSet ordinals);
//  2. schedulingRequirementsHash: a renamed Pod in a SubGroup-enabled PodGroup
//     claims an intent with equivalent normalized scheduling requirements;
//  3. homogeneous PodGroup fallback: an empty hash explicitly means any pending
//     Pod in the same PodGroup can claim the next available intent.
func (n *Nominator) matchNomination(
	run *repackv1alpha1.RepackRun,
	pod *corev1.Pod,
	candidateSchedulingRequirementsHash string,
) (*repackv1alpha1.PodRelocationStatus, nominationMatchMethod) {
	if pod == nil || !placementRunActive(run) {
		return nil, ""
	}
	now := n.now()
	podGroupName := placement.PodGroupName(pod)
	var homogeneousNomination *repackv1alpha1.PodRelocationStatus
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if nominationUnavailableForClaim(nomination) {
			continue
		}
		if executionDeadlinePassed(run, now) {
			continue
		}
		// 1. An exact victim name has the strongest Pod identity, but it
		// still belongs to a specific original or durably-recorded
		// replacement PodGroup. This prevents a same-name Pod in an
		// ambiguous scale-out group from claiming the placement.
		if nomination.VictimPodName != "" && nomination.Namespace == pod.Namespace &&
			nomination.VictimPodName == pod.Name &&
			placement.RelocationUsesPodGroup(nomination, podGroupName) {
			return nomination, nominationMatchedByVictimName
		}
		// Hash and homogeneous matching require the same namespace and the
		// original or durably recorded replacement PodGroup.
		if nomination.Namespace != pod.Namespace || !placement.RelocationUsesPodGroup(nomination, podGroupName) {
			continue
		}
		if nomination.SchedulingRequirementsHash != "" {
			if nomination.SchedulingRequirementsHash == candidateSchedulingRequirementsHash &&
				n.victimGone(nomination) {
				return nomination, nominationMatchedBySchedulingRequirements
			}
			continue
		}
		// 3. Homogeneous PodGroup: first pending record for this group.
		// Do not consume it while the original victim still exists: prepared
		// relocations are persisted before eviction, and a failed eviction must
		// not redirect an unrelated Pending gang member.
		if !n.victimGone(nomination) {
			continue
		}
		if homogeneousNomination == nil {
			homogeneousNomination = nomination
		}
	}
	if homogeneousNomination != nil {
		return homogeneousNomination, nominationMatchedByHomogeneousPodGroup
	}
	return nil, ""
}

func executionDeadlinePassed(run *repackv1alpha1.RepackRun, now time.Time) bool {
	return run != nil && run.Status.ExecutionDeadline != nil &&
		!now.Before(run.Status.ExecutionDeadline.Time)
}

// ensureReplacementPodGroup persists the workload-level PodGroup recreation
// before a concrete Pod claims a nomination. It runs in the same single worker
// as Pod nomination updates, so mapping and claiming cannot race each other.
//
// Multiple PodGroups under one workload are required to be equivalent. The
// original PodGroup name remains the stable audit identity, while
// ReplacementPodGroupName advances to the latest generation when an unfinished
// placement's current group is deleted again.
func (n *Nominator) ensureReplacementPodGroup(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	pod *corev1.Pod,
) (*repackv1alpha1.RepackRun, error) {
	if n.volcanoClient == nil || run == nil || pod == nil {
		return run, nil
	}
	podGroupName := placement.PodGroupName(pod)
	if podGroupName == "" || placement.ActiveForPodGroup(run, pod.Namespace, podGroupName) {
		return run, nil
	}
	podGroup, err := n.volcanoClient.SchedulingV1beta1().PodGroups(pod.Namespace).Get(
		ctx, podGroupName, metav1.GetOptions{})
	if err != nil {
		return run, ignoreNotFound(err)
	}
	expectedLease := placement.OwnerValue(run.Name, run.UID)
	if podGroup.Annotations[repackv1alpha1.PlacementLeaseAnnotation] != expectedLease ||
		!placement.PlacementAppliesToPodGroup(run, podGroup) {
		return run, nil
	}

	workload := placement.WorkloadKeyForPodGroup(podGroup)
	sources := append([]string(nil), placement.SourcePodGroupsByWorkload(run)[workload]...)
	sort.Strings(sources)
	if len(sources) == 0 {
		return run, nil
	}

	if source, used := sourcePodGroupForReplacement(run, pod.Namespace, podGroupName); used {
		klog.V(4).InfoS("repack nominator: replacement PodGroup mapping already exists",
			"run", run.Name, "sourcePodGroup", pod.Namespace+"/"+source,
			"replacementPodGroup", pod.Namespace+"/"+podGroupName)
		return run, nil
	}

	sourcePodGroupName, previousPodGroupName, err := n.firstDeletedRecoverableSourcePodGroup(
		ctx, run, pod.Namespace, podGroupName, sources)
	if err != nil {
		return run, err
	}
	if sourcePodGroupName == "" {
		return run, nil
	}

	updatedRun, updatedCount, err := n.recordReplacementPodGroupMapping(
		ctx, run.Name, pod.Namespace, sourcePodGroupName, previousPodGroupName, podGroupName)
	if err != nil {
		return run, err
	}
	if updatedRun == nil {
		return run, nil
	}
	if updatedCount > 0 {
		klog.V(3).InfoS("repack nominator: recorded recreated PodGroup for placement records",
			"run", run.Name, "workload", workload,
			"sourcePodGroup", pod.Namespace+"/"+sourcePodGroupName,
			"previousPodGroup", pod.Namespace+"/"+previousPodGroupName,
			"replacementPodGroup", pod.Namespace+"/"+podGroupName,
			"relocationCount", updatedCount)
		n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPodGroupRecreated,
			fmt.Sprintf("RepackRun %s recognized PodGroup %s as the replacement for current PodGroup %s (original %s).",
				run.Name, podGroupName, previousPodGroupName, sourcePodGroupName))
	}
	return updatedRun, nil
}

func sourcePodGroupForReplacement(
	run *repackv1alpha1.RepackRun,
	namespace, replacementPodGroupName string,
) (string, bool) {
	if run == nil || namespace == "" || replacementPodGroupName == "" {
		return "", false
	}
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if nomination.Namespace == namespace &&
			nomination.ReplacementPodGroupName == replacementPodGroupName {
			return nomination.PodGroupName, true
		}
	}
	return "", false
}

// firstDeletedRecoverableSourcePodGroup selects the next original group whose
// current generation has disappeared while placement is unfinished. A
// still-existing current group means the candidate may only be scale-out, so
// its gate remains closed until deletion makes reconstruction unambiguous.
func (n *Nominator) firstDeletedRecoverableSourcePodGroup(
	ctx context.Context,
	run *repackv1alpha1.RepackRun,
	namespace, candidatePodGroupName string,
	sources []string,
) (sourcePodGroupName, previousPodGroupName string, err error) {
	for _, source := range sources {
		currentPodGroupName, recoverable := currentPodGroupForRecoverablePlacement(run, namespace, source)
		if !recoverable {
			continue
		}
		currentPodGroup, getErr := n.volcanoClient.SchedulingV1beta1().PodGroups(namespace).Get(
			ctx, currentPodGroupName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(getErr):
			return source, currentPodGroupName, nil
		case getErr != nil:
			return "", "", getErr
		case currentPodGroup.DeletionTimestamp != nil:
			return source, currentPodGroupName, nil
		default:
			klog.V(4).InfoS("repack nominator: current PodGroup still exists; candidate is treated as concurrent scale-out",
				"run", run.Name, "sourcePodGroup", namespace+"/"+source,
				"currentPodGroup", namespace+"/"+currentPodGroupName,
				"candidatePodGroup", namespace+"/"+candidatePodGroupName)
		}
	}
	return "", "", nil
}

// recordReplacementPodGroupMapping atomically advances every placement for one
// original PodGroup from its expected current generation to the new generation.
// A repeated reconstruction clears concrete Pod and node observations so the
// new Pods can claim the same durable placement intents idempotently.
func (n *Nominator) recordReplacementPodGroupMapping(
	ctx context.Context,
	runName, namespace, sourcePodGroupName, expectedCurrentPodGroupName, replacementPodGroupName string,
) (*repackv1alpha1.RepackRun, int, error) {
	var updatedRun *repackv1alpha1.RepackRun
	updatedCount := 0
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		attemptUpdatedCount := 0
		latest, getErr := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if !placementRunActive(latest) {
			updatedRun = latest
			return nil
		}
		for index := range latest.Status.Relocations {
			nomination := &latest.Status.Relocations[index]
			if nomination.Namespace != namespace || nomination.PodGroupName != sourcePodGroupName ||
				nomination.Placement.Phase == repackv1alpha1.PodPlacementTimedOut ||
				effectivePodGroupName(nomination) != expectedCurrentPodGroupName {
				continue
			}
			nomination.ReplacementPodGroupName = replacementPodGroupName
			resetReplacementPlacement(nomination)
			attemptUpdatedCount++
		}
		if attemptUpdatedCount == 0 {
			updatedRun = latest
			updatedCount = 0
			return nil
		}
		updatedRun, getErr = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(
			ctx, latest, metav1.UpdateOptions{})
		if getErr == nil {
			updatedCount = attemptUpdatedCount
		}
		return getErr
	})
	return updatedRun, updatedCount, err
}

func currentPodGroupForRecoverablePlacement(
	run *repackv1alpha1.RepackRun,
	namespace, sourcePodGroupName string,
) (string, bool) {
	if run == nil {
		return "", false
	}
	currentPodGroupName := ""
	recoverable := false
	for index := range run.Status.Relocations {
		nomination := &run.Status.Relocations[index]
		if nomination.Namespace != namespace || nomination.PodGroupName != sourcePodGroupName ||
			nomination.Placement.Phase == repackv1alpha1.PodPlacementTimedOut {
			continue
		}
		effectiveName := effectivePodGroupName(nomination)
		if currentPodGroupName != "" && currentPodGroupName != effectiveName {
			// A source PodGroup must advance as one equivalent scheduling unit.
			// Conflicting status is ambiguous and must not claim another group.
			return "", false
		}
		currentPodGroupName = effectiveName
		if !placement.PlacementReachedTerminalPhase(nomination) {
			recoverable = true
		}
	}
	return currentPodGroupName, recoverable && currentPodGroupName != ""
}

func effectivePodGroupName(nomination *repackv1alpha1.PodRelocationStatus) string {
	if nomination == nil {
		return ""
	}
	if nomination.ReplacementPodGroupName != "" {
		return nomination.ReplacementPodGroupName
	}
	return nomination.PodGroupName
}

// resetReplacementPlacement preserves the original plan and PodGroup lineage
// while making the intent claimable by the next replacement Pod.
func resetReplacementPlacement(nomination *repackv1alpha1.PodRelocationStatus) {
	if nomination == nil {
		return
	}
	nomination.Placement.SelectedNodeName = ""
	nomination.Placement.ReplacementPodName = ""
	nomination.Placement.ReplacementPodUID = ""
	nomination.Placement.ActualNodeName = ""
	nomination.Placement.Phase = repackv1alpha1.PodPlacementWaitingForReplacement
}

// placementRunForPod reads only the Run recorded by the gate owner annotation.
// The direct read closes informer ordering windows without scanning or matching
// unrelated Runs.
func (n *Nominator) placementRunForPod(ctx context.Context, pod *corev1.Pod) (*repackv1alpha1.RepackRun, error) {
	if n.volcanoClient == nil || pod == nil {
		return nil, nil
	}
	runName, runUID, ok := placement.ParseOwner(
		pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation])
	if !ok {
		return nil, nil
	}
	run, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if run.UID != runUID {
		return nil, nil
	}
	return run, nil
}

func placementAwaitingBinding(run *repackv1alpha1.RepackRun, pod *corev1.Pod) bool {
	if run == nil || pod == nil {
		return false
	}
	for i := range run.Status.Relocations {
		nomination := &run.Status.Relocations[i]
		if nomination.Namespace == pod.Namespace && nomination.Placement.ReplacementPodName == pod.Name &&
			nomination.Placement.ReplacementPodUID == pod.UID && nomination.Placement.Phase == repackv1alpha1.PodPlacementNominated {
			return true
		}
	}
	return false
}

func (n *Nominator) victimGone(nomination *repackv1alpha1.PodRelocationStatus) bool {
	if nomination == nil || nomination.VictimPodName == "" || n.podLister == nil {
		return true
	}
	pod, err := n.podLister.Pods(nomination.Namespace).Get(nomination.VictimPodName)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil || nomination.VictimPodUID == "" {
		// Without the plan-time UID, preserve the conservative legacy behavior:
		// a same-name Pod may still be the original victim.
		return false
	}
	// Kubernetes permits a controller to recreate the same Pod name after the
	// original instance is deleted. The durable victim UID distinguishes that
	// replacement from the instance Repack actually evicted.
	if pod.UID != nomination.VictimPodUID {
		klog.V(4).InfoS("repack nominator: victim Pod name now belongs to a recreated instance",
			"pod", nomination.Namespace+"/"+nomination.VictimPodName,
			"victimPodUID", nomination.VictimPodUID, "currentPodUID", pod.UID)
		return true
	}
	return pod.DeletionTimestamp != nil
}

// patchNominatedNode writes pod.status.nominatedNodeName via the status
// subresource (a soft hint; the scheduler may still place elsewhere).
func (n *Nominator) patchNominatedNode(ctx context.Context, pod *corev1.Pod, node string) error {
	patch := map[string]interface{}{"status": map[string]interface{}{"nominatedNodeName": node}}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = n.kubernetesClient.CoreV1().Pods(pod.Namespace).Patch(
		ctx, pod.Name, types.StrategicMergePatchType, body, metav1.PatchOptions{}, "status")
	return ignoreNotFound(err)
}

// openPlacementGate removes only Repack's scheduling gate after the selected
// receiver is durable. The owner marker remains until actual binding is
// observed, so scheduled Pod updates can find their Run without global scans.
func (n *Nominator) openPlacementGate(ctx context.Context, pod *corev1.Pod) error {
	return n.patchPlacementGate(ctx, pod, false, "", "")
}

// clearPlacementGate removes both Repack's gate and owner marker on terminal,
// stale, unrelated, or already-observed paths.
func (n *Nominator) clearPlacementGate(ctx context.Context, pod *corev1.Pod) error {
	expectedOwner := ""
	if pod != nil {
		expectedOwner = pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation]
	}
	return n.clearPlacementGateWithReason(ctx, pod, eventReasonPlacementReleased,
		fmt.Sprintf("Released the Repack placement gate owned by %s.", expectedOwner))
}

func (n *Nominator) clearPlacementGateWithReason(
	ctx context.Context,
	pod *corev1.Pod,
	reason, message string,
) error {
	return n.patchPlacementGate(ctx, pod, true, reason, message)
}

func (n *Nominator) patchPlacementGate(
	ctx context.Context,
	pod *corev1.Pod,
	removeOwner bool,
	releaseReason, releaseMessage string,
) error {
	if pod == nil || (!hasPlacementGate(pod) &&
		(!removeOwner || pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] == "")) {
		return nil
	}
	expectedOwner := pod.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation]
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := n.kubernetesClient.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if latest.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != expectedOwner {
			return nil
		}
		operations := make([]string, 0, 2)
		for index := range latest.Spec.SchedulingGates {
			if latest.Spec.SchedulingGates[index].Name != repackv1alpha1.PlacementGateName {
				continue
			}
			operations = append(operations, fmt.Sprintf(`{"op":"remove","path":"/spec/schedulingGates/%d"}`, index))
			break
		}
		if removeOwner && latest.Annotations[repackv1alpha1.PlacementGateOwnerAnnotation] != "" {
			operations = append(operations, `{"op":"remove","path":"/metadata/annotations/repack.volcano.sh~1placement-gate-owner"}`)
		}
		if len(operations) == 0 {
			return nil
		}
		body := []byte("[" + strings.Join(operations, ",") + "]")
		_, err = n.kubernetesClient.CoreV1().Pods(latest.Namespace).Patch(ctx, latest.Name, types.JSONPatchType, body, metav1.PatchOptions{})
		patched = err == nil
		return ignoreNotFound(err)
	})
	if err != nil || !patched {
		return err
	}
	if removeOwner {
		klog.V(3).InfoS("repack nominator: released placement gate and owner marker",
			"pod", pod.Namespace+"/"+pod.Name, "gateOwner", expectedOwner,
			"reason", releaseReason)
		n.recordPodEvent(pod, corev1.EventTypeNormal, releaseReason, releaseMessage)
	} else {
		klog.V(4).InfoS("repack nominator: opened placement gate after nomination became durable",
			"pod", pod.Namespace+"/"+pod.Name, "gateOwner", expectedOwner)
		n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPlacementGateOpened,
			"Opened the Repack placement gate after persisting the selected receiver.")
	}
	return nil
}

func hasPlacementGate(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == repackv1alpha1.PlacementGateName {
			return true
		}
	}
	return false
}

// markPlacementNominated records the concrete replacement before opening its
// gate. This makes the later bound-node observation unambiguous even for
// homogeneous PodGroups whose Pods are intentionally interchangeable.
func (n *Nominator) markPlacementNominated(ctx context.Context, runName string, nomination *repackv1alpha1.PodRelocationStatus, pod *corev1.Pod, selectedNode string) error {
	if runName == "" || nomination == nil || pod == nil {
		return fmt.Errorf("cannot persist replacement nomination without Run, placement, and Pod identity")
	}
	key := nominationStatusKey(nomination)
	durable := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			// Do not open the gate when the owning Run vanished between lookup and
			// status persistence. A fresh reconcile will release stale metadata.
			return err
		}
		for index := range run.Status.Relocations {
			current := &run.Status.Relocations[index]
			if nominationStatusKey(current) != key {
				continue
			}
			if current.Placement.ReplacementPodName != "" &&
				(current.Placement.ReplacementPodName != pod.Name || current.Placement.ReplacementPodUID != pod.UID) {
				return nil
			}
			switch current.Placement.Phase {
			case repackv1alpha1.PodPlacementNominated:
				durable =
					current.Placement.SelectedNodeName == selectedNode &&
						current.Placement.ReplacementPodName == pod.Name && current.Placement.ReplacementPodUID == pod.UID
				return nil
			case repackv1alpha1.PodPlacementPlaced,
				repackv1alpha1.PodPlacementTimedOut:
				return nil
			}
			current.Placement.SelectedNodeName = selectedNode
			current.Placement.ReplacementPodName = pod.Name
			current.Placement.ReplacementPodUID = pod.UID
			current.Placement.Phase = repackv1alpha1.PodPlacementNominated
			_, err = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
			durable = err == nil
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !durable {
		return fmt.Errorf("replacement placement %s in RepackRun %q changed before nomination became durable", key, runName)
	}
	return nil
}

func (n *Nominator) markPlacementGated(
	ctx context.Context,
	runName string,
	pod *corev1.Pod,
	candidateSchedulingRequirementsHash string,
) error {
	if runName == "" || pod == nil {
		return nil
	}
	updated := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			return ignoreNotFound(err)
		}
		current := nominationForReplacement(run, pod)
		if current != nil && current.Placement.Phase == repackv1alpha1.PodPlacementWaitingForNodeSelection {
			updated = true
			return nil
		}
		current, _ = n.matchNomination(run, pod, candidateSchedulingRequirementsHash)
		if current == nil {
			return nil
		}
		current.Placement.ReplacementPodName = pod.Name
		current.Placement.ReplacementPodUID = pod.UID
		current.Placement.Phase = repackv1alpha1.PodPlacementWaitingForNodeSelection
		_, err = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
		updated = err == nil
		return err
	})
	if err != nil || !updated {
		return err
	}
	klog.V(3).InfoS("repack nominator: replacement Pod associated with placement intent and held by gate",
		"run", runName, "pod", pod.Namespace+"/"+pod.Name,
		"podGroup", placement.PodGroupName(pod), "podUID", pod.UID)
	n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonReplacementGated,
		fmt.Sprintf("RepackRun %s is holding this replacement Pod while selecting a feasible receiver node.", runName))
	return nil
}

// observePlacement records the scheduler's actual binding. A selected node is a
// soft preference by design; an alternative binding remains Placed and is
// visible by comparing SelectedNodeName with ActualNodeName.
func (n *Nominator) observePlacement(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil || pod.Spec.NodeName == "" {
		return nil
	}
	cachedRun, err := n.placementRunForPod(ctx, pod)
	if err != nil {
		return err
	}
	if cachedRun == nil {
		return nil
	}
	placementRecorded := false
	selectedNode := ""
	usedAlternativeNode := false
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		run, err := n.volcanoClient.RepackV1alpha1().RepackRuns().Get(ctx, cachedRun.Name, metav1.GetOptions{})
		if err != nil {
			return ignoreNotFound(err)
		}
		for index := range run.Status.Relocations {
			nomination := &run.Status.Relocations[index]
			if nomination.Namespace != pod.Namespace || nomination.Placement.ReplacementPodName != pod.Name || nomination.Placement.ReplacementPodUID != pod.UID {
				continue
			}
			nomination.Placement.ActualNodeName = pod.Spec.NodeName
			selectedNode = nomination.Placement.SelectedNodeName
			switch nomination.Placement.Phase {
			case repackv1alpha1.PodPlacementNominated:
				usedAlternativeNode = nomination.Placement.SelectedNodeName != pod.Spec.NodeName
				nomination.Placement.Phase = repackv1alpha1.PodPlacementPlaced
			case repackv1alpha1.PodPlacementWaitingForNodeSelection:
				// Another actor bypassed the gate before the engine selected a
				// receiver. The placement still completed, while empty
				// SelectedNodeName makes the bypass visible in status.
				usedAlternativeNode = true
				nomination.Placement.Phase = repackv1alpha1.PodPlacementPlaced
			default:
				return nil
			}
			placementRecorded = true
			_, err = n.volcanoClient.RepackV1alpha1().RepackRuns().UpdateStatus(ctx, run, metav1.UpdateOptions{})
			return err
		}
		return nil
	})
	if err != nil || !placementRecorded {
		return err
	}
	if !usedAlternativeNode {
		klog.V(3).InfoS("repack nominator: replacement Pod reached selected receiver",
			"run", cachedRun.Name, "pod", pod.Namespace+"/"+pod.Name, "node", pod.Spec.NodeName)
		n.recordPodEvent(pod, corev1.EventTypeNormal, eventReasonPlacementSucceeded,
			fmt.Sprintf("Replacement Pod reached Repack-selected node %s.", pod.Spec.NodeName))
	} else {
		klog.V(3).InfoS("repack nominator: replacement Pod used an alternative node",
			"run", cachedRun.Name, "pod", pod.Namespace+"/"+pod.Name,
			"selectedNode", selectedNode, "actualNode", pod.Spec.NodeName)
		eventMessage := fmt.Sprintf(
			"Replacement Pod bound to %s instead of Repack-selected node %s.",
			pod.Spec.NodeName, selectedNode)
		if selectedNode == "" {
			eventMessage = fmt.Sprintf(
				"Replacement Pod bound to %s before Repack selected a receiver node.",
				pod.Spec.NodeName)
		}
		n.recordPodEvent(pod, corev1.EventTypeWarning, eventReasonAlternativePlacement, eventMessage)
	}
	return nil
}

// nominationUnavailableForClaim reports that this intent is already associated
// with a concrete replacement Pod or has reached a terminal phase. A non-terminal
// association may first be reset when its Pod or PodGroup generation disappears.
func nominationUnavailableForClaim(nomination *repackv1alpha1.PodRelocationStatus) bool {
	if nomination == nil || !placement.EvictionAllowsPlacement(nomination) {
		return true
	}
	if nomination.Placement.ReplacementPodName != "" || nomination.Placement.ReplacementPodUID != "" {
		return true
	}
	switch nomination.Placement.Phase {
	case repackv1alpha1.PodPlacementNominated, repackv1alpha1.PodPlacementPlaced,
		repackv1alpha1.PodPlacementTimedOut:
		return true
	default:
		return false
	}
}

type nominationStatusIdentity struct {
	Namespace     string
	PodGroupName  string
	VictimPodName string
	TargetNode    string
}

func (identity nominationStatusIdentity) String() string {
	return fmt.Sprintf("%s/%s:%s->%s",
		identity.Namespace, identity.PodGroupName, identity.VictimPodName, identity.TargetNode)
}

func nominationStatusKey(nomination *repackv1alpha1.PodRelocationStatus) nominationStatusIdentity {
	if nomination == nil {
		return nominationStatusIdentity{}
	}
	return nominationStatusIdentity{
		Namespace: nomination.Namespace, PodGroupName: nomination.PodGroupName,
		VictimPodName: nomination.VictimPodName, TargetNode: nomination.PlannedNodeName,
	}
}
