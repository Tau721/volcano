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

package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/pflag"
	_ "go.uber.org/automaxprocs"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	cliflag "k8s.io/component-base/cli/flag"
	componentbaseoptions "k8s.io/component-base/config/options"
	"k8s.io/klog/v2"

	schedoptions "volcano.sh/volcano/cmd/scheduler/app/options"
	"volcano.sh/volcano/cmd/volcano-repack-engine/app"
	"volcano.sh/volcano/cmd/volcano-repack-engine/app/options"
	commonutil "volcano.sh/volcano/pkg/util"
	"volcano.sh/volcano/pkg/version"

	// Reuse the scheduler's plugins so the engine's sessions behave identically to
	// the running scheduler (predicates/filters). The scheduler actions are imported
	// only so UnmarshalSchedulerConf can resolve the shared conf's `actions:` names;
	// the engine consumes the tiers/configurations, not those actions.
	_ "volcano.sh/volcano/pkg/scheduler/actions"
	_ "volcano.sh/volcano/pkg/scheduler/plugins"

	// Register the repack action pipeline and capability plugins.
	_ "volcano.sh/volcano/pkg/repackengine/actions/repack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/gangdisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/networktopologyaware"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/nodeconsolidation"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/pdbconstraint"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/repackbudget"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloaddisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloadscope"
)

var logFlushFreq = pflag.Duration("log-flush-frequency", 5*time.Second, "Maximum number of seconds between log flushes")

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	klog.InitFlags(nil)
	flag.Set("logtostderr", "true") //nolint:errcheck

	fs := pflag.CommandLine
	s := options.NewServerOption()
	s.AddFlags(fs)
	utilfeature.DefaultMutableFeatureGate.AddFlag(fs)

	commonutil.LeaderElectionDefault(&s.LeaderElection)
	s.LeaderElection.ResourceName = "volcano-repack-engine"
	componentbaseoptions.BindLeaderElectionFlags(&s.LeaderElection, fs)

	cliflag.InitFlags()

	if s.PrintVersion {
		version.PrintVersionAndExit()
		return
	}

	if err := s.CheckOptionOrDie(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	ensureSchedulerServerOpts()

	klog.StartFlushDaemon(*logFlushFreq)
	defer klog.Flush()

	if err := app.Run(s); err != nil {
		klog.Fatalf("%v\n", err)
	}
}

// ensureSchedulerServerOpts initializes the scheduler's global options before the
// engine constructs schedcache.New. The cache shares scheduler globals (e.g.
// sharding mode) even when used read-only; without this, addEventHandler panics
// on a nil ServerOpts.
func ensureSchedulerServerOpts() {
	if schedoptions.ServerOpts != nil {
		return
	}
	schedOpts := schedoptions.NewServerOption()
	schedOpts.ShardingMode = commonutil.NoneShardingMode
	schedOpts.SchedulerNames = []string{"volcano"}
	schedOpts.DefaultQueue = "default"
	schedOpts.RegisterOptions()
}
