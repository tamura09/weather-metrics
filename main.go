package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
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

	// One Call 4.0's daily timeline regularly takes longer than 20 seconds to
	// send its headers, around the quarter hours when all three locations ask
	// at once. The /current call made before it is already billed, and the
	// timed-out request may be too, so cutting it short wastes calls against
	// the 1,000-a-day allowance and loses that run's observation. The function's
	// own timeout is 15 minutes, so waiting longer costs only Lambda time.
	httpTimeout = 60 * time.Second

	// Total time a run may spend on /alert/{id} calls of every kind: new IDs,
	// cached texts past their end re-fetched for an extension, and the three-hourly
	// re-check of IDs confirmed over. With the 60-second timeout that is at
	// most three slow lookups, which these share; see lookupAlerts.
	alertLookupBudget = 3 * time.Minute
	// Kept free of alert lookups at the end of an invocation: the S3 writes
	// and the Grafana push that follow them, the push allowed the full HTTP
	// timeout.
	afterAlertsReserve = httpTimeout + 30*time.Second

	// How long the second read of an unreadable snapshot may take.
	snapshotRetryTimeout = 20 * time.Second
)

// parameterStore is the one SSM call this function makes, narrowed so tests
// can stand in for it.
type parameterStore interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

type app struct {
	parameters parameterStore
	objects    *s3.Client
	httpClient *http.Client
	now        func() time.Time

	bucket   string
	prefix   string
	zone     *time.Location
	location string
	// locations is WEATHER_LOCATIONS. Empty means single-location mode; see
	// locations.go.
	locations []string
}

