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

package conf

import (
	"reflect"
	"strings"
	"testing"

	"volcano.sh/volcano/pkg/repackengine/framework"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/binpack"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/gangdisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/nodeconsolidation"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/pdbconstraint"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/repackbudget"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloaddisruption"
	_ "volcano.sh/volcano/pkg/repackengine/plugins/workloadscope"
)

type capabilityTestPlugin struct{ name string }

type configurationTestAction struct{}

func (*configurationTestAction) Name() string { return framework.ActionRepack }

func (*configurationTestAction) Execute(*framework.ActionContext) framework.ActionResult {
	return framework.ActionResult{Stop: true}
}

func init() {
	// Configuration validation only needs the action's declared capability.
	// Registering a local stub keeps this package independent of the concrete
	// Repack action and prevents a status -> conf -> action -> status test cycle.
	framework.RegisterAction(framework.ActionRepack, framework.ActionRegistration{
		Factory:  func() framework.Action { return &configurationTestAction{} },
		Requires: []framework.PluginCapability{framework.CapabilityDomain},
	})
}

func (p *capabilityTestPlugin) Name() string                    { return p.name }
func (*capabilityTestPlugin) OnSessionOpen(*framework.Session)  {}
func (*capabilityTestPlugin) OnSessionClose(*framework.Session) {}

func TestRepackConfigurationAndExplicitPrecedence(t *testing.T) {
	configured, err := Decode([]byte(`
actions: "prepare, repack"
plugins:
  - name: workloaddisruption
    arguments:
      movedPodsWeight: 25
  - name: binpack
`))
	if err != nil {
		t.Fatalf("decode configuration: %v", err)
	}
	if configured.Actions != "prepare, repack" {
		t.Fatalf("actions=%q, want comma-separated source value", configured.Actions)
	}
	if len(configured.Plugins) != 2 || configured.Plugins[0].Name != "workloaddisruption" ||
		configured.Plugins[0].Arguments["movedPodsWeight"] != 25 {
		t.Fatalf("plugins=%+v, want ordered plugins with arguments", configured.Plugins)
	}

	fromFile := Config{Plugins: DefaultPluginOptions()}
	ApplyFile(&fromFile, configured, false, false)
	if got := fromFile.Actions; !reflect.DeepEqual(got, []string{"prepare", "repack"}) {
		t.Fatalf("parsed actions=%v, want [prepare repack]", got)
	}
	if got := testConfiguredPluginNames(fromFile.Plugins); !reflect.DeepEqual(got, []string{"workloaddisruption", "binpack"}) {
		t.Fatalf("file plugins=%v, want [workloaddisruption binpack]", got)
	}

	explicit := Config{Actions: []string{"repack"}, Plugins: framework.PluginOptions("workloadscope", "nodeconsolidation")}
	ApplyFile(&explicit, configured, true, true)
	if got := explicit.Actions; !reflect.DeepEqual(got, []string{"repack"}) {
		t.Fatalf("explicit actions=%v, want command/programmatic override", got)
	}
	if got := testConfiguredPluginNames(explicit.Plugins); !reflect.DeepEqual(got, []string{"workloadscope", "nodeconsolidation"}) {
		t.Fatalf("explicit plugins=%v, want command/programmatic override", got)
	}
}

func TestDecodeRepackConfigurationRejectsActionList(t *testing.T) {
	_, err := Decode([]byte(`
actions:
  - repack
`))
	if err == nil {
		t.Fatal("actions list must be rejected; want Scheduler-compatible comma-separated string")
	}
}

func TestDecodeRepackConfigurationRejectsUnknownFields(t *testing.T) {
	tests := map[string]string{
		"top-level": `action: "repack"`,
		"duplicate": `
actions: "repack"
actions: "repack"
`,
		"plugin-option": `
actions: "repack"
plugins:
  - name: gangdisruption
    argments:
      gangBreachesWeight: 0
`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatal("unknown configuration field must be rejected")
			}
		})
	}
}

