package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// One artifact, two deployments. WEATHER_MODE=collect holds the OpenWeather key
// and the Grafana push token and writes the archive; WEATHER_MODE=serve reads it
// back over a Function URL and is given neither credential.
const (
	modeCollect = "collect"
	modeServe   = "serve"

	// How far back a collect run asks the daily timeline to start. The same
	// endpoint returns settled history and forecast, so reaching two days back
	// re-reads yesterday and the day before, replacing the forecasts stored for
	// them with what actually happened -- and it costs nothing extra, since one
	// page covers daysPerPage days either way.
	dailyLookbackDays = 2
)

type app struct {
	parameters *ssm.Client
	objects    *s3.Client
	httpClient *http.Client
	now        func() time.Time

	bucket   string
	prefix   string
	zone     *time.Location
	location string
}

// collectEvent is empty for a scheduled run. The backfill fields turn the same
// function into a one-shot history loader; see backfill.go.
type collectEvent struct {
	BackfillFrom string `json:"backfill_from"` // YYYY-MM-DD
	BackfillTo   string `json:"backfill_to"`   // YYYY-MM-DD, defaults to today
}

func main() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS config: %v", err)
	}

	zone, err := configuredZone()
	if err != nil {
		log.Fatalf("resolve timezone: %v", err)
	}

	instance := &app{
		parameters: ssm.NewFromConfig(cfg),
		objects:    s3.NewFromConfig(cfg),
		httpClient: &http.Client{Timeout: 20 * time.Second},
		now:        time.Now,
		bucket:     envOr("ARCHIVE_BUCKET", ""),
		prefix:     envOr("ARCHIVE_PREFIX", "weather/"),
		zone:       zone,
		location:   envOr("WEATHER_LOCATION", "home"),
	}

	if strings.EqualFold(envOr("WEATHER_MODE", modeCollect), modeServe) {
		lambda.Start(instance.serve)
		return
	}
	lambda.Start(instance.collect)
}

// configuredZone reads the location's UTC offset from configuration rather than
// a zone name, because the Lambda runtime ships no tzdata and the offset is the
// only thing the day boundaries actually need. Japan has no DST, so a fixed
// offset is exact here; a location that observes it would need real tzdata
// bundled instead.
func configuredZone() (*time.Location, error) {
	raw := envOr("WEATHER_TIMEZONE_OFFSET_SECONDS", "32400") // JST
	offset, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("WEATHER_TIMEZONE_OFFSET_SECONDS must be an integer, got %q", raw)
	}
	return time.FixedZone("local", offset), nil
}

func (a *app) collect(ctx context.Context, event collectEvent) (any, error) {
	if a.bucket == "" {
		return nil, errors.New("ARCHIVE_BUCKET is required")
	}

	weather, err := a.openWeather(ctx)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(event.BackfillFrom) != "" {
		return a.backfill(ctx, weather, event)
	}

	now := a.now()

	current, err := weather.current(ctx)
	if err != nil {
		return nil, fmt.Errorf("read current conditions: %w", err)
	}
	if len(current.Data) == 0 {
		return nil, errors.New("current conditions came back empty")
	}
	present := current.Data[0]

	days, err := weather.days(ctx, startOfDay(now, a.zone).AddDate(0, 0, -dailyLookbackDays).Unix())
	if err != nil {
		return nil, fmt.Errorf("read daily timeline: %w", err)
	}
	hours, err := weather.hours(ctx, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("read hourly timeline: %w", err)
	}

	// Air quality is a separate 2.5 endpoint on its own quota, and a nice-to-
	// have next to the temperature the dashboard exists for. A failure here
	// should not cost us the reading we came for.
	air, err := weather.air(ctx)
	if err != nil {
		log.Printf("air pollution: %v", err)
	}

	reading := buildObservation(present, air, now)
	if err := a.archiveObservation(ctx, reading); err != nil {
		return nil, fmt.Errorf("archive observation: %w", err)
	}

	dayRecords := buildDays(days.Data, a.zone, now)
	if err := a.archiveDays(ctx, dayRecords); err != nil {
		return nil, fmt.Errorf("archive daily timeline: %w", err)
	}

	alerts, err := a.resolveAlerts(ctx, weather, present.Alerts, now)
	if err != nil {
		// Alert text is the least important thing in the run and the only part
		// that can cost extra billed calls, so a failure is logged and the rest
		// of the snapshot is stored without it.
		log.Printf("resolve alerts: %v", err)
	}

	snapshot := forecastSnapshot{
		UpdatedAt:      now.UTC(),
		Timezone:       current.Timezone,
		TimezoneOffset: current.TimezoneOffset,
		Hourly:         buildHours(hours.Data),
		Daily:          forecastDays(dayRecords, now, a.zone),
		Alerts:         alerts,
	}
	if err := a.saveForecast(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("save forecast snapshot: %w", err)
	}

	// Read back the month we just wrote to, so the derived series -- today's
	// observed extremes, the pressure and temperature trends -- are computed
	// over the archive rather than over whatever this one run happened to see.
	month, err := a.loadMonth(ctx, now.In(a.zone).Format("2006-01"))
	if err != nil {
		return nil, fmt.Errorf("read month for derived metrics: %w", err)
	}

	pushed, err := a.push(ctx, buildTimeSeries(a.location, reading, snapshot, month.Readings, a.zone, now))
	if err != nil {
		return nil, fmt.Errorf("remote_write to Grafana Cloud: %w", err)
	}

	return map[string]any{
		"observed_at":    reading.Time,
		"temp":           reading.Temp,
		"days_archived":  len(dayRecords),
		"hours_stored":   len(snapshot.Hourly),
		"alerts_active":  len(snapshot.Alerts),
		"series_pushed":  pushed,
		"api_calls_made": 3, // current + one day page + one hour page
	}, nil
}

