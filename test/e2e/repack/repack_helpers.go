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

// Package repack holds the repack (runtime GPU/NPU defragmentation) e2e suite.
//
// Fixtures: real GPU/NPU hardware is not present in CI, so the tests advertise a
// FAKE fully-qualified extended resource (npuResource) on the worker nodes via the
// node status subresource. The scheduler accounts for it and kubelet admits pods
// requesting it (the container does not really use a device), which is enough to
// build a controllable fragmented layout of running pods.
package repack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

// npuResource is a fake, fully-qualified extended resource (contains "/", so it
// passes the goals.resource CEL rule). 8 units are advertised per worker node.
const (
	npuResource    = v1.ResourceName("volcano.sh/e2e-npu")
	altNPUResource = v1.ResourceName("volcano.sh/e2e-alt-npu")
	npuPerNode     = 8
	repackTimeout  = 3 * time.Minute
	repackPoll     = 2 * time.Second
	fixtureTimeout = 45 * time.Second

	nativeWorkloadLabel  = "repack-e2e-workload"
	nativeScopeLabel     = "repack-e2e-scope"
	nativePlacementTaint = "repack.volcano.sh/e2e-initial-placement"
)

// ---- node fixtures -------------------------------------------------------

// schedulableNodes returns worker node names (Ready, unschedulable=false, no
// control-plane taint) — the nodes the tests advertise fake NPUs on.
func schedulableNodes(ctx *e2eutil.TestContext) []string {
	nodes, err := ctx.Kubeclient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	var out []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable {
			continue
		}
		control := false
		for _, taint := range n.Spec.Taints {
			if taint.Key == "node-role.kubernetes.io/control-plane" || taint.Key == "node-role.kubernetes.io/master" {
				control = true
			}
		}
		if control {
			continue
		}
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == v1.NodeReady && c.Status == v1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// advertiseResource patches an extended resource onto a node's
// capacity+allocatable.
func advertiseResource(ctx *e2eutil.TestContext, node string, res v1.ResourceName, qty int) {
	patch := fmt.Sprintf(`{"status":{"capacity":{"%s":"%d"},"allocatable":{"%s":"%d"}}}`,
		res, qty, res, qty)
	_, err := ctx.Kubeclient.CoreV1().Nodes().Patch(
		context.TODO(), node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}, "status")
	Expect(err).NotTo(HaveOccurred())
}

// clearResource removes an extended resource from a node (JSON-merge null deletes it).
func clearResource(ctx *e2eutil.TestContext, node string, res v1.ResourceName) {
	patch := fmt.Sprintf(`{"status":{"capacity":{"%s":null},"allocatable":{"%s":null}}}`, res, res)
	_, _ = ctx.Kubeclient.CoreV1().Nodes().Patch(
		context.TODO(), node, types.MergePatchType, []byte(patch), metav1.PatchOptions{}, "status")
}

func advertiseNPU(ctx *e2eutil.TestContext, node string, qty int) {
	advertiseResource(ctx, node, npuResource, qty)
}

func clearNPU(ctx *e2eutil.TestContext, node string) {
	clearResource(ctx, node, npuResource)
}

// npuFixture advertises fake NPUs on up to len(want) worker nodes and returns the
// node names it used. Registers cleanup that clears them again.
func npuFixture(ctx *e2eutil.TestContext, nodes int) []string {
	all := schedulableNodes(ctx)
	Expect(len(all)).To(BeNumerically(">=", nodes), "need at least %d schedulable worker nodes", nodes)
	used := all[:nodes]
	for _, n := range used {
		advertiseNPU(ctx, n, npuPerNode)
	}
	return used
}

// ---- occupying workloads -------------------------------------------------

// occupy creates a vcjob with one task requesting `cards` NPUs. If node != "" the
// task is pinned there (deterministic fragmented layout for DryRun); otherwise the
// scheduler places it (used for Execute, so the replacement can move). Waits Ready.
func occupy(ctx *e2eutil.TestContext, name, node string, cards int) *batchv1alpha1.Job {
	return occupyResource(ctx, name, node, npuResource, cards)
}

