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
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

// newEventRecorder owns both the recorder and its broadcaster so Run can stop
// the broadcaster and release its sink goroutines during shutdown.
func newEventRecorder(config *rest.Config) (record.EventRecorder, record.EventBroadcaster) {
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.ErrorS(err, "repack: event recorder disabled (client build failed)")
		return nil, nil
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(kubescheme.AddToScheme(scheme))
	utilruntime.Must(repackv1alpha1.AddToScheme(scheme))
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return broadcaster.NewRecorder(scheme, v1.EventSource{Component: "volcano-repack-engine"}), broadcaster
}

const (
	eventReasonPlanComputed            = "PlanComputed"
	eventReasonExecutePrepared         = "ExecutePrepared"
	eventReasonEvictionsIssued         = "EvictionsIssued"
	eventReasonEvictionRetryScheduled  = "EvictionRetryScheduled"
	eventReasonIndirectRemovalObserved = "IndirectRemovalObserved"
	eventReasonReconcilingPlacements   = "ReconcilingPlacements"
	eventReasonPlacementSelected       = "PlacementSelected"
	eventReasonPlacementLeaseRepaired  = "PlacementLeaseRepaired"
	eventReasonWaitingForNodeSelection = "WaitingForNodeSelection"
)

func (e *Engine) recordRunEvent(run *repackv1alpha1.RepackRun, eventType, reason, message string) {
	if e == nil || e.recorder == nil || run == nil {
		return
	}
	if eventType == "" {
		eventType = v1.EventTypeNormal
	}
	e.recorder.Event(run, eventType, reason, message)
}

func plannedBenefitEventMessage(run *repackv1alpha1.RepackRun) string {
	if run == nil || run.Status.Plan == nil || run.Status.Plan.Summary == nil {
		return "Repack plan computed."
	}
	summary := run.Status.Plan.Summary
	return fmt.Sprintf(
		"Plan computed: move %d PodGroups and %d cards to free %d nodes; cluster fragmentation %d%% -> %d%%.",
		len(run.Status.Plan.Moves), summary.MovedCardCount, summary.FreedNodeCount,
		summary.FragBeforePercent, summary.FragAfterPercent)
}
