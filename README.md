# weather-metrics

Scheduled Lambda that reads OpenWeather's One Call API 4.0 and Air Pollution
API for each configured location, pushes the present conditions to Grafana Cloud as
Prometheus metrics, and archives observations and the daily timeline to S3. The
same artifact also deploys a second, read-only mode (`WEATHER_MODE=serve`) that
serves that archive over HTTP for Grafana's Infinity datasource.

## Why both a metric push and an S3 archive

Grafana Cloud's free tier keeps metrics for **14 days**, and its Prometheus
refuses samples that are not roughly current — the same
`err-mimir-sample-timestamp-too-old` wall `kyuden-metrics` ran into, and its
mirror image for samples in the future. That rules metrics out for two of the
things this dashboard is for:

| Data | Where it lives | Why |
| --- | --- | --- |
| Present conditions, derived indices | Prometheus remote_write | Always stamped *now*, so nothing is rejected. Drives the live panels and alerts. |
| Forecast (today's max/min, next days, next hours) | S3 → Infinity | A forecast's true timestamps are in the future, which remote_write will not accept. |
| History beyond two weeks | S3 → Infinity | Outlives the free tier's retention. |

Present conditions are pushed as metrics *and* archived, so the same number is
available live and years later.

## The call budget is the binding constraint

One Call 4.0 is on the "One Call by Call" subscription: **1,000 calls a day
free**, pay-per-call above it, and — unlike 3.0 — every page of a paginated
timeline is its own billed call. A scheduled run therefore makes exactly three:

| Call | Returns | Why one page is enough |
| --- | --- | --- |
| `/current` | 1 record | There is only one present. |
| `/timeline/1day?start=<2 days ago>` | 10 records | Reaches back far enough to re-read yesterday and forward eight days. |
| `/timeline/1h?start=<now>` | 20 records | Just under a day ahead, which is the horizon the hourly panels show. |

At a fifteen-minute schedule that is 288 calls a day per location, and 864 for
the three this collects (盛岡市, 宮古市, 江東区) against the free 1,000. Ten
minutes, the cadence it started with, would be 1,296 and spill into paid calls.
The Air Pollution API is still a 2.5 endpoint on the ordinary free plan, so its call
draws on a separate quota.

`/alert/{id}` is only called for alert IDs whose text is not already in the
stored snapshot, or whose cached text is past its end (to catch an
extension), and that are not already known to be over (`ended_alert_ids`,
kept while `/current` still lists them) -- except that IDs known to be over
are re-checked every three hours. On a day with no weather warnings
that is none of them. If an alert cannot be decoded it is never stored, so it
is fetched again on later runs while it is still in force; a `decode alert`
line in the logs means calls are being spent that way. All these lookups share
a three-minute budget per run, and the ID they start from rotates (by
fifteen-minute slot, and by re-check on the three-hourly re-check runs), so every ID
gets turns at the front over time but not necessarily on consecutive runs;
with several of them a given one is fetched only when its turn comes within
the budget. How an alert without text is counted and archived is described with
the archive layout under [The archive](#the-archive) below.

A failed run spends calls too: `/current` has already been paid for by the
time `/timeline/1day` fails. The daily timeline often takes more than 20
seconds to answer, so the HTTP timeout is 60 seconds, and the function is
invoked with no asynchronous retries
(`aws_lambda_function_event_invoke_config.weather_metrics` in
`tamura09/terraform`): a retry would pay for
`/current` again. Because `/timeline/1day` fails before the observation is
saved, that run's observation stays missing; the next scheduled run fifteen
minutes later picks up with the conditions at that time. (A run that fails
later, say on the Grafana push, has already archived its observation.)

For JMA warnings `/alert/{id}` returns an empty `event`, the description as a
list of translations (also empty), and English `tags` such as `Wind`. The kind
of warning is only in the ID, `VPWW54_<office>_...:<code>:<area>:...`, where
`<code>` is the VPWW54 warning code (`15` 強風注意報, `20` 濃霧注意報, ...), so
the event name is taken from that (`alert_detail.go`). An unknown code falls
back to `気象情報 (<tags>)`.

### `temp_min` / `temp_max` on the 2.5 current-weather endpoint are not this

Worth stating because it is a silent trap: `main.temp_min` and `main.temp_max`
on `/data/2.5/weather` are the spread across observation stations inside a city
area, not the day's high and low. For a single point they sit within a degree of
the current temperature, so a dashboard built on them shows three lines lying on
top of each other. The day's range comes from the daily timeline
(`temp.max` / `temp.min`), and what has actually been measured so far today is
computed from this repo's own archive.

## Two views of "today's maximum"

The dashboard shows the forecast maximum and the observed maximum as separate
series, on purpose. Before noon the observed maximum is not yet meaningful; by
evening the forecast is the less interesting of the two. Having both is what
makes either readable, and the gap between them is itself worth watching.

The same applies per date in the archive: `temp_max`/`temp_min` on a daily row
are the API's, and become history once the date settles, while
`observed_temp_max`/`observed_temp_min` are computed from the readings this
collector took: fifteen-minute for the last two weeks, hourly before that (see
Retention). A backfilled date has the former and not the latter --
`observations` on the row says which.

## Locations

One collect function serves every location. `WEATHER_LOCATIONS` lists the
location IDs, and each scheduled invocation names the one it is for in its event
(`{"location":"miyako"}`), so the locations are collected in separate
invocations and one location's failure never costs another its reading.

The ID is used three ways, which is why it is a short slug
(`^[a-z][a-z0-9-]{0,31}$`):

- the `location` label on every metric,
- the archive folder, `ARCHIVE_PREFIX` + ID + `/`,
- the `{location}` placeholder in `WEATHER_LATITUDE_PARAMETER_NAME` and
  `WEATHER_LONGITUDE_PARAMETER_NAME`. With several locations the placeholder is
  required, so they cannot all read the same coordinates by mistake.

Serve mode takes the same ID as `?location=` and refuses a request without one.

Leaving `WEATHER_LOCATIONS` unset keeps the original single-location behaviour:
the `WEATHER_LOCATION` label, the archive at `ARCHIVE_PREFIX` itself, and the
parameter names as given. In that mode no event or query may name a location.

### Moving a single-location archive to a location

Setting `WEATHER_LOCATIONS` moves every read and write to `ARCHIVE_PREFIX` +
ID + `/`, so the archive at `ARCHIVE_PREFIX` itself is no longer read by
either mode, and a dashboard query without `location=` gets a 400. To keep the
history, copy it under the location it was collected for once the new
configuration is live (so the last old-mode run is included), and before that
location's coordinates are filled in (so no run has written there yet):

```bash
aws s3 cp --recursive s3://<bucket>/weather/observations/ s3://<bucket>/weather/morioka/observations/
aws s3 cp --recursive s3://<bucket>/weather/daily/ s3://<bucket>/weather/morioka/daily/
aws s3 cp s3://<bucket>/weather/forecast.json s3://<bucket>/weather/morioka/forecast.json
aws s3 cp --recursive s3://<bucket>/weather/alerts/ s3://<bucket>/weather/morioka/alerts/
```

The dashboard's queries need `location=` added in the same change. Metrics move
from `location="home"` to the ID, so the Prometheus panels start over for the
14 days the old series is kept.

## Metrics

All gauges, all labelled `job="weather-metrics"` and `location=<location ID>`.

| Metric | Notes |
| --- | --- |
| `weather_temp_celsius`, `weather_feels_like_celsius`, `weather_dew_point_celsius` | |
| `weather_humidity_percent`, `weather_pressure_hpa`, `weather_clouds_percent`, `weather_visibility_meters` | |
| `weather_wind_speed_mps`, `weather_wind_gust_mps`, `weather_wind_degrees` | |
| `weather_wind_direction{compass}` | Constant 1; the 16-point name is the label. |
| `weather_uv_index`, `weather_rain_1h_mm`, `weather_snow_1h_mm` | |
| `weather_condition{condition,description,icon}` | Constant 1, for a state-timeline panel. |
| `weather_sunrise_timestamp_seconds`, `weather_sunset_timestamp_seconds`, `weather_daylight_seconds` | |
| `weather_forecast_temp_max_celsius`, `weather_forecast_temp_min_celsius` | Today, as of this run. |
| `weather_forecast_pop_ratio`, `weather_forecast_uv_index_max`, `weather_forecast_rain_mm`, `weather_forecast_wind_speed_mps` | |
| `weather_observed_temp_max_celsius`, `weather_observed_temp_min_celsius` | Measured since local midnight. |
| `weather_observed_wind_gust_max_mps`, `weather_observed_rain_today_mm` | |
| `weather_discomfort_index` | 不快指数. 70 comfortable, 75 muggy, 80+ uncomfortable. |
| `weather_wbgt_celsius` | 暑さ指数, estimated from temperature and humidity only. See below. |
| `weather_pressure_change_3h_hpa` | Absent, not zero, when the archive has no baseline near three hours back. |
| `weather_temp_change_24h_celsius` | Same-clock-time comparison against yesterday. |
| `weather_air_quality_index` | OpenWeather's own 1–5 scale, not the Japanese or US AQI. |
| `weather_air_pollutant_micrograms_per_cubic_meter{pollutant}` | `pm2_5`, `pm10`, `o3`, `no2`, `so2`, `co`, `nh3`, `no`. |
| `weather_alerts_active`, `weather_alert{event,sender}` | |

### The two computed indices

Neither is returned by the API; both are computed from temperature and humidity.

**不快指数 (THI)** — `0.81T + 0.01H(0.99T − 14.3) + 46.3`.

**暑さ指数 (WBGT)** — the Ono–Tonouchi regression for Japan,
`0.735T + 0.0374H + 0.00292TH − 4.064`. A real WBGT reading also needs solar
radiation and wind, so this is an estimate, and one fitted to warm conditions —
it is meaningful in summer and meaningless in winter. Japan's advisory
thresholds are 25 (警戒), 28 (厳重警戒) and 31 (危険).

`weather_pressure_change_3h_hpa` is the other one worth a panel: a sharp
three-hour drop is what people who get weather headaches want warning of, and
the API does not return a tendency.

## Configuration (environment variables)

| Name | Required | Description |
| --- | --- | --- |
| `ARCHIVE_BUCKET` | yes | S3 bucket for the archive. Required in both modes. |
| `ARCHIVE_PREFIX` | no | Key prefix, defaults to `weather/`. |
| `WEATHER_MODE` | no | `collect` (default) or `serve`. |
| `WEATHER_LATITUDE_PARAMETER_NAME`, `WEATHER_LONGITUDE_PARAMETER_NAME` | collect | SSM parameters holding the coordinates to read. In SSM rather than the environment for the same reason kyuden-metrics keeps its 供給地点特定番号 there -- not a credential, but a Lambda's environment variables are readable by anyone who can describe the function. Range-checked on read, so a parameter left at its Terraform placeholder fails loudly instead of collecting the weather at 0,0. |
| `WEATHER_LOCATIONS` | no | Comma-separated location IDs. See [Locations](#locations). Unset means single-location mode. |
| `WEATHER_LOCATION` | no | Label on every metric in single-location mode, defaults to `home`. |
| `WEATHER_TIMEZONE_OFFSET_SECONDS` | no | UTC offset the day boundaries are cut on, defaults to `32400` (JST). An offset rather than a zone name because the Lambda runtime ships no tzdata; exact for Japan, which has no DST. |
| `WEATHER_LANG` | no | Language for `description`, defaults to `ja`. |
| `OPENWEATHER_API_KEY_PARAMETER_NAME` | collect | SSM parameter holding the One Call by Call API key. |
| `GRAFANA_REMOTE_WRITE_URL_PARAMETER_NAME` | no | SSM parameter holding the remote_write URL. Leave unset to run archive-only. |
| `GRAFANA_PROMETHEUS_USERNAME_PARAMETER_NAME` | with the above | SSM parameter holding the remote_write basic-auth username. |
| `GRAFANA_PUSH_TOKEN_PARAMETER_NAME` | with the above | SSM parameter holding a `metrics:write`-scoped Grafana Cloud access policy token. |
| `API_TOKEN_PARAMETER_NAME` | serve | SSM parameter holding the shared secret the read API requires. |
| `OPENWEATHER_ONECALL_BASE_URL`, `OPENWEATHER_AIR_POLLUTION_BASE_URL` | no | Overrides for testing. |

## The archive

`<prefix>` below is `ARCHIVE_PREFIX` + location ID + `/`, e.g. `weather/miyako/`.

```text
<prefix>observations/2026-08.json   { "month": "2026-08", "readings": [ { "time": ..., "temp": ..., ... } ] }   fifteen-minute for two weeks, then hourly
<prefix>daily/2026.json             { "year": "2026", "days": [ { "date": "2026-08-08", "settled": false, "temp_max": ..., ... } ] }
<prefix>forecast.json               { "updated_at": ..., "hourly": [...], "daily": [...], "alerts": [...], "alerts_active": 2, "ended_alert_ids": [...] }
<prefix>alerts/2026.json            { "year": "2026", "alerts": [ { "id": ..., "event": ..., "start": ..., "first_seen": ..., "last_seen": ... } ] }
```

`alerts_active` in the snapshot is the number of alerts taken to be in force:
those in `alerts` plus any ID `/current` reported whose text could not be
fetched that run, so `weather_alerts_active` and the `?resource=summary` count
can exceed the rows of `?resource=alerts` and the `weather_alert` series. An ID
is left out of the count only when its end is confirmed: text fetched for it
says its announced end has passed, or the API answers 404/410 for it. Such IDs
go into `ended_alert_ids` while `/current` keeps listing
them, so they are not counted again and are fetched again only in the
three-hourly re-check below. An ID with no text at all is counted
even if it has in fact ended, since nothing says so yet. A cached text that has
passed its end is fetched again before the ID is taken as over, in case the
alert was extended; if that fetch fails (other than 404/410) or does not fit
the budget, the ID is counted and re-checked on the next run, at most one call
per run. Unfetched texts are tried again on a
later run, and such an alert is not written to `alerts/` until its text has
been fetched: its record starts, and `first_seen` is set, on the first run that
got the text, and an alert lifted before then never appears in the history.
An ID in `ended_alert_ids` is fetched again only every three hours (on the run
in the first fifteen-minute slot of 00:00, 03:00, ... UTC; a manual run in that
slot re-checks too) while `/current` lists it, so an alert extended under the
same ID after being found over is picked up by the first re-check that fetches
it successfully. A re-check that fails (other than 404/410) or does not fit the
budget leaves the ID ended until the next one. Each lingering ended ID costs 8
calls a day per location; with the scheduled runs at 864 of the 1,000 free
calls, about 17 such ID-days fit, and an hourly re-check (24 a day each) would
spill into paid calls with six. If the stored snapshot cannot be
read at all (a transient S3 error), the run reads it once more; if that fails
too, the run makes no alert lookups, does not write the snapshot, forecast
included, and sends no alert series, leaving the alerts to the next run. If it
is corrupt, the run treats it as empty, fetches the texts again within the
budget, and its save replaces it: `ended_alert_ids` starts over, and an ended alert whose fetch
fails is counted as active, and tried again on later runs, until a fetched
text shows it is over or `/current` drops it. Snapshots written
before `alerts_active` existed fall back to the length of `alerts`.

`alerts/` is the only record of past weather alerts: the snapshot holds just
the ones in force, and OpenWeather has no way to ask for old ones. Each run
that sees an alert updates its record by ID, under the year of its announced
start -- or, for an alert with no start, the year it was first seen, which a
run after New Year finds by checking the previous year's file. `first_seen`/`last_seen` are when this collector saw it; they matter
because a warning lifted early keeps its announced `end`, and only `last_seen`
shows that it was gone. The text is fetched once per ID and fetched
again only when the cached copy passes its end (to check for an extension), so
other revisions the issuer makes under the same ID are not followed until
then.

Observations are keyed on the minute they were taken, so a retry inside the same
minute corrects the stored copy instead of adding a near-duplicate point.

Daily records are keyed on the date, and a record that has settled is never
replaced by a forecast for the same date — which is what lets a backfill walk
forward past today without undoing history.

`forecast.json` is rewritten on every run, with the alert cache -- `alerts`
texts and `ended_alert_ids` -- read from the previous one and carried forward,
because re-fetching costs billed calls. If the snapshot cannot be read on
either of a run's two attempts, that run does not write it at all, so the
hourly and daily forecast stay as the previous run left them until the next
run.

## Retention: two weeks dense, hourly for good

Observations are kept at the collector's fifteen-minute resolution for two
weeks. After that they are thinned to one reading per clock hour -- the first
one in the hour, so the same hours `observed_hourly` groups by -- and the hourly
readings are kept for good. The kept reading carries the `rain_1h`/`snow_1h`
of its hour's wettest reading rather than its own, because a rolling one-hour
total taken at the top of the hour describes the hour before; that keeps the
observed precipitation for a day the same after thinning as before.

Each run thins the month it writes, the month the two-week line is in, and the
month before that. Anything older that is still dense -- the archive from
before this rule, or a month an outage of more than a month left behind -- is
thinned by the compact event, which makes no API calls and leaves the month a
scheduled run may be writing (the current one, and the previous one for the
first hours of a month) to those runs:

```bash
aws lambda invoke --region us-east-1 --function-name weather-metrics \
  --payload '{"location":"morioka","compact":true}' --cli-binary-format raw-in-base64-out \
  /dev/stdout
```

This is about the serve function, not storage. The raw observation endpoint
returns every reading in the window in one Lambda response, capped at 6MB
(about 440 bytes a reading as JSON): at fifteen minutes one location went over
it at about three months. With two dense weeks and hourly beyond, a year is
about 10,000 rows, 4.4MB as JSON and about 400KB gzipped (see Serve mode).

What it costs, for anything older than two weeks: the kept reading is the
snapshot at the top of the hour, so the temperature, humidity, pressure and
wind speed between two hours are gone. That shows in `observed_temp_max`/
`observed_temp_min` on a daily row (a peak between hours is missed) and in
`observed_hourly`, whose `temp_max`/`temp_min` for an hour become the one
value. What is kept from the whole hour: the precipitation (as above), the
dominant `condition`, and the strongest `wind_gust`. For a settled date the API's own
`temp_max`/`temp_min`/`rain` on the daily row are the better figures anyway.

## Rainfall is not a sum

`rain_1h` is a **rolling** one-hour total, so at a fifteen-minute cadence four
consecutive readings all describe overlapping windows of the same rain. Adding
them up (at the original ten-minute cadence) counted 2026-08-09 at 4.21mm against the 1.95mm its settled daily record
gives — more than double.

Every observed rainfall figure therefore takes the largest reading within each
clock hour and sums those. On a settled day that lands within a rounding error
of the API's own daily total (1.99mm against 1.95mm). It is still an estimate:
a rolling window straddles the hour boundary, so rain near the top of an hour
can be seen by two of them. For a date that has settled, the daily row's `rain`
is the authoritative figure; the observed one answers the different question of
how much has fallen *so far today*.

## Counting conditions

`monthly` buckets each day by OpenWeather's coarse `main` value rather than its
`description`. The description splits one rainy sky into 小雨 / 適度な雨 /
激しい雨 and would scatter a year across a dozen near-synonyms; `main` gives the
four that actually characterise a month here — Clear, Clouds, Rain, Snow.
Drizzle is counted as rain, and Thunderstorm, Mist, Fog and the dust family fall
into `other_days`, which keeps the stack at five series instead of twelve, most
of them empty. Every day lands in exactly one bucket, so the buckets sum to
`days` and a stacked bar reaches the same height as the month is long.

## Backfill

One Call 4.0's daily timeline reaches back decades on the same endpoint the
forecast comes from, so the dashboard does not have to start empty and grow:

```bash
aws lambda invoke --region us-east-1 --function-name weather-metrics \
  --cli-read-timeout 960 \
  --payload '{"location":"miyako","backfill_from":"2024-01-01","backfill_to":"2026-08-07"}' --cli-binary-format raw-in-base64-out \
  /dev/stdout
```

`location` is required when `WEATHER_LOCATIONS` is set and must be left out
when it is not.

It walks forward a page (10 days) at a time, flushing as it goes, and stops at
the page limit or before the invocation times out rather than losing work. The
response reports `completed_through`, so a long range is resumed by invoking
again from the next day. A year costs about 37 calls; the per-invocation page
limit is 60, so one accidental backfill cannot spend the whole daily allowance.

## Serve mode

The same artifact deployed with `WEATHER_MODE=serve` sits behind a Lambda
Function URL and returns the archive as JSON for the Grafana Infinity
datasource. It is a separate function so the read-only path never holds the
OpenWeather key or the Grafana push token.

Every query also takes `location=<location ID>`, which is required when
`WEATHER_LOCATIONS` is set.

| Query | Returns |
| --- | --- |
| `?from=<ms>&to=<ms>` | Observations in the range, with the derived indices already computed. Accepts epoch milliseconds (what Grafana's `${__from}` interpolates to) or RFC3339. Defaults to the last 30 days. |
| `?resource=summary` | One object (in a one-element array, like the others) with the present conditions, today's forecast and observed extremes, the trends, air quality, and `data_lag_seconds`. |
| `?resource=daily&from=<ms>&to=<ms>` | One row per date: the API's max/min plus our own observed max/min/mean and an `observations` count. Whole days, never cut by where the window lands. |
| `?resource=observed_hourly&from=<ms>&to=<ms>` | One row per clock hour of our **own** readings: average, high and low temperature, the hour's dominant condition, rainfall, and a `readings` count (4 on a complete hour within the last two weeks, 1 for older hours, which are thinned). Starts when the collector did, unlike the backfilled daily history. |
| `?resource=monthly&from=<ms>&to=<ms>` | One row per calendar month: how many days of each condition, average and peak temperatures, total rain and snow. Rolled up from the same daily rows the `daily` endpoint serves, so the two cannot disagree. |
| `?resource=forecast` | The stored daily forecast, today onward. |
| `?resource=hourly` | The stored hourly forecast, about a day ahead. |
| `?resource=alerts` | Active alerts, with epoch-millisecond start and end for a time axis. |
| `?resource=alert_history&from=<ms>&to=<ms>` | Every recorded alert whose span overlaps the window, newest first. `active_from` is the announced start, or `first_seen` if there is none or it is earlier. `active_to` is now while `ongoing`; once over, it is the announced end if that falls within one grace period (30 minutes) after `last_seen` -- the alert ran its course -- and otherwise `last_seen`, because it was lifted early. |
| `?resource=alert_timeline&from=<ms>&to=<ms>` | The same spans as `{time, event, state}` transitions, merged per event, for a state-timeline partitioned by `event`. `state` is the level (`特別警報` / `警報` / `注意報`, or `発表中` for a name outside the JMA scheme), or empty from the moment it was lifted. With nothing in the window it returns one empty row labelled `期間内の発表なし`, because Grafana's state-timeline reports a missing time field for no rows at all. |

A lag that keeps growing is the same signal a stalled collector would otherwise
need a separate success/failure metric for: nothing new lands in the archive,
whatever the reason.

Responses are gzipped when the request's `Accept-Encoding` allows it, which
Grafana's Infinity datasource does by default. That is what keeps a long
window well inside the 6MB Lambda response cap (JSON of this shape compresses
about 14x); a client that does not ask still gets plain JSON. Responses over
1MB before compression are logged with their size, to see how close the cap is.

Every request must carry the shared secret in an `x-weather-token` header; the
Function URL has no AWS auth of its own because Infinity cannot sign SigV4.

## Secrets

The SSM parameters are declared in `tamura09/terraform` but their values are
set outside Terraform, so they never land in state:

```bash
aws ssm put-parameter --region us-east-1 --name /weather-metrics/openweather-api-key --type SecureString --overwrite --value 'YOUR_API_KEY'
aws ssm put-parameter --region us-east-1 --name /weather-metrics/locations/miyako/latitude --type SecureString --overwrite --value '39.6414'
aws ssm put-parameter --region us-east-1 --name /weather-metrics/locations/miyako/longitude --type SecureString --overwrite --value '141.9571'
```

The API key must be on a **One Call by Call** subscription — a plain free-tier
key is rejected by `/data/4.0/onecall` even though it works on the 2.5
endpoints. Subscribing requires a card on file; the first 1,000 calls a day are
free, and an account-level call limit can be set in the OpenWeather console to
cap spend.

The function and its schedule are defined in Terraform in `tamura09/terraform`
(`modules/aws-us-east-1/lambda.tf`). The code is deployed by this repository's
Build: on push to `main` it uploads the artifact to S3 and runs
`aws lambda update-function-code`. Terraform does not track the artifact
version.
