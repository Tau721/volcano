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

// Package cache owns the scheduler-backed cluster snapshot used by the Repack
// Engine. Keeping cache construction and session opening here prevents the
// runtime orchestration layer from depending on scheduler cache details.
package cache

import (
	"context"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	schedoptions "volcano.sh/volcano/cmd/scheduler/app/options"
	schedcache "volcano.sh/volcano/pkg/scheduler/cache"
	schedconf "volcano.sh/volcano/pkg/scheduler/conf"
	schedframework "volcano.sh/volcano/pkg/scheduler/framework"
	commonutil "volcano.sh/volcano/pkg/util"
)

const nodeWorkers = 4

// resourceSyncTimeout bounds the scheduler-cache handler drain at Engine
// startup, matching the vc-scheduler default. Without it the Engine can open a
// session while the initial node list is still draining and reconcile against a
// partial node set.
const resourceSyncTimeout = 60 * time.Second

// Cluster is a read-only scheduler cache view for Repack planning. The engine
// opens read-only scheduler sessions from it and never writes scheduler-owned
// PodGroup, Queue, or Job status.
type Cluster struct {
	scheduler schedcache.Cache
}

func NewCluster(config *rest.Config) *Cluster {
	// Scheduler predicates and cache helpers read the scheduler's process-wide
	// options. The standalone Repack binary must initialize safe defaults before
	// constructing that shared machinery.
	if schedoptions.ServerOpts == nil {
		options := schedoptions.NewServerOption()
		options.ShardingMode = commonutil.NoneShardingMode
		options.RegisterOptions()
	}

	schedulerNames := schedoptions.ServerOpts.SchedulerNames
	if len(schedulerNames) == 0 {
		schedulerNames = []string{"volcano"}
	}
	return &Cluster{scheduler: schedcache.New(
		config,
		schedulerNames,
		schedoptions.ServerOpts.DefaultQueue,
		schedoptions.ServerOpts.NodeSelector,
		nodeWorkers,
		schedoptions.ServerOpts.IgnoredCSIProvisioners,
		0,
		resourceSyncTimeout,
	)}
}

func (c *Cluster) Run(ctx context.Context) {
	c.scheduler.Run(ctx.Done())
}

func (c *Cluster) OpenSession(tiers []schedconf.Tier, configurations []schedconf.Configuration) *schedframework.Session {
	return schedframework.OpenSession(c.scheduler, tiers, configurations)
}

func (c *Cluster) Client() kubernetes.Interface {
	return c.scheduler.Client()
}
