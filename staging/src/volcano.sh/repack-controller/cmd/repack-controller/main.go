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

// Command repack-controller runs the RepackRun lifecycle, RepackPolicy, and
// nomination controllers as a STANDALONE binary. The same logic also ships inside
// volcano-controller-manager via the pkg/controllers/repack shim; this entrypoint
// is the "build/deploy on its own" option for the independent module. It depends
// only on client-go and the generated repack client — never on the main volcano
// module.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	vcclientset "volcano.sh/apis/pkg/client/clientset/versioned"
	vcinformers "volcano.sh/apis/pkg/client/informers/externalversions"

	repackcontroller "volcano.sh/repack-controller/pkg"
	repackpolicy "volcano.sh/repack-controller/pkg/policy"
)

var (
	kubeconfig  = flag.String("kubeconfig", "", "Path to kubeconfig (out-of-cluster)")
	master      = flag.String("master", "", "Apiserver address (overrides kubeconfig)")
	leaderElect = flag.Bool("leader-elect", true, "Enable leader election (run a single active replica)")
	leNamespace = flag.String("leader-elect-namespace", "volcano-system", "Namespace for the leader-election lease")
	resync      = flag.Duration("resync-period", 0, "Informer resync period (0 = disabled)")
	workers     = flag.Int("workers", 1, "Reconcile worker count shared by the RepackRun and RepackPolicy controllers; nomination status writes remain single-threaded")
	// Keep in sync with the engine's --repack-execute-cooldown so GC does not delete
	// a finished Execute run while it is still the engine's cooldown anchor.
	execCooldown = flag.Duration("repack-execute-cooldown", 10*time.Minute, "Minimum gap the engine enforces between Execute runs; GC retains a finished Execute run at least this long to preserve the cooldown anchor")
	// Same flag name and default as the controller-manager shim's
	// --repack-policy-frag-eval-cycle.
	policyEvalCycle = flag.Duration("repack-policy-frag-eval-cycle", repackpolicy.DefaultFragEvalCycle,
		"Interval between onFragAbovePercent trigger evaluations; keep it >= --repack-execute-cooldown")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	cfg, err := buildConfig(*master, *kubeconfig)
	if err != nil {
		klog.Fatalf("build config: %v", err)
	}
	kube := kubernetes.NewForConfigOrDie(cfg)
	vc := vcclientset.NewForConfigOrDie(cfg)
	ctx := signalContext()

	run := func(ctx context.Context) {
		vcFactory := vcinformers.NewSharedInformerFactory(vc, *resync)
		kubeFactory := informers.NewSharedInformerFactory(kube, *resync)

		ctrl := repackcontroller.New(vc, vcFactory, repackcontroller.Options{Workers: *workers, ExecuteCooldown: *execCooldown})
		podInformer := kubeFactory.Core().V1().Pods()
		repackInformer := vcFactory.Repack().V1alpha1().RepackRuns()
		nom := repackcontroller.NewNominator(kube, vc, podInformer, repackInformer)
		nom.SetEventRecorder(repackcontroller.NewEventRecorder(kube, "volcano-repack-controller"))

		policyCtrl := repackpolicy.New(vc, kubeFactory, vcFactory, repackpolicy.Options{
			Workers:       *workers,
			FragEvalCycle: *policyEvalCycle,
		})

		kubeFactory.Start(ctx.Done())
		go func() {
			if err := ctrl.Run(ctx); err != nil {
				klog.ErrorS(err, "RepackRun controller stopped")
			}
		}()
		go func() {
			if err := nom.Run(ctx); err != nil {
				klog.ErrorS(err, "Repack nominator stopped")
			}
		}()
		go func() {
			if err := policyCtrl.Run(ctx); err != nil {
				klog.ErrorS(err, "RepackPolicy controller stopped")
			}
		}()
		<-ctx.Done()
	}

	if !*leaderElect {
		run(ctx)
		return
	}

	id, _ := os.Hostname()
	id = id + "_" + string(uuid.NewUUID())
	lock, err := resourcelock.New(resourcelock.LeasesResourceLock, *leNamespace, "repack-controller",
		kube.CoreV1(), kube.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: id})
	if err != nil {
		klog.Fatalf("resource lock: %v", err)
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() { klog.Fatal("lost leadership") },
		},
	})
}

func buildConfig(master, kubeconfig string) (*rest.Config, error) {
	if master == "" && kubeconfig == "" {
		if c, err := rest.InClusterConfig(); err == nil {
			return c, nil
		}
	}
	return clientcmd.BuildConfigFromFlags(master, kubeconfig)
}

func signalContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
		<-ch
		os.Exit(1)
	}()
	return ctx
}
