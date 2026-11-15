package main

import (
	"testing"
	"time"
)

func readingsAt(times ...time.Time) []observation {
	readings := make([]observation, 0, len(times))
	for _, t := range times {
		readings = append(readings, observation{Time: t})
	}
	return readings
}

func TestThinBeforeKeepsTheFirstReadingOfEachClockHour(t *testing.T) {
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

	readings := readingsAt(
		at(-1),                        // 09:59, alone in hour 9
		at(0), at(15), at(30), at(45), // hour 10: 10:00 kept
		at(75), at(90), // hour 11 missed its :00 run: 11:15 kept, not 10:45
		at(120), at(135), // after the cutoff: all kept
	)
	cutoff := at(120)

	got := thinBefore(readings, cutoff)
	want := []time.Time{at(-1), at(0), at(75), at(120), at(135)}
	if len(got) != len(want) {
		t.Fatalf("got %d readings %v, want %v", len(got), got, want)
	}
	for i := range want {
		if !got[i].Time.Equal(want[i]) {
			t.Errorf("reading %d: got %s, want %s", i, got[i].Time, want[i])
		}
	}

	// Running it again, as every collect run does, changes nothing.
	if again := thinBefore(got, cutoff); len(again) != len(got) {
		t.Errorf("not idempotent: %d readings became %d", len(got), len(again))
	}
}

// The thinned hours must line up with what observed_hourly groups by, or a
// thinned hour shows up as two readings in one hour and none in the next.
func TestThinBeforeMatchesObservedHourlyBuckets(t *testing.T) {
	base := time.Date(2026, 10, 31, 22, 0, 0, 0, time.UTC)
	var times []time.Time
	for m := 0; m < 6*60; m += 15 {
		if m == 60 { // one missed run at the top of an hour
			continue
		}
		times = append(times, base.Add(time.Duration(m)*time.Minute))
	}
	thinned := thinBefore(readingsAt(times...), base.Add(24*time.Hour))

	perHour := map[int64]int{}
	for _, reading := range thinned {
		perHour[reading.Time.Truncate(time.Hour).Unix()]++
	}
	if len(perHour) != 6 {
		t.Errorf("got %d hours, want 6", len(perHour))
	}
	for hour, count := range perHour {
		if count != 1 {
			t.Errorf("hour %s holds %d readings, want 1", time.Unix(hour, 0).UTC(), count)
		}
	}
}

func TestThinBeforeWithNothingOldEnough(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	readings := readingsAt(now.Add(-45*time.Minute), now.Add(-30*time.Minute), now.Add(-15*time.Minute), now)
	if got := thinBefore(readings, now.Add(-denseRetention)); len(got) != len(readings) {
		t.Errorf("got %d readings, want all %d kept", len(got), len(readings))
	}
}