// occupyMovableVCJob places the initial Pod deterministically without persisting
// spec.nodeName. Its replacement therefore remains free to follow Repack's live
// receiver selection.
func occupyMovableVCJob(ctx *e2eutil.TestContext, name, initialNode string, cards int) *batchv1alpha1.Job {
	releaseNodes := holdNonTargetNodes(ctx, initialNode)
	defer releaseNodes()
	return occupy(ctx, name, "", cards)
}

func occupyVCJobReplicas(ctx *e2eutil.TestContext, name, node string, cardsPerPod int, replicas, minAvailable int32) *batchv1alpha1.Job {
	quantity := resource.MustParse(fmt.Sprintf("%d", cardsPerPod))
	resources := v1.ResourceList{npuResource: quantity}
	taskMinAvailable := minAvailable
	job := e2eutil.CreateJob(ctx, &e2eutil.JobSpec{
		Name:      name,
		Namespace: ctx.Namespace,
		NodeName:  node,
		Min:       minAvailable,
		Tasks: []e2eutil.TaskSpec{{
			Name: "w", Min: minAvailable, Rep: replicas, MinAvailable: &taskMinAvailable,
			Img: e2eutil.DefaultNginxImage, Req: resources, Limit: resources,
		}},
	})
	Expect(e2eutil.WaitTasksReady(ctx, job, int(replicas))).NotTo(HaveOccurred())
	return job
}

// occupyResource creates a one-task vcjob requesting cards of res. It is used
// to verify that spec.goals[0].resource selects a resource independently from
// the engine's configured default resource.
func occupyResource(ctx *e2eutil.TestContext, name, node string, res v1.ResourceName, cards int) *batchv1alpha1.Job {
	npuQty := resource.MustParse(fmt.Sprintf("%d", cards))
	npuList := v1.ResourceList{res: npuQty}
	spec := &e2eutil.JobSpec{
		Name:      name,
		Namespace: ctx.Namespace,
		NodeName:  node,
		Tasks: []e2eutil.TaskSpec{{
			Name: "w", Min: 1, Rep: 1, Img: e2eutil.DefaultNginxImage,
			Req: npuList, Limit: npuList,
		}},
	}
	job := e2eutil.CreateJob(ctx, spec)
	Expect(e2eutil.WaitTasksReady(ctx, job, 1)).NotTo(HaveOccurred())
	return job
}

// nativeWorkload is a scheduler-placed controller-owned Pod that starts at a
// deterministic node only for fixture setup. Its PodGroup is created by the
// generic pg-controller path; no workload-specific Repack integration is
// involved.
type nativeWorkload struct {
	deployment  *appsv1.Deployment
	statefulSet *appsv1.StatefulSet
	podName     string
	podUID      types.UID
	podGroup    string
}

// occupyNativeDeployment creates a Deployment replica on a deterministic node
// without changing its template: other workers are temporarily tainted during
// the initial scheduling decision. A replacement therefore remains owned by
// the same ReplicaSet and retains the same controller-derived PodGroup name.
func occupyNativeDeployment(ctx *e2eutil.TestContext, name, node, scopeValue string, cards int) *nativeWorkload {
	releaseNodes := holdNonTargetNodes(ctx, node)
	defer releaseNodes()

	replicas := int32(1)
	labels := map[string]string{
		nativeWorkloadLabel: name,
		nativeScopeLabel:    scopeValue,
	}
	quantity := resource.MustParse(fmt.Sprintf("%d", cards))
	deployment, err := ctx.Kubeclient.AppsV1().Deployments(ctx.Namespace).Create(context.TODO(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ctx.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{nativeWorkloadLabel: name}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: v1.PodSpec{
					SchedulerName: e2eutil.SchedulerName,
					RestartPolicy: v1.RestartPolicyAlways,
					Containers: []v1.Container{{
						Name:            name,
						Image:           e2eutil.DefaultNginxImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Resources:       v1.ResourceRequirements{Requests: v1.ResourceList{npuResource: quantity}, Limits: v1.ResourceList{npuResource: quantity}},
					}},
				},
			},
		},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "create native deployment")

	var initialPod *v1.Pod
	Eventually(func() bool {
		pods, listErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: nativeWorkloadLabel + "=" + name,
		})
		if listErr != nil || len(pods.Items) != 1 {
			return false
		}
		pod := &pods.Items[0]
		if pod.Annotations["scheduling.k8s.io/group-name"] == "" || pod.Spec.NodeName != node || pod.Status.Phase != v1.PodRunning {
			return false
		}
		initialPod = pod.DeepCopy()
		return true
	}, fixtureTimeout, repackPoll).Should(BeTrue(), "native pod must run on the deterministic fixture node with an automatic PodGroup")

	return &nativeWorkload{
		deployment: deployment,
		podName:    initialPod.Name,
		podUID:     initialPod.UID,
		podGroup:   initialPod.Annotations["scheduling.k8s.io/group-name"],
	}
}

