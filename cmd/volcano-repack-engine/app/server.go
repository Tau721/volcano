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

package app

import (
	"context"
	"fmt"
	"net/http"
	"os"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"volcano.sh/apis/pkg/apis/helpers"

	"volcano.sh/volcano/cmd/volcano-repack-engine/app/options"
	"volcano.sh/volcano/pkg/kube"
	"volcano.sh/volcano/pkg/repackengine"
	schedmetrics "volcano.sh/volcano/pkg/scheduler/metrics"
	"volcano.sh/volcano/pkg/signals"
	commonutil "volcano.sh/volcano/pkg/util"
)

const componentName = "volcano-repack-engine"

// Run starts the repack engine, optionally behind leader election.
func Run(opt *options.ServerOption) error {
	config, err := kube.BuildConfig(opt.KubeClientOptions)
	if err != nil {
		return err
	}

	// Reuse the scheduler cache against clusters of different Kubernetes
	// versions. Its feature-gated informers (notably DRA) must match the API
	// server before the cache is constructed, otherwise an older cluster can
	// never complete cache sync for resources it does not serve.
	if err := commonutil.SetupComponentGlobals(config); err != nil {
		klog.Errorf("failed to set component globals: %v", err)
		return err
	}

	// k8s scheduler framework plugins (interpodaffinity PreFilter, etc.) use
	// k8smetrics.Goroutines via Parallelizer.Until; init before opening sessions.
	schedmetrics.InitKubeSchedulerRelatedMetrics()

	// Liveness: /healthz so Kubernetes can restart a wedged engine. Started before
	// leader election so standby replicas are also live.
	if opt.EnableHealthz {
		if err := helpers.StartHealthz(opt.HealthzBindAddress, componentName, nil, nil, nil); err != nil {
			return err
		}
	}
	// Observability: Prometheus /metrics.
	if opt.EnableMetrics {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", commonutil.PromHandler())
			server := &http.Server{
				Addr:              opt.ListenAddress,
				Handler:           mux,
				ReadHeaderTimeout: helpers.DefaultReadHeaderTimeout,
				ReadTimeout:       helpers.DefaultReadTimeout,
				WriteTimeout:      helpers.DefaultWriteTimeout,
			}
			klog.Fatalf("repack-engine metrics server failed: %s", server.ListenAndServe())
		}()
	}

	engine, err := repackengine.NewEngine(config, repackengine.Config{
		SchedulerConf:    opt.SchedulerConf,
		RepackConf:       opt.RepackConf,
		ResyncPeriod:     opt.ResyncPeriod,
		Cooldown:         opt.Cooldown,
		Actions:          opt.Actions,
		Plugins:          repackengine.PluginOptions(opt.Plugins),
		MinNodesFreed:    opt.MinNodesFreed,
		DefaultResource:  opt.DefaultResource,
		ExecutionTimeout: opt.ExecutionTimeout,
	})
	if err != nil {
		return err
	}

	ctx := signals.SetupSignalContext()
	run := func(ctx context.Context) {
		engine.Run(ctx)
	}

	if !opt.LeaderElection.LeaderElect {
		run(ctx)
		return fmt.Errorf("finished without leader elect")
	}

	leaderElectionClient, err := clientset.NewForConfig(restclient.AddUserAgent(config, "leader-election"))
	if err != nil {
		return err
	}
	broadcaster := record.NewBroadcaster()
	defer broadcaster.Shutdown()
	broadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: leaderElectionClient.CoreV1().Events(opt.LeaderElection.ResourceNamespace)})
	eventRecorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: componentName})

	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("unable to get hostname: %v", err)
	}
	id := hostname + "_" + string(uuid.NewUUID())
	if len(opt.LockObjectNamespace) > 0 {
		opt.LeaderElection.ResourceNamespace = opt.LockObjectNamespace
	}
	rl, err := resourcelock.New(resourcelock.LeasesResourceLock,
		opt.LeaderElection.ResourceNamespace,
		opt.LeaderElection.ResourceName,
		leaderElectionClient.CoreV1(),
		leaderElectionClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: id, EventRecorder: eventRecorder})
	if err != nil {
		return fmt.Errorf("couldn't create resource lock: %v", err)
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: opt.LeaderElection.LeaseDuration.Duration,
		RenewDeadline: opt.LeaderElection.RenewDeadline.Duration,
		RetryPeriod:   opt.LeaderElection.RetryPeriod.Duration,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() { klog.Fatalf("leaderelection lost") },
		},
	})
	return fmt.Errorf("lost lease")
}
