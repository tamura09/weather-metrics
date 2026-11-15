package main

import (
	"testing"
	"time"
)

func TestMergeAlertHistoryKeepsFirstSeen(t *testing.T) {
	first := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	later := first.Add(45 * time.Minute)

	stored := mergeAlertHistory(nil, []alertDetail{{ID: "a", Event: "大雨注意報"}}, first)
	stored = mergeAlertHistory(stored, []alertDetail{{ID: "a", Event: "大雨注意報"}, {ID: "b", Event: "雷注意報"}}, later)

	if len(stored) != 2 {
		t.Fatalf("got %d records, want 2", len(stored))
	}
	a := stored[0]
	if a.ID != "a" || a.FirstSeen != first.Unix() || a.LastSeen != later.Unix() {
		t.Errorf("a: got %+v; want first_seen kept and last_seen moved on", a)
	}
	if b := stored[1]; b.FirstSeen != later.Unix() || b.LastSeen != later.Unix() {
		t.Errorf("b: got %+v", b)
	}
}

func TestPresentAlertRecordSpan(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	start := now.Add(-6 * time.Hour)

	// Lifted early: the announced end is still ahead, but no run has seen it
	// for an hour, so the span stops at the last sighting.
	lifted := presentAlertRecord(alertRecord{
		alertDetail: alertDetail{Event: "強風注意報", Start: start.Unix(), End: now.Add(6 * time.Hour).Unix()},
		FirstSeen:   start.Add(10 * time.Minute).Unix(),
		LastSeen:    now.Add(-time.Hour).Unix(),
	}, now)
	if lifted.Ongoing || lifted.State != "解除" || lifted.ActiveFrom != start.UnixMilli() || lifted.ActiveTo != now.Add(-time.Hour).UnixMilli() {
		t.Errorf("lifted: got %+v", lifted)
	}

	// Seen on the last run: still in force, so the span runs to now.
	ongoing := presentAlertRecord(alertRecord{
		alertDetail: alertDetail{Event: "大雪警報", Start: start.Unix()},
		FirstSeen:   start.Unix(),
		LastSeen:    now.Add(-15 * time.Minute).Unix(),
	}, now)
	if !ongoing.Ongoing || ongoing.State != "発表中" || ongoing.ActiveTo != now.UnixMilli() {
		t.Errorf("ongoing: got %+v", ongoing)
	}

	// Seen on the last run but past its announced end since: over, and the
	// span stops at the end rather than running on to now.
	end := now.Add(-5 * time.Minute)
	expired := presentAlertRecord(alertRecord{
		alertDetail: alertDetail{Event: "雷注意報", Start: start.Unix(), End: end.Unix()},
		FirstSeen:   start.Unix(),
		LastSeen:    now.Add(-15 * time.Minute).Unix(),
	}, now)
	if expired.Ongoing || expired.State != "解除" || expired.ActiveTo != end.UnixMilli() {
		t.Errorf("expired: got %+v", expired)
	}
	// ...and stays there however much later it is asked about.
	if later := presentAlertRecord(alertRecord{
		alertDetail: alertDetail{Event: "雷注意報", Start: start.Unix(), End: end.Unix()},
		FirstSeen:   start.Unix(),
		LastSeen:    now.Add(-15 * time.Minute).Unix(),
	}, now.Add(24*time.Hour)); later.ActiveTo != end.UnixMilli() {
		t.Errorf("expired, a day later: active_to %d, want %d", later.ActiveTo, end.UnixMilli())
	}
}

func TestAlertTimelineMergesOverlappingSpansPerEvent(t *testing.T) {
	rows := alertTimeline([]servedAlertRecord{
		{Event: "大雨注意報", ActiveFrom: 100, ActiveTo: 300},
		{Event: "大雨注意報", ActiveFrom: 200, ActiveTo: 400}, // reissued under a new ID
		{Event: "大雨注意報", ActiveFrom: 600, ActiveTo: 700},
		{Event: "雷注意報", ActiveFrom: 150, ActiveTo: 250},
	})

	want := []alertTransition{
		{100, "大雨注意報", "注意報"},
		{150, "雷注意報", "注意報"},
		{250, "雷注意報", ""},
		{400, "大雨注意報", ""},
		{600, "大雨注意報", "注意報"},
		{700, "大雨注意報", ""},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows %+v, want %d", len(rows), rows, len(want))
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestAlertLevel(t *testing.T) {
	for event, want := range map[string]string{
		"大雨特別警報": "特別警報",
		"暴風警報":   "警報",
		"乾燥注意報":  "注意報",
		"Flood":  "発表中",
	} {
		if got := alertLevel(event); got != want {
			t.Errorf("%s: got %s, want %s", event, got, want)
		}
	}
}

func TestAlertTimelinePlaceholderWhenEmpty(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rows := alertTimelineOrPlaceholder(alertTimeline(nil), from)
	if len(rows) != 1 || rows[0].Event != noAlertsEvent || rows[0].State != "" || rows[0].Time != from.UnixMilli() {
		t.Errorf("got %+v", rows)
	}
	busy := []alertTransition{{Time: 1, Event: "雷注意報", State: "注意報"}}
	if got := alertTimelineOrPlaceholder(busy, from); len(got) != 1 || got[0].Event != "雷注意報" {
		t.Errorf("non-empty timeline changed: %+v", got)
	}
}