// occupyNativeStatefulSet creates a StatefulSet replica on a deterministic
// node. Its replacement keeps both the stable ordinal name and the
// StatefulSet-derived automatic PodGroup.
func occupyNativeStatefulSet(ctx *e2eutil.TestContext, name, node, scopeValue string, cards int) *nativeWorkload {
	releaseNodes := holdNonTargetNodes(ctx, node)
	defer releaseNodes()

	replicas := int32(1)
	labels := map[string]string{
		nativeWorkloadLabel: name,
		nativeScopeLabel:    scopeValue,
	}
	quantity := resource.MustParse(fmt.Sprintf("%d", cards))
	statefulSet, err := ctx.Kubeclient.AppsV1().StatefulSets(ctx.Namespace).Create(context.TODO(), &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ctx.Namespace},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{nativeWorkloadLabel: name}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: v1.PodSpec{
					SchedulerName: e2eutil.SchedulerName,
					RestartPolicy: v1.RestartPolicyAlways,
					Containers: []v1.Container{{
						Name:            name,
						Image:           e2eutil.DefaultNginxImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Resources:       v1.ResourceRequirements{Requests: v1.ResourceList{npuResource: quantity}, Limits: v1.ResourceList{npuResource: quantity}},
					}},
				},
			},
		},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "create native StatefulSet")

	var initialPod *v1.Pod
	Eventually(func() bool {
		pods, listErr := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: nativeWorkloadLabel + "=" + name,
		})
		if listErr != nil || len(pods.Items) != 1 {
			return false
		}
		pod := &pods.Items[0]
		if pod.Annotations["scheduling.k8s.io/group-name"] == "" || pod.Spec.NodeName != node || pod.Status.Phase != v1.PodRunning {
			return false
		}
		initialPod = pod.DeepCopy()
		return true
	}, fixtureTimeout, repackPoll).Should(BeTrue(), "StatefulSet pod must run on the deterministic fixture node with an automatic PodGroup")

	return &nativeWorkload{
		statefulSet: statefulSet,
		podName:     initialPod.Name,
		podUID:      initialPod.UID,
		podGroup:    initialPod.Annotations["scheduling.k8s.io/group-name"],
	}
}

func holdNonTargetNodes(ctx *e2eutil.TestContext, target string) func() {
	taint := v1.Taint{Key: nativePlacementTaint, Value: "true", Effect: v1.TaintEffectNoSchedule}
	var added []string
	for _, nodeName := range schedulableNodes(ctx) {
		if nodeName == target {
			continue
		}
		node, err := ctx.Kubeclient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		node = node.DeepCopy()
		alreadyHeld := false
		for _, existing := range node.Spec.Taints {
			if existing.Key == taint.Key && existing.Value == taint.Value && existing.Effect == taint.Effect {
				alreadyHeld = true
				break
			}
		}
		if alreadyHeld {
			continue
		}
		node.Spec.Taints = append(node.Spec.Taints, taint)
		_, err = ctx.Kubeclient.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "hold non-target fixture node")
		added = append(added, nodeName)
	}
	return func() {
		for _, nodeName := range added {
			node, err := ctx.Kubeclient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			node = node.DeepCopy()
			filtered := node.Spec.Taints[:0]
			for _, existing := range node.Spec.Taints {
				if existing.Key == taint.Key && existing.Value == taint.Value && existing.Effect == taint.Effect {
					continue
				}
				filtered = append(filtered, existing)
			}
			node.Spec.Taints = filtered
			_, err = ctx.Kubeclient.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "release non-target fixture node")
		}
	}
}

