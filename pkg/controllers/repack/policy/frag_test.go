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

package policy

import (
	"math"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// measureFrag fills providing/occupied/optimal and clamps the rate into [0,1] on
// the shared half-fragmented cluster (halfFragCluster, fixture_test.go).
func TestFragMeasureHalfFragmented(t *testing.T) {
	f := newFixture(t, tCreate, DefaultFragEvalCycle)
	halfFragCluster(f)

	r, err := f.c.measureFrag(testResource)
	if err != nil {
		t.Fatalf("measureFrag returned error: %v", err)
	}
	if r.Providing != 2 || r.Occupied != 2 || r.Optimal != 1 {
		t.Fatalf("measureFrag = %+v, want providing=2 occupied=2 optimal=1", r)
	}
	if !almostEqual(r.Rate, 0.5) {
		t.Errorf("rate = %v, want 0.5", r.Rate)
	}
}

// Providing counts nodes with Allocatable[R]>0; occupied counts only nodes
// carrying a bound, non-terminal pod request.
func TestFragMeasureTerminalPodFiltered(t *testing.T) {
	f := newFixture(t, tCreate, DefaultFragEvalCycle)
	addNode(f, "n1", 8)
	addNode(f, "n2", 8)
	addNode(f, "n3", 8) // providing but empty
	addPod(f, "p-live", "n1", v1.PodRunning, 4)
	// Terminal pods must not count as occupied, or onFrag never converges.
	addPod(f, "p-done", "n2", v1.PodSucceeded, 4)
	addPod(f, "p-failed", "n2", v1.PodFailed, 4)
	// Unbound pods are ignored entirely.
	addPod(f, "p-pending", "", v1.PodPending, 4)

	r, err := f.c.measureFrag(testResource)
	if err != nil {
		t.Fatalf("measureFrag returned error: %v", err)
	}
	if r.Providing != 3 {
		t.Errorf("providing = %d, want 3 (nodes with allocatable)", r.Providing)
	}
	if r.Occupied != 1 {
		t.Errorf("occupied = %d, want 1 (only n1 holds a live bound pod)", r.Occupied)
	}
	if r.Optimal != 1 {
		t.Errorf("optimal = %d, want 1 (single 4-device demand fits one 8-device node)", r.Optimal)
	}
}

// A pod on a node that does not provide the resource is not part of the demand;
// no-provider clusters report a zero rate instead of panicking.
func TestFragMeasurePodsOnNonProvidingAndEmptyCluster(t *testing.T) {
	f := newFixture(t, tCreate, DefaultFragEvalCycle)
	addNode(f, "n1", 8)
	addPod(f, "p-on-n1", "n1", v1.PodRunning, 4)
	addPod(f, "p-on-nowhere", "ghost-node", v1.PodRunning, 4) // no allocatable[R] node

	r, err := f.c.measureFrag(testResource)
	if err != nil {
		t.Fatalf("measureFrag returned error: %v", err)
	}
	if r.Providing != 1 || r.Occupied != 1 || r.Optimal != 1 {
		t.Fatalf("measureFrag = %+v, want providing=1 occupied=1 optimal=1", r)
	}

	empty := newFixture(t, tCreate, DefaultFragEvalCycle)
	z, err := empty.c.measureFrag(testResource)
	if err != nil {
		t.Fatalf("empty-cluster measureFrag returned error: %v", err)
	}
	if z.Providing != 0 || z.Occupied != 0 || z.Rate != 0 {
		t.Fatalf("empty-cluster measureFrag = %+v, want all zeros / rate 0", z)
	}
}
