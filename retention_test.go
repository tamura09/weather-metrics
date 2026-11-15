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

	got := thinBefore(readings, cutoff, time.UTC)
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
	if again := thinBefore(got, cutoff, time.UTC); len(again) != len(got) {
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
	thinned := thinBefore(readingsAt(times...), base.Add(24*time.Hour), time.UTC)

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
	if got := thinBefore(readings, now.Add(-denseRetention), time.UTC); len(got) != len(readings) {
		t.Errorf("got %d readings, want all %d kept", len(got), len(readings))
	}
}

// In a zone offset by a fraction of an hour, the hours are that zone's.
func TestThinBeforeUsesTheLocationZone(t *testing.T) {
	zone := time.FixedZone("plus0530", 5*3600+30*60)
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, zone) // 04:30 UTC
	readings := readingsAt(base, base.Add(15*time.Minute), base.Add(30*time.Minute), base.Add(45*time.Minute))
	if got := thinBefore(readings, base.Add(2*time.Hour), zone); len(got) != 1 || !got[0].Time.Equal(base) {
		t.Errorf("got %v, want only the 10:00 local reading", got)
	}
}

// The hour's observed rainfall must come out the same after thinning: the kept
// reading carries the wettest reading's rain_1h and snow_1h, not its own.
func TestThinBeforeKeepsTheHoursRainfall(t *testing.T) {
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	readings := []observation{
		{Time: base, Rain1h: 0.2, Snow1h: 0},
		{Time: base.Add(15 * time.Minute), Rain1h: 0.9},
		{Time: base.Add(30 * time.Minute), Rain1h: 1.4, Snow1h: 0.3},
		{Time: base.Add(45 * time.Minute), Rain1h: 1.1},
	}
	before := accumulateRain(readings, time.UTC, func(r observation) float64 { return r.Rain1h })

	thinned := thinBefore(readings, base.Add(2*time.Hour), time.UTC)
	if len(thinned) != 1 || !thinned[0].Time.Equal(base) {
		t.Fatalf("got %v, want only the 10:00 reading", thinned)
	}
	if thinned[0].Rain1h != 1.4 || thinned[0].Snow1h != 0.3 {
		t.Errorf("kept reading has rain %v snow %v, want the wettest reading's 1.4 / 0.3", thinned[0].Rain1h, thinned[0].Snow1h)
	}
	if after := accumulateRain(thinned, time.UTC, func(r observation) float64 { return r.Rain1h }); after != before {
		t.Errorf("rainfall %v after thinning, %v before", after, before)
	}
}

// Rain and snow travel as a pair, so the daily rain+snow figure is not inflated
// by combining one reading's rain with another's snow.
func TestThinBeforeKeepsRainAndSnowTogether(t *testing.T) {
	base := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	readings := []observation{
		{Time: base, Rain1h: 2.0, Snow1h: 0},
		{Time: base.Add(30 * time.Minute), Rain1h: 0, Snow1h: 1.5},
	}
	total := func(r observation) float64 { return r.Rain1h + r.Snow1h }
	before := accumulateRain(readings, time.UTC, total)
	thinned := thinBefore(readings, base.Add(2*time.Hour), time.UTC)
	if after := accumulateRain(thinned, time.UTC, total); after != before {
		t.Errorf("rain+snow %v after thinning, %v before", after, before)
	}
}

func TestMonthsInUse(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	instance := &app{zone: jst}

	early := instance.monthsInUse(time.Date(2026, 12, 1, 1, 0, 0, 0, jst))
	if !early["2026-12"] || !early["2026-11"] {
		t.Errorf("early in a month: got %v, want both months", early)
	}
	later := instance.monthsInUse(time.Date(2026, 12, 15, 12, 0, 0, 0, jst))
	if !later["2026-12"] || later["2026-11"] {
		t.Errorf("mid-month: got %v, want only the current month", later)
	}
}
