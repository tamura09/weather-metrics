package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

var testZone = time.FixedZone("local", 9*60*60)

func decode(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func at(day int, hour, minute int) time.Time {
	return time.Date(2026, 8, day, hour, minute, 0, 0, testZone)
}

// A day is settled only once it has fully passed in the location's zone. Until
// then its max and min are a forecast, however recently they were refreshed --
// and storing a forecast as settled would freeze it, because mergeDays refuses
// to overwrite settled history.
func TestBuildDaysMarksOnlyPastDatesSettled(t *testing.T) {
	now := at(8, 14, 30)
	points := []weatherPoint{
		dailyPoint(at(6, 0, 0), 30, 22),
		dailyPoint(at(7, 0, 0), 31, 23),
		dailyPoint(at(8, 0, 0), 33, 24),
		dailyPoint(at(9, 0, 0), 34, 25),
	}

	days := buildDays(points, testZone, now)
	if len(days) != 4 {
		t.Fatalf("got %d days, want 4", len(days))
	}

	settled := map[string]bool{}
	for _, day := range days {
		settled[day.Date] = day.Settled
	}
	for date, want := range map[string]bool{
		"2026-08-06": true,
		"2026-08-07": true,
		"2026-08-08": false, // today is still happening
		"2026-08-09": false,
	} {
		if settled[date] != want {
			t.Errorf("%s settled = %v, want %v", date, settled[date], want)
		}
	}

	if days[2].TempMax != 33 || days[2].TempMin != 24 {
		t.Errorf("today's range = %v/%v, want 33/24", days[2].TempMax, days[2].TempMin)
	}
	if days[0].Time != at(6, 0, 0).UnixMilli() {
		t.Errorf("day time = %d, want local midnight", days[0].Time)
	}
}

// A backfill walking forward can run past today and return forecasts for dates
// already settled. Those must not overwrite the history that is stored.
func TestMergeDaysKeepsSettledHistoryOverForecasts(t *testing.T) {
	stored := []dayRecord{
		{Date: "2026-08-06", Settled: true, TempMax: 30.4},
		{Date: "2026-08-08", Settled: false, TempMax: 33},
	}
	incoming := []dayRecord{
		{Date: "2026-08-06", Settled: false, TempMax: 99}, // a forecast, arriving late
		{Date: "2026-08-08", Settled: true, TempMax: 34.2},
		{Date: "2026-08-09", Settled: false, TempMax: 35},
	}

	merged := mergeDays(stored, incoming)
	byDate := map[string]dayRecord{}
	for _, day := range merged {
		byDate[day.Date] = day
	}

	if byDate["2026-08-06"].TempMax != 30.4 {
		t.Errorf("settled history was overwritten by a forecast: %v", byDate["2026-08-06"].TempMax)
	}
	if !byDate["2026-08-08"].Settled || byDate["2026-08-08"].TempMax != 34.2 {
		t.Errorf("a date that settled should be replaced by what happened: %+v", byDate["2026-08-08"])
	}
	if len(merged) != 3 {
		t.Errorf("got %d dates, want 3", len(merged))
	}
	if merged[0].Date > merged[1].Date || merged[1].Date > merged[2].Date {
		t.Error("merged days should come back ascending")
	}
}

// The collector runs far more often than the API's model updates, so several
// runs can see the same observation instant. Keying on the minute keeps that
// from stacking near-duplicate points onto every graph.
func TestMergeObservationsDedupesWithinTheMinute(t *testing.T) {
	first := observation{Time: at(8, 10, 0), Temp: 28}
	retry := observation{Time: at(8, 10, 0).Add(30 * time.Second), Temp: 28.4}
	later := observation{Time: at(8, 10, 20), Temp: 29}

	merged := mergeObservations([]observation{first}, []observation{retry, later})
	if len(merged) != 2 {
		t.Fatalf("got %d readings, want 2", len(merged))
	}
	if merged[0].Temp != 28.4 {
		t.Errorf("the later reading in a minute should win, got %v", merged[0].Temp)
	}
	if !merged[0].Time.Before(merged[1].Time) {
		t.Error("readings should come back ascending")
	}
}

func TestMergeAlertsDropsExpiredAndKeepsFetchedText(t *testing.T) {
	now := at(8, 14, 0)
	stored := []alertDetail{
		{ID: "a", Event: "大雨警報", Description: "already fetched", Start: at(8, 6, 0).Unix(), End: at(8, 18, 0).Unix()},
		{ID: "old", Event: "強風注意報", Start: at(7, 6, 0).Unix(), End: at(7, 18, 0).Unix()},
	}

	merged := mergeAlerts(stored, nil, now)
	if len(merged) != 1 {
		t.Fatalf("got %d alerts, want only the unexpired one", len(merged))
	}
	if merged[0].Description != "already fetched" {
		t.Error("text fetched on an earlier run should be kept rather than re-fetched")
	}
}

func TestDiscomfortIndexAndWBGT(t *testing.T) {
	// 30C at 70% RH: THI is in the 80+ "almost everyone is uncomfortable" band
	// and the WBGT estimate sits between Japan's 25 (警戒) and 28 (厳重警戒)
	// advisory thresholds.
	if got := discomfortIndex(30, 70); math.Abs(got-81.38) > 0.05 {
		t.Errorf("THI(30,70) = %v, want about 81.38", got)
	}
	if got := wbgtEstimate(30, 70); math.Abs(got-26.7) > 0.3 {
		t.Errorf("WBGT(30,70) = %v, want about 26.7", got)
	}
	// A cool, dry day should land well below every advisory threshold.
	if got := discomfortIndex(10, 40); got > 60 {
		t.Errorf("THI(10,40) = %v, want a comfortable figure", got)
	}
}

func TestChangeOverNeedsABaselineInsideTheTolerance(t *testing.T) {
	now := at(8, 12, 0)
	readings := []observation{
		{Time: at(8, 9, 0), Pressure: 1013},
		{Time: at(8, 10, 30), Pressure: 1010},
		{Time: at(8, 12, 0), Pressure: 1004},
	}

	change, ok := changeOver(readings, now, 3*time.Hour, 30*time.Minute, func(r observation) float64 { return r.Pressure })
	if !ok {
		t.Fatal("a reading exactly three hours back should be usable")
	}
	if change != -9 {
		t.Errorf("change = %v, want -9", change)
	}

	// Nothing near 24 hours back: the series must be absent rather than read as
	// a real zero change.
	if _, ok := changeOver(readings, now, 24*time.Hour, time.Hour, func(r observation) float64 { return r.Pressure }); ok {
		t.Error("want ok=false when the archive has no baseline that far back")
	}
}

func TestCompass(t *testing.T) {
	for degrees, want := range map[float64]string{0: "N", 45: "NE", 90: "E", 180: "S", 247: "WSW", 350: "N", 361: "N", -90: "W"} {
		if got := compass(degrees); got != want {
			t.Errorf("compass(%v) = %q, want %q", degrees, got, want)
		}
	}
}

func TestExtremesAndBetween(t *testing.T) {
	readings := []observation{
		{Time: at(7, 23, 0), Temp: 40}, // yesterday: must not count
		{Time: at(8, 6, 0), Temp: 24.5},
		{Time: at(8, 14, 0), Temp: 33.1},
		{Time: at(9, 1, 0), Temp: 5}, // tomorrow: must not count
	}

	today := between(readings, at(8, 0, 0), at(9, 0, 0))
	high, low, ok := extremes(today, func(r observation) float64 { return r.Temp })
	if !ok {
		t.Fatal("want extremes for a day with readings")
	}
	if high != 33.1 || low != 24.5 {
		t.Errorf("high/low = %v/%v, want 33.1/24.5", high, low)
	}

	if _, _, ok := extremes(nil, func(r observation) float64 { return r.Temp }); ok {
		t.Error("no readings should report ok=false, not a zero maximum")
	}
}

func TestSummarizeCombinesForecastAndObserved(t *testing.T) {
	now := at(8, 14, 0)
	readings := []observation{
		{Time: at(7, 14, 0), Temp: 30, Pressure: 1015},
		{Time: at(8, 6, 0), Temp: 25.2, Pressure: 1012, Rain1h: 1.5},
		{Time: at(8, 11, 0), Temp: 31, Pressure: 1010},
		{Time: at(8, 14, 0), Temp: 33.4, FeelsLike: 38.1, Humidity: 62, Pressure: 1005,
			WindDeg: 247, WindSpeed: 4.2, Condition: "Clouds", Sunrise: at(8, 5, 30).Unix(), Sunset: at(8, 19, 15).Unix(),
			AQI: 2, Pollutants: map[string]float64{"pm2_5": 8.4, "pm10": 14.1}, Rain1h: 0.5},
	}
	snapshot := forecastSnapshot{
		Daily: []dayRecord{
			{Date: "2026-08-08", Time: at(8, 0, 0).UnixMilli(), TempMax: 34, TempMin: 24.5, Pop: 0.4},
			{Date: "2026-08-09", Time: at(9, 0, 0).UnixMilli(), TempMax: 35, TempMin: 25},
		},
		Alerts: []alertDetail{{ID: "a", Event: "熱中症警戒アラート"}},
	}

	summary := summarize(readings, snapshot, testZone, now)

	if summary.Temp != 33.4 || summary.FeelsLike != 38.1 {
		t.Errorf("current conditions came from the wrong reading: %+v", summary)
	}
	if summary.FeelsLikeDelta != 4.7 {
		t.Errorf("feels-like delta = %v, want 4.7", summary.FeelsLikeDelta)
	}
	if summary.ForecastMax != 34 || summary.ForecastMin != 24.5 || summary.ForecastPop != 0.4 {
		t.Errorf("today's forecast was not picked out: %+v", summary)
	}
	// Observed extremes cover today only, so yesterday's 30C must not become
	// today's minimum.
	if summary.ObservedMax == nil || summary.ObservedMin == nil {
		t.Fatalf("observed extremes missing: %+v", summary)
	}
	if *summary.ObservedMax != 33.4 || *summary.ObservedMin != 25.2 {
		t.Errorf("observed max/min = %v/%v, want 33.4/25.2", *summary.ObservedMax, *summary.ObservedMin)
	}
	// The 11:00 reading is the baseline three hours before the 14:00 latest.
	if summary.PressureChange == nil || *summary.PressureChange != -5 {
		t.Errorf("3h pressure change = %v, want -5", summary.PressureChange)
	}
	if summary.TempChange24h == nil || *summary.TempChange24h != 3.4 {
		t.Errorf("24h temp change = %v, want 3.4", summary.TempChange24h)
	}
	if summary.RainToday != 2 {
		t.Errorf("rain today = %v, want 2 (yesterday's excluded)", summary.RainToday)
	}
	if summary.WindCompass != "WSW" {
		t.Errorf("wind compass = %q, want WSW", summary.WindCompass)
	}
	if summary.AlertsActive != 1 || summary.PM25 != 8.4 {
		t.Errorf("alerts/air did not carry through: %+v", summary)
	}
	if summary.Daylight != float64(at(8, 19, 15).Unix()-at(8, 5, 30).Unix()) {
		t.Errorf("daylight = %v", summary.Daylight)
	}
	if summary.DataLagSeconds != 0 {
		t.Errorf("data lag = %v, want 0 for a reading taken now", summary.DataLagSeconds)
	}
}

func TestSummarizeWithNoReadings(t *testing.T) {
	summary := summarize(nil, forecastSnapshot{}, testZone, at(8, 14, 0))
	if summary.Time != 0 || summary.Temp != 0 {
		t.Errorf("an empty archive should summarise to zeroes, got %+v", summary)
	}
}

// Every sample must be stamped with the run's own instant. Grafana Cloud
// rejects samples that are not roughly current, so a forecast written at the
// time it is for would be dropped -- which is exactly the failure the S3
// archive exists to avoid.
func TestBuildTimeSeriesStampsEverythingNow(t *testing.T) {
	now := at(8, 14, 0)
	reading := observation{
		Time: at(8, 13, 50), Temp: 33.4, FeelsLike: 38.1, Humidity: 62, Pressure: 1005,
		WindSpeed: 4.2, WindDeg: 247, Condition: "Clouds", Description: "曇りがち",
		AQI: 2, Pollutants: map[string]float64{"pm2_5": 8.4, "pm10": 14.1},
	}
	snapshot := forecastSnapshot{Daily: []dayRecord{
		{Date: "2026-08-08", Time: at(8, 0, 0).UnixMilli(), TempMax: 34, TempMin: 24.5, Pop: 0.4},
		{Date: "2026-08-09", Time: at(9, 0, 0).UnixMilli(), TempMax: 35, TempMin: 25},
	}}
	month := []observation{
		{Time: at(7, 14, 0), Temp: 30, Pressure: 1015},
		{Time: at(8, 11, 0), Temp: 31, Pressure: 1010},
		{Time: at(8, 13, 50), Temp: 33.4, Pressure: 1005},
	}

	series := buildTimeSeries("home", reading, snapshot, month, testZone, now)

	values := map[string]float64{}
	for _, one := range series {
		for _, sample := range one.Samples {
			if sample.Timestamp != now.UnixMilli() {
				t.Fatalf("%s carried timestamp %d, want the run's own %d", seriesName(one), sample.Timestamp, now.UnixMilli())
			}
		}
		values[seriesName(one)] = one.Samples[0].Value
	}

	for name, want := range map[string]float64{
		"weather_temp_celsius":              33.4,
		"weather_feels_like_celsius":        38.1,
		"weather_forecast_temp_max_celsius": 34,
		"weather_forecast_temp_min_celsius": 24.5,
		"weather_observed_temp_max_celsius": 33.4,
		"weather_observed_temp_min_celsius": 31,
		"weather_pressure_change_3h_hpa":    -5, // 1005 now against 1010 at 11:00, the nearest reading to three hours back

		"weather_air_quality_index": 2,
		"weather_alerts_active":     0,
	} {
		got, ok := values[name]
		if !ok {
			t.Errorf("%s was not pushed", name)
			continue
		}
		if math.Abs(got-want) > 0.001 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	// Tomorrow's forecast must not leak into today's gauges.
	if values["weather_forecast_temp_max_celsius"] == 35 {
		t.Error("picked tomorrow's maximum instead of today's")
	}

	if _, ok := values["weather_condition"]; !ok {
		t.Error("the condition series is what a state-timeline panel groups on")
	}
	for _, one := range series {
		if seriesName(one) != "weather_condition" {
			continue
		}
		if label(one, "description") != "曇りがち" || label(one, "location") != "home" {
			t.Errorf("condition labels = %v", one.Labels)
		}
	}
}

func TestBuildTimeSeriesOmitsTrendsWithoutABaseline(t *testing.T) {
	now := at(8, 14, 0)
	reading := observation{Time: now, Temp: 33.4, Humidity: 62, Pressure: 1005}
	series := buildTimeSeries("home", reading, forecastSnapshot{}, []observation{{Time: now, Pressure: 1005}}, testZone, now)

	for _, one := range series {
		if seriesName(one) == "weather_pressure_change_3h_hpa" {
			t.Error("a trend with no baseline should be absent, not zero -- zero means 'steady', which is a different claim")
		}
	}
}

func seriesName(one prompb.TimeSeries) string {
	return label(one, "__name__")
}

func label(one prompb.TimeSeries, name string) string {
	for _, l := range one.Labels {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

func dailyPoint(midnight time.Time, high, low float64) weatherPoint {
	span := &dailyTempRange{Min: low, Max: high, Day: (high + low) / 2}
	return weatherPoint{
		Dt:        midnight.Unix(),
		Temp:      scalarTemp{Value: span.Day, Range: span},
		TempRange: span,
	}
}

// A date carried only by the daily archive -- a backfilled one, with no
// readings of our own behind it -- must serialise its observed fields as null.
// Zero would draw a 0C line across the min/max chart for every historical date,
// which is data-shaped without being data.
func TestServedDayOmitsObservedFieldsWithoutReadings(t *testing.T) {
	encoded, err := json.Marshal(servedDay{Date: "2024-03-01", TempMax: 18, TempMin: 7, Settled: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	decode(t, string(encoded), &decoded)
	for _, field := range []string{"observed_temp_max", "observed_temp_min", "observed_temp_mean", "observed_rain_mm"} {
		if decoded[field] != nil {
			t.Errorf("%s = %v, want null when there are no observations", field, decoded[field])
		}
	}
	if decoded["temp_max"] != 18.0 {
		t.Errorf("temp_max = %v, want the archived figure to survive", decoded["temp_max"])
	}
}

func TestSummarizeLeavesObservedExtremesNullBeforeTheFirstReadingOfTheDay(t *testing.T) {
	// Only yesterday's readings are in the archive, so today has no measured
	// maximum yet.
	readings := []observation{{Time: at(7, 14, 0), Temp: 30}}
	summary := summarize(readings, forecastSnapshot{}, testZone, at(8, 2, 0))

	if summary.ObservedMax != nil || summary.ObservedMin != nil {
		t.Errorf("observed extremes = %v/%v, want nil", summary.ObservedMax, summary.ObservedMin)
	}
}

// A coordinate parameter left at its Terraform placeholder, or holding a typo,
// must stop the run. Parsed as 0 it would not crash -- it would quietly collect
// the weather in the Gulf of Guinea for as long as nobody looked at the map.
func TestParameterFloatRangeChecks(t *testing.T) {
	for _, tc := range []struct {
		stored string
		limit  float64
		ok     bool
	}{
		{"33.5904", 90, true},
		{"-33.5904", 90, true},
		{"130.4017", 180, true},
		{"0", 90, true}, // a real, if unlikely, coordinate
		{"MANAGED_OUTSIDE_TERRAFORM", 90, false},
		{"", 90, false},
		{"91", 90, false},
		{"181", 180, false},
	} {
		instance := &app{parameters: nil}
		value, err := instance.parseCoordinate(tc.stored, "/weather-metrics/latitude", tc.limit)
		if tc.ok && err != nil {
			t.Errorf("%q: unexpected error %v", tc.stored, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%q: want an error, got %v", tc.stored, value)
		}
		// Whatever went wrong, the coordinate must not be in the message: the
		// point of keeping it in SSM is that it stays out of CloudWatch Logs.
		if err != nil && tc.stored != "" && strings.Contains(err.Error(), tc.stored) {
			t.Errorf("%q: error leaked the stored value: %v", tc.stored, err)
		}
	}
}

// The first run after a deployment has nothing to compare against. Reporting 0
// there would claim the pressure is steady, which is a statement about the
// weather rather than an admission that the archive is too short to answer --
// and a steady barometer is exactly the reassuring reading someone checking for
// a headache trigger would act on.
func TestSummarizeLeavesTrendsNullWithoutABaseline(t *testing.T) {
	now := at(8, 14, 0)
	summary := summarize([]observation{{Time: now, Temp: 33.4, Pressure: 1005}}, forecastSnapshot{}, testZone, now)

	if summary.PressureChange != nil {
		t.Errorf("pressure_change_3h = %v, want nil on a single reading", *summary.PressureChange)
	}
	if summary.TempChange24h != nil {
		t.Errorf("temp_change_24h = %v, want nil on a single reading", *summary.TempChange24h)
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	decode(t, string(encoded), &decoded)
	for _, field := range []string{"pressure_change_3h", "temp_change_24h"} {
		if decoded[field] != nil {
			t.Errorf("%s = %v, want null", field, decoded[field])
		}
	}
}

func TestMonthlyRollupCountsConditionsAndExtremes(t *testing.T) {
	day := func(date string, cond string, high, low, rain, snow float64) servedDay {
		d, _ := time.ParseInLocation("2006-01-02", date, testZone)
		return servedDay{Date: date, Time: d.UnixMilli(), Condition: cond, TempMax: high, TempMin: low, Rain: rain, Snow: snow, Humidity: 60, Pressure: 1010, Clouds: 50, WindSpeed: 3}
	}

	months := monthlyRollup([]servedDay{
		day("2026-01-05", "Snow", 1, -9, 0, 12),
		day("2026-01-06", "Snow", 2, -4, 0, 3),
		day("2026-01-07", "Clouds", 5, -1, 0, 0),
		day("2026-01-08", "Drizzle", 6, 0, 1.5, 0), // folded into rain days
		day("2026-01-09", "Thunderstorm", 7, 1, 20, 0),
		day("2026-08-01", "Clear", 33, 24, 0, 0),
		day("2026-08-02", "Rain", 28, 22, 40, 0),
	})

	if len(months) != 2 {
		t.Fatalf("got %d months, want 2", len(months))
	}
	jan, aug := months[0], months[1]

	if jan.YearMonth != "2026-01" || aug.YearMonth != "2026-08" {
		t.Fatalf("months came back as %s, %s", jan.YearMonth, aug.YearMonth)
	}
	// Ascending, because a time axis reads left to right.
	if jan.MonthTime >= aug.MonthTime {
		t.Error("months should be ascending by time")
	}

	firstOfJan, _ := time.ParseInLocation("2006-01-02", "2026-01-01", testZone)
	if jan.MonthTime != firstOfJan.UnixMilli() {
		t.Errorf("month_time = %d, want local midnight on the 1st (%d)", jan.MonthTime, firstOfJan.UnixMilli())
	}

	if jan.Days != 5 || jan.SnowDays != 2 || jan.CloudDays != 1 || jan.RainDays != 1 || jan.OtherDays != 1 {
		t.Errorf("january counts wrong: %+v", jan)
	}
	// Drizzle counts as rain; Thunderstorm falls to other. Every day lands in
	// exactly one bucket, or the stacked bar would not add up to Days.
	if jan.ClearDays+jan.CloudDays+jan.RainDays+jan.SnowDays+jan.OtherDays != jan.Days {
		t.Errorf("condition buckets do not sum to Days: %+v", jan)
	}

	// The month's extremes, not the average of the daily ones.
	if jan.PeakTempMax != 7 || jan.PeakTempMin != -9 {
		t.Errorf("january peaks = %v/%v, want 7/-9", jan.PeakTempMax, jan.PeakTempMin)
	}
	if jan.AvgTempMax != 4.2 {
		t.Errorf("january avg_temp_max = %v, want 4.2", jan.AvgTempMax)
	}
	if jan.TotalSnowMm != 15 || jan.TotalRainMm != 21.5 {
		t.Errorf("january totals = snow %v rain %v, want 15/21.5", jan.TotalSnowMm, jan.TotalRainMm)
	}
	if aug.ClearDays != 1 || aug.RainDays != 1 || aug.Days != 2 {
		t.Errorf("august counts wrong: %+v", aug)
	}
}

func TestMonthlyRollupOfNothing(t *testing.T) {
	if got := monthlyRollup(nil); len(got) != 0 {
		t.Errorf("want an empty slice, got %v", got)
	}
}
