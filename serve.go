package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// authHeader carries a shared secret. The Function URL itself is
// unauthenticated because Grafana Cloud's Infinity datasource cannot sign
// SigV4, so this header is the only thing in front of the archive.
const authHeader = "x-weather-token"

func (a *app) serve(ctx context.Context, request events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	parameterName, err := requiredEnv("API_TOKEN_PARAMETER_NAME")
	if err != nil {
		log.Printf("read API token: %v", err)
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "server misconfigured"})
	}
	expected, err := a.parameterString(ctx, parameterName)
	if err != nil {
		log.Printf("read API token: %v", err)
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "server misconfigured"})
	}

	presented := ""
	for name, value := range request.Headers {
		if strings.EqualFold(name, authHeader) {
			presented = value
			break
		}
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
		return jsonResponse(http.StatusForbidden, map[string]string{"error": "forbidden"})
	}

	if a.bucket == "" {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "ARCHIVE_BUCKET is not set"})
	}

	now := a.now()
	resource := strings.ToLower(strings.TrimSpace(request.QueryStringParameters["resource"]))

	switch resource {
	case "summary":
		summary, err := a.servedSummary(ctx, now)
		if err != nil {
			log.Printf("build summary: %v", err)
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot read archive"})
		}
		// A one-element array, like every other endpoint here, so Infinity can
		// parse them all the same way.
		return jsonResponse(http.StatusOK, []weatherSummaryRow{summary})

	case "forecast":
		snapshot, err := a.loadForecast(ctx)
		if err != nil {
			log.Printf("read forecast snapshot: %v", err)
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot read forecast"})
		}
		return jsonResponse(http.StatusOK, snapshot.Daily)

	case "hourly":
		snapshot, err := a.loadForecast(ctx)
		if err != nil {
			log.Printf("read forecast snapshot: %v", err)
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot read forecast"})
		}
		return jsonResponse(http.StatusOK, snapshot.Hourly)

	case "alerts":
		snapshot, err := a.loadForecast(ctx)
		if err != nil {
			log.Printf("read forecast snapshot: %v", err)
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot read forecast"})
		}
		return jsonResponse(http.StatusOK, servedAlerts(snapshot.Alerts))

	case "daily":
		from, to, err := timeRange(request.QueryStringParameters, now)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		days, err := a.servedDays(ctx, from, to)
		if err != nil {
			log.Printf("read daily archive: %v", err)
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot read daily archive"})
		}
		return jsonResponse(http.StatusOK, days)

	case "monthly":
		from, to, err := timeRange(request.QueryStringParameters, now)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		days, err := a.servedDays(ctx, from, to)
		if err != nil {
			log.Printf("read daily archive: %v", err)
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot read daily archive"})
		}
		return jsonResponse(http.StatusOK, monthlyRollup(days))

	case "", "observations":
		from, to, err := timeRange(request.QueryStringParameters, now)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		readings, err := a.servedObservations(ctx, from, to)
		if err != nil {
			log.Printf("read observations archive: %v", err)
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "cannot read observations archive"})
		}
		return jsonResponse(http.StatusOK, readings)
	}

	return jsonResponse(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown resource %q", resource)})
}

// servedObservation is one stored reading in the shape Infinity consumes: a
// millisecond epoch plus plain numbers, with the derived indices computed here
// so the dashboard does not have to carry the formulas.
type servedObservation struct {
	Time            int64   `json:"time"`
	ISO             string  `json:"iso"`
	Date            string  `json:"date"`        // YYYY-MM-DD, for grouping by day
	TimeOfDay       string  `json:"time_of_day"` // HH:MM, for time-of-day heatmaps
	Temp            float64 `json:"temp"`
	FeelsLike       float64 `json:"feels_like"`
	DewPoint        float64 `json:"dew_point"`
	Humidity        float64 `json:"humidity"`
	Pressure        float64 `json:"pressure"`
	Clouds          float64 `json:"clouds"`
	Visibility      float64 `json:"visibility"`
	WindSpeed       float64 `json:"wind_speed"`
	WindGust        float64 `json:"wind_gust"`
	WindDeg         float64 `json:"wind_deg"`
	WindCompass     string  `json:"wind_compass"`
	UVI             float64 `json:"uvi"`
	Rain1h          float64 `json:"rain_1h"`
	Snow1h          float64 `json:"snow_1h"`
	Condition       string  `json:"condition"`
	Description     string  `json:"description"`
	Icon            string  `json:"icon"`
	AQI             float64 `json:"aqi"`
	PM25            float64 `json:"pm2_5"`
	PM10            float64 `json:"pm10"`
	DiscomfortIndex float64 `json:"discomfort_index"`
	WBGT            float64 `json:"wbgt"`
}

