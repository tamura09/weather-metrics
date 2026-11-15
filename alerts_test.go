package main

import (
	"testing"
	"time"
)

func TestMergeAlertHistoryKeepsFirstSeen(t *testing.T) {
	first := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	later := first.Add(45 * time.Minute)

	stored := mergeAlertHistory(nil, []alertDetail{{ID: "a", Event: "大雨注意報"}}, first)
	stored = mergeAlertHistory(stored, []alertDetail{{ID: "a", Event: "大雨警報"}, {ID: "b", Event: "雷注意報"}}, later)

	if len(stored) != 2 {
		t.Fatalf("got %d records, want 2", len(stored))
	}
	a := stored[0]
	if a.ID != "a" || a.FirstSeen != first.Unix() || a.LastSeen != later.Unix() || a.Event != "大雨警報" {
		t.Errorf("a: got %+v; want first_seen kept, last_seen and text updated", a)
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
}

func TestAlertTimelineMergesOverlappingSpansPerEvent(t *testing.T) {
	rows := alertTimeline([]servedAlertRecord{
		{Event: "大雨注意報", ActiveFrom: 100, ActiveTo: 300},
		{Event: "大雨注意報", ActiveFrom: 200, ActiveTo: 400}, // reissued under a new ID
		{Event: "大雨注意報", ActiveFrom: 600, ActiveTo: 700},
		{Event: "雷注意報", ActiveFrom: 150, ActiveTo: 250},
	})

	want := []alertTransition{
		{100, "大雨注意報", "発表中"},
		{150, "雷注意報", "発表中"},
		{250, "雷注意報", ""},
		{400, "大雨注意報", ""},
		{600, "大雨注意報", "発表中"},
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