// collectEvent carries the location a scheduled run is for, when several are
// configured. The backfill fields turn the same function into a one-shot
// history loader; see backfill.go.
type collectEvent struct {
	Location string `json:"location"`
	// Compact thins every stored month to the retention rule and returns,
	// without calling the API; see retention.go.
	Compact      bool   `json:"compact"`
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

	locations, err := parseLocations(os.Getenv("WEATHER_LOCATIONS"))
	if err != nil {
		log.Fatal(err)
	}

	instance := &app{
		parameters: ssm.NewFromConfig(cfg),
		objects:    s3.NewFromConfig(cfg),
		httpClient: &http.Client{Timeout: httpTimeout},
		now:        time.Now,
		bucket:     envOr("ARCHIVE_BUCKET", ""),
		prefix:     envOr("ARCHIVE_PREFIX", "weather/"),
		zone:       zone,
		location:   envOr("WEATHER_LOCATION", "home"),
		locations:  locations,
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
	scoped, err := a.forLocation(event.Location)
	if err != nil {
		return nil, err
	}
	return scoped.collectLocation(ctx, event)
}

func (a *app) collectLocation(ctx context.Context, event collectEvent) (any, error) {
	if a.bucket == "" {
		return nil, errors.New("ARCHIVE_BUCKET is required")
	}
	if event.Compact {
		return a.compactAll(ctx, a.now())
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

	// Air quality is a separate 2.5 endpoint on its own quota, and a nice-to-
	// have next to the temperature the dashboard exists for. A failure here
	// should not cost us the reading we came for.
	air, err := weather.air(ctx)
	if err != nil {
		log.Printf("air pollution: %v", err)
	}

	// The reading is archived before the timelines are asked for. The daily
	// timeline in particular stalls for a minute or more around OpenWeather's
	// three-hourly updates (00, 03, ... UTC), and a run that stopped there
	// used to lose the reading it had already paid for.
	reading := buildObservation(present, air, now)
	if err := a.archiveObservation(ctx, reading, now); err != nil {
		return nil, fmt.Errorf("archive observation: %w", err)
	}
	// Thinning an earlier month is housekeeping; the next run retries it, so a
	// failure here should not cost this run its reading.
	if err := a.compactEarlierMonths(ctx, now); err != nil {
		log.Printf("thin earlier observations: %v", err)
	}

	// A timeline that fails is not fatal either: the forecast changes slowly,
	// so the previous run's copy stands in until the next run, and the
	// reading still gets pushed.
	var previous *forecastSnapshot
	previousForecast := func() forecastSnapshot {
		if previous == nil {
			stored, err := a.loadForecast(ctx)
			if err != nil {
				log.Printf("read previous forecast: %v", err)
			}
			previous = &stored
		}
		return *previous
	}

	var dayRecords, dailyForecast []dayRecord
	days, err := weather.days(ctx, startOfDay(now, a.zone).AddDate(0, 0, -dailyLookbackDays).Unix())
	if err != nil {
		log.Printf("read daily timeline, keeping the previous forecast: %v", err)
		dailyForecast = forecastDays(previousForecast().Daily, now, a.zone)
	} else {
		dayRecords = buildDays(days.Data, a.zone, now)
		if err := a.archiveDays(ctx, dayRecords); err != nil {
			return nil, fmt.Errorf("archive daily timeline: %w", err)
		}
		dailyForecast = forecastDays(dayRecords, now, a.zone)
	}

	var hourly []hourRecord
	hours, err := weather.hours(ctx, now.Unix())
	if err != nil {
		log.Printf("read hourly timeline, keeping the previous forecast: %v", err)
		hourly = upcomingHours(previousForecast().Hourly, now)
	} else {
		hourly = buildHours(hours.Data)
	}

	lookup, err := a.resolveAlerts(ctx, weather, present.Alerts, now)
	alerts := lookup.alerts
	if err != nil {
		// Alert text is the least important thing in the run and the only part
		// that can cost extra billed calls, so a failure is logged and the rest
		// of the snapshot is stored without it -- unless the stored snapshot
		// could not be read at all (keepStored), when nothing is stored.
		log.Printf("resolve alerts: %v", err)
	}
	snapshot := forecastSnapshot{
		UpdatedAt:      now.UTC(),
		Timezone:       current.Timezone,
		TimezoneOffset: current.TimezoneOffset,
		Hourly:         hourly,
		Daily:          dailyForecast,
		Alerts:         alerts,
		AlertsActive:   len(alerts) + lookup.unfetched,
		EndedAlertIDs:  lookup.ended,
	}
	// The history is for the dashboard's record of past warnings; losing one
	// run's entry costs a fifteen-minute gap at worst, not the reading.
	if err := a.archiveAlerts(ctx, snapshot.Alerts, now); err != nil {
		log.Printf("archive alerts: %v", err)
	}
	// keepStored: the stored snapshot could not be read even on a second try,
	// so this run's alert cache was built without it. Leave the stored one in
	// place and send no alert series rather than a count that may revive an
	// alert already known to be over.
	save := !lookup.keepStored
	snapshot.alertsUnknown = lookup.keepStored
	if !save {
		log.Printf("forecast snapshot left as it was and alert series not sent, since it could not be read this run")
	}
	hoursStored := 0
	if save {
		hoursStored = len(snapshot.Hourly)
		if err := a.saveForecast(ctx, snapshot); err != nil {
			return nil, fmt.Errorf("save forecast snapshot: %w", err)
		}
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

	// With the alert cache unreadable the count is unknown, so it is null
	// rather than a zero that reads like "no alerts".
	var alertsActive any = snapshot.activeAlerts()
	if snapshot.alertsUnknown {
		alertsActive = nil
	}
	return map[string]any{
		"location":       a.location,
		"observed_at":    reading.Time,
		"temp":           reading.Temp,
		"days_archived":  len(dayRecords),
		"hours_stored":   hoursStored,
		"alerts_active":  alertsActive,
		"series_pushed":  pushed,
		"api_calls_made": 3 + lookup.calls, // current + one day page + one hour page + alert texts
	}, nil
}

// alertLookup is what resolveAlerts found for the IDs /current reported.
type alertLookup struct {
	alerts []alertDetail // in force, with text
	// unfetched is how many IDs are presumably in force but have no text yet,
	// because their lookup failed or did not fit in the budget.
	unfetched int
	// ended lists IDs /current still reports that are confirmed over: fetched
	// text says so, or the API answered 404/410 for the ID. They are kept so
	// later runs do not count them and fetch them again only on the
	// three-hourly re-check runs (endedRecheckDue). A failed or skipped
	// re-check of a cached text past its end does not put an ID here; it is
	// counted in unfetched and tried again on the next run.
	ended []string
	calls int // /alert/{id} requests sent, failed ones included
	// keepStored is set when the stored snapshot could not be read for a
	// reason other than corruption. Its cached texts and ended IDs are still
	// there, so this run must not overwrite them with what it rebuilt.
	keepStored bool
}

// resolveAlerts turns the alert IDs embedded in the weather payload into text.
// 4.0 charges for /alert/{id} like any other call, so only these are fetched:
// IDs whose text is not in the stored snapshot, IDs whose stored text has
// passed its end (re-checked for an extension), and, on the three-hourly
// re-check runs (endedRecheckDue), IDs already confirmed over that /current
// still lists -- normally none.
func (a *app) resolveAlerts(ctx context.Context, weather *openWeather, ids []string, now time.Time) (alertLookup, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return alertLookup{}, nil
	}

	// A read that fails outright (a transient S3 error) is tried once more
	// before any lookup, so a passing glitch costs no billed calls. Neither
	// case below stops the run, which would leave the dashboard without
	// metrics for as long as the alert lasts. A corrupt snapshot is treated as
	// empty: its texts are fetched again within the budget and this run's
	// save, the only thing that rewrites it, replaces it. One that still
	// could not be read is intact in S3, so the run makes no lookups, does not
	// overwrite it and sends no alert series (keepStored), leaving the alerts
	// to the next run.
	stored, err := a.loadForecast(ctx)
	if err != nil && !errors.Is(err, errUndecodable) {
		log.Printf("read stored alerts, trying once more: %v", err)
		// Bounded, so a hanging retry cannot eat the time the rest of the
		// run (the month read, the push) needs.
		retryCtx, cancel := context.WithTimeout(ctx, snapshotRetryTimeout)
		stored, err = a.loadForecast(retryCtx)
		cancel()
	}
	if errors.Is(err, errUndecodable) {
		log.Printf("stored alerts are corrupt, fetching them again: %v", err)
		return lookupAlerts(ctx, weather, ids, forecastSnapshot{}, now)
	}
	if err != nil {
		// Nothing fetched now could be saved or sent (see keepStored), so no
		// lookups are made.
		return alertLookup{keepStored: true}, fmt.Errorf("read stored alerts: %w", err)
	}
	return lookupAlerts(ctx, weather, ids, stored, now)
}

// lookupAlerts is resolveAlerts after the snapshot has been read: it sorts
// the IDs into texts in force, ended IDs and IDs still without text, and
// fetches what it must within the budget.
func lookupAlerts(ctx context.Context, weather *openWeather, ids []string, stored forecastSnapshot, now time.Time) (alertLookup, error) {
	known := make(map[string]alertDetail, len(stored.Alerts))
	for _, alert := range stored.Alerts {
		known[alert.ID] = alert
	}
	wasEnded := make(map[string]bool, len(stored.EndedAlertIDs))
	for _, id := range stored.EndedAlertIDs {
		wasEnded[id] = true
	}

	// Unknown IDs are fetched one by one, each allowed the full HTTP timeout.
	// A day with many new alerts and a slow API could otherwise spend the
	// whole invocation here and never save the forecast or push the metrics,
	// so the lookups share one budget. An ID cut off by it is counted and
	// tried again on the next run.
	// The budget also ends early enough before the invocation's own deadline
	// to leave time for saving the snapshot and pushing to Grafana after it.
	budgetEnd := time.Now().Add(alertLookupBudget)
	if invocationEnd, ok := ctx.Deadline(); ok {
		if latest := invocationEnd.Add(-afterAlertsReserve); latest.Before(budgetEnd) {
			budgetEnd = latest
		}
	}
	alertCtx, cancel := context.WithDeadline(ctx, budgetEnd)
	defer cancel()

	var result alertLookup
	var resolved []alertDetail
	var unknown []string
	for _, id := range ids {
		// A cached text past its end is fetched again rather than taken as
		// over: the issuer may have extended the alert under the same ID.
		if alert, ok := known[id]; ok && !alertOver(alert, now) {
			resolved = append(resolved, alert)
			continue
		}
		// An ID confirmed over is not fetched again, except every three hours in
		// case the issuer extended it under the same ID afterwards.
		if wasEnded[id] && !endedRecheckDue(now) {
			result.ended = append(result.ended, id)
			continue
		}
		unknown = append(unknown, id)
	}

	// The API lists IDs in the same order every run, so IDs that keep timing
	// out at the front would use up the budget each time and starve the rest.
	// Rotating the starting ID (see rotationKey) gives every one turns at the
	// front over time without keeping any state between runs.
	var failures []string
	// A lookup only starts if it can wait out the full HTTP timeout before the
	// budget ends; one cut off part-way may still have been billed and would
	// leave nothing to store.
	order := rotateIDs(unknown, rotationKey(now))
	for i, id := range order {
		if time.Until(budgetEnd) < httpTimeout {
			// A re-check of an ID already confirmed over that does not
			// happen leaves it ended; anything else is counted and retried.
			for _, rest := range order[i:] {
				if wasEnded[rest] {
					result.ended = append(result.ended, rest)
				} else {
					result.unfetched++
				}
			}
			failures = append(failures, fmt.Sprintf("%s left for alert lookups, under the %s a request may take; %d alert(s) left for the next run", time.Until(budgetEnd).Round(time.Second), httpTimeout, len(order)-i))
			break
		}
		result.calls++
		alert, err := weather.alert(alertCtx, id)
		if err != nil {
			// A 404 or 410 confirms the alert is gone, so stop paying for it;
			// that is an answer, not a failure. Anything else (a timeout, a
			// 5xx) may pass, so the ID is counted and tried again on the next
			// run, except that a failed re-check of an ID already
			// confirmed over leaves it ended.
			if isGone(err) {
				result.ended = append(result.ended, id)
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
			if wasEnded[id] {
				result.ended = append(result.ended, id)
			} else {
				result.unfetched++
			}
			continue
		}
		if alertOver(alert, now) {
			result.ended = append(result.ended, id)
			continue
		}
		resolved = append(resolved, alert)
	}

	result.alerts = mergeAlerts(nil, resolved, now)
	sort.Strings(result.ended)

	if len(failures) > 0 {
		return result, errors.New(strings.Join(failures, "; "))
	}
	return result, nil
}

// endedRecheckSlots is how many fifteen-minute slots apart the re-checks of
// IDs already confirmed over are: every three hours. Each such ID costs a
// call per re-check while /current lists it, 8 a day per location, and the
// three locations' scheduled runs already use 864 of the 1,000 free calls; an
// hourly re-check (24 a day) would put six lingering IDs over the limit.
const endedRecheckSlots = 12

// endedRecheckDue reports whether this run is one of the runs, every three
// hours, that re-check IDs already confirmed over.
func endedRecheckDue(now time.Time) bool {
	return (now.Unix()/(15*60))%endedRecheckSlots == 0
}

// alertOver reports whether an alert's announced end has passed, the same
// test mergeAlerts uses to drop it.
func alertOver(alert alertDetail, now time.Time) bool {
	return alert.End > 0 && time.Unix(alert.End, 0).Before(now)
}

// uniqueIDs drops empty and repeated IDs, keeping the order they came in.
func uniqueIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; id == "" || dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}

// rotationKey picks where this run's lookups start. Re-check runs (every
// endedRecheckSlots-th slot, the only runs where ended IDs join the lookups)
// and ordinary runs each count up through consecutive integers of their own:
// the n-th re-check run gets n, the n-th ordinary run gets n. Using the raw
// slot instead would give either kind of run only some offsets -- multiples
// of gcd(endedRecheckSlots, len) on re-check runs, and never a multiple of
// endedRecheckSlots on ordinary ones -- and skip some IDs for good.
func rotationKey(now time.Time) int64 {
	slot := now.Unix() / (15 * 60)
	if endedRecheckDue(now) {
		return slot / endedRecheckSlots
	}
	return slot - slot/endedRecheckSlots - 1
}

// rotateIDs returns ids starting from an offset that advances with key.
func rotateIDs(ids []string, key int64) []string {
	if len(ids) < 2 {
		return ids
	}
	offset := int(key % int64(len(ids)))
	return append(append([]string{}, ids[offset:]...), ids[:offset]...)
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

	// The coordinates come from SSM rather than the environment for the same
	// reason kyuden-metrics keeps its 供給地点特定番号 there: not a credential,
	// but it is where someone lives, and a Lambda's environment variables are
	// readable by anyone who can describe the function.
	latParameter, err := a.locationParameterName("WEATHER_LATITUDE_PARAMETER_NAME")
	if err != nil {
		return nil, err
	}
	lat, err := a.parameterFloat(ctx, latParameter, 90)
	if err != nil {
		return nil, fmt.Errorf("read latitude parameter: %w", err)
	}
	lonParameter, err := a.locationParameterName("WEATHER_LONGITUDE_PARAMETER_NAME")
	if err != nil {
		return nil, err
	}
	lon, err := a.parameterFloat(ctx, lonParameter, 180)
	if err != nil {
		return nil, fmt.Errorf("read longitude parameter: %w", err)
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

// upcomingHours keeps the stored hourly forecast from the current hour on,
// for a run whose own hourly timeline failed.
func upcomingHours(hours []hourRecord, now time.Time) []hourRecord {
	from := now.Truncate(time.Hour).UnixMilli()
	var upcoming []hourRecord
	for _, hour := range hours {
		if hour.Time >= from {
			upcoming = append(upcoming, hour)
		}
	}
	return upcoming
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

// parameterFloat reads a coordinate and range-checks it, because the failure it
// guards against is not a crash. A parameter left at its Terraform placeholder,
// or holding a typo, would otherwise be read as 0 and the collector would
// cheerfully archive the weather in the Gulf of Guinea.
func (a *app) parameterFloat(ctx context.Context, parameterName string, limit float64) (float64, error) {
	raw, err := a.parameterString(ctx, parameterName)
	if err != nil {
		return 0, err
	}
	return a.parseCoordinate(raw, parameterName, limit)
}

// parseCoordinate is split out from the SSM read so the range check can be
// tested without a client. The value never appears in an error: keeping the
// location out of CloudWatch Logs is the whole reason it lives in SSM.
func (a *app) parseCoordinate(raw, parameterName string, limit float64) (float64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("%s does not hold a number", parameterName)
	}
	if math.Abs(value) > limit {
		return 0, fmt.Errorf("%s is outside +/-%g", parameterName, limit)
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