func (a *app) servedObservations(ctx context.Context, from, to time.Time) ([]servedObservation, error) {
	readings := []servedObservation{}
	for _, month := range monthsBetween(from, to, a.zone) {
		archive, err := a.loadMonth(ctx, month)
		if err != nil {
			return nil, fmt.Errorf("month %s: %w", month, err)
		}
		for _, reading := range archive.Readings {
			if reading.Time.Before(from) || reading.Time.After(to) {
				continue
			}
			readings = append(readings, presentObservation(reading, a.zone))
		}
	}
	return readings, nil
}

func presentObservation(reading observation, zone *time.Location) servedObservation {
	local := reading.Time.In(zone)
	return servedObservation{
		Time:            reading.Time.UnixMilli(),
		ISO:             local.Format(time.RFC3339),
		Date:            local.Format("2006-01-02"),
		TimeOfDay:       local.Format("15:04"),
		Temp:            reading.Temp,
		FeelsLike:       reading.FeelsLike,
		DewPoint:        reading.DewPoint,
		Humidity:        reading.Humidity,
		Pressure:        reading.Pressure,
		Clouds:          reading.Clouds,
		Visibility:      reading.Visibility,
		WindSpeed:       reading.WindSpeed,
		WindGust:        reading.WindGust,
		WindDeg:         reading.WindDeg,
		WindCompass:     compass(reading.WindDeg),
		UVI:             reading.UVI,
		Rain1h:          reading.Rain1h,
		Snow1h:          reading.Snow1h,
		Condition:       reading.Condition,
		Description:     reading.Description,
		Icon:            reading.Icon,
		AQI:             reading.AQI,
		PM25:            reading.Pollutants["pm2_5"],
		PM10:            reading.Pollutants["pm10"],
		DiscomfortIndex: round(discomfortIndex(reading.Temp, reading.Humidity), 2),
		WBGT:            round(wbgtEstimate(reading.Temp, reading.Humidity), 2),
	}
}

// servedDay is one date, carrying both what the API says about it and what we
// measured ourselves. For a settled date the API's max/min are history and the
// two should roughly agree; for today the API's are a forecast and ours are how
// far the day has actually got. Observations is 0 for a date that predates the
// collector -- backfilled history has no readings of our own behind it, which
// is worth being able to see rather than guess at.
type servedDay struct {
	Time        int64   `json:"time"` // local midnight, epoch ms
	Date        string  `json:"date"`
	Settled     bool    `json:"settled"`
	TempMax     float64 `json:"temp_max"`
	TempMin     float64 `json:"temp_min"`
	TempRange   float64 `json:"temp_range"`
	TempDay     float64 `json:"temp_day"`
	TempNight   float64 `json:"temp_night"`
	FeelsLike   float64 `json:"feels_like_day"`
	Humidity    float64 `json:"humidity"`
	Pressure    float64 `json:"pressure"`
	UVI         float64 `json:"uvi"`
	Clouds      float64 `json:"clouds"`
	WindSpeed   float64 `json:"wind_speed"`
	WindGust    float64 `json:"wind_gust"`
	Pop         float64 `json:"pop"`
	Rain        float64 `json:"rain"`
	Snow        float64 `json:"snow"`
	Condition   string  `json:"condition"`
	Description string  `json:"description"`
	Icon        string  `json:"icon"`
	Sunrise     int64   `json:"sunrise"`
	Sunset      int64   `json:"sunset"`
	Daylight    float64 `json:"daylight_seconds"`
	MoonPhase   float64 `json:"moon_phase"`
	// Null rather than zero when Observations is 0. A backfilled date has no
	// readings of our own behind it, and a zero here would draw a 0 degree line
	// across every chart that plots them -- data-shaped, but not data.
	ObservedMax  *float64 `json:"observed_temp_max"`
	ObservedMin  *float64 `json:"observed_temp_min"`
	ObservedMean *float64 `json:"observed_temp_mean"`
	ObservedRain *float64 `json:"observed_rain_mm"`
	Observations int      `json:"observations"`
}

