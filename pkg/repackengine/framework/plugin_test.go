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

package framework

import (
	"reflect"
	"testing"
)

type argumentSpyPlugin struct{}

func (*argumentSpyPlugin) Name() string            { return "argument-spy" }
func (*argumentSpyPlugin) OnSessionOpen(*Session)  {}
func (*argumentSpyPlugin) OnSessionClose(*Session) {}

type orderingSpyPlugin struct{ name string }

func (p *orderingSpyPlugin) Name() string          { return p.name }
func (*orderingSpyPlugin) OnSessionOpen(*Session)  {}
func (*orderingSpyPlugin) OnSessionClose(*Session) {}

func TestOpenSessionPassesPluginArguments(t *testing.T) {
	const name = "test-argument-spy"
	var received Arguments
	RegisterPlugin(name, PluginRegistration{
		Factory: func(arguments Arguments) Plugin {
			received = arguments
			return &argumentSpyPlugin{}
		},
	})
	t.Cleanup(func() { delete(pluginRegistry, name) })

	ssn := OpenSession(SessionConfig{}, []PluginOption{{
		Name:      name,
		Arguments: Arguments{"weight": 2},
	}})
	CloseSession(ssn)

	got, err := received.NonNegativeInt("weight", 0)
	if err != nil || got != 2 {
		t.Fatalf("plugin weight=%v, want 2", got)
	}
}

func TestOpenSessionPreservesConfiguredPluginOrder(t *testing.T) {
	const first, second = "test-order-z", "test-order-a"
	for _, name := range []string{first, second} {
		pluginName := name
		RegisterPlugin(pluginName, PluginRegistration{
			Factory: func(Arguments) Plugin { return &orderingSpyPlugin{name: pluginName} },
		})
		t.Cleanup(func() { delete(pluginRegistry, pluginName) })
	}

	configured := []PluginOption{{Name: first}, {Name: second}}
	ssn := OpenSession(SessionConfig{}, configured)
	defer CloseSession(ssn)

	got := make([]string, 0, len(ssn.plugins))
	for _, plugin := range ssn.plugins {
		got = append(got, plugin.Name())
	}
	if want := []string{first, second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opened plugins=%v, want configured order %v", got, want)
	}
	if configured[0].Name != first || configured[1].Name != second {
		t.Fatalf("OpenSession mutated caller configuration: %+v", configured)
	}
}

func TestNonNegativeInt(t *testing.T) {
	arguments := Arguments{"integer": 2, "zero": 0}
	for key, want := range map[string]int64{"integer": 2, "zero": 0} {
		if got, err := arguments.NonNegativeInt(key, 1); err != nil || got != want {
			t.Errorf("%s=%v err=%v, want %d", key, got, err, want)
		}
	}
	if got, err := arguments.NonNegativeInt("omitted", 10); err != nil || got != 10 {
		t.Fatalf("omitted=%v err=%v, want default 10", got, err)
	}
	for name, value := range map[string]interface{}{
		"negative":   -1,
		"fractional": 0.5,
		"float":      3.0,
		"string":     "1",
		"tooLarge":   int64(maxPluginWeight + 1),
	} {
		if _, err := (Arguments{"weight": value}).NonNegativeInt("weight", 1); err == nil {
			t.Errorf("%s value %v should be rejected", name, value)
		}
	}
	if err := (Arguments{"movedPodWeight": 1}).ValidateKeys("movedPodsWeight"); err == nil {
		t.Fatal("unknown/misspelled argument should be rejected")
	}
}

func TestBool(t *testing.T) {
	arguments := Arguments{"on": true, "off": false}
	for key, want := range map[string]bool{"on": true, "off": false} {
		if got, err := arguments.Bool(key, !want); err != nil || got != want {
			t.Errorf("%s=%v err=%v, want %v", key, got, err, want)
		}
	}
	if got, err := arguments.Bool("omitted", true); err != nil || !got {
		t.Fatalf("omitted=%v err=%v, want default true", got, err)
	}
	for name, value := range map[string]interface{}{
		"string": "true",
		"int":    1,
		"nil":    nil,
	} {
		if _, err := (Arguments{"enabled": value}).Bool("enabled", false); err == nil {
			t.Errorf("%s value %v should be rejected", name, value)
		}
	}
}
