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

package repack

import (
	"context"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/controllers/framework"
	"volcano.sh/volcano/pkg/controllers/repack/policy"
)

func init() {
	framework.RegisterController(&frameworkController{})
}

// frameworkController runs this package's controllers inside
// volcano-controller-manager: it builds them from the shared ControllerOption and
// launches them in the background, registered like job/podgroup/queue as
// "repack-controller". It is disabled by default because the Repack CRD and RBAC
// are optional installation components.
type frameworkController struct {
	runCtrl       *Controller
	nominator     *Nominator
	policyCtrl    *policy.Controller
	workers       int
	fragEvalCycle time.Duration
}

func (c *frameworkController) Name() string { return "repack-controller" }

// AddFlags implements framework.FlagProvider so controller-manager can parse
// --repack-policy-frag-eval-cycle before Initialize. A zero value (AddFlags not
// called, e.g. wiring tests) falls back to policy.DefaultFragEvalCycle inside
// policy.New.
func (c *frameworkController) AddFlags(fs *pflag.FlagSet) {
	fs.DurationVar(&c.fragEvalCycle, "repack-policy-frag-eval-cycle", policy.DefaultFragEvalCycle,
		"interval between onFragAbovePercent trigger evaluations; keep it >= --repack-execute-cooldown")
}

// Initialize wires the lifecycle controller and the nomination reconciler onto
// the shared informer factories (no factory Start here — that happens in Run).
// Execute serialization/cooldown is the engine's concern; the controller only
// needs the cooldown as a GC retention floor (so it never deletes a finished
// Execute run that is still the engine's cooldown anchor). Left unset here, it
// defaults to state.DefaultExecuteCooldown, which matches the engine's flag
// default; override the engine flag and this stays safe as long as it is >= it.
func (c *frameworkController) Initialize(opt *framework.ControllerOption) error {
	c.workers = int(opt.WorkerNum)

	c.runCtrl = New(opt.VolcanoClient, opt.VCSharedInformerFactory, Options{
		Workers: c.workers,
	})

	podInformer := opt.SharedInformerFactory.Core().V1().Pods()
	repackInformer := opt.VCSharedInformerFactory.Repack().V1alpha1().RepackRuns()
	c.nominator = NewNominator(opt.KubeClient, opt.VolcanoClient, podInformer, repackInformer)
	c.nominator.SetEventRecorder(NewEventRecorder(opt.KubeClient, "vc-controller-manager"))

	c.policyCtrl = policy.New(opt.VolcanoClient, opt.SharedInformerFactory, opt.VCSharedInformerFactory, policy.Options{
		Workers:       c.workers,
		FragEvalCycle: c.fragEvalCycle,
	})
	return nil
}

// Run launches all three loops in the background, cancelling on stopCh. It
// returns immediately (controller-manager keeps the process alive), matching the
// other controllers. The shared factories are started centrally and again
// (idempotently) by the wrapped controllers' own Run.
func (c *frameworkController) Run(stopCh <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stopCh
		cancel()
	}()

	go func() {
		if err := c.runCtrl.Run(ctx); err != nil {
			klog.ErrorS(err, "RepackRun controller stopped")
		}
	}()
	go func() {
		if err := c.nominator.Run(ctx); err != nil {
			klog.ErrorS(err, "Repack nominator stopped")
		}
	}()
	go func() {
		if err := c.policyCtrl.Run(ctx); err != nil {
			klog.ErrorS(err, "RepackPolicy controller stopped")
		}
	}()
	klog.InfoS("Repack controller is running ......")
}