// ---- RepackRun helpers ---------------------------------------------------

type runBuilder struct{ run *repackv1alpha1.RepackRun }

func newRun(name string, mode repackv1alpha1.RepackMode) *runBuilder {
	return &runBuilder{run: &repackv1alpha1.RepackRun{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-"},
		Spec:       repackv1alpha1.RepackRunSpec{Mode: mode},
	}}
}
func (b *runBuilder) goal(res v1.ResourceName) *runBuilder {
	return b.goalWithMinFragImprovement(res, 0)
}
func (b *runBuilder) goalWithMinFragImprovement(res v1.ResourceName, percent int32) *runBuilder {
	b.run.Spec.Goals = []repackv1alpha1.RepackGoal{{Resource: res, MinFragImprovementPercent: percent}}
	return b
}
func (b *runBuilder) ttl(sec int64) *runBuilder { b.run.Spec.TTLSecondsAfterFinished = &sec; return b }
func (b *runBuilder) scope(s *repackv1alpha1.RepackScope) *runBuilder {
	b.run.Spec.Scope = s
	return b
}
func (b *runBuilder) maxPerRun(m *repackv1alpha1.MaxPerRun) *runBuilder {
	b.run.Spec.MaxPerRun = m
	return b
}
func (b *runBuilder) networkTopology(t *repackv1alpha1.NetworkTopology) *runBuilder {
	b.run.Spec.NetworkTopology = t
	return b
}

