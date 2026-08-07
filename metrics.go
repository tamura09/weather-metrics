package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

const metricJobLabel = "weather-metrics"

// Everything pushed here is stamped "now", never with the timestamp the value
// belongs to. Grafana Cloud's Prometheus rejects samples that are not roughly
// current in either direction, so a forecast for tomorrow cannot be written at
// tomorrow's instant; it is written now, as "what tomorrow is expected to be,
// as of this run". The forecast at its own timestamps lives in the S3 archive
// and is read through Infinity instead.

// pressureWindow is the interval a barometric trend is measured over. Three
// hours is the conventional window for a pressure tendency, and it is short
// enough that the archive nearly always has a reading to compare against.
const (
	pressureWindow    = 3 * time.Hour
	pressureTolerance = 30 * time.Minute
	temperatureWindow = 24 * time.Hour
	// A day-on-day comparison is anchored to the same clock time, so it can
	// afford to accept a baseline that is up to an hour off; a run that failed
	// should not silently drop the series.
	temperatureTolerance = time.Hour
)

func buildTimeSeries(location string, reading observation, snapshot forecastSnapshot, month []observation, zone *time.Location, now time.Time) []prompb.TimeSeries {
	timestamp := now.UnixMilli()
	base := map[string]string{"location": location}

	series := []prompb.TimeSeries{
		gauge("weather_temp_celsius", base, reading.Temp, timestamp),
		gauge("weather_feels_like_celsius", base, reading.FeelsLike, timestamp),
		gauge("weather_dew_point_celsius", base, reading.DewPoint, timestamp),
		gauge("weather_humidity_percent", base, reading.Humidity, timestamp),
		gauge("weather_pressure_hpa", base, reading.Pressure, timestamp),
		gauge("weather_clouds_percent", base, reading.Clouds, timestamp),
		gauge("weather_visibility_meters", base, reading.Visibility, timestamp),
		gauge("weather_wind_speed_mps", base, reading.WindSpeed, timestamp),
		gauge("weather_wind_gust_mps", base, reading.WindGust, timestamp),
		gauge("weather_wind_degrees", base, reading.WindDeg, timestamp),
		gauge("weather_uv_index", base, reading.UVI, timestamp),
		gauge("weather_rain_1h_mm", base, reading.Rain1h, timestamp),
		gauge("weather_snow_1h_mm", base, reading.Snow1h, timestamp),

		// Comfort and heat stress, computed from the reading above.
		gauge("weather_discomfort_index", base, round(discomfortIndex(reading.Temp, reading.Humidity), 2), timestamp),
		gauge("weather_wbgt_celsius", base, round(wbgtEstimate(reading.Temp, reading.Humidity), 2), timestamp),
	}

	// The condition and wind direction are labels rather than values, so they
	// go on a constant-1 series -- the standard shape for "what is it right
	// now" in Prometheus, and the only one a table or state-timeline panel can
	// group on.
	if reading.Condition != "" {
		series = append(series, gauge("weather_condition", merge(base, map[string]string{
			"condition":   reading.Condition,
			"description": reading.Description,
			"icon":        reading.Icon,
		}), 1, timestamp))
	}
	if reading.WindSpeed > 0 {
		series = append(series, gauge("weather_wind_direction", merge(base, map[string]string{
			"compass": compass(reading.WindDeg),
		}), 1, timestamp))
	}

	if reading.Sunrise > 0 {
		series = append(series, gauge("weather_sunrise_timestamp_seconds", base, float64(reading.Sunrise), timestamp))
	}
	if reading.Sunset > 0 {
		series = append(series, gauge("weather_sunset_timestamp_seconds", base, float64(reading.Sunset), timestamp))
	}
	if reading.Sunrise > 0 && reading.Sunset > reading.Sunrise {
		series = append(series, gauge("weather_daylight_seconds", base, float64(reading.Sunset-reading.Sunrise), timestamp))
	}

	series = append(series, airSeries(base, reading, timestamp)...)
	series = append(series, todaySeries(base, snapshot, month, zone, now, timestamp)...)
	series = append(series, trendSeries(base, month, now, timestamp)...)
	series = append(series, alertSeries(base, snapshot.Alerts, timestamp)...)

	return series
}

func airSeries(base map[string]string, reading observation, timestamp int64) []prompb.TimeSeries {
	if reading.AQI == 0 && len(reading.Pollutants) == 0 {
		return nil
	}

	series := []prompb.TimeSeries{
		// 1 (good) through 5 (very poor) on OpenWeather's own scale, which is
		// not the Japanese or US AQI -- the thresholds on the panel are set to
		// match this one.
		gauge("weather_air_quality_index", base, reading.AQI, timestamp),
	}

	names := make([]string, 0, len(reading.Pollutants))
	for name := range reading.Pollutants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		series = append(series, gauge("weather_air_pollutant_micrograms_per_cubic_meter",
			merge(base, map[string]string{"pollutant": name}), reading.Pollutants[name], timestamp))
	}
	return series
}

