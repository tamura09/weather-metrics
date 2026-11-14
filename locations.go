package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// One collect function serves every location. Each scheduled invocation names
// the location it is for in its event, so the locations are collected in
// separate invocations: one location's API failure, timeout or retry never
// costs another its reading.
//
// A location ID is used three ways, which is why it is kept to a short slug:
// it is the `location` label on every metric, the folder its archive lives in
// (ARCHIVE_PREFIX + id + "/"), and the {location} placeholder in the SSM
// parameter names its coordinates are read from.
//
// WEATHER_LOCATIONS unset keeps the original single-location behaviour --
// WEATHER_LOCATION label, archive at ARCHIVE_PREFIX itself, parameter names
// used as given -- so this code can be deployed before the configuration that
// turns it on.
var locationIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

const locationPlaceholder = "{location}"

// parseLocations reads WEATHER_LOCATIONS, a comma-separated list of location
// IDs. Empty means single-location mode.
func parseLocations(raw string) ([]string, error) {
	var locations []string
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if !locationIDPattern.MatchString(id) {
			return nil, fmt.Errorf("WEATHER_LOCATIONS: %q is not a valid location ID (lowercase letters, digits and hyphens, starting with a letter)", id)
		}
		if slices.Contains(locations, id) {
			return nil, fmt.Errorf("WEATHER_LOCATIONS: %q is listed twice", id)
		}
		locations = append(locations, id)
	}
	return locations, nil
}

// forLocation returns a copy of the app scoped to one location: its metric
// label and its archive folder. In single-location mode there is nothing to
// choose between, so a request that names a location is refused rather than
// silently answered with the only archive there is.
func (a *app) forLocation(requested string) (*app, error) {
	requested = strings.TrimSpace(requested)
	if len(a.locations) == 0 {
		if requested != "" {
			return nil, fmt.Errorf("location %q was requested, but WEATHER_LOCATIONS is not set", requested)
		}
		return a, nil
	}
	if requested == "" {
		return nil, fmt.Errorf("location is required; one of %s", strings.Join(a.locations, ", "))
	}
	if !slices.Contains(a.locations, requested) {
		return nil, fmt.Errorf("unknown location %q; one of %s", requested, strings.Join(a.locations, ", "))
	}

	scoped := *a
	scoped.location = requested
	scoped.prefix = a.prefix + requested + "/"
	return &scoped, nil
}

// locationParameterName resolves an SSM parameter name from the environment
// for the app's location. With several locations the name must contain the
// placeholder: without it every location would read the same coordinates and
// archive identical weather under three names, which nothing downstream would
// notice.
func (a *app) locationParameterName(envName string) (string, error) {
	name, err := requiredEnv(envName)
	if err != nil {
		return "", err
	}
	if len(a.locations) == 0 {
		return name, nil
	}
	if !strings.Contains(name, locationPlaceholder) {
		return "", errors.New(envName + " must contain " + locationPlaceholder + " when WEATHER_LOCATIONS is set")
	}
	return strings.ReplaceAll(name, locationPlaceholder, a.location), nil
}