// create submits the RepackRun and immediately registers best-effort cleanup.
// Registration here, before a fixture performs any further assertions, keeps a
// failed setup from leaking a cluster-scoped run into later serial specs.
func (b *runBuilder) create(ctx *e2eutil.TestContext) (*repackv1alpha1.RepackRun, error) {
	created, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Create(
		context.TODO(), b.run, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	DeferCleanup(func() {
		deleteRun(ctx, created.Name)
	})
	return created, nil
}

func getRun(ctx *e2eutil.TestContext, name string) *repackv1alpha1.RepackRun {
	r, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(context.TODO(), name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return r
}

func deleteRun(ctx *e2eutil.TestContext, name string) {
	_ = ctx.Vcclient.RepackV1alpha1().RepackRuns().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// waitTerminal blocks until the run reaches a terminal phase and returns it.
func waitTerminal(ctx *e2eutil.TestContext, name string) *repackv1alpha1.RepackRun {
	var last *repackv1alpha1.RepackRun
	var lastGetError error
	err := wait.PollUntilContextTimeout(context.TODO(), repackPoll, repackTimeout, false,
		func(c context.Context) (bool, error) {
			r, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(c, name, metav1.GetOptions{})
			if err != nil {
				lastGetError = err
				return false, nil
			}
			lastGetError = nil
			last = r
			switch r.Status.Phase {
			case repackv1alpha1.RepackSucceeded, repackv1alpha1.RepackFailed:
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		recordRepackDiagnostics(ctx, name, fmt.Errorf("terminal wait failed: %w (last GET error: %v)", err, lastGetError))
	}
	Expect(err).NotTo(HaveOccurred(), "run %s did not reach a terminal phase (is the repack engine running?)", name)
	Expect(last.Status.Message).NotTo(BeEmpty(), "terminal RepackRun must provide an operator-readable status.message")
	Expect(last.Status.CompletionTime).NotTo(BeNil(), "terminal RepackRun must provide completionTime")
	if last.Status.StartTime != nil {
		Expect(last.Status.CompletionTime.Time.Before(last.Status.StartTime.Time)).To(BeFalse(),
			"completionTime must not precede startTime")
	}
	var terminalCondition *metav1.Condition
	for index := range last.Status.Conditions {
		condition := &last.Status.Conditions[index]
		if condition.Status == metav1.ConditionTrue &&
			(condition.Type == "Complete" || condition.Type == "Failed" || condition.Type == "Cancelled") {
			terminalCondition = condition
			break
		}
	}
	Expect(terminalCondition).NotTo(BeNil(), "terminal RepackRun must have a True terminal condition")
	Expect(terminalCondition.Message).NotTo(BeEmpty(), "terminal condition must provide an operator-readable message")
	Expect(terminalCondition.Message).NotTo(Equal("engine finished"), "terminal condition message must contain the actual result")
	return last
}

// waitConditionReason blocks until the run has the requested condition status,
// then returns its reason. Pending runs intentionally use Progressing=False.
func waitConditionReason(ctx *e2eutil.TestContext, name, condType string, status metav1.ConditionStatus) string {
	var reason string
	var lastGetError error
	err := wait.PollUntilContextTimeout(context.TODO(), repackPoll, repackTimeout, false,
		func(c context.Context) (bool, error) {
			r, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(c, name, metav1.GetOptions{})
			if err != nil {
				lastGetError = err
				return false, nil
			}
			lastGetError = nil
			for _, cond := range r.Status.Conditions {
				if cond.Type == condType && cond.Status == status {
					reason = cond.Reason
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		recordRepackDiagnostics(ctx, name, fmt.Errorf(
			"condition %s=%s wait failed: %w (last GET error: %v)",
			condType, status, err, lastGetError))
	}
	Expect(err).NotTo(HaveOccurred(), "run %s never got condition %s=%s", name, condType, status)
	return reason
}

// recordSpecFailureDiagnostics captures the test namespace, RepackRun events,
// and the tail of every Repack component log before cleanup destroys evidence.
// It is intentionally best-effort and must never mask the original assertion.
func recordSpecFailureDiagnostics(ctx *e2eutil.TestContext) {
	if !CurrentSpecReport().Failed() {
		return
	}
	recordRepackDiagnostics(ctx, "", fmt.Errorf("spec failed: %s", CurrentSpecReport().LeafNodeText))
}

func recordRepackDiagnostics(ctx *e2eutil.TestContext, runName string, cause error) {
	if ctx == nil {
		return
	}
	AddReportEntry("Repack failure cause", cause.Error())

	snapshot := map[string]interface{}{}
	diagnosticNamespaces := map[string]struct{}{
		metav1.NamespaceDefault: {},
		ctx.Namespace:           {},
	}
	collectRunNamespaces := func(run *repackv1alpha1.RepackRun) {
		if run == nil {
			return
		}
		for index := range run.Status.Relocations {
			namespace := run.Status.Relocations[index].Namespace
			if namespace != "" {
				diagnosticNamespaces[namespace] = struct{}{}
			}
		}
		if run.Status.Plan != nil {
			for index := range run.Status.Plan.Moves {
				namespace := run.Status.Plan.Moves[index].Namespace
				if namespace != "" {
					diagnosticNamespaces[namespace] = struct{}{}
				}
			}
		}
	}
	if runName != "" {
		if run, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().Get(context.TODO(), runName, metav1.GetOptions{}); err == nil {
			snapshot["run"] = run
			collectRunNamespaces(run)
		} else {
			snapshot["runGetError"] = err.Error()
		}
	} else if runs, err := ctx.Vcclient.RepackV1alpha1().RepackRuns().List(context.TODO(), metav1.ListOptions{}); err == nil {
		snapshot["runs"] = runs
		for index := range runs.Items {
			collectRunNamespaces(&runs.Items[index])
		}
	} else {
		snapshot["runListError"] = err.Error()
	}
	namespaces := make([]string, 0, len(diagnosticNamespaces))
	for namespace := range diagnosticNamespaces {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)

	namespacePods := map[string]interface{}{}
	podGroups := map[string]interface{}{}
	events := map[string]interface{}{}
	for _, namespace := range namespaces {
		if pods, err := ctx.Kubeclient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
			namespacePods[namespace] = pods
		} else {
			namespacePods[namespace] = err.Error()
		}
		if groups, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
			podGroups[namespace] = groups
		} else {
			podGroups[namespace] = err.Error()
		}
		if eventList, err := ctx.Kubeclient.CoreV1().Events(namespace).List(context.TODO(), metav1.ListOptions{}); err == nil {
			events[namespace] = eventList
		} else {
			events[namespace] = err.Error()
		}
	}
	snapshot["namespacePods"] = namespacePods
	snapshot["podGroups"] = podGroups
	snapshot["events"] = events
	if data, err := json.MarshalIndent(snapshot, "", "  "); err == nil {
		AddReportEntry("Repack API snapshot", string(data))
	}

	tailLines := int64(200)
	for _, component := range []string{
		"volcano-repack-engine",
		"volcano-controller",
		"volcano-admission",
		"volcano-scheduler",
	} {
		pods, err := ctx.Kubeclient.CoreV1().Pods(repackSystemNamespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: "app=" + component,
		})
		if err != nil {
			AddReportEntry("Repack component logs: "+component, "list Pods: "+err.Error())
			continue
		}
		for index := range pods.Items {
			pod := &pods.Items[index]
			raw, logErr := ctx.Kubeclient.CoreV1().Pods(repackSystemNamespace).GetLogs(
				pod.Name, &v1.PodLogOptions{TailLines: &tailLines}).DoRaw(context.TODO())
			entryName := fmt.Sprintf("Repack component logs: %s/%s", component, pod.Name)
			if logErr != nil {
				AddReportEntry(entryName, logErr.Error())
				continue
			}
			AddReportEntry(entryName, string(raw))
		}
	}
}

// completeReason returns the reason of the True Complete/Failed condition.
func completeReason(run *repackv1alpha1.RepackRun) string {
	for _, c := range run.Status.Conditions {
		if c.Status == metav1.ConditionTrue && (c.Type == "Complete" || c.Type == "Failed") {
			return c.Reason
		}
	}
	return ""
}

func waitRunEventReasons(ctx *e2eutil.TestContext, run *repackv1alpha1.RepackRun, reasons ...string) {
	Expect(run).NotTo(BeNil())
	want := make(map[string]bool, len(reasons))
	for _, reason := range reasons {
		want[reason] = true
	}
	Eventually(func() map[string]bool {
		found := make(map[string]bool)
		events, err := ctx.Kubeclient.CoreV1().Events(metav1.NamespaceDefault).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return found
		}
		for index := range events.Items {
			event := &events.Items[index]
			if event.InvolvedObject.UID == run.UID && want[event.Reason] {
				found[event.Reason] = true
			}
		}
		return found
	}, fixtureTimeout, repackPoll).Should(HaveLen(len(want)),
		"RepackRun events must expose every core lifecycle milestone")
}

func waitPodEventReasons(ctx *e2eutil.TestContext, pod *v1.Pod, reasons ...string) {
	Expect(pod).NotTo(BeNil())
	want := make(map[string]bool, len(reasons))
	for _, reason := range reasons {
		want[reason] = true
	}
	Eventually(func() map[string]bool {
		found := make(map[string]bool)
		events, err := ctx.Kubeclient.CoreV1().Events(pod.Namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return found
		}
		for index := range events.Items {
			event := &events.Items[index]
			if event.InvolvedObject.UID == pod.UID && want[event.Reason] {
				found[event.Reason] = true
			}
		}
		return found
	}, fixtureTimeout, repackPoll).Should(HaveLen(len(want)),
		"replacement Pod events must expose every placement milestone")
}

func podGroupNameForOwner(ctx *e2eutil.TestContext, ownerUID types.UID) string {
	var result string
	Eventually(func() string {
		podGroups, err := ctx.Vcclient.SchedulingV1beta1().PodGroups(ctx.Namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return ""
		}
		for index := range podGroups.Items {
			podGroup := &podGroups.Items[index]
			for _, owner := range podGroup.OwnerReferences {
				if owner.UID == ownerUID {
					result = ctx.Namespace + "/" + podGroup.Name
					return result
				}
			}
		}
		return ""
	}, fixtureTimeout, repackPoll).ShouldNot(BeEmpty(), "PodGroup for workload owner %s must exist", ownerUID)
	return result
}

// runningPodCount is the number of Running pods in the test namespace (to assert
// DryRun evicts nothing).
func runningPodCount(ctx *e2eutil.TestContext) int {
	pods, err := ctx.Kubeclient.CoreV1().Pods(ctx.Namespace).List(context.TODO(), metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	n := 0
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == v1.PodRunning {
			n++
		}
	}
	return n
}
