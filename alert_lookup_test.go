package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// alertServer answers /alert/{id} from texts, and with 500 for any other ID.
// It records which IDs were asked for.
func alertServer(t *testing.T, texts map[string]alertDetail) (*openWeather, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		asked = append(asked, id)
		mu.Unlock()
		alert, ok := texts[id]
		if !ok {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"id":%q,"sender_name":"JMA","event":"強風注意報","start":%d,"end":%d,"description":"x"}`, alert.ID, alert.Start, alert.End)
	}))
	t.Cleanup(server.Close)
	client := &openWeather{client: server.Client(), apiKey: "k", oneCallBaseURL: server.URL, lang: "ja"}
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

func TestLookupAlertsSortsIDs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	past := now.Add(-time.Hour).Unix()
	future := now.Add(time.Hour).Unix()

	weather, asked := alertServer(t, map[string]alertDetail{
		"new":      {ID: "new", End: future},
		"extended": {ID: "extended", End: future},
		"over":     {ID: "over", End: past},
	})
	stored := forecastSnapshot{
		Alerts: []alertDetail{
			{ID: "cached", Event: "大雨注意報", End: future},
			{ID: "extended", End: past},      // re-checked, now runs on
			{ID: "stale-failing", End: past}, // re-check fails: cached end stands
		},
		EndedAlertIDs: []string{"known-ended"},
	}
	ids := []string{"cached", "new", "extended", "over", "stale-failing", "never-fetched", "known-ended"}

	got, err := lookupAlerts(context.Background(), weather, ids, stored, now)
	if err == nil {
		t.Fatal("want an error for the failed lookups")
	}

	var inForce []string
	for _, alert := range got.alerts {
		inForce = append(inForce, alert.ID)
	}
	if strings.Join(sorted(inForce), ",") != "cached,extended,new" {
		t.Errorf("in force = %v, want cached, extended, new", inForce)
	}
	if strings.Join(got.ended, ",") != "known-ended,over,stale-failing" {
		t.Errorf("ended = %v, want known-ended, over, stale-failing", got.ended)
	}
	if got.unfetched != 1 {
		t.Errorf("unfetched = %d, want 1 (never-fetched)", got.unfetched)
	}
	requested := asked()
	for _, id := range requested {
		if id == "cached" || id == "known-ended" {
			t.Errorf("fetched %s, which needed no call", id)
		}
	}
	if got.calls != len(requested) || got.calls != 5 {
		t.Errorf("calls = %d, server saw %v; want 5", got.calls, requested)
	}
}

func TestLookupAlertsOutOfBudgetSortsTheRest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	weather, asked := alertServer(t, map[string]alertDetail{})
	stored := forecastSnapshot{Alerts: []alertDetail{{ID: "stale", End: now.Add(-time.Hour).Unix()}}}

	// Too little of the invocation is left for even one lookup after the
	// reserve for saving and pushing.
	ctx, cancel := context.WithTimeout(context.Background(), afterAlertsReserve+httpTimeout/2)
	defer cancel()

	got, err := lookupAlerts(ctx, weather, []string{"stale", "fresh-a", "fresh-b"}, stored, now)
	if err == nil {
		t.Fatal("want an error saying the budget ran out")
	}
	if n := len(asked()); n != 0 || got.calls != 0 {
		t.Errorf("sent %d requests (calls = %d), want none", n, got.calls)
	}
	if strings.Join(got.ended, ",") != "stale" || got.unfetched != 2 || len(got.alerts) != 0 {
		t.Errorf("got ended=%v unfetched=%d alerts=%v; want ended=[stale] unfetched=2", got.ended, got.unfetched, got.alerts)
	}
}

func sorted(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func TestCarryAlertCacheKeepsWhatTheRunCouldNotSee(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(time.Hour).Unix()
	stored := forecastSnapshot{
		Alerts: []alertDetail{
			{ID: "unfetched", End: future},                 // run had no text: take the cached one
			{ID: "lifted", End: future},                    // /current no longer lists it
			{ID: "ended-now", End: future},                 // run found it over: run wins
			{ID: "stale", End: now.Add(-time.Hour).Unix()}, // past its end: not revived
		},
		EndedAlertIDs: []string{"ended-before", "ended-gone", "new"},
	}
	built := forecastSnapshot{
		Alerts:        []alertDetail{{ID: "new", End: future}},
		EndedAlertIDs: []string{"ended-now"},
	}
	ids := []string{"unfetched", "ended-now", "stale", "ended-before", "new", "no-text"}

	alerts, ended, active := carryAlertCache(stored, built, ids, now)
	var got []string
	for _, alert := range alerts {
		got = append(got, alert.ID)
	}
	if strings.Join(sorted(got), ",") != "new,unfetched" {
		t.Errorf("alerts = %v, want new, unfetched", got)
	}
	if strings.Join(ended, ",") != "ended-before,ended-now" {
		t.Errorf("ended = %v, want ended-before, ended-now", ended)
	}
	// new, unfetched with text; stale and no-text still without.
	if active != 4 {
		t.Errorf("active = %d, want 4", active)
	}
}
