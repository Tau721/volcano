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

// Package repack registers the RepackRun lifecycle controller into the volcano
// controller-manager. The actual reconcile logic lives in the independent,
// framework-light module volcano.sh/repack-controller (depends only on the CRD
// types / generated client); this file is a thin framework.Controller adapter
// that builds it from the shared ControllerOption and runs it alongside the
// other controllers. Keeping the logic in a leaf module preserves "buildable on
// its own", while this shim gives the default "runs inside volcano-controller-
// manager" behaviour when Repack is explicitly enabled (registered like
// job/podgroup/queue). It is disabled by default because its CRD and RBAC are
// optional installation components.
package repack

import (
	"context"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	rc "volcano.sh/repack-controller/pkg"
	rcpolicy "volcano.sh/repack-controller/pkg/policy"
	"volcano.sh/volcano/pkg/controllers/framework"
)

func init() {
	framework.RegisterController(&repackController{})
}

// repackController adapts the standalone repack-controller to framework.Controller.
type repackController struct {
	runCtrl         *rc.Controller
	nominator       *rc.Nominator
	policyCtrl      *rcpolicy.Controller
	workers         int
	policyEvalCycle time.Duration
}

func (c *repackController) Name() string { return "repack-controller" }

// AddFlags implements framework.FlagProvider.
func (c *repackController) AddFlags(fs *pflag.FlagSet) {
	fs.DurationVar(&c.policyEvalCycle, "repack-policy-eval-period", 10*time.Minute,
		"onFragAbovePercent evaluation cycle; should >= Execute cooldown")
}

// Initialize wires the lifecycle controller, policy controller, and the
// nomination reconciler onto the shared informer factories (no factory Start
// here — that happens in Run).
func (c *repackController) Initialize(opt *framework.ControllerOption) error {
	c.workers = int(opt.WorkerNum)

	c.runCtrl = rc.New(opt.VolcanoClient, opt.VCSharedInformerFactory, rc.Options{
		Workers: c.workers,
	})

	podInformer := opt.SharedInformerFactory.Core().V1().Pods()
	repackInformer := opt.VCSharedInformerFactory.Repack().V1alpha1().RepackRuns()
	c.nominator = rc.NewNominator(opt.KubeClient, opt.VolcanoClient, podInformer, repackInformer)
	c.nominator.SetEventRecorder(rc.NewEventRecorder(opt.KubeClient, "vc-controller-manager"))

	c.policyCtrl = rcpolicy.New(opt.VolcanoClient,
		opt.VCSharedInformerFactory,
		opt.SharedInformerFactory,
		rcpolicy.Options{
			Workers:                   c.workers,
			EvalCycle:                 c.policyEvalCycle,
			DefaultSuccessHistoryLimit: 3,
			DefaultFailedHistoryLimit:  3,
		})
	return nil
}

// Run launches all sub-controllers in the background, cancelling on stopCh. It
// returns immediately (controller-manager keeps the process alive), matching
// the other controllers. The shared factories are started centrally and again
// (idempotently) by the wrapped controllers' own Run.
func (c *repackController) Run(stopCh <-chan struct{}) {
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
	klog.InfoS("Repack controller is running ......")
}
