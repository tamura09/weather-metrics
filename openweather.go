package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// One Call 4.0 replaced 3.0's single all-in-one document with one endpoint per
// timeline resolution, each returning a fixed-size page. That shape drives the
// call budget, which is the binding constraint here: the "One Call by Call"
// subscription gives 1,000 calls a day free and bills every page beyond it --
// including each `next` page of a paginated timeline. So a collect run makes
// exactly three calls (current, one page of days, one page of hours) and the
// day page is deliberately asked for starting two days back, because the same
// endpoint serves settled history and forecast: re-reading yesterday replaces
// the forecast we stored for it with what actually happened.
const (
	defaultOneCallBaseURL      = "https://api.openweathermap.org/data/4.0/onecall"
	defaultAirPollutionBaseURL = "https://api.openweathermap.org/data/2.5/air_pollution"
	userAgent                  = "weather-metrics-lambda"

	// Page sizes the API documents. They are not requestable -- they are what
	// one call returns -- so they only matter for working out how many calls a
	// span costs.
	daysPerPage  = 10
	hoursPerPage = 20
)

// weatherPoint is one record of any One Call 4.0 timeline. The endpoints share
// a field vocabulary and differ mostly in which fields are populated: `temp` is
// a bare number on the current and hourly timelines and an object on the daily
// one, so it is decoded into two fields and only one of them is ever set.
type weatherPoint struct {
	Dt         int64            `json:"dt"`
	Sunrise    int64            `json:"sunrise,omitempty"`
	Sunset     int64            `json:"sunset,omitempty"`
	Moonrise   int64            `json:"moonrise,omitempty"`
	Moonset    int64            `json:"moonset,omitempty"`
	MoonPhase  float64          `json:"moon_phase,omitempty"`
	Temp       scalarTemp       `json:"temp"`
	TempRange  *dailyTempRange  `json:"-"`
	FeelsLike  scalarTemp       `json:"feels_like"`
	Pressure   float64          `json:"pressure"`
	Humidity   float64          `json:"humidity"`
	DewPoint   float64          `json:"dew_point"`
	UVI        float64          `json:"uvi"`
	Clouds     float64          `json:"clouds"`
	Visibility float64          `json:"visibility"`
	WindSpeed  float64          `json:"wind_speed"`
	WindGust   float64          `json:"wind_gust"`
	WindDeg    float64          `json:"wind_deg"`
	Pop        float64          `json:"pop"`
	Rain       precipitation    `json:"rain"`
	Snow       precipitation    `json:"snow"`
	Weather    []weatherSummary `json:"weather"`
	// Alerts carries alert IDs only in 4.0; the text lives behind /alert/{id}.
	Alerts []string `json:"alerts"`
}

type weatherSummary struct {
	ID          int    `json:"id"`
	Main        string `json:"main"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
}

type dailyTempRange struct {
	Morn  float64 `json:"morn"`
	Day   float64 `json:"day"`
	Eve   float64 `json:"eve"`
	Night float64 `json:"night"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

// scalarTemp holds a temperature that the daily timeline reports as an object
// and every other timeline reports as a number. Both decode into Value; the
// object form additionally fills Range so daily min/max survive.
type scalarTemp struct {
	Value float64
	Range *dailyTempRange
}

func (t *scalarTemp) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "" {
		return nil
	}
	if trimmed[0] == '{' {
		var span dailyTempRange
		if err := json.Unmarshal(data, &span); err != nil {
			return err
		}
		t.Range = &span
		// `day` is the representative value for a whole day, so a caller that
		// only wants "the temperature" gets something sensible either way.
		t.Value = span.Day
		return nil
	}
	return json.Unmarshal(data, &t.Value)
}

// precipitation is millimetres of rain or snow. The daily timeline reports a
// bare total for the day; the current and hourly timelines wrap it as
// {"1h": n}. Absent means none fell, not unknown.
type precipitation struct {
	Millimetres float64
}

func (p *precipitation) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "" {
		return nil
	}
	if trimmed[0] == '{' {
		var buckets map[string]float64
		if err := json.Unmarshal(data, &buckets); err != nil {
			return err
		}
		// "1h" is the only bucket these endpoints emit, but taking the largest
		// of whatever is present avoids silently reading zero if that changes.
		for _, value := range buckets {
			if value > p.Millimetres {
				p.Millimetres = value
			}
		}
		return nil
	}
	return json.Unmarshal(data, &p.Millimetres)
}

func (p precipitation) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.Millimetres)
}

// timelineResponse is the envelope every 4.0 endpoint uses. Prev and Next are
// fully-formed URLs; they are recorded but not followed automatically, because
// each one is a billed call.
type timelineResponse struct {
	Lat            float64        `json:"lat"`
	Lon            float64        `json:"lon"`
	Timezone       string         `json:"timezone"`
	TimezoneOffset int            `json:"timezone_offset"`
	Data           []weatherPoint `json:"data"`
	Prev           string         `json:"prev"`
	Next           string         `json:"next"`
}

