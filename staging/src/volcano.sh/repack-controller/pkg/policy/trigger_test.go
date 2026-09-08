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
)

// nextCronFire pins the controller's cron contract: a bare 5-field standard spec
// (admission rejects timezones) fires strictly after the anchor; anything else
// errors so reconcile reports a failing condition instead of never firing. robfig
// treats an unqualified spec as local to the anchor, so UTC anchors keep these absolute.
func TestNextCronFire(t *testing.T) {
	// Every-minute spec fires at the next minute boundary, 15s past a :45 anchor.
	after := time.Date(2026, 9, 9, 10, 30, 45, 0, time.UTC)
	next, err := nextCronFire("* * * * *", after)
	if err != nil {
		t.Fatalf("valid cron returned error: %v", err)
	}
	if d := next.Sub(after); d != 15*time.Second {
		t.Errorf("* * * * * after 10:30:45: next.Sub(after)=%v, want 15s", d)
	}

	// Strictly after: an anchor exactly on an activation advances to the next one.
	onFire := time.Date(2026, 9, 9, 10, 31, 0, 0, time.UTC)
	next, err = nextCronFire("* * * * *", onFire)
	if err != nil {
		t.Fatalf("valid cron returned error: %v", err)
	}
	if d := next.Sub(onFire); d != time.Minute {
		t.Errorf("anchor on a fire instant: next.Sub(after)=%v, want 1m", d)
	}

	// Anything but a 5-field standard spec is rejected: garbage, seconds, a
	// missing field.
	for _, bad := range []string{"not a cron", "0 0 * * * *", "0 0 * *"} {
		if got, err := nextCronFire(bad, time.Now()); err == nil {
			t.Errorf("nextCronFire(%q) = %v, want error", bad, got)
		}
	}
}