func TestValidatePluginOptionsAllowsOptionalPolicies(t *testing.T) {
	if err := ValidatePluginOptions(nil); err != nil {
		t.Fatalf("empty optional plugin pipeline should be valid: %v", err)
	}
	if err := ValidatePluginOptions(framework.PluginOptions("workloadscope")); err != nil {
		t.Fatalf("workloadscope should be independently configurable: %v", err)
	}
	if err := ValidatePluginOptions(framework.PluginOptions("repackbudget")); err != nil {
		t.Fatalf("repackbudget should be independently configurable: %v", err)
	}
	if err := ValidatePluginOptions(framework.PluginOptions("pdbconstraint")); err != nil {
		t.Fatalf("pdbconstraint should be independently configurable: %v", err)
	}
	if err := ValidatePluginOptions([]framework.PluginOption{{Name: "pdbconstraint", Arguments: framework.Arguments{"unexpected": true}}}); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("pdbconstraint argument error=%v, want unknown-argument rejection", err)
	}
	if err := ValidatePluginOptions(framework.PluginOptions("workloadscope", "workloadscope", "repackbudget")); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate error=%v, want duplicate-plugin rejection", err)
	}
	invalidWeight := []framework.PluginOption{
		{Name: "workloadscope"},
		{Name: "repackbudget"},
		{Name: "workloaddisruption", Arguments: framework.Arguments{"movedPodsWeight": -1}},
	}
	if err := ValidatePluginOptions(invalidWeight); err == nil || !strings.Contains(err.Error(), "movedPodsWeight") {
		t.Fatalf("invalid weight error=%v, want plugin argument rejection", err)
	}
}

func TestValidatePipelineConfigurationRequiresDomainCapability(t *testing.T) {
	withoutDomain := framework.PluginOptions("workloadscope", "repackbudget", "binpack")
	if err := ValidatePipeline([]string{"repack"}, withoutDomain); err == nil ||
		!strings.Contains(err.Error(), `requires capability "domain"`) {
		t.Fatalf("pipeline validation error=%v, want missing domain capability", err)
	}

	minimal := framework.PluginOptions("nodeconsolidation")
	if err := ValidatePipeline([]string{"repack"}, minimal); err != nil {
		t.Fatalf("nodeconsolidation should satisfy the repack domain capability: %v", err)
	}

	const replacement = "test-replacement-domain"
	framework.RegisterPlugin(replacement, framework.PluginRegistration{
		Factory:  func(framework.Arguments) framework.Plugin { return &capabilityTestPlugin{name: replacement} },
		Provides: []framework.PluginCapability{framework.CapabilityDomain},
	})
	if err := ValidatePipeline([]string{"repack"}, framework.PluginOptions(replacement)); err != nil {
		t.Fatalf("a replacement domain provider must satisfy repack without nodeconsolidation: %v", err)
	}
	emptyProvider := framework.OpenSession(framework.SessionConfig{}, framework.PluginOptions(replacement))
	defer framework.CloseSession(emptyProvider)
	if err := ValidateSession([]string{"repack"}, framework.PluginOptions(replacement), emptyProvider); err == nil ||
		!strings.Contains(err.Error(), `runtime capability "domain"`) {
		t.Fatalf("runtime capability error=%v, want missing AddDomainFn rejection", err)
	}

	workingProvider := framework.OpenSession(framework.SessionConfig{}, minimal)
	defer framework.CloseSession(workingProvider)
	if err := ValidateSession([]string{"repack"}, minimal, workingProvider); err != nil {
		t.Fatalf("nodeconsolidation must register the runtime domain capability: %v", err)
	}
}

func testConfiguredPluginNames(options []framework.PluginOption) []string {
	names := make([]string, 0, len(options))
	for _, option := range options {
		names = append(names, option.Name)
	}
	return names
}