// resolveAlerts turns the alert IDs embedded in the weather payload into text.
// 4.0 charges for /alert/{id} like any other call, so only IDs whose text is not
// already in the stored snapshot are fetched -- normally none.
func (a *app) resolveAlerts(ctx context.Context, weather *openWeather, ids []string, now time.Time) ([]alertDetail, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	stored, err := a.loadForecast(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]alertDetail, len(stored.Alerts))
	for _, alert := range stored.Alerts {
		known[alert.ID] = alert
	}

	var resolved []alertDetail
	var failures []string
	for _, id := range ids {
		if alert, ok := known[id]; ok {
			resolved = append(resolved, alert)
			continue
		}
		alert, err := weather.alert(ctx, id)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		resolved = append(resolved, alert)
	}

	merged := mergeAlerts(nil, resolved, now)
	if len(failures) > 0 {
		return merged, errors.New(strings.Join(failures, "; "))
	}
	return merged, nil
}

func (a *app) openWeather(ctx context.Context) (*openWeather, error) {
	keyParameter, err := requiredEnv("OPENWEATHER_API_KEY_PARAMETER_NAME")
	if err != nil {
		return nil, err
	}
	apiKey, err := a.parameterString(ctx, keyParameter)
	if err != nil {
		return nil, fmt.Errorf("read OpenWeather API key parameter: %w", err)
	}

	lat, err := requiredFloatEnv("WEATHER_LATITUDE")
	if err != nil {
		return nil, err
	}
	lon, err := requiredFloatEnv("WEATHER_LONGITUDE")
	if err != nil {
		return nil, err
	}

	return &openWeather{
		client:              a.httpClient,
		apiKey:              apiKey,
		oneCallBaseURL:      strings.TrimRight(envOr("OPENWEATHER_ONECALL_BASE_URL", defaultOneCallBaseURL), "/"),
		airPollutionBaseURL: strings.TrimRight(envOr("OPENWEATHER_AIR_POLLUTION_BASE_URL", defaultAirPollutionBaseURL), "/"),
		lat:                 lat,
		lon:                 lon,
		lang:                envOr("WEATHER_LANG", "ja"),
	}, nil
}

func buildObservation(present weatherPoint, air airPollution, fallback time.Time) observation {
	observed := fallback
	if present.Dt > 0 {
		observed = time.Unix(present.Dt, 0).UTC()
	}

	reading := observation{
		Time:       observed,
		Temp:       present.Temp.Value,
		FeelsLike:  present.FeelsLike.Value,
		DewPoint:   present.DewPoint,
		Humidity:   present.Humidity,
		Pressure:   present.Pressure,
		Clouds:     present.Clouds,
		Visibility: present.Visibility,
		WindSpeed:  present.WindSpeed,
		WindGust:   present.WindGust,
		WindDeg:    present.WindDeg,
		UVI:        present.UVI,
		Rain1h:     present.Rain.Millimetres,
		Snow1h:     present.Snow.Millimetres,
		Sunrise:    present.Sunrise,
		Sunset:     present.Sunset,
	}
	if len(present.Weather) > 0 {
		reading.Condition = present.Weather[0].Main
		reading.Description = present.Weather[0].Description
		reading.Icon = present.Weather[0].Icon
	}
	if len(air.List) > 0 {
		reading.AQI = air.List[0].Main.AQI
		reading.Pollutants = air.List[0].Components
	}
	return reading
}

