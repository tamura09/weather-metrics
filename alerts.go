package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// The forecast snapshot only ever holds the alerts in force right now, and
// OpenWeather offers no way to ask for past ones, so without a record of our
// own there is no answering "which warnings were out, and when". Every run
// that sees an alert folds it into alerts/YYYY.json, keyed by its ID, noting
// when this collector first and last saw it.
//
// first_seen/last_seen are what was observed; start/end are what the issuer
// announced. They differ in the case that matters most: a warning lifted
// before its announced end keeps that end in the API's record, and only
// last_seen shows it was gone by the next run.

const alertsPrefix = "alerts/"

// alertOngoingWithin is how recently an alert must have been seen to count as
// still in force. Two fifteen-minute runs: one missed run should not end it.
const alertOngoingWithin = 30 * time.Minute

type alertRecord struct {
	alertDetail
	FirstSeen int64 `json:"first_seen"` // epoch seconds
	LastSeen  int64 `json:"last_seen"`
}

type yearAlerts struct {
	Year   string        `json:"year"`
	Alerts []alertRecord `json:"alerts"`
}

func alertsKey(prefix, year string) string {
	return fmt.Sprintf("%s%s%s.json", prefix, alertsPrefix, year)
}

// alertYear files an alert under the year its announced start falls in, which
// unlike first_seen is known before the record is read, so a later run finds
// the record it is updating without searching.
func alertYear(alert alertDetail, zone *time.Location, now time.Time) string {
	if alert.Start > 0 {
		return strconv.Itoa(time.Unix(alert.Start, 0).In(zone).Year())
	}
	return strconv.Itoa(now.In(zone).Year())
}

// archiveAlerts records the alerts in force on this run. Nothing is written on
// a run without alerts, which is nearly all of them.
func (a *app) archiveAlerts(ctx context.Context, alerts []alertDetail, now time.Time) error {
	byYear := map[string][]alertDetail{}
	for _, alert := range alerts {
		if alert.ID == "" {
			continue
		}
		year := alertYear(alert, a.zone, now)
		byYear[year] = append(byYear[year], alert)
	}

	years := make([]string, 0, len(byYear))
	for year := range byYear {
		years = append(years, year)
	}
	sort.Strings(years)

	for _, year := range years {
		stored, err := a.loadAlertYear(ctx, year)
		if err != nil {
			return fmt.Errorf("read alerts %s: %w", year, err)
		}
		merged := mergeAlertHistory(stored.Alerts, byYear[year], now)
		if err := a.saveJSON(ctx, alertsKey(a.prefix, year), yearAlerts{Year: year, Alerts: merged}); err != nil {
			return fmt.Errorf("write alerts %s: %w", year, err)
		}
	}
	return nil
}

func (a *app) loadAlertYear(ctx context.Context, year string) (yearAlerts, error) {
	var archive yearAlerts
	found, err := a.loadJSON(ctx, alertsKey(a.prefix, year), &archive)
	if err != nil {
		return yearAlerts{}, err
	}
	if !found {
		return yearAlerts{Year: year}, nil
	}
	return archive, nil
}

// mergeAlertHistory updates the stored records with the alerts seen now. An
// alert seen again keeps its first_seen and takes the latest text, since the
// issuer can revise an alert under the same ID.
func mergeAlertHistory(stored []alertRecord, seen []alertDetail, now time.Time) []alertRecord {
	byID := make(map[string]alertRecord, len(stored)+len(seen))
	for _, record := range stored {
		byID[record.ID] = record
	}
	for _, alert := range seen {
		record, ok := byID[alert.ID]
		if !ok {
			record.FirstSeen = now.Unix()
		}
		record.alertDetail = alert
		record.LastSeen = now.Unix()
		byID[alert.ID] = record
	}

	merged := make([]alertRecord, 0, len(byID))
	for _, record := range byID {
		merged = append(merged, record)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].FirstSeen != merged[j].FirstSeen {
			return merged[i].FirstSeen < merged[j].FirstSeen
		}
		return merged[i].ID < merged[j].ID
	})
	return merged
}

