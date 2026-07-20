// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// TestParseTimerScheduleTimeZone pins M9-f: a field schedule evaluates in the
// configured zone. A 0 0 * * * daily schedule fires at the zone's midnight, a
// different instant from the same schedule in UTC.
func TestParseTimerScheduleTimeZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable on this host")
	}
	from := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	nyTZ, utcTZ := "America/New_York", "UTC"

	nySched, err := ParseTimerSchedule(&TimerSpec{Schedule: "0 0 * * *", TimeZone: &nyTZ})
	if err != nil {
		t.Fatalf("ParseTimerSchedule(NY): %v", err)
	}
	nyNext := nySched.Next(from)
	if h := nyNext.In(ny).Hour(); h != 0 {
		t.Errorf("next fire in NY = %s (hour %d), want midnight", nyNext.In(ny).Format(time.RFC3339), h)
	}

	utcSched, err := ParseTimerSchedule(&TimerSpec{Schedule: "0 0 * * *", TimeZone: &utcTZ})
	if err != nil {
		t.Fatalf("ParseTimerSchedule(UTC): %v", err)
	}
	if nyNext.Equal(utcSched.Next(from)) {
		t.Error("NY-zone and UTC-zone daily-midnight schedules fired at the same instant; the zone was not applied")
	}
}

// TestParseTimerScheduleEveryIsZoneIndependent pins that @every is duration-
// based and unaffected by timeZone (verified against robfig/cron behavior).
func TestParseTimerScheduleEveryIsZoneIndependent(t *testing.T) {
	from := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	ny := "America/New_York"
	withTZ, err := ParseTimerSchedule(&TimerSpec{Schedule: "@every 30s", TimeZone: &ny})
	if err != nil {
		t.Fatalf("with TZ: %v", err)
	}
	noTZ, err := ParseTimerSchedule(&TimerSpec{Schedule: "@every 30s"})
	if err != nil {
		t.Fatalf("no TZ: %v", err)
	}
	if !withTZ.Next(from).Equal(noTZ.Next(from)) {
		t.Error("@every schedule shifted by timeZone; it should be duration-based")
	}
}

// TestParseTimerScheduleNilZoneMatchesHost pins that a nil timeZone is
// byte-for-byte the pre-M9 host-local behavior.
func TestParseTimerScheduleNilZoneMatchesHost(t *testing.T) {
	from := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	got, err := ParseTimerSchedule(&TimerSpec{Schedule: "0 0 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := cron.ParseStandard("0 0 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Next(from).Equal(want.Next(from)) {
		t.Error("nil timeZone did not match host-local cron.ParseStandard")
	}
}

func TestParseTimerScheduleBadZone(t *testing.T) {
	bad := "Not/AZone"
	if _, err := ParseTimerSchedule(&TimerSpec{Schedule: "0 0 * * *", TimeZone: &bad}); err == nil {
		t.Error("bad timeZone accepted; want error")
	}
}
