package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The daily timeline reports temp as an object and every other timeline reports
// it as a number. Getting this wrong is silent -- a bare number decoded into a
// struct yields a zero max and min, which looks like a flat line rather than an
// error -- so both shapes are pinned here.
func TestScalarTempDecodesBothShapes(t *testing.T) {
	var daily weatherPoint
	decode(t, `{"dt":1,"temp":{"morn":18,"day":24,"eve":22,"night":19,"min":17.5,"max":26.5}}`, &daily)
	if daily.Temp.Range == nil {
		t.Fatal("daily temp object did not fill Range")
	}
	if daily.Temp.Range.Max != 26.5 || daily.Temp.Range.Min != 17.5 {
		t.Errorf("max/min = %v/%v, want 26.5/17.5", daily.Temp.Range.Max, daily.Temp.Range.Min)
	}
	if daily.Temp.Value != 24 {
		t.Errorf("representative value = %v, want the day figure 24", daily.Temp.Value)
	}

	var hourly weatherPoint
	decode(t, `{"dt":1,"temp":21.3}`, &hourly)
	if hourly.Temp.Value != 21.3 {
		t.Errorf("scalar temp = %v, want 21.3", hourly.Temp.Value)
	}
	if hourly.Temp.Range != nil {
		t.Error("scalar temp should not fill Range")
	}
}

func TestPrecipitationDecodesBothShapes(t *testing.T) {
	var hourly weatherPoint
	decode(t, `{"dt":1,"rain":{"1h":2.5}}`, &hourly)
	if hourly.Rain.Millimetres != 2.5 {
		t.Errorf("bucketed rain = %v, want 2.5", hourly.Rain.Millimetres)
	}

	var daily weatherPoint
	decode(t, `{"dt":1,"rain":7.25}`, &daily)
	if daily.Rain.Millimetres != 7.25 {
		t.Errorf("bare rain = %v, want 7.25", daily.Rain.Millimetres)
	}

	var dry weatherPoint
	decode(t, `{"dt":1}`, &dry)
	if dry.Rain.Millimetres != 0 || dry.Snow.Millimetres != 0 {
		t.Error("absent precipitation should read as zero")
	}
}

func TestTimelineRequestsCarryCoordinatesAndStart(t *testing.T) {
	var gotPath, gotStart, gotUnits string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotStart = r.URL.Query().Get("start")
		gotUnits = r.URL.Query().Get("units")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"timezone_offset":32400,"data":[{"dt":1754611200,"temp":{"min":24,"max":33}}],"next":"https://example.invalid/next"}`))
	}))
	defer server.Close()

	client := &openWeather{client: server.Client(), apiKey: "k", oneCallBaseURL: server.URL, lat: 33.59, lon: 130.4, lang: "ja"}
	page, err := client.days(context.Background(), 1754524800)
	if err != nil {
		t.Fatalf("days: %v", err)
	}

	if gotPath != "/timeline/1day" {
		t.Errorf("path = %q, want /timeline/1day", gotPath)
	}
	if gotStart != "1754524800" {
		t.Errorf("start = %q, want the unix second passed in", gotStart)
	}
	if gotUnits != "metric" {
		t.Errorf("units = %q, want metric -- the whole dashboard assumes celsius", gotUnits)
	}
	if len(page.Data) != 1 || page.Data[0].TempRange == nil || page.Data[0].TempRange.Max != 33 {
		t.Fatalf("daily record did not carry its temp range: %+v", page.Data)
	}
	if page.Next == "" {
		t.Error("pagination link was dropped")
	}
}

// A failed call must not put the query string in the error: the API key and the
// house's coordinates are both in it, and the error goes to CloudWatch Logs.
func TestRequestErrorsOmitTheQueryString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"cod":401,"message":"Invalid API key"}`))
	}))
	defer server.Close()

	client := &openWeather{client: server.Client(), apiKey: "super-secret-key", oneCallBaseURL: server.URL, lat: 33.59, lon: 130.4, lang: "ja"}
	_, err := client.current(context.Background())
	if err == nil {
		t.Fatal("want an error for a 401")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Errorf("error leaked the API key: %v", err)
	}
	if strings.Contains(err.Error(), "33.59") || strings.Contains(err.Error(), "130.4") {
		t.Errorf("error leaked the coordinates: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should still say what went wrong: %v", err)
	}
}

func TestAlertFillsIDWhenTheBodyOmitsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sender_name":"Japan Meteorological Agency","event":"大雨警報","start":1754611200,"end":1754654400,"description":"..."}`))
	}))
	defer server.Close()

	client := &openWeather{client: server.Client(), apiKey: "k", oneCallBaseURL: server.URL, lang: "ja"}
	alert, err := client.alert(context.Background(), "jma-1234")
	if err != nil {
		t.Fatalf("alert: %v", err)
	}
	if alert.ID != "jma-1234" {
		t.Errorf("id = %q, want the requested id echoed back so merging can key on it", alert.ID)
	}
	if alert.Event != "大雨警報" {
		t.Errorf("event = %q", alert.Event)
	}
}

func TestAirPollutionDecodesComponents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"list":[{"dt":1754611200,"main":{"aqi":2},"components":{"pm2_5":8.4,"pm10":14.1,"o3":61.5}}]}`))
	}))
	defer server.Close()

	client := &openWeather{client: server.Client(), apiKey: "k", airPollutionBaseURL: server.URL}
	air, err := client.air(context.Background())
	if err != nil {
		t.Fatalf("air: %v", err)
	}
	if len(air.List) != 1 || air.List[0].Main.AQI != 2 || air.List[0].Components["pm2_5"] != 8.4 {
		t.Fatalf("air pollution decoded wrong: %+v", air)
	}
}

func TestCurrentTimeoutIsRespected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	client := &openWeather{client: server.Client(), apiKey: "k", oneCallBaseURL: server.URL, lang: "ja"}
	if _, err := client.current(ctx); err == nil {
		t.Fatal("want an error when the context expires")
	}
}
