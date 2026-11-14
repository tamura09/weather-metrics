package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestParseLocations(t *testing.T) {
	got, err := parseLocations(" morioka, miyako ,koto,")
	if err != nil {
		t.Fatalf("parseLocations: %v", err)
	}
	if strings.Join(got, ",") != "morioka,miyako,koto" {
		t.Errorf("got %v", got)
	}

	if got, err := parseLocations(""); err != nil || len(got) != 0 {
		t.Errorf("empty: got %v, %v; want single-location mode", got, err)
	}

	// The ID becomes an S3 folder and part of an SSM parameter name, so
	// anything that could escape either is refused at startup.
	for _, bad := range []string{"Morioka", "../home", "a/b", "morioka,morioka", "1st"} {
		if _, err := parseLocations(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestForLocationScopesLabelAndArchive(t *testing.T) {
	base := &app{prefix: "weather/", location: "home", locations: []string{"morioka", "miyako"}}

	scoped, err := base.forLocation("miyako")
	if err != nil {
		t.Fatalf("forLocation: %v", err)
	}
	if scoped.location != "miyako" || scoped.prefix != "weather/miyako/" {
		t.Errorf("got location %q prefix %q", scoped.location, scoped.prefix)
	}
	// A copy, so one invocation's scope cannot leak into the next.
	if base.location != "home" || base.prefix != "weather/" {
		t.Errorf("base app modified: location %q prefix %q", base.location, base.prefix)
	}

	for _, requested := range []string{"", "koto"} {
		if _, err := base.forLocation(requested); err == nil {
			t.Errorf("%q: accepted", requested)
		}
	}
}

func TestForLocationInSingleLocationMode(t *testing.T) {
	base := &app{prefix: "weather/", location: "home"}

	scoped, err := base.forLocation("")
	if err != nil || scoped.prefix != "weather/" || scoped.location != "home" {
		t.Errorf("got %+v, %v; want the app unchanged", scoped, err)
	}
	if _, err := base.forLocation("morioka"); err == nil {
		t.Error("a named location was accepted without WEATHER_LOCATIONS")
	}
}

func TestLocationParameterName(t *testing.T) {
	t.Setenv("WEATHER_LATITUDE_PARAMETER_NAME", "/weather-metrics/locations/{location}/latitude")
	scoped := &app{location: "koto", locations: []string{"koto"}}
	if got, err := scoped.locationParameterName("WEATHER_LATITUDE_PARAMETER_NAME"); err != nil || got != "/weather-metrics/locations/koto/latitude" {
		t.Errorf("got %q, %v", got, err)
	}

	// Without the placeholder every location would read the same coordinates.
	t.Setenv("WEATHER_LATITUDE_PARAMETER_NAME", "/weather-metrics/latitude")
	if _, err := scoped.locationParameterName("WEATHER_LATITUDE_PARAMETER_NAME"); err == nil {
		t.Error("a name without the placeholder was accepted with several locations")
	}

	single := &app{location: "home"}
	if got, err := single.locationParameterName("WEATHER_LATITUDE_PARAMETER_NAME"); err != nil || got != "/weather-metrics/latitude" {
		t.Errorf("single-location mode: got %q, %v", got, err)
	}
}

func TestServeRejectsAnUnknownLocation(t *testing.T) {
	t.Setenv("API_TOKEN_PARAMETER_NAME", "/weather-metrics/api-token")
	instance := &app{
		parameters: fakeParameters{"/weather-metrics/api-token": "secret"},
		bucket:     "archive",
		prefix:     "weather/",
		locations:  []string{"morioka"},
	}

	for _, query := range []map[string]string{nil, {"location": "koto"}} {
		response, err := instance.serve(context.Background(), events.LambdaFunctionURLRequest{
			Headers:               map[string]string{authHeader: "secret"},
			QueryStringParameters: query,
		})
		if err != nil {
			t.Fatalf("%v: serve: %v", query, err)
		}
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(response.Body, "location") {
			t.Errorf("%v: got %d %s, want 400 about the location", query, response.StatusCode, response.Body)
		}
	}
}
