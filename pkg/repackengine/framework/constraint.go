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

package framework

import (
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/repackengine/api"
)

// PlanConstraintFn is a hard admissibility gate on a finished plan. Constraints
// are AND-aggregated; one false result rejects the plan.
type PlanConstraintFn func(ctx *api.PlanContext, plan *api.RepackPlan) bool

// registerBuiltinConstraints exposes the run's benefit gates through the same
// seam used by plugin-provided plan constraints.
func (s *Session) registerBuiltinConstraints() {
	run := runNameOf(s.configuration)
	minFreed := s.configuration.MinNodesFreed
	if minFreed < 1 {
		minFreed = 1
	}
	s.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) bool {
		admitted := plan != nil && plan.Benefit() >= float64(minFreed)
		klog.V(4).InfoS("repack: min-nodes-freed constraint", "run", run,
			"benefit", benefitOf(plan), "minFreed", minFreed, "admitted", admitted)
		return admitted
	})
	if minImprove := s.configuration.MinFragImprovementPercent; minImprove > 0 {
		s.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) bool {
			admitted, improvePct := false, 0
			if plan != nil {
				improvePct = int(-plan.FragmentationRateDelta()*100 + 0.5)
				admitted = improvePct >= minImprove
			}
			klog.V(4).InfoS("repack: frag-improvement constraint", "run", run,
				"improvePct", improvePct, "minImprove", minImprove, "admitted", admitted)
			return admitted
		})
	}
}

// benefitOf reports a nil-safe plan benefit, for constraint verdict logging.
func benefitOf(plan *api.RepackPlan) float64 {
	if plan == nil {
		return 0
	}
	return plan.Benefit()
}

func (s *Session) AddConstraintFn(fn PlanConstraintFn) {
	if fn != nil {
		s.constraintFns = append(s.constraintFns, fn)
	}
}

// PlanAdmissible reports whether a finished plan passes every built-in and
// plugin-provided hard constraint.
func (s *Session) PlanAdmissible(plan *api.RepackPlan) bool {
	ctx := s.PlanContext()
	for _, fn := range s.constraintFns {
		if !fn(ctx, plan) {
			return false
		}
	}
	return true
}