// buildDays converts daily timeline records into archive rows. A record is
// settled once the date it covers has fully passed in the location's zone:
// until then its min/max are still a forecast, however recently they were
// refreshed.
func buildDays(points []weatherPoint, zone *time.Location, now time.Time) []dayRecord {
	today := startOfDay(now, zone)

	days := make([]dayRecord, 0, len(points))
	for _, point := range points {
		if point.Dt == 0 {
			continue
		}
		local := time.Unix(point.Dt, 0).In(zone)
		midnight := startOfDay(local, zone)

		day := dayRecord{
			Date:      midnight.Format("2006-01-02"),
			Time:      midnight.UnixMilli(),
			Settled:   midnight.Before(today),
			FeelsLike: point.FeelsLike.Value,
			Humidity:  point.Humidity,
			Pressure:  point.Pressure,
			DewPoint:  point.DewPoint,
			UVI:       point.UVI,
			Clouds:    point.Clouds,
			WindSpeed: point.WindSpeed,
			WindGust:  point.WindGust,
			WindDeg:   point.WindDeg,
			Pop:       point.Pop,
			Rain:      point.Rain.Millimetres,
			Snow:      point.Snow.Millimetres,
			Sunrise:   point.Sunrise,
			Sunset:    point.Sunset,
			MoonPhase: point.MoonPhase,
		}
		if span := point.TempRange; span != nil {
			day.TempMax = span.Max
			day.TempMin = span.Min
			day.TempMorn = span.Morn
			day.TempDay = span.Day
			day.TempEve = span.Eve
			day.TempNight = span.Night
		}
		if len(point.Weather) > 0 {
			day.Condition = point.Weather[0].Main
			day.Description = point.Weather[0].Description
			day.Icon = point.Weather[0].Icon
		}
		days = append(days, day)
	}
	return days
}

// forecastDays is the forward-looking slice of what buildDays produced: today
// and after. The snapshot is what the "next N days" panels read, and a settled
// day belongs on the history axis instead.
func forecastDays(days []dayRecord, now time.Time, zone *time.Location) []dayRecord {
	today := startOfDay(now, zone).UnixMilli()
	forward := make([]dayRecord, 0, len(days))
	for _, day := range days {
		if day.Time < today {
			continue
		}
		forward = append(forward, day)
	}
	return forward
}

func buildHours(points []weatherPoint) []hourRecord {
	hours := make([]hourRecord, 0, len(points))
	for _, point := range points {
		if point.Dt == 0 {
			continue
		}
		hour := hourRecord{
			Time:      point.Dt * 1000,
			Temp:      point.Temp.Value,
			FeelsLike: point.FeelsLike.Value,
			Humidity:  point.Humidity,
			Pressure:  point.Pressure,
			UVI:       point.UVI,
			Clouds:    point.Clouds,
			WindSpeed: point.WindSpeed,
			WindGust:  point.WindGust,
			WindDeg:   point.WindDeg,
			Pop:       point.Pop,
			Rain:      point.Rain.Millimetres,
			Snow:      point.Snow.Millimetres,
		}
		if len(point.Weather) > 0 {
			hour.Condition = point.Weather[0].Main
			hour.Description = point.Weather[0].Description
			hour.Icon = point.Weather[0].Icon
		}
		hours = append(hours, hour)
	}
	return hours
}

func (a *app) parameterString(ctx context.Context, parameterName string) (string, error) {
	withDecryption := true
	out, err := a.parameters.GetParameter(ctx, &ssm.GetParameterInput{Name: &parameterName, WithDecryption: &withDecryption})
	if err != nil {
		return "", err
	}
	if out.Parameter != nil && out.Parameter.Value != nil {
		return strings.TrimSpace(*out.Parameter.Value), nil
	}
	return "", errors.New("parameter has no value")
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func requiredFloatEnv(name string) (float64, error) {
	raw, err := requiredEnv(name)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", name, raw)
	}
	return value, nil
}

func envOr(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}
