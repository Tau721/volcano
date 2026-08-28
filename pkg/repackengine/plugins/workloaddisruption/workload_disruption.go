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

// Package workloaddisruption registers the gang-agnostic disruption plan scores every repack run
// wants: number of affected PodGroups, moved target resources, and moved pods.
// Cluster-level weights are configurable through the workloaddisruption plugin
// arguments. Gang-semantics scores live in the gangdisruption plugin.
package workloaddisruption

import (
	"volcano.sh/volcano/pkg/repackengine/api"
	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Name is the config name for this plugin.
const Name = "workloaddisruption"

// Default weights for the generic disruption dimensions. repack-conf plugin
// arguments override them; a zero weight disables the corresponding term.
const (
	weightAffectedPodGroups int64 = 10
	weightMovedResource     int64 = 3
	weightMovedPods         int64 = 1

	argAffectedPodGroupsWeight = "affectedPodGroupsWeight"
	argMovedResourceWeight     = "movedResourceWeight"
	argMovedPodsWeight         = "movedPodsWeight"
)

func init() {
	framework.RegisterPlugin(Name, framework.PluginRegistration{
		Factory: newPlugin, Validator: validateArguments,
	})
}

type workloadDisruptionPlugin struct {
	affectedPodGroupsWeight int64
	movedResourceWeight     int64
	movedPodsWeight         int64
}

func newPlugin(arguments framework.Arguments) framework.Plugin {
	return &workloadDisruptionPlugin{
		affectedPodGroupsWeight: configuredWeight(arguments, argAffectedPodGroupsWeight, weightAffectedPodGroups),
		movedResourceWeight:     configuredWeight(arguments, argMovedResourceWeight, weightMovedResource),
		movedPodsWeight:         configuredWeight(arguments, argMovedPodsWeight, weightMovedPods),
	}
}

func configuredWeight(arguments framework.Arguments, key string, defaultValue int64) int64 {
	value, err := arguments.NonNegativeInt(key, defaultValue)
	if err != nil {
		return defaultValue
	}
	return value
}

func validateArguments(arguments framework.Arguments) error {
	if err := arguments.ValidateKeys(argAffectedPodGroupsWeight, argMovedResourceWeight, argMovedPodsWeight); err != nil {
		return err
	}
	for _, item := range []struct {
		key          string
		defaultValue int64
	}{
		{argAffectedPodGroupsWeight, weightAffectedPodGroups},
		{argMovedResourceWeight, weightMovedResource},
		{argMovedPodsWeight, weightMovedPods},
	} {
		if _, err := arguments.NonNegativeInt(item.key, item.defaultValue); err != nil {
			return err
		}
	}
	return nil
}

func (*workloadDisruptionPlugin) Name() string { return Name }

func (p *workloadDisruptionPlugin) OnSessionOpen(ssn *framework.Session) {
	// Disruption is a cost, so each dimension returns its negated magnitude:
	// higher PlanScoreFn values mean a more preferred candidate plan.
	ssn.AddPlanScoreFn("affectedPodGroups", p.affectedPodGroupsWeight, func(ctx *api.PlanContext, p *api.CandidatePlan) int64 {
		return -p.MoveAggregate(ctx).AffectedPodGroups
	})
	ssn.AddPlanScoreFn("movedResource", p.movedResourceWeight, func(ctx *api.PlanContext, p *api.CandidatePlan) int64 {
		return -p.MoveAggregate(ctx).MovedResource
	})
	ssn.AddPlanScoreFn("movedPods", p.movedPodsWeight, func(ctx *api.PlanContext, p *api.CandidatePlan) int64 {
		return -p.MoveAggregate(ctx).MovedPods
	})
}

func (*workloadDisruptionPlugin) OnSessionClose(*framework.Session) {}
