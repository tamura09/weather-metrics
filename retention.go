package main

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Observations are kept at the collector's full fifteen-minute resolution for
// two weeks, then thinned to one reading per clock hour, which is kept for
// good. Two reasons, both about the serve function rather than storage (a year
// of one location is a few megabytes either way):
//
//   - the raw observation panels return every reading in the dashboard window
//     in one Lambda response, which is capped at 6MB; at fifteen minutes one
//     location went over it at about three months, at one an hour a year is
//     about 8,800 rows;
//   - nobody reads a reading from last spring at fifteen-minute resolution,
//     while the last two weeks are where the detail is worth having.
//
// Each run thins whatever has crossed the two-week line since the last one, so
// the archive never holds more than two weeks of dense readings.
const denseRetention = 14 * 24 * time.Hour

// nearestHour is the top of the hour a reading is closest to. Grouping by it
// rather than by the hour a reading falls in keeps the reading taken just
// before the hour, which is the one that describes it.
func nearestHour(t time.Time) time.Time {
	return t.Add(30 * time.Minute).Truncate(time.Hour)
}

// thinBefore keeps every reading at or after cutoff and, of those before it,
// the one nearest the top of each hour. readings must be sorted by time, as
// mergeObservations leaves them; ties go to the earlier reading. Thinning
// already-thinned readings changes nothing, so it is safe to run every time.
func thinBefore(readings []observation, cutoff time.Time) []observation {
	type pick struct {
		reading  observation
		distance time.Duration
	}
	best := map[int64]pick{}
	var kept []observation
	for _, reading := range readings {
		if !reading.Time.Before(cutoff) {
			kept = append(kept, reading)
			continue
		}
		hour := nearestHour(reading.Time)
		distance := reading.Time.Sub(hour).Abs()
		if current, ok := best[hour.Unix()]; !ok || distance < current.distance {
			best[hour.Unix()] = pick{reading, distance}
		}
	}

	thinned := make([]observation, 0, len(best)+len(kept))
	for _, chosen := range best {
		thinned = append(thinned, chosen.reading)
	}
	thinned = append(thinned, kept...)
	sort.Slice(thinned, func(i, j int) bool { return thinned[i].Time.Before(thinned[j].Time) })
	return thinned
}

// compactEarlierMonths thins the month the two-week line currently falls in,
// when that is not the month this run just wrote (which archiveObservation
// thins itself). For the first two weeks of a month the line is still in the
// previous one; the day before the line is checked too, so the last readings
// of a month are thinned once the line has moved past its end.
func (a *app) compactEarlierMonths(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-denseRetention)
	current := now.In(a.zone).Format("2006-01")

	seen := map[string]bool{current: true}
	for _, at := range []time.Time{cutoff.AddDate(0, 0, -1), cutoff} {
		month := at.In(a.zone).Format("2006-01")
		if seen[month] {
			continue
		}
		seen[month] = true

		stored, err := a.loadMonth(ctx, month)
		if err != nil {
			return fmt.Errorf("load %s: %w", month, err)
		}
		thinned := thinBefore(stored.Readings, cutoff)
		if len(thinned) == len(stored.Readings) {
			continue
		}
		if err := a.saveJSON(ctx, observationsKey(a.prefix, month), monthObservations{Month: month, Readings: thinned}); err != nil {
			return fmt.Errorf("write %s: %w", month, err)
		}
	}
	return nil
}
