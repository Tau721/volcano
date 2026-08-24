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

// Package conf owns volcano-repack-engine configuration, defaults and
// validation. It deliberately contains no informer, cache or execution state.
package conf

import (
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"volcano.sh/volcano/pkg/repackengine/framework"
)

// Config holds the engine's runtime parameters.
type Config struct {
	SchedulerConf   string
	RepackConf      string
	ResyncPeriod    time.Duration
	Cooldown        time.Duration
	Plugins         []framework.PluginOption
	Actions         []string
	MinNodesFreed   int
	DefaultResource string
	NominationTTL   time.Duration
}

// FileConfiguration is the strict YAML representation of repack-engine.conf.
type FileConfiguration struct {
	Actions string                   `yaml:"actions"`
	Plugins []framework.PluginOption `yaml:"plugins"`
}

const DefaultNominationTTL = 10 * time.Minute

// ApplyDefaults fills runtime defaults that are independent of command flags.
func ApplyDefaults(config *Config) {
	if len(config.Plugins) == 0 {
		config.Plugins = DefaultPluginOptions()
	}
	if config.NominationTTL <= 0 {
		config.NominationTTL = DefaultNominationTTL
	}
}

// Decode strictly decodes repack-engine.conf and rejects unknown fields.
func Decode(raw []byte) (FileConfiguration, error) {
	var fileConfig FileConfiguration
	if err := yaml.UnmarshalStrict(raw, &fileConfig); err != nil {
		return FileConfiguration{}, err
	}
	return fileConfig, nil
}

// ApplyFile merges file configuration while preserving explicit command-line
// or programmatic overrides.
func ApplyFile(config *Config, fileConfig FileConfiguration, actionsExplicit, pluginsExplicit bool) {
	if !actionsExplicit {
		config.Actions = ParseActionNames(fileConfig.Actions)
	}
	if !pluginsExplicit && len(fileConfig.Plugins) > 0 {
		config.Plugins = fileConfig.Plugins
	}
}

func ParseActionNames(actions string) []string {
	var names []string
	for _, name := range strings.Split(actions, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names
}

func DefaultPluginOptions() []framework.PluginOption {
	return framework.PluginOptions("workloadscope", "repackbudget", "nodeconsolidation", "workloaddisruption", "gangdisruption", "binpack")
}

func PluginOptions(names []string) []framework.PluginOption {
	return framework.PluginOptions(names...)
}