// todaySeries carries the two numbers the dashboard is really about, and it
// emits each of them twice on purpose. The forecast pair is what the day is
// expected to reach; the observed pair is what has actually been measured since
// local midnight. Early in the morning the observed maximum is meaningless on
// its own, and by evening the forecast is the less interesting of the two --
// having both is what makes either readable.
func todaySeries(base map[string]string, snapshot forecastSnapshot, month []observation, zone *time.Location, now time.Time, timestamp int64) []prompb.TimeSeries {
	var series []prompb.TimeSeries

	today := startOfDay(now, zone)
	for _, day := range snapshot.Daily {
		if day.Time != today.UnixMilli() {
			continue
		}
		series = append(series,
			gauge("weather_forecast_temp_max_celsius", base, day.TempMax, timestamp),
			gauge("weather_forecast_temp_min_celsius", base, day.TempMin, timestamp),
			gauge("weather_forecast_pop_ratio", base, day.Pop, timestamp),
			gauge("weather_forecast_uv_index_max", base, day.UVI, timestamp),
			gauge("weather_forecast_rain_mm", base, day.Rain, timestamp),
			gauge("weather_forecast_wind_speed_mps", base, day.WindSpeed, timestamp),
		)
		break
	}

	readings := between(month, today, today.AddDate(0, 0, 1))
	if high, low, ok := extremes(readings, func(r observation) float64 { return r.Temp }); ok {
		series = append(series,
			gauge("weather_observed_temp_max_celsius", base, round(high, 2), timestamp),
			gauge("weather_observed_temp_min_celsius", base, round(low, 2), timestamp),
		)
	}
	if high, _, ok := extremes(readings, func(r observation) float64 { return r.WindGust }); ok {
		series = append(series, gauge("weather_observed_wind_gust_max_mps", base, round(high, 2), timestamp))
	}

	var rainfall float64
	for _, r := range readings {
		rainfall += r.Rain1h
	}
	series = append(series, gauge("weather_observed_rain_today_mm", base, round(rainfall, 2), timestamp))

	return series
}

// trendSeries is the rate-of-change pair. The pressure one is the reason this
// function exists: a sharp three-hour drop is the thing people who get weather
// headaches actually want warning of, and it is not a number the API returns.
func trendSeries(base map[string]string, month []observation, now time.Time, timestamp int64) []prompb.TimeSeries {
	var series []prompb.TimeSeries

	if change, ok := changeOver(month, now, pressureWindow, pressureTolerance, func(r observation) float64 { return r.Pressure }); ok {
		series = append(series, gauge("weather_pressure_change_3h_hpa", base, round(change, 2), timestamp))
	}
	if change, ok := changeOver(month, now, temperatureWindow, temperatureTolerance, func(r observation) float64 { return r.Temp }); ok {
		series = append(series, gauge("weather_temp_change_24h_celsius", base, round(change, 2), timestamp))
	}
	return series
}

func alertSeries(base map[string]string, alerts []alertDetail, timestamp int64) []prompb.TimeSeries {
	series := []prompb.TimeSeries{
		gauge("weather_alerts_active", base, float64(len(alerts)), timestamp),
	}
	for _, alert := range alerts {
		series = append(series, gauge("weather_alert", merge(base, map[string]string{
			"event":  alert.Event,
			"sender": alert.SenderName,
		}), 1, timestamp))
	}
	return series
}

func gauge(name string, labels map[string]string, value float64, timestamp int64) prompb.TimeSeries {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)

	promLabels := make([]prompb.Label, 0, len(labels)+2)
	promLabels = append(promLabels,
		prompb.Label{Name: "__name__", Value: name},
		prompb.Label{Name: "job", Value: metricJobLabel},
	)
	for _, label := range names {
		if labels[label] == "" {
			continue
		}
		promLabels = append(promLabels, prompb.Label{Name: label, Value: labels[label]})
	}

	return prompb.TimeSeries{
		Labels:  promLabels,
		Samples: []prompb.Sample{{Value: value, Timestamp: timestamp}},
	}
}

func merge(base, extra map[string]string) map[string]string {
	combined := make(map[string]string, len(base)+len(extra))
	for name, value := range base {
		combined[name] = value
	}
	for name, value := range extra {
		combined[name] = value
	}
	return combined
}

// push sends the series to Grafana Cloud, or does nothing if the remote_write
// parameters are not configured. Leaving them unset is a supported way to run
// the collector as an archive-only job -- the dashboard still works off the
// Infinity datasource, it just loses the sub-14-day live panels.
func (a *app) push(ctx context.Context, series []prompb.TimeSeries) (int, error) {
	urlParameter := strings.TrimSpace(os.Getenv("GRAFANA_REMOTE_WRITE_URL_PARAMETER_NAME"))
	if urlParameter == "" {
		return 0, nil
	}
	usernameParameter, err := requiredEnv("GRAFANA_PROMETHEUS_USERNAME_PARAMETER_NAME")
	if err != nil {
		return 0, err
	}
	tokenParameter, err := requiredEnv("GRAFANA_PUSH_TOKEN_PARAMETER_NAME")
	if err != nil {
		return 0, err
	}

	remoteWriteURL, err := a.parameterString(ctx, urlParameter)
	if err != nil {
		return 0, fmt.Errorf("read Grafana remote_write URL parameter: %w", err)
	}
	username, err := a.parameterString(ctx, usernameParameter)
	if err != nil {
		return 0, fmt.Errorf("read Grafana Prometheus username parameter: %w", err)
	}
	token, err := a.parameterString(ctx, tokenParameter)
	if err != nil {
		return 0, fmt.Errorf("read Grafana push token parameter: %w", err)
	}

	if err := a.remoteWrite(ctx, remoteWriteURL, username, token, series); err != nil {
		return 0, err
	}
	return len(series), nil
}

func (a *app) remoteWrite(ctx context.Context, remoteWriteURL, username, password string, series []prompb.TimeSeries) error {
	data, err := (&prompb.WriteRequest{Timeseries: series}).Marshal()
	if err != nil {
		return fmt.Errorf("marshal WriteRequest: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, remoteWriteURL, bytes.NewReader(snappy.Encode(nil, data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	req.SetBasicAuth(username, password)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("remote_write returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
