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

// Package repack is the planning action: run the plugin-driven lazy planner and
// render the report. Execute side effects deliberately live in the Engine
// runtime, after the plan and relocation journal have been durably persisted.
// Future planning actions (relief, simulate) compose after it in the pipeline.
package repack

import (
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
	"volcano.sh/volcano/pkg/repackengine/planner/drain"
)

func init() {
	framework.RegisterAction(framework.ActionRepack, framework.ActionRegistration{
		Factory:  func() framework.Action { return &repackAction{} },
		Requires: []framework.PluginCapability{framework.CapabilityDomain},
	})
}

type repackAction struct{}

func (*repackAction) Name() string { return framework.ActionRepack }

func (*repackAction) Execute(ssn *framework.Session) {
	runName := ""
	if run := ssn.Run(); run != nil {
		runName = run.Name
	}
	resource := ssn.Resource()
	nodes := ssn.Nodes()
	before := api.MeasureResourceFragmentation(nodes, resource)
	klog.V(3).InfoS("repack: planning pass started", "run", runName, "resource", resource,
		"nodes", len(nodes),
		"occupiedNodes", before.OccupiedNodeCount, "optimalNodes", before.OptimalOccupiedNodeCount,
		"providingNodes", before.ProvidingNodeCount)

	plan := drain.BuildPlan(ssn)
	if plan != nil {
		plan.Before = before
		if !ssn.PlanAdmissible(plan) {
			klog.V(3).InfoS("repack: plan rejected by benefit constraints", "run", runName, "resource", resource,
				"freedNodeCount", len(plan.FreedNodes), "moveCount", len(plan.Moves),
				"fragmentationBefore", before.FragmentationRate(), "fragmentationDelta", plan.FragmentationRateDelta())
			plan = nil
		} else {
			plan.Cost = api.CalculateDisruptionCost(plan.Moves, resource)
			klog.V(3).InfoS("repack: plan accepted", "run", runName, "resource", resource,
				"freedNodeCount", len(plan.FreedNodes), "moveCount", len(plan.Moves),
				"movedResource", plan.Cost.MovedResource, "affectedPodGroupCount", plan.Cost.AffectedPodGroups,
				"fragmentationBefore", before.FragmentationRate(), "fragmentationDelta", plan.FragmentationRateDelta())
			klog.V(4).InfoS("repack: accepted plan details", "run", runName, "freedNodes", plan.FreedNodes,
				"affectedPodGroups", plan.AffectedPodGroups())
		}
	} else {
		klog.V(3).InfoS("repack: no plan produced", "run", runName, "resource", resource, "reason", "NoFreeableUnit")
	}
	ssn.SetPlan(plan)
	report := api.RenderReport(plan)
	if plan == nil {
		// No plan: still record the cluster's current fragmentation so the Engine
		// can tell NoFragmentation (clean) apart from BelowGoalThreshold (fragmented
		// but no worthwhile plan). RenderReport(nil) leaves FragmentationRateBefore at 0.
		currentFragmentationRate := before.FragmentationRate()
		report.FragmentationRateBefore, report.FragmentationRateAfter = currentFragmentationRate, currentFragmentationRate
	}
	ssn.SetReport(report)
	if plan == nil {
		return // NoRepackNeeded
	}
}
