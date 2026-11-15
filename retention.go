package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

// thinBefore keeps every reading in an hour that has not wholly passed cutoff
// and, for each clock hour that has, the first reading in it -- the one nearest the top of the hour
// that still falls inside it. Choosing within the clock hour, rather than the
// nearest top of an hour, is what observed_hourly and accumulateRain group by,
// so each thinned hour holds exactly one reading there; and a clock hour never
// spans two months, so thinning month files one at a time cannot keep two
// readings for the same hour. Thinning already-thinned readings changes
// nothing, so it is safe to run on every collect.
//
// Rainfall needs care: rain_1h is a rolling total for the hour ending at the
// reading, so the first reading of an hour mostly describes the hour before.
// The kept reading therefore carries the rain_1h and snow_1h of its hour's
// wettest reading (largest rain plus snow), as a pair: the daily observed
// precipitation takes each hour's largest rain+snow, and carrying the two
// maxima separately would add a rainy reading's rain to a snowy one's snow.
// The day's figure, and the hour's rain or snow alone in any hour that had
// only one of them, come out the same after thinning as before.
//
// For the same reason the kept reading takes the hour's dominant condition (as
// observed_hourly picks it) and its strongest gust, so a thinned hour is not
// "Clear" with rain in it, and a gust is not lost to the reading that happened
// to be first. Everything else -- temperature, humidity, pressure, wind speed
// -- is the snapshot at the top of the hour.
func thinBefore(readings []observation, cutoff time.Time, zone *time.Location) []observation {
	first := map[int64]observation{}
	wettest := map[int64]observation{}
	inHour := map[int64][]observation{}
	var kept []observation
	for _, reading := range readings {
		// The hour in the location's zone, as observed_hourly groups it;
		// Truncate alone would cut on UTC hours, which differ for an offset
		// that is not a whole number of hours.
		local := reading.Time.In(zone)
		start := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, zone)
		// Only an hour that lies wholly before the cutoff is thinned. The
		// cutoff moves fifteen minutes a run, so thinning the part of an hour
		// it has passed would summarise the hour from a growing slice of it,
		// each pass voting on the previous pass's summary instead of on the
		// readings.
		if start.Add(time.Hour).After(cutoff) {
			kept = append(kept, reading)
			continue
		}
		hour := start.Unix()
		if current, ok := first[hour]; !ok || reading.Time.Before(current.Time) {
			first[hour] = reading
		}
		if current, ok := wettest[hour]; !ok || reading.Rain1h+reading.Snow1h > current.Rain1h+current.Snow1h {
			wettest[hour] = reading
		}
		inHour[hour] = append(inHour[hour], reading)
	}

	thinned := make([]observation, 0, len(first)+len(kept))
	for hour, reading := range first {
		reading.Rain1h = wettest[hour].Rain1h
		reading.Snow1h = wettest[hour].Snow1h
		if condition, description := dominantCondition(inHour[hour]); condition != "" && condition != reading.Condition {
			reading.Condition, reading.Description = condition, description
			// The icon belongs to whichever reading had that condition.
			for _, other := range inHour[hour] {
				if other.Condition == condition {
					reading.Icon = other.Icon
					break
				}
			}
		}
		for _, other := range inHour[hour] {
			reading.WindGust = max(reading.WindGust, other.WindGust)
		}
		thinned = append(thinned, reading)
	}
	thinned = append(thinned, kept...)
	sort.Slice(thinned, func(i, j int) bool { return thinned[i].Time.Before(thinned[j].Time) })
	return thinned
}

// compactEarlierMonths thins the month the two-week line falls in and the one
// before it, leaving out the month this run just wrote (archiveObservation
// thins that itself). Checking the month before as well means the end of a
// month is thinned once the line has passed it even if the collector was down
// when it did; a month older than that is left to the compact event.
func (a *app) compactEarlierMonths(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-denseRetention)
	cutoffMonth := time.Date(cutoff.In(a.zone).Year(), cutoff.In(a.zone).Month(), 1, 0, 0, 0, 0, a.zone)
	inUse := a.monthsInUse(now)

	for _, start := range []time.Time{cutoffMonth.AddDate(0, -1, 0), cutoffMonth} {
		month := start.Format("2006-01")
		if inUse[month] {
			continue
		}
		if _, err := a.compactMonth(ctx, month, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// compactMonth thins one month file, writing it back only if anything was
// removed, and reports how many readings it removed.
func (a *app) compactMonth(ctx context.Context, month string, cutoff time.Time) (int, error) {
	stored, err := a.loadMonth(ctx, month)
	if err != nil {
		return 0, fmt.Errorf("load %s: %w", month, err)
	}
	thinned := thinBefore(stored.Readings, cutoff, a.zone)
	removed := len(stored.Readings) - len(thinned)
	if removed == 0 {
		return 0, nil
	}
	if err := a.saveJSON(ctx, observationsKey(a.prefix, month), monthObservations{Month: month, Readings: thinned}); err != nil {
		return 0, fmt.Errorf("write %s: %w", month, err)
	}
	return removed, nil
}

// compactAll thins every month stored for the location except the current
// one. It is what the {"location": ..., "compact": true} event runs: once when
// the retention rule was introduced, and again after anything that left an old
// month dense, such as an outage longer than a month. It makes no API calls.
//
// The months a scheduled run may be writing are left alone (monthsInUse), so
// a read-modify-write here cannot put back a copy without that run's reading.
// The scheduled runs thin them themselves.
func (a *app) compactAll(ctx context.Context, now time.Time) (any, error) {
	cutoff := now.Add(-denseRetention)
	prefix := a.prefix + observationsPrefix
	inUse := a.monthsInUse(now)

	months := []string{}
	paginator := s3.NewListObjectsV2Paginator(a.objects, &s3.ListObjectsV2Input{Bucket: &a.bucket, Prefix: &prefix})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, object := range page.Contents {
			name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(object.Key), prefix), ".json")
			if _, err := time.Parse("2006-01", name); err == nil && !inUse[name] {
				months = append(months, name)
			}
		}
	}
	sort.Strings(months)

	removed := map[string]int{}
	for _, month := range months {
		count, err := a.compactMonth(ctx, month, cutoff)
		if err != nil {
			return nil, err
		}
		removed[month] = count
	}
	return map[string]any{"location": a.location, "months": months, "readings_removed": removed}, nil
}

// monthsInUse is the month files a collect run may be writing around now: the
// current month, and the previous one for the first hours of a month, because
// a reading is filed by the time the API stamped it, which can still be the
// last minutes of the old month, and a slow or retried run can land late.
// Thinning leaves these to the run that writes them.
func (a *app) monthsInUse(now time.Time) map[string]bool {
	return map[string]bool{
		now.In(a.zone).Format("2006-01"):                     true,
		now.Add(-3 * time.Hour).In(a.zone).Format("2006-01"): true,
	}
}
