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

package api

import (
	"testing"

	"volcano.sh/volcano/pkg/controllers/repack/frag"
)

// OptimalNodes is the fragmentation packing bound — the hot inner call of every
// MeasureResourceFragmentation. Bench a large mixed request set.
func BenchmarkOptimalNodes(b *testing.B) {
	reqs := make([]int64, 0, 2000)
	for i := 0; i < 2000; i++ {
		reqs = append(reqs, int64(1<<(i%4))) // 1,2,4,8 cards
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		frag.OptimalNodes(reqs, 8)
	}
}
