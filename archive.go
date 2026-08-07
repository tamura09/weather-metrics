package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The archive exists because Grafana Cloud's free tier keeps metrics for 14
// days and its Prometheus refuses samples that are not roughly current -- too
// old in either direction. Neither the history behind a year-on-year
// comparison nor a forecast, whose samples are by definition in the future,
// can live in metrics. They live here and are read back through the Infinity
// datasource.
//
// Three objects, split by how they change:
//
//	observations/YYYY-MM.json  append-only, one record per collect run
//	daily/YYYY.json            one record per date, rewritten as a date settles
//	forecast.json              latest snapshot, replaced wholesale each run
const (
	observationsPrefix = "observations/"
	dailyPrefix        = "daily/"
	forecastKey        = "forecast.json"
)

// observation is one collect run's snapshot of the present. Fields are the
// subset of the current timeline worth keeping at ten-minute resolution --
// enough to recompute daily extremes and the derived indices without another
// API call.
type observation struct {
	Time        time.Time          `json:"time"`
	Temp        float64            `json:"temp"`
	FeelsLike   float64            `json:"feels_like"`
	DewPoint    float64            `json:"dew_point"`
	Humidity    float64            `json:"humidity"`
	Pressure    float64            `json:"pressure"`
	Clouds      float64            `json:"clouds"`
	Visibility  float64            `json:"visibility"`
	WindSpeed   float64            `json:"wind_speed"`
	WindGust    float64            `json:"wind_gust"`
	WindDeg     float64            `json:"wind_deg"`
	UVI         float64            `json:"uvi"`
	Rain1h      float64            `json:"rain_1h"`
	Snow1h      float64            `json:"snow_1h"`
	Condition   string             `json:"condition"`
	Description string             `json:"description"`
	Icon        string             `json:"icon"`
	Sunrise     int64              `json:"sunrise"`
	Sunset      int64              `json:"sunset"`
	AQI         float64            `json:"aqi"`
	Pollutants  map[string]float64 `json:"pollutants,omitempty"`
}

type monthObservations struct {
	Month    string        `json:"month"` // YYYY-MM in the location's zone
	Readings []observation `json:"readings"`
}

// dayRecord is one calendar date from the daily timeline. Settled marks a date
// whose data is history rather than forecast; a date is read again after it
// passes precisely so its forecast is overwritten by what happened.
type dayRecord struct {
	Date        string  `json:"date"` // YYYY-MM-DD in the location's zone
	Time        int64   `json:"time"` // local midnight, epoch ms
	Settled     bool    `json:"settled"`
	TempMax     float64 `json:"temp_max"`
	TempMin     float64 `json:"temp_min"`
	TempMorn    float64 `json:"temp_morn"`
	TempDay     float64 `json:"temp_day"`
	TempEve     float64 `json:"temp_eve"`
	TempNight   float64 `json:"temp_night"`
	FeelsLike   float64 `json:"feels_like_day"`
	Humidity    float64 `json:"humidity"`
	Pressure    float64 `json:"pressure"`
	DewPoint    float64 `json:"dew_point"`
	UVI         float64 `json:"uvi"`
	Clouds      float64 `json:"clouds"`
	WindSpeed   float64 `json:"wind_speed"`
	WindGust    float64 `json:"wind_gust"`
	WindDeg     float64 `json:"wind_deg"`
	Pop         float64 `json:"pop"`
	Rain        float64 `json:"rain"`
	Snow        float64 `json:"snow"`
	Sunrise     int64   `json:"sunrise"`
	Sunset      int64   `json:"sunset"`
	MoonPhase   float64 `json:"moon_phase"`
	Condition   string  `json:"condition"`
	Description string  `json:"description"`
	Icon        string  `json:"icon"`
}

type yearDays struct {
	Year string      `json:"year"`
	Days []dayRecord `json:"days"`
}

// forecastSnapshot is what the dashboard's forward-looking panels read. It is
// replaced whole on every run rather than merged, because a superseded forecast
// is not worth keeping -- except for alerts, whose text costs an extra billed
// call to fetch and so is carried across runs.
type forecastSnapshot struct {
	UpdatedAt      time.Time     `json:"updated_at"`
	Timezone       string        `json:"timezone"`
	TimezoneOffset int           `json:"timezone_offset"`
	Hourly         []hourRecord  `json:"hourly"`
	Daily          []dayRecord   `json:"daily"`
	Alerts         []alertDetail `json:"alerts"`
}

type hourRecord struct {
	Time        int64   `json:"time"` // epoch ms
	Temp        float64 `json:"temp"`
	FeelsLike   float64 `json:"feels_like"`
	Humidity    float64 `json:"humidity"`
	Pressure    float64 `json:"pressure"`
	UVI         float64 `json:"uvi"`
	Clouds      float64 `json:"clouds"`
	WindSpeed   float64 `json:"wind_speed"`
	WindGust    float64 `json:"wind_gust"`
	WindDeg     float64 `json:"wind_deg"`
	Pop         float64 `json:"pop"`
	Rain        float64 `json:"rain"`
	Snow        float64 `json:"snow"`
	Condition   string  `json:"condition"`
	Description string  `json:"description"`
	Icon        string  `json:"icon"`
}

func observationsKey(prefix, month string) string {
	return fmt.Sprintf("%s%s%s.json", prefix, observationsPrefix, month)
}

func dailyKey(prefix, year string) string {
	return fmt.Sprintf("%s%s%s.json", prefix, dailyPrefix, year)
}

