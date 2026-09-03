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
	"reflect"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func TestRepackConfigurationFlagsPreserveOrder(t *testing.T) {
	option := NewServerOption()
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	option.AddFlags(flags)
	if err := flags.Parse([]string{
		"--repack-conf=/etc/volcano/repack-engine.conf",
		"--repack-actions=prepare,repack",
		"--repack-plugins=workloadscope,nodeconsolidation,gangdisruption,binpack",
	}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if want := []string{"workloadscope", "nodeconsolidation", "gangdisruption", "binpack"}; !reflect.DeepEqual(option.Plugins, want) {
		t.Fatalf("plugins=%v, want %v", option.Plugins, want)
	}
	if want := []string{"prepare", "repack"}; !reflect.DeepEqual(option.Actions, want) {
		t.Fatalf("actions=%v, want %v", option.Actions, want)
	}
	if option.RepackConf != "/etc/volcano/repack-engine.conf" {
		t.Fatalf("repack-conf=%q, want configured path", option.RepackConf)
	}
}

func TestExecutionTimeoutReplacesNominationTTLFlag(t *testing.T) {
	option := NewServerOption()
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	option.AddFlags(flags)
	if flags.Lookup("repack-nomination-ttl") != nil {
		t.Fatal("legacy --repack-nomination-ttl flag must not be registered")
	}
	if err := flags.Parse([]string{"--repack-execution-timeout=7m"}); err != nil {
		t.Fatalf("parse execution timeout: %v", err)
	}
	if option.ExecutionTimeout != 7*time.Minute {
		t.Fatalf("ExecutionTimeout=%s, want 7m", option.ExecutionTimeout)
	}
}
