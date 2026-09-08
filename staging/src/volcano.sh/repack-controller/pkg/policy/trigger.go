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
	"time"

	"github.com/robfig/cron/v3"
)

// nextCronFire returns the next activation of a standard 5-field cron schedule
// strictly after the given instant.
func nextCronFire(expr string, after time.Time) (time.Time, error) {
	schedule, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(after), nil
}
