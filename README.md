# weather-metrics

Scheduled Lambda that reads OpenWeather's One Call API 4.0 and Air Pollution
API for one location, pushes the present conditions to Grafana Cloud as
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

At a ten-minute schedule that is 432 calls a day against the free 1,000. The
Air Pollution API is still a 2.5 endpoint on the ordinary free plan, so its call
draws on a separate quota.

`/alert/{id}` is only called for alert IDs whose text is not already in the
stored snapshot, which on a day with no weather warnings is none of them.

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
`observed_temp_max`/`observed_temp_min` are computed from the ten-minute
readings this collector took. A backfilled date has the former and not the
latter — `observations` on the row says which.

## Metrics

All gauges, all labelled `job="weather-metrics"` and `location=$WEATHER_LOCATION`.

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
| `WEATHER_LOCATION` | no | Label on every metric, defaults to `home`. |
| `WEATHER_TIMEZONE_OFFSET_SECONDS` | no | UTC offset the day boundaries are cut on, defaults to `32400` (JST). An offset rather than a zone name because the Lambda runtime ships no tzdata; exact for Japan, which has no DST. |
| `WEATHER_LANG` | no | Language for `description`, defaults to `ja`. |
| `OPENWEATHER_API_KEY_PARAMETER_NAME` | collect | SSM parameter holding the One Call by Call API key. |
| `GRAFANA_REMOTE_WRITE_URL_PARAMETER_NAME` | no | SSM parameter holding the remote_write URL. Leave unset to run archive-only. |
| `GRAFANA_PROMETHEUS_USERNAME_PARAMETER_NAME` | with the above | SSM parameter holding the remote_write basic-auth username. |
| `GRAFANA_PUSH_TOKEN_PARAMETER_NAME` | with the above | SSM parameter holding a `metrics:write`-scoped Grafana Cloud access policy token. |
| `API_TOKEN_PARAMETER_NAME` | serve | SSM parameter holding the shared secret the read API requires. |
| `OPENWEATHER_ONECALL_BASE_URL`, `OPENWEATHER_AIR_POLLUTION_BASE_URL` | no | Overrides for testing. |

## The archive

```text
<prefix>observations/2026-08.json   { "month": "2026-08", "readings": [ { "time": ..., "temp": ..., ... } ] }
<prefix>daily/2026.json             { "year": "2026", "days": [ { "date": "2026-08-08", "settled": false, "temp_max": ..., ... } ] }
<prefix>forecast.json               { "updated_at": ..., "hourly": [...], "daily": [...], "alerts": [...] }
```

Observations are keyed on the minute they were taken, so a retry inside the same
minute corrects the stored copy instead of adding a near-duplicate point.

Daily records are keyed on the date, and a record that has settled is never
replaced by a forecast for the same date — which is what lets a backfill walk
forward past today without undoing history.

`forecast.json` is replaced whole on every run, except for alert text, which is
carried across runs because re-fetching it costs a billed call.

## Rainfall is not a sum

`rain_1h` is a **rolling** one-hour total, so at a ten-minute cadence six
consecutive readings all describe overlapping windows of the same rain. Adding
them up counted 2026-08-09 at 4.21mm against the 1.95mm its settled daily record
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
  --payload '{"backfill_from":"2024-01-01","backfill_to":"2026-08-07"}' --cli-binary-format raw-in-base64-out \
  /dev/stdout
```

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

| Query | Returns |
| --- | --- |
| `?from=<ms>&to=<ms>` | Observations in the range, with the derived indices already computed. Accepts epoch milliseconds (what Grafana's `${__from}` interpolates to) or RFC3339. Defaults to the last 30 days. |
| `?resource=summary` | One object (in a one-element array, like the others) with the present conditions, today's forecast and observed extremes, the trends, air quality, and `data_lag_seconds`. |
| `?resource=daily&from=<ms>&to=<ms>` | One row per date: the API's max/min plus our own observed max/min/mean and an `observations` count. Whole days, never cut by where the window lands. |
| `?resource=observed_hourly&from=<ms>&to=<ms>` | One row per clock hour of our **own** readings: average, high and low temperature, the hour's dominant condition, rainfall, and a `readings` count (6 on a complete hour). Starts when the collector did, unlike the backfilled daily history. |
| `?resource=monthly&from=<ms>&to=<ms>` | One row per calendar month: how many days of each condition, average and peak temperatures, total rain and snow. Rolled up from the same daily rows the `daily` endpoint serves, so the two cannot disagree. |
| `?resource=forecast` | The stored daily forecast, today onward. |
| `?resource=hourly` | The stored hourly forecast, about a day ahead. |
| `?resource=alerts` | Active alerts, with epoch-millisecond start and end for a time axis. |

A lag that keeps growing is the same signal a stalled collector would otherwise
need a separate success/failure metric for: nothing new lands in the archive,
whatever the reason.

Every request must carry the shared secret in an `x-weather-token` header; the
Function URL has no AWS auth of its own because Infinity cannot sign SigV4.

## Secrets

The SSM parameters are declared in `tamura09/aws-terraform` but their values are
set outside Terraform, so they never land in state:

```bash
aws ssm put-parameter --region us-east-1 --name /weather-metrics/openweather-api-key --type SecureString --overwrite --value 'YOUR_API_KEY'
aws ssm put-parameter --region us-east-1 --name /weather-metrics/latitude --type SecureString --overwrite --value '33.5904'
aws ssm put-parameter --region us-east-1 --name /weather-metrics/longitude --type SecureString --overwrite --value '130.4017'
```

The API key must be on a **One Call by Call** subscription — a plain free-tier
key is rejected by `/data/4.0/onecall` even though it works on the 2.5
endpoints. Subscribing requires a card on file; the first 1,000 calls a day are
free, and an account-level call limit can be set in the OpenWeather console to
cap spend.

Deployed and scheduled via Terraform in `tamura09/aws-terraform`
(`regions/us-east-1/lambda.tf`).