func (a *app) servedDays(ctx context.Context, from, to time.Time) ([]servedDay, error) {
	byDate := map[string]*servedDay{}
	order := []string{}

	fromDate := startOfDay(from, a.zone).Format("2006-01-02")
	toDate := startOfDay(to, a.zone).Format("2006-01-02")

	for _, year := range yearsBetween(from, to, a.zone) {
		archive, err := a.loadYear(ctx, year)
		if err != nil {
			return nil, fmt.Errorf("year %s: %w", year, err)
		}
		for _, day := range archive.Days {
			// Whole days only: a date is kept or dropped by the date itself,
			// never cut by where the dashboard window happens to land inside it.
			if day.Date < fromDate || day.Date > toDate {
				continue
			}
			served := &servedDay{
				Time:        day.Time,
				Date:        day.Date,
				Settled:     day.Settled,
				TempMax:     day.TempMax,
				TempMin:     day.TempMin,
				TempRange:   round(day.TempMax-day.TempMin, 2),
				TempDay:     day.TempDay,
				TempNight:   day.TempNight,
				FeelsLike:   day.FeelsLike,
				Humidity:    day.Humidity,
				Pressure:    day.Pressure,
				UVI:         day.UVI,
				Clouds:      day.Clouds,
				WindSpeed:   day.WindSpeed,
				WindGust:    day.WindGust,
				Pop:         day.Pop,
				Rain:        day.Rain,
				Snow:        day.Snow,
				Condition:   day.Condition,
				Description: day.Description,
				Icon:        day.Icon,
				Sunrise:     day.Sunrise,
				Sunset:      day.Sunset,
				MoonPhase:   day.MoonPhase,
			}
			if day.Sunrise > 0 && day.Sunset > day.Sunrise {
				served.Daylight = float64(day.Sunset - day.Sunrise)
			}
			byDate[day.Date] = served
			order = append(order, day.Date)
		}
	}

	// Fold our own readings in on top. A date with no stored daily record still
	// gets a row this way, which is what happens for the current day before the
	// first collect run of it lands.
	for _, month := range monthsBetween(from, to, a.zone) {
		archive, err := a.loadMonth(ctx, month)
		if err != nil {
			return nil, fmt.Errorf("month %s: %w", month, err)
		}
		for _, reading := range archive.Readings {
			local := reading.Time.In(a.zone)
			date := local.Format("2006-01-02")
			if date < fromDate || date > toDate {
				continue
			}
			day, ok := byDate[date]
			if !ok {
				day = &servedDay{Time: startOfDay(local, a.zone).UnixMilli(), Date: date}
				byDate[date] = day
				order = append(order, date)
			}
			if day.Observations == 0 {
				day.ObservedMax = ptr(reading.Temp)
				day.ObservedMin = ptr(reading.Temp)
				day.ObservedMean = ptr(0)
				day.ObservedRain = ptr(0)
			}
			if reading.Temp > *day.ObservedMax {
				*day.ObservedMax = reading.Temp
			}
			if reading.Temp < *day.ObservedMin {
				*day.ObservedMin = reading.Temp
			}
			*day.ObservedMean += reading.Temp
			*day.ObservedRain += reading.Rain1h
			day.Observations++
		}
	}

	days := make([]servedDay, 0, len(byDate))
	seen := map[string]bool{}
	for _, date := range order {
		if seen[date] {
			continue
		}
		seen[date] = true
		day := byDate[date]
		if day.Observations > 0 {
			*day.ObservedMean = round(*day.ObservedMean/float64(day.Observations), 2)
			*day.ObservedMax = round(*day.ObservedMax, 2)
			*day.ObservedMin = round(*day.ObservedMin, 2)
			*day.ObservedRain = round(*day.ObservedRain, 2)
		}
		days = append(days, *day)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
	return days, nil
}

// weatherSummaryRow is the handful of numbers a row of stat panels wants,
// assembled in one request so the dashboard's header does not need six queries.
type weatherSummaryRow struct {
	Time           int64   `json:"time"`
	ISO            string  `json:"iso"`
	Temp           float64 `json:"temp"`
	FeelsLike      float64 `json:"feels_like"`
	FeelsLikeDelta float64 `json:"feels_like_delta"`
	Humidity       float64 `json:"humidity"`
	Pressure       float64 `json:"pressure"`
	// Null, not zero, until the archive holds a reading far enough back to
	// compare against. Zero here would read as "steady" -- which is a claim
	// about the weather, not an admission that we cannot answer yet. The
	// Prometheus copy of these two already omits the series in that case;
	// this makes the JSON agree with it.
	PressureChange *float64 `json:"pressure_change_3h"`
	TempChange24h  *float64 `json:"temp_change_24h"`
	WindSpeed      float64  `json:"wind_speed"`
	WindGust       float64  `json:"wind_gust"`
	WindCompass    string   `json:"wind_compass"`
	Clouds         float64  `json:"clouds"`
	UVI            float64  `json:"uvi"`
	Condition      string   `json:"condition"`
	Description    string   `json:"description"`
	Icon           string   `json:"icon"`
	ForecastMax    float64  `json:"forecast_temp_max"`
	ForecastMin    float64  `json:"forecast_temp_min"`
	ForecastPop    float64  `json:"forecast_pop"`
	// Null before the first reading of the day lands, for the same reason the
	// daily rows use pointers: an unmeasured maximum is not a maximum of zero.
	ObservedMax     *float64 `json:"observed_temp_max"`
	ObservedMin     *float64 `json:"observed_temp_min"`
	RainToday       float64  `json:"rain_today_mm"`
	AQI             float64  `json:"aqi"`
	PM25            float64  `json:"pm2_5"`
	PM10            float64  `json:"pm10"`
	DiscomfortIndex float64  `json:"discomfort_index"`
	WBGT            float64  `json:"wbgt"`
	Sunrise         int64    `json:"sunrise"`
	Sunset          int64    `json:"sunset"`
	Daylight        float64  `json:"daylight_seconds"`
	AlertsActive    int      `json:"alerts_active"`
	// DataLagSeconds growing is how a stalled collector shows up: nothing new
	// lands in the archive, whatever the reason, so no separate success flag is
	// needed.
	DataLagSeconds float64 `json:"data_lag_seconds"`
}

func (a *app) servedSummary(ctx context.Context, now time.Time) (weatherSummaryRow, error) {
	today := startOfDay(now, a.zone)

	// Yesterday's month too, so the 24-hour comparison and a day that has just
	// rolled over a month boundary both have their baseline available.
	month, err := a.loadMonth(ctx, now.In(a.zone).Format("2006-01"))
	if err != nil {
		return weatherSummaryRow{}, err
	}
	readings := month.Readings
	previous := today.AddDate(0, 0, -1).Format("2006-01")
	if previous != month.Month {
		earlier, err := a.loadMonth(ctx, previous)
		if err != nil {
			return weatherSummaryRow{}, err
		}
		readings = mergeObservations(earlier.Readings, readings)
	}

	snapshot, err := a.loadForecast(ctx)
	if err != nil {
		return weatherSummaryRow{}, err
	}

	return summarize(readings, snapshot, a.zone, now), nil
}

// summarize touches no S3, so it can be tested directly against synthetic
// readings and snapshots.
func summarize(readings []observation, snapshot forecastSnapshot, zone *time.Location, now time.Time) weatherSummaryRow {
	summary := weatherSummaryRow{AlertsActive: len(snapshot.Alerts)}
	if len(readings) == 0 {
		return summary
	}

	latest := readings[len(readings)-1]
	summary.Time = latest.Time.UnixMilli()
	summary.ISO = latest.Time.In(zone).Format(time.RFC3339)
	summary.Temp = latest.Temp
	summary.FeelsLike = latest.FeelsLike
	summary.FeelsLikeDelta = round(latest.FeelsLike-latest.Temp, 2)
	summary.Humidity = latest.Humidity
	summary.Pressure = latest.Pressure
	summary.WindSpeed = latest.WindSpeed
	summary.WindGust = latest.WindGust
	summary.WindCompass = compass(latest.WindDeg)
	summary.Clouds = latest.Clouds
	summary.UVI = latest.UVI
	summary.Condition = latest.Condition
	summary.Description = latest.Description
	summary.Icon = latest.Icon
	summary.AQI = latest.AQI
	summary.PM25 = latest.Pollutants["pm2_5"]
	summary.PM10 = latest.Pollutants["pm10"]
	summary.DiscomfortIndex = round(discomfortIndex(latest.Temp, latest.Humidity), 2)
	summary.WBGT = round(wbgtEstimate(latest.Temp, latest.Humidity), 2)
	summary.Sunrise = latest.Sunrise
	summary.Sunset = latest.Sunset
	if latest.Sunrise > 0 && latest.Sunset > latest.Sunrise {
		summary.Daylight = float64(latest.Sunset - latest.Sunrise)
	}
	summary.DataLagSeconds = round(now.Sub(latest.Time).Seconds(), 0)

	if change, ok := changeOver(readings, now, pressureWindow, pressureTolerance, func(r observation) float64 { return r.Pressure }); ok {
		summary.PressureChange = ptr(round(change, 2))
	}
	if change, ok := changeOver(readings, now, temperatureWindow, temperatureTolerance, func(r observation) float64 { return r.Temp }); ok {
		summary.TempChange24h = ptr(round(change, 2))
	}

	today := startOfDay(now, zone)
	sinceMidnight := between(readings, today, today.AddDate(0, 0, 1))
	if high, low, ok := extremes(sinceMidnight, func(r observation) float64 { return r.Temp }); ok {
		summary.ObservedMax = ptr(round(high, 2))
		summary.ObservedMin = ptr(round(low, 2))
	}
	for _, reading := range sinceMidnight {
		summary.RainToday += reading.Rain1h
	}
	summary.RainToday = round(summary.RainToday, 2)

	for _, day := range snapshot.Daily {
		if day.Time != today.UnixMilli() {
			continue
		}
		summary.ForecastMax = day.TempMax
		summary.ForecastMin = day.TempMin
		summary.ForecastPop = day.Pop
		break
	}

	return summary
}

type servedAlert struct {
	alertDetail
	StartTime int64 `json:"start_time"` // epoch ms, for a time axis
	EndTime   int64 `json:"end_time"`
}

func servedAlerts(alerts []alertDetail) []servedAlert {
	served := make([]servedAlert, 0, len(alerts))
	for _, alert := range alerts {
		served = append(served, servedAlert{
			alertDetail: alert,
			StartTime:   alert.Start * 1000,
			EndTime:     alert.End * 1000,
		})
	}
	return served
}

// timeRange reads Grafana's dashboard window. Infinity passes ${__from} and
// ${__to}, which are millisecond epochs; RFC3339 is accepted too so the
// endpoint is usable by hand.
func timeRange(query map[string]string, now time.Time) (time.Time, time.Time, error) {
	from, err := parseBound(query["from"], now.AddDate(0, 0, -30))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid from: %w", err)
	}
	to, err := parseBound(query["to"], now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid to: %w", err)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("to is before from")
	}
	return from, to, nil
}

