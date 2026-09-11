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

package repack

import (
	"testing"
	"time"

	"github.com/spf13/pflag"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"

	vcfake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	vcinformer "volcano.sh/apis/pkg/client/informers/externalversions"

	"volcano.sh/volcano/pkg/controllers/framework"
	repackpolicy "volcano.sh/volcano/pkg/controllers/repack/policy"
)

// AddFlags registers --repack-policy-frag-eval-cycle with the 10m default.
func TestAddFlagsRegistersFragEvalCycle(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	c := &frameworkController{}
	c.AddFlags(fs)

	flag := fs.Lookup("repack-policy-frag-eval-cycle")
	if flag == nil {
		t.Fatal("--repack-policy-frag-eval-cycle not registered")
	}
	if got := c.fragEvalCycle; got != repackpolicy.DefaultFragEvalCycle {
		t.Errorf("flag value = %v, want default %v (10m)", got, repackpolicy.DefaultFragEvalCycle)
	}
	if got := repackpolicy.DefaultFragEvalCycle; got != 10*time.Minute {
		t.Errorf("DefaultFragEvalCycle = %v, want 10m", got)
	}
}

// Initialize constructs the policy controller from the shared factories and
// propagates the eval cycle; a bare controller (AddFlags skipped, e.g. in wiring
// tests) falls back to the package default.
func TestInitializeBuildsPolicyController(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	vcClient := vcfake.NewSimpleClientset()
	coreFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	vcFactory := vcinformer.NewSharedInformerFactory(vcClient, 0)

	c := &frameworkController{}
	err := c.Initialize(&framework.ControllerOption{
		KubeClient:              kubeClient,
		VolcanoClient:           vcClient,
		SharedInformerFactory:   coreFactory,
		VCSharedInformerFactory: vcFactory,
		WorkerNum:               2,
	})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if c.policyCtrl == nil {
		t.Error("policyCtrl not constructed")
	}
	if c.runCtrl == nil {
		t.Error("RepackRun lifecycle controller not constructed")
	}
	if c.nominator == nil {
		t.Error("nominator not constructed")
	}
}
