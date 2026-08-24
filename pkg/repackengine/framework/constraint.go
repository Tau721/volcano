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

import "volcano.sh/volcano/pkg/repackengine/api"

// PlanConstraintFn is a hard admissibility gate on a finished plan. Constraints
// are AND-aggregated; one false result rejects the plan.
type PlanConstraintFn func(ctx *api.PlanContext, plan *api.RepackPlan) bool

// registerBuiltinConstraints exposes the run's benefit gates through the same
// seam used by plugin-provided plan constraints.
func (s *Session) registerBuiltinConstraints() {
	minFreed := s.configuration.MinNodesFreed
	if minFreed < 1 {
		minFreed = 1
	}
	s.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) bool {
		return plan != nil && plan.Benefit() >= float64(minFreed)
	})
	if minImprove := s.configuration.MinFragImprovementPercent; minImprove > 0 {
		s.AddConstraintFn(func(_ *api.PlanContext, plan *api.RepackPlan) bool {
			if plan == nil {
				return false
			}
			improvePct := int(-plan.FragmentationRateDelta()*100 + 0.5)
			return improvePct >= minImprove
		})
	}
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
