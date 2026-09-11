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

import "time"

// DefaultFragEvalCycle paces onFragAbovePercent re-sampling. It stays aligned
// with the engine's Execute cooldown so a fresh run is not created mid-cooldown.
const DefaultFragEvalCycle = 10 * time.Minute

// Options carries per-instance RepackPolicy controller knobs.
type Options struct {
	// Workers is the number of reconcile workers.
	Workers int
	// FragEvalCycle is the interval between onFragAbovePercent evaluations.
	FragEvalCycle time.Duration
}

// applyDefaults fills unset knobs so callers (shim, tests) need not repeat them.
func (o *Options) applyDefaults() {
	if o.Workers <= 0 {
		o.Workers = 1
	}
	if o.FragEvalCycle <= 0 {
		o.FragEvalCycle = DefaultFragEvalCycle
	}
}