type alertDetail struct {
	ID          string `json:"id"`
	SenderName  string `json:"sender_name"`
	Event       string `json:"event"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	Description string `json:"description"`
}

type airPollution struct {
	List []struct {
		Dt   int64 `json:"dt"`
		Main struct {
			AQI float64 `json:"aqi"`
		} `json:"main"`
		Components map[string]float64 `json:"components"`
	} `json:"list"`
}

type openWeather struct {
	client              *http.Client
	apiKey              string
	oneCallBaseURL      string
	airPollutionBaseURL string
	lat                 float64
	lon                 float64
	lang                string
}

func (o *openWeather) coordinates(query url.Values) url.Values {
	query.Set("lat", strconv.FormatFloat(o.lat, 'f', -1, 64))
	query.Set("lon", strconv.FormatFloat(o.lon, 'f', -1, 64))
	query.Set("appid", o.apiKey)
	return query
}

// current is the single observation for right now.
func (o *openWeather) current(ctx context.Context) (timelineResponse, error) {
	query := o.coordinates(url.Values{})
	query.Set("units", "metric")
	query.Set("lang", o.lang)
	return o.timeline(ctx, o.oneCallBaseURL+"/current", query)
}

// days returns one page of the daily timeline -- daysPerPage records starting
// at start. Records before now are settled observations, records after it are
// forecasts, and the caller cannot tell them apart from the payload alone; the
// distinction is made by comparing dt to now.
func (o *openWeather) days(ctx context.Context, start int64) (timelineResponse, error) {
	query := o.coordinates(url.Values{})
	query.Set("units", "metric")
	query.Set("lang", o.lang)
	query.Set("start", strconv.FormatInt(start, 10))
	return o.timeline(ctx, o.oneCallBaseURL+"/timeline/1day", query)
}

// hours returns one page of the hourly timeline -- hoursPerPage records from
// start. One page is a little under a day ahead, which is the horizon the
// dashboard's hourly panels show; going further costs another call per page.
func (o *openWeather) hours(ctx context.Context, start int64) (timelineResponse, error) {
	query := o.coordinates(url.Values{})
	query.Set("units", "metric")
	query.Set("lang", o.lang)
	query.Set("start", strconv.FormatInt(start, 10))
	return o.timeline(ctx, o.oneCallBaseURL+"/timeline/1h", query)
}

func (o *openWeather) timeline(ctx context.Context, endpoint string, query url.Values) (timelineResponse, error) {
	body, err := o.get(ctx, endpoint, query)
	if err != nil {
		return timelineResponse{}, err
	}
	var out timelineResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return timelineResponse{}, fmt.Errorf("decode %s: %w (body: %s)", endpoint, err, truncate(body, 500))
	}
	for i := range out.Data {
		out.Data[i].TempRange = out.Data[i].Temp.Range
	}
	return out, nil
}

// alert fetches one alert's text. 4.0 embeds only IDs in the weather payloads,
// so this is called just for IDs the archive has not already stored -- which on
// an ordinary day is none of them.
func (o *openWeather) alert(ctx context.Context, id string) (alertDetail, error) {
	query := url.Values{}
	query.Set("appid", o.apiKey)
	query.Set("lang", o.lang)

	body, err := o.get(ctx, o.oneCallBaseURL+"/alert/"+url.PathEscape(id), query)
	if err != nil {
		return alertDetail{}, err
	}
	var out alertDetail
	if err := json.Unmarshal(body, &out); err != nil {
		return alertDetail{}, fmt.Errorf("decode alert %s: %w (body: %s)", id, err, truncate(body, 500))
	}
	if out.ID == "" {
		out.ID = id
	}
	return out, nil
}

// air reads the Air Pollution API, which is still a 2.5 endpoint on the ordinary
// free plan and so draws on a separate quota from One Call's.
func (o *openWeather) air(ctx context.Context) (airPollution, error) {
	body, err := o.get(ctx, o.airPollutionBaseURL, o.coordinates(url.Values{}))
	if err != nil {
		return airPollution{}, err
	}
	var out airPollution
	if err := json.Unmarshal(body, &out); err != nil {
		return airPollution{}, fmt.Errorf("decode air pollution: %w (body: %s)", err, truncate(body, 500))
	}
	return out, nil
}

func (o *openWeather) get(ctx context.Context, endpoint string, query url.Values) ([]byte, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	parsed.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The key and coordinates are in the query string, so the URL is not
		// safe to put in an error that lands in CloudWatch Logs.
		return nil, fmt.Errorf("OpenWeather %s returned %d: %s", parsed.Path, resp.StatusCode, truncate(body, 500))
	}
	return body, nil
}

func truncate(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