func parseBound(value string, fallback time.Time) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback, nil
	}
	if epoch, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return time.UnixMilli(epoch), nil
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("want milliseconds since epoch or RFC3339, got %q", trimmed)
	}
	return parsed, nil
}

func monthsBetween(from, to time.Time, zone *time.Location) []string {
	start := from.In(zone)
	end := to.In(zone)

	var months []string
	cursor := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, zone)
	last := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, zone)
	for !cursor.After(last) {
		months = append(months, cursor.Format("2006-01"))
		cursor = cursor.AddDate(0, 1, 0)
	}
	return months
}

func yearsBetween(from, to time.Time, zone *time.Location) []string {
	var years []string
	for year := from.In(zone).Year(); year <= to.In(zone).Year(); year++ {
		years = append(years, strconv.Itoa(year))
	}
	return years
}

func jsonResponse(status int, body any) (events.LambdaFunctionURLResponse, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return events.LambdaFunctionURLResponse{StatusCode: http.StatusInternalServerError}, err
	}
	return events.LambdaFunctionURLResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(encoded),
	}, nil
}

// servedMonth is one calendar month rolled up from its days. The counts are the
// point of it: "how many days did it rain in January" is a question the daily
// rows can answer only by being counted, and counting 374 rows in the browser
// to draw one bar is work the dashboard should not be doing.
//
// Condition days are counted per OpenWeather's coarse `main` value, which is
// the one worth charting -- its `description` splits the same sky into 小雨 /
// 適度な雨 / 激しい雨 and would scatter a year across a dozen near-synonyms.
type servedMonth struct {
	MonthTime int64  `json:"month_time"` // local midnight on the 1st, epoch ms
	YearMonth string `json:"year_month"` // YYYY-MM
	Days      int    `json:"days"`

	ClearDays int `json:"clear_days"`
	CloudDays int `json:"cloud_days"`
	RainDays  int `json:"rain_days"`
	SnowDays  int `json:"snow_days"`
	OtherDays int `json:"other_days"`

	AvgTempMax float64 `json:"avg_temp_max"`
	AvgTempMin float64 `json:"avg_temp_min"`
	// The month's own extremes, not the average of the daily ones -- the
	// hottest afternoon of August is a different fact from a typical August day.
	PeakTempMax float64 `json:"peak_temp_max"`
	PeakTempMin float64 `json:"peak_temp_min"`

	TotalRainMm float64 `json:"total_rain_mm"`
	TotalSnowMm float64 `json:"total_snow_mm"`

	AvgHumidity  float64 `json:"avg_humidity"`
	AvgPressure  float64 `json:"avg_pressure"`
	AvgClouds    float64 `json:"avg_clouds"`
	AvgWindSpeed float64 `json:"avg_wind_speed"`
}

