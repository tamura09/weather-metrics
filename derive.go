package main

import (
	"math"
	"time"
)

// The numbers in this file are the ones OpenWeather does not return: comfort
// and heat-stress indices, and rates of change. They are computed from stored
// observations rather than asked for, which costs no API calls and means a
// change to a formula can be applied to history by redeploying.

// discomfortIndex is the 不快指数 (temperature-humidity index):
//
//	THI = 0.81T + 0.01H(0.99T - 14.3) + 46.3
//
// Roughly: below 70 is comfortable, 75+ starts to feel muggy, 80+ is
// uncomfortable for most people and 85+ is oppressive.
func discomfortIndex(tempCelsius, humidityPercent float64) float64 {
	return 0.81*tempCelsius + 0.01*humidityPercent*(0.99*tempCelsius-14.3) + 46.3
}

// wbgtEstimate approximates the outdoor 暑さ指数 (wet-bulb globe temperature)
// from temperature and humidity alone, using the regression Ono and Tonouchi
// fit for Japan:
//
//	WBGT = 0.735T + 0.0374H + 0.00292TH - 4.064
//
// A real WBGT reading also needs solar radiation and wind, so this is an
// estimate, and one fitted to warm conditions -- it is meaningful in summer and
// meaningless in winter. Japan's heatstroke advisories start at 25 (警戒),
// 28 (厳重警戒) and 31 (危険).
func wbgtEstimate(tempCelsius, humidityPercent float64) float64 {
	return 0.735*tempCelsius + 0.0374*humidityPercent + 0.00292*tempCelsius*humidityPercent - 4.064
}

// changeOver returns how much a value has moved over the given window, using
// the stored observation closest to that far back. It reports ok=false when
// nothing in the archive is near enough to compare against -- during the first
// hours after deployment, or after a gap in collection -- so a missing series
// is distinguishable from a genuine zero change.
//
// tolerance bounds how far from the target instant a reading may be and still
// count: an observation from six hours ago is not a useful baseline for a
// three-hour trend.
func changeOver(readings []observation, now time.Time, window, tolerance time.Duration, value func(observation) float64) (float64, bool) {
	if len(readings) == 0 {
		return 0, false
	}

	latest := readings[len(readings)-1]
	target := latest.Time.Add(-window)

	var baseline observation
	var found bool
	best := tolerance
	for _, reading := range readings {
		gap := reading.Time.Sub(target)
		if gap < 0 {
			gap = -gap
		}
		if gap <= best {
			best = gap
			baseline = reading
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return value(latest) - value(baseline), true
}

// extremes returns the highest and lowest observed value among readings.
// Unlike the daily timeline's temp.max/temp.min, which are a forecast until
// the date settles, these are what was actually measured -- which is the honest
// answer to "how hot did it get today" while today is still happening.
func extremes(readings []observation, value func(observation) float64) (high, low float64, ok bool) {
	for i, reading := range readings {
		current := value(reading)
		if i == 0 || current > high {
			high = current
		}
		if i == 0 || current < low {
			low = current
		}
		ok = true
	}
	return high, low, ok
}

// between filters readings to a half-open window, which is what "today" means
// once the day boundary is a local midnight.
func between(readings []observation, from, to time.Time) []observation {
	filtered := make([]observation, 0, len(readings))
	for _, reading := range readings {
		if reading.Time.Before(from) || !reading.Time.Before(to) {
			continue
		}
		filtered = append(filtered, reading)
	}
	return filtered
}

var compassPoints = [...]string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"}

// compass turns a wind bearing into a 16-point name, which reads better on a
// stat panel than "247°" and is what a label needs to be if it is going to be
// grouped on.
func compass(degrees float64) string {
	if math.IsNaN(degrees) {
		return "N"
	}
	normalized := math.Mod(math.Mod(degrees, 360)+360, 360)
	index := int(math.Round(normalized/22.5)) % len(compassPoints)
	return compassPoints[index]
}

// startOfDay is midnight in the location's zone, the boundary every "today"
// figure and every daily rollup is cut on.
func startOfDay(moment time.Time, zone *time.Location) time.Time {
	local := moment.In(zone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
}

func round(value float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(value*scale) / scale
}

// ptr is for the served fields that must serialise as null rather than zero
// when nothing was measured. Go has no other way to say "absent" for a float
// in a JSON struct.
func ptr(value float64) *float64 {
	return &value
}
