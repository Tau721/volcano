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
