// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// ParseTimerSchedule parses a Timer's schedule in its configured time zone
// (M9-f). It is the single author of the CRON_TZ composition, shared by
// validation and the TimerController so neither drifts on how the zone is
// applied (the HashDaemonRevision precedent). A nil TimeZone parses in
// host-local time, byte-for-byte the pre-M9 behavior. @every schedules are
// duration-based and unaffected by the zone (robfig/cron applies the location
// only to field/descriptor schedules), but composing the prefix is still valid.
func ParseTimerSchedule(spec *TimerSpec) (cron.Schedule, error) {
	sched := spec.Schedule
	if spec.TimeZone != nil {
		// LoadLocation up front for a clear error; robfig would otherwise
		// report the same failure as "provided bad location".
		if _, err := time.LoadLocation(*spec.TimeZone); err != nil {
			return nil, fmt.Errorf("invalid timeZone %q: %w", *spec.TimeZone, err)
		}
		sched = "CRON_TZ=" + *spec.TimeZone + " " + sched
	}
	return cron.ParseStandard(sched)
}