// monthlyRollup groups whole days by the month their date falls in. It takes
// the already-served days rather than reading the archive again, so the two
// endpoints cannot disagree about what a day was.
func monthlyRollup(days []servedDay) []servedMonth {
	type accumulator struct {
		month                                              *servedMonth
		tempMax, tempMin, humidity, pressure, clouds, wind float64
	}

	byMonth := map[string]*accumulator{}
	order := []string{}

	for _, day := range days {
		if len(day.Date) < 7 {
			continue
		}
		key := day.Date[:7]
		acc, ok := byMonth[key]
		if !ok {
			// Midnight on the 1st, derived from the day's own timestamp so the
			// zone the archive was written in is the zone this lands in.
			first := time.UnixMilli(day.Time).UTC().AddDate(0, 0, -(dayOfMonth(day.Date) - 1))
			acc = &accumulator{month: &servedMonth{
				MonthTime:   first.UnixMilli(),
				YearMonth:   key,
				PeakTempMax: day.TempMax,
				PeakTempMin: day.TempMin,
			}}
			byMonth[key] = acc
			order = append(order, key)
		}
		m := acc.month
		m.Days++

		switch day.Condition {
		case "Clear":
			m.ClearDays++
		case "Clouds":
			m.CloudDays++
		case "Rain", "Drizzle":
			m.RainDays++
		case "Snow":
			m.SnowDays++
		default:
			// Thunderstorm, Mist, Fog and the dust/ash family all land here.
			// They are rare enough that a slot each would be four empty series
			// on every chart, and lumping them keeps the stack at five.
			m.OtherDays++
		}

		if day.TempMax > m.PeakTempMax {
			m.PeakTempMax = day.TempMax
		}
		if day.TempMin < m.PeakTempMin {
			m.PeakTempMin = day.TempMin
		}
		m.TotalRainMm += day.Rain
		m.TotalSnowMm += day.Snow

		acc.tempMax += day.TempMax
		acc.tempMin += day.TempMin
		acc.humidity += day.Humidity
		acc.pressure += day.Pressure
		acc.clouds += day.Clouds
		acc.wind += day.WindSpeed
	}

	months := make([]servedMonth, 0, len(byMonth))
	for _, key := range order {
		acc := byMonth[key]
		m := acc.month
		if m.Days > 0 {
			n := float64(m.Days)
			m.AvgTempMax = round(acc.tempMax/n, 2)
			m.AvgTempMin = round(acc.tempMin/n, 2)
			m.AvgHumidity = round(acc.humidity/n, 1)
			m.AvgPressure = round(acc.pressure/n, 1)
			m.AvgClouds = round(acc.clouds/n, 1)
			m.AvgWindSpeed = round(acc.wind/n, 2)
		}
		m.TotalRainMm = round(m.TotalRainMm, 1)
		m.TotalSnowMm = round(m.TotalSnowMm, 1)
		months = append(months, *m)
	}
	// Ascending, because this feeds a time axis.
	sort.Slice(months, func(i, j int) bool { return months[i].YearMonth < months[j].YearMonth })
	return months
}

func dayOfMonth(date string) int {
	day, err := strconv.Atoi(date[8:])
	if err != nil {
		return 1
	}
	return day
}
