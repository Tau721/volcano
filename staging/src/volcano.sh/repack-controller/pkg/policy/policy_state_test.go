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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"

	repackv1alpha1 "volcano.sh/apis/pkg/apis/repack/v1alpha1"
)

func TestHasInProgressRun(t *testing.T) {
	tests := []struct {
		name       string
		inProgress []v1.ObjectReference
		want       bool
	}{
		{
			name:       "empty list",
			inProgress: nil,
			want:       false,
		},
		{
			name:       "empty slice",
			inProgress: []v1.ObjectReference{},
			want:       false,
		},
		{
			name: "non-empty",
			inProgress: []v1.ObjectReference{
				{Name: "test-run-1"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasInProgressRun(tt.inProgress)
			if got != tt.want {
				t.Errorf("hasInProgressRun() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNextCronFire(t *testing.T) {
	// Fixed reference times for deterministic tests.
	created := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	lastTrigger := time.Date(2026, 8, 17, 6, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		cronSchedule string
		lastTrigger  time.Time
		creationTime time.Time
		now          time.Time
		want         time.Time
		wantNonZero  bool
		wantNotAfter bool // true if result should be <= now (immediate fire)
		wantZero     bool // true if result should be zero time
	}{
		{
			name:         "empty cron",
			cronSchedule: "",
			wantZero:     true,
		},
		{
			name:         "invalid cron",
			cronSchedule: "invalid",
			wantZero:     true,
		},
		{
			name:         "hourly, next fire in future",
			cronSchedule: "0 * * * *",
			lastTrigger:  lastTrigger,
			creationTime: created,
			now:          time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
			want:         time.Date(2026, 8, 17, 13, 0, 0, 0, time.UTC),
		},
		{
			name:         "hourly, next fire overdue (cron at :30, now 12:05)",
			cronSchedule: "30 * * * *",
			lastTrigger:  time.Date(2026, 8, 17, 6, 30, 0, 0, time.UTC),
			creationTime: created,
			now:          time.Date(2026, 8, 17, 12, 5, 0, 0, time.UTC),
			wantNotAfter: true,
		},
		{
			name:         "never triggered, use creation as reference",
			cronSchedule: "0 */6 * * *",
			lastTrigger:  time.Time{},
			creationTime: time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC),
			now:          time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
			want:         time.Date(2026, 8, 17, 15, 0, 0, 0, time.UTC), // next after 03:00+6=09:00 skipped, next is 15:00
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextCronFire(tt.cronSchedule, tt.lastTrigger, tt.creationTime, tt.now)

			if tt.wantZero {
				if !got.IsZero() {
					t.Errorf("nextCronFire() = %v, want zero time", got)
				}
				return
			}
			if tt.wantNotAfter {
				if got.After(tt.now) {
					t.Errorf("nextCronFire() = %v, want time <= now %v", got, tt.now)
				}
				if got.IsZero() {
					t.Errorf("nextCronFire() returned zero time, expected non-zero")
				}
				return
			}
			if !got.Equal(tt.want) {
				t.Errorf("nextCronFire() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetHistoryLimit(t *testing.T) {
	three := int32(3)
	zero := int32(0)
	five := int32(5)

	tests := []struct {
		name       string
		limit      *int32
		defaultVal int32
		want       int32
	}{
		{"nil pointer, default 3", nil, 3, 3},
		{"nil pointer, default 0", nil, 0, 0},
		{"set to 3", &three, 3, 3},
		{"set to 0", &zero, 3, 0},
		{"set to 5, default 3", &five, 3, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getHistoryLimit(tt.limit, tt.defaultVal)
			if got != tt.want {
				t.Errorf("getHistoryLimit() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRunName(t *testing.T) {
	tm := time.Date(2026, 8, 17, 6, 30, 0, 0, time.UTC)

	tests := []struct {
		name        string
		policyName  string
		triggerTime time.Time
		want        string
	}{
		{
			name:        "standard format",
			policyName:  "a100-policy",
			triggerTime: tm,
			want:        "a100-policy-20260817063000",
		},
		{
			name:        "same input same output",
			policyName:  "a100-policy",
			triggerTime: tm,
			want:        "a100-policy-20260817063000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runName(tt.policyName, tt.triggerTime)
			if got != tt.want {
				t.Errorf("runName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunNameDeterministic(t *testing.T) {
	// Two calls with same inputs should produce identical output.
	tm := time.Date(2026, 8, 17, 6, 30, 0, 0, time.UTC)
	a := runName("my-policy", tm)
	b := runName("my-policy", tm)
	if a != b {
		t.Errorf("runName() not deterministic: %q vs %q", a, b)
	}
}

func TestTriggerLabelValue(t *testing.T) {
	cronVal := "0 */6 * * *"
	fragVal := int32(35)

	tests := []struct {
		name    string
		trigger repackv1alpha1.RepackRunTrigger
		want    string
	}{
		{
			name:    "cron only",
			trigger: repackv1alpha1.RepackRunTrigger{CronSchedule: &cronVal},
			want:    "cronSchedule",
		},
		{
			name:    "frag only",
			trigger: repackv1alpha1.RepackRunTrigger{OnFragAbovePercent: &fragVal},
			want:    "onFragAbovePercent",
		},
		{
			name:    "both, cron wins",
			trigger: repackv1alpha1.RepackRunTrigger{CronSchedule: &cronVal, OnFragAbovePercent: &fragVal},
			want:    "cronSchedule",
		},
		{
			name:    "neither set",
			trigger: repackv1alpha1.RepackRunTrigger{},
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := triggerLabelValue(tt.trigger)
			if got != tt.want {
				t.Errorf("triggerLabelValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsSameTimeWindow(t *testing.T) {
	base := time.Date(2026, 8, 17, 12, 30, 45, 0, time.UTC)

	tests := []struct {
		name string
		ref  time.Time
		now  time.Time
		want bool
	}{
		{"same exact time", base, base, true},
		{"same second", base, base.Add(500 * time.Millisecond), true},
		{"different second", base, base.Add(1 * time.Second), false},
		{"different minute", base, base.Add(1 * time.Minute), false},
		{"both zero", time.Time{}, time.Time{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSameTimeWindow(tt.ref, tt.now)
			if got != tt.want {
				t.Errorf("isSameTimeWindow(%v, %v) = %v, want %v", tt.ref, tt.now, got, tt.want)
			}
		})
	}
}
