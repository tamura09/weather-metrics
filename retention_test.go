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

func TestThinBeforeKeepsOneReadingPerHourBeforeTheCutoff(t *testing.T) {
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

	readings := readingsAt(
		at(-1), at(14), at(29), at(44), // 09:59 is nearest 10:00; 10:44 is nearest 11:00
		at(59), at(74), at(89), // 10:59 beats 10:44 for 11:00
		at(120), at(135), // after the cutoff: all kept
	)
	cutoff := at(120)

	got := thinBefore(readings, cutoff)
	want := []time.Time{at(-1), at(59), at(120), at(135)}
	if len(got) != len(want) {
		t.Fatalf("got %d readings %v, want %v", len(got), got, want)
	}
	for i := range want {
		if !got[i].Time.Equal(want[i]) {
			t.Errorf("reading %d: got %s, want %s", i, got[i].Time, want[i])
		}
	}

	// Running it again, as every collect run does, changes nothing.
	again := thinBefore(got, cutoff)
	if len(again) != len(got) {
		t.Errorf("not idempotent: %d readings became %d", len(got), len(again))
	}
}

func TestThinBeforeWithNothingOldEnough(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	readings := readingsAt(now.Add(-45*time.Minute), now.Add(-30*time.Minute), now.Add(-15*time.Minute), now)
	if got := thinBefore(readings, now.Add(-denseRetention)); len(got) != len(readings) {
		t.Errorf("got %d readings, want all %d kept", len(got), len(readings))
	}
}

func TestNearestHour(t *testing.T) {
	for raw, want := range map[string]string{
		"2026-11-01T10:29:59Z": "2026-11-01T10:00:00Z",
		"2026-11-01T10:30:00Z": "2026-11-01T11:00:00Z",
		"2026-11-01T23:50:00Z": "2026-11-02T00:00:00Z",
	} {
		at, _ := time.Parse(time.RFC3339, raw)
		if got := nearestHour(at).Format(time.RFC3339); got != want {
			t.Errorf("%s: got %s, want %s", raw, got, want)
		}
	}
}
