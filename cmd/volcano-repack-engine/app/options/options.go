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

package options

import (
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/validation/field"
	componentbaseconfig "k8s.io/component-base/config"
	componentbaseconfigvalidation "k8s.io/component-base/config/validation"

	"volcano.sh/volcano/pkg/kube"
)

const (
	defaultQPS   = 50.0
	defaultBurst = 100

	// Aligned with the other Volcano components: healthz :11251 (same as
	// volcano-scheduler / volcano-controller-manager), metrics :8081 (same as
	// volcano-controller-manager). Each runs in its own pod, so there is no port
	// collision — this is purely for a consistent, memorable convention.
	defaultHealthzAddress = ":11251"
	defaultMetricsAddress = ":8081"
	// defaultExecuteCooldown is kept in sync with the controller's GC cooldown
	// floor. defaultExecutionTimeout bounds the complete Execute workflow.
	defaultExecuteCooldown  = 10 * time.Minute
	defaultExecutionTimeout = 10 * time.Minute
	defaultResyncPeriod     = 10 * time.Minute
)

// ServerOption holds the volcano-repack-engine configuration.
type ServerOption struct {
	KubeClientOptions kube.ClientOptions
	// SchedulerConf is the SAME --scheduler-conf the volcano-scheduler uses, so
	// the engine's tiers/plugins match the scheduler's exactly.
	SchedulerConf string
	// RepackConf is the engine-owned action/plugin configuration file.
	RepackConf   string
	ResyncPeriod time.Duration

	// Actions override the ordered action pipeline. Plugins select an
	// order-independent capability set.
	// MinNodesFreed is the benefit gate. DefaultResource is the target when a
	// RepackRun's spec.goals is empty.
	Actions          []string
	Plugins          []string
	MinNodesFreed    int
	DefaultResource  string
	ExecutionTimeout time.Duration
	Cooldown         time.Duration

	LeaderElection      componentbaseconfig.LeaderElectionConfiguration
	LockObjectNamespace string

	// EnableHealthz serves /healthz (liveness) on HealthzBindAddress; EnableMetrics
	// serves Prometheus /metrics on ListenAddress.
	EnableHealthz      bool
	HealthzBindAddress string
	EnableMetrics      bool
	ListenAddress      string

	PrintVersion bool
}

// NewServerOption creates a ServerOption with defaults.
func NewServerOption() *ServerOption {
	return &ServerOption{}
}

// AddFlags binds the options to a flag set.
func (s *ServerOption) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&s.KubeClientOptions.Master, "master", s.KubeClientOptions.Master,
		"The address of the Kubernetes API server (overrides any value in kubeconfig)")
	fs.StringVar(&s.KubeClientOptions.KubeConfig, "kubeconfig", s.KubeClientOptions.KubeConfig,
		"Path to kubeconfig file with authorization and master location information")
	fs.Float32Var(&s.KubeClientOptions.QPS, "kube-api-qps", defaultQPS, "QPS to use while talking with kubernetes apiserver")
	fs.IntVar(&s.KubeClientOptions.Burst, "kube-api-burst", defaultBurst, "Burst to use while talking with kubernetes apiserver")

	fs.StringVar(&s.SchedulerConf, "scheduler-conf", "", "Absolute path of the shared scheduler configuration file (same as volcano-scheduler)")
	fs.StringVar(&s.RepackConf, "repack-conf", "", "Absolute path of the Repack Engine action/plugin configuration file")
	fs.DurationVar(&s.ResyncPeriod, "resync-period", defaultResyncPeriod, "Informer resync safety-net period so a dropped watch that misses events self-heals (0 = pure event-driven)")
	fs.DurationVar(&s.Cooldown, "repack-execute-cooldown", defaultExecuteCooldown, "Minimum gap after an Execute RepackRun finishes before the next Execute may start")

	fs.StringSliceVar(&s.Actions, "repack-actions", nil, "Ordered action pipeline override (comma-separated; default: repack)")
	fs.StringSliceVar(&s.Plugins, "repack-plugins", nil,
		"Repack capability plugin set; input order does not affect behavior (default: workloadscope,repackbudget,nodeconsolidation,networktopologyaware,workloaddisruption,gangdisruption,binpack)")
	fs.IntVar(&s.MinNodesFreed, "repack-min-nodes-freed", 0, "Benefit gate: minimum whole nodes a plan must free (0 = engine default 1)")
	fs.StringVar(&s.DefaultResource, "repack-default-resource", "", "Target resource when a RepackRun's spec.goals is empty (e.g. nvidia.com/gpu)")
	fs.DurationVar(&s.ExecutionTimeout, "repack-execution-timeout", defaultExecutionTimeout, "Maximum time from the first eviction batch until eviction, replacement placement, and result verification complete")

	fs.BoolVar(&s.EnableHealthz, "enable-healthz", false, "Enable the /healthz liveness endpoint (false by default)")
	fs.StringVar(&s.HealthzBindAddress, "healthz-address", defaultHealthzAddress, "The address to listen on for the /healthz health-check server")
	fs.BoolVar(&s.EnableMetrics, "enable-metrics", false, "Enable the Prometheus /metrics endpoint (false by default)")
	fs.StringVar(&s.ListenAddress, "listen-address", defaultMetricsAddress, "The address to listen on for the Prometheus /metrics server")

	fs.BoolVar(&s.PrintVersion, "version", false, "Show version and quit")
	//lint:ignore SA1019 kept for compatibility with the other components' flags
	fs.StringVar(&s.LockObjectNamespace, "lock-object-namespace", "", "Define the namespace of the lock object that is used for leader election")
}

// CheckOptionOrDie validates the leader-election configuration.
func (s *ServerOption) CheckOptionOrDie() error {
	return componentbaseconfigvalidation.ValidateLeaderElectionConfiguration(
		&s.LeaderElection, field.NewPath("leaderElection")).ToAggregate()
}