// servedAlertRecord is one alert as it was observed, in epoch milliseconds.
// active_from/active_to is the span the dashboard draws: from the announced
// start (or when first seen, if the API gave none or gave a later one) to the
// last run that still saw it, or to now while it is still in force.
type servedAlertRecord struct {
	Event       string `json:"event"`
	SenderName  string `json:"sender_name"`
	Description string `json:"description"`
	ActiveFrom  int64  `json:"active_from"`
	ActiveTo    int64  `json:"active_to"`
	StartTime   int64  `json:"start_time"` // announced, epoch ms
	EndTime     int64  `json:"end_time"`
	Ongoing     bool   `json:"ongoing"`
	State       string `json:"state"` // 発表中 / 解除, for the table
}

func presentAlertRecord(record alertRecord, now time.Time) servedAlertRecord {
	from := record.FirstSeen
	if record.Start > 0 && record.Start < from {
		from = record.Start
	}
	ongoing := now.Sub(time.Unix(record.LastSeen, 0)) <= alertOngoingWithin
	to := time.Unix(record.LastSeen, 0)
	state := "解除"
	if ongoing {
		to = now
		state = "発表中"
	}
	return servedAlertRecord{
		Event:       record.Event,
		SenderName:  record.SenderName,
		Description: record.Description,
		ActiveFrom:  from * 1000,
		ActiveTo:    to.UnixMilli(),
		StartTime:   record.Start * 1000,
		EndTime:     record.End * 1000,
		Ongoing:     ongoing,
		State:       state,
	}
}

// servedAlertHistory returns the alerts whose observed span overlaps the
// window, newest first. The year before the window is read too, because an
// alert is filed under the year it started and can run past New Year.
func (a *app) servedAlertHistory(ctx context.Context, from, to, now time.Time) ([]servedAlertRecord, error) {
	served := []servedAlertRecord{}
	for _, year := range yearsBetween(from.AddDate(-1, 0, 0), to, a.zone) {
		archive, err := a.loadAlertYear(ctx, year)
		if err != nil {
			return nil, fmt.Errorf("alerts %s: %w", year, err)
		}
		for _, record := range archive.Alerts {
			alert := presentAlertRecord(record, now)
			if alert.ActiveTo < from.UnixMilli() || alert.ActiveFrom > to.UnixMilli() {
				continue
			}
			served = append(served, alert)
		}
	}
	sort.Slice(served, func(i, j int) bool { return served[i].ActiveFrom > served[j].ActiveFrom })
	return served, nil
}

// alertTransition is one point of the state-timeline: from Time onward, the
// alert named Event is State ("発表中", or "" once it has been lifted). The
// dashboard partitions the rows by event, giving one band per kind of alert.
type alertTransition struct {
	Time  int64  `json:"time"`
	Event string `json:"event"`
	State string `json:"state"`
}

// alertTimeline merges each event's spans -- the same warning reissued under a
// new ID, or two senders announcing it at once, would otherwise draw over each
// other -- and turns them into transitions.
func alertTimeline(alerts []servedAlertRecord) []alertTransition {
	type span struct{ from, to int64 }
	byEvent := map[string][]span{}
	for _, alert := range alerts {
		byEvent[alert.Event] = append(byEvent[alert.Event], span{alert.ActiveFrom, alert.ActiveTo})
	}

	events := make([]string, 0, len(byEvent))
	for event := range byEvent {
		events = append(events, event)
	}
	sort.Strings(events)

	rows := []alertTransition{}
	for _, event := range events {
		spans := byEvent[event]
		sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })

		merged := []span{spans[0]}
		for _, next := range spans[1:] {
			last := &merged[len(merged)-1]
			if next.from <= last.to {
				if next.to > last.to {
					last.to = next.to
				}
				continue
			}
			merged = append(merged, next)
		}
		for _, s := range merged {
			rows = append(rows,
				alertTransition{Time: s.from, Event: event, State: "発表中"},
				alertTransition{Time: s.to, Event: event, State: ""},
			)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Time < rows[j].Time })
	return rows
}