// archiveObservation folds one reading into its month. Readings are keyed on
// the minute they were taken, so a retry inside the same minute corrects the
// stored copy rather than adding a near-duplicate point to every graph.
func (a *app) archiveObservation(ctx context.Context, reading observation) error {
	month := reading.Time.In(a.zone).Format("2006-01")
	existing, err := a.loadMonth(ctx, month)
	if err != nil {
		return fmt.Errorf("load %s: %w", month, err)
	}

	merged := mergeObservations(existing.Readings, []observation{reading})
	return a.saveJSON(ctx, observationsKey(a.prefix, month), monthObservations{Month: month, Readings: merged})
}

// archiveDays merges daily records into the year objects they belong to. A
// record replaces whatever is stored for the same date, which is how a date
// first written as a forecast becomes history once it is re-read.
func (a *app) archiveDays(ctx context.Context, days []dayRecord) error {
	byYear := map[string][]dayRecord{}
	for _, day := range days {
		if len(day.Date) < 4 {
			continue
		}
		year := day.Date[:4]
		byYear[year] = append(byYear[year], day)
	}

	years := make([]string, 0, len(byYear))
	for year := range byYear {
		years = append(years, year)
	}
	sort.Strings(years)

	for _, year := range years {
		existing, err := a.loadYear(ctx, year)
		if err != nil {
			return fmt.Errorf("load %s: %w", year, err)
		}
		merged := mergeDays(existing.Days, byYear[year])
		if sameDays(merged, existing.Days) {
			continue
		}
		if err := a.saveJSON(ctx, dailyKey(a.prefix, year), yearDays{Year: year, Days: merged}); err != nil {
			return fmt.Errorf("save %s: %w", year, err)
		}
	}
	return nil
}

func (a *app) loadMonth(ctx context.Context, month string) (monthObservations, error) {
	var archive monthObservations
	found, err := a.loadJSON(ctx, observationsKey(a.prefix, month), &archive)
	if err != nil {
		return monthObservations{}, err
	}
	if !found {
		return monthObservations{Month: month}, nil
	}
	return archive, nil
}

func (a *app) loadYear(ctx context.Context, year string) (yearDays, error) {
	var archive yearDays
	found, err := a.loadJSON(ctx, dailyKey(a.prefix, year), &archive)
	if err != nil {
		return yearDays{}, err
	}
	if !found {
		return yearDays{Year: year}, nil
	}
	return archive, nil
}

func (a *app) loadForecast(ctx context.Context) (forecastSnapshot, error) {
	var snapshot forecastSnapshot
	if _, err := a.loadJSON(ctx, a.prefix+forecastKey, &snapshot); err != nil {
		return forecastSnapshot{}, err
	}
	return snapshot, nil
}

func (a *app) saveForecast(ctx context.Context, snapshot forecastSnapshot) error {
	return a.saveJSON(ctx, a.prefix+forecastKey, snapshot)
}

// loadJSON reports found=false for a key that is not there yet, which is the
// normal state for a month or year nothing has been written to.
func (a *app) loadJSON(ctx context.Context, key string, into any) (bool, error) {
	out, err := a.objects.GetObject(ctx, &s3.GetObjectInput{Bucket: &a.bucket, Key: &key})
	if err != nil {
		var missing *types.NoSuchKey
		if errors.As(err, &missing) {
			return false, nil
		}
		return false, err
	}
	defer out.Body.Close()

	body, err := io.ReadAll(io.LimitReader(out.Body, 64<<20))
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return false, fmt.Errorf("decode %s: %w", key, err)
	}
	return true, nil
}

func (a *app) saveJSON(ctx context.Context, key string, document any) error {
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	contentType := "application/json"
	_, err = a.objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &a.bucket,
		Key:         &key,
		Body:        bytes.NewReader(encoded),
		ContentType: &contentType,
	})
	return err
}

func mergeObservations(existing, incoming []observation) []observation {
	byMinute := make(map[int64]observation, len(existing)+len(incoming))
	for _, reading := range existing {
		byMinute[reading.Time.Unix()/60] = reading
	}
	for _, reading := range incoming {
		byMinute[reading.Time.Unix()/60] = reading
	}

	merged := make([]observation, 0, len(byMinute))
	for _, reading := range byMinute {
		merged = append(merged, reading)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Time.Before(merged[j].Time) })
	return merged
}

func mergeDays(existing, incoming []dayRecord) []dayRecord {
	byDate := make(map[string]dayRecord, len(existing)+len(incoming))
	for _, day := range existing {
		byDate[day.Date] = day
	}
	for _, day := range incoming {
		// A settled record is the final word on a date. Guarding against the
		// reverse keeps a backfill that walks forward past today from
		// overwriting settled history with a forecast for the same date.
		if stored, ok := byDate[day.Date]; ok && stored.Settled && !day.Settled {
			continue
		}
		byDate[day.Date] = day
	}

	merged := make([]dayRecord, 0, len(byDate))
	for _, day := range byDate {
		merged = append(merged, day)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Date < merged[j].Date })
	return merged
}

func sameDays(left, right []dayRecord) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// mergeAlerts keeps the text of alerts already fetched and drops any that have
// expired, so /alert/{id} is called once per alert rather than once per run.
func mergeAlerts(existing, incoming []alertDetail, now time.Time) []alertDetail {
	byID := make(map[string]alertDetail, len(existing)+len(incoming))
	for _, alert := range existing {
		byID[alert.ID] = alert
	}
	for _, alert := range incoming {
		byID[alert.ID] = alert
	}

	merged := make([]alertDetail, 0, len(byID))
	for _, alert := range byID {
		if alert.End > 0 && time.Unix(alert.End, 0).Before(now) {
			continue
		}
		merged = append(merged, alert)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Start != merged[j].Start {
			return merged[i].Start < merged[j].Start
		}
		return merged[i].ID < merged[j].ID
	})
	return merged
}
