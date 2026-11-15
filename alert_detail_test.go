package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The shape One Call 4.0 actually returns for a JMA warning: no event, and the
// description as a list of translations. Until this decoded, every alert
// failed, nothing was ever archived, and every run paid to fetch them again.
func TestAlertDecodesTheJMAShape(t *testing.T) {
	const id = "VPWW54_JPDC_150757_02_202611150757400_001_95089:20:0320200:0:0:7c519c27660b4627275b0abbf876a135"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + id + `","sender_name":"JMA","event":"","start":1794722401,"end":1794798000,"description":[{"language":"ja-JP","description":""}],"tags":["Fog"]}`))
	}))
	defer server.Close()

	client := &openWeather{client: server.Client(), apiKey: "k", oneCallBaseURL: server.URL, lang: "ja"}
	alert, err := client.alert(context.Background(), id)
	if err != nil {
		t.Fatalf("alert: %v", err)
	}
	if alert.Event != "濃霧注意報" || alertLevel(alert.Event) != "注意報" {
		t.Errorf("event = %q (level %s), want 濃霧注意報", alert.Event, alertLevel(alert.Event))
	}
	if alert.Start != 1794722401 || alert.End != 1794798000 || len(alert.Tags) != 1 {
		t.Errorf("got %+v", alert)
	}
}

func TestAlertTextReadsBothShapes(t *testing.T) {
	for body, want := range map[string]string{
		`"大雨に警戒"`: "大雨に警戒",
		`[{"language":"en","description":"Heavy rain"},{"language":"ja-JP","description":"大雨"}]`: "大雨",
		`[{"language":"en","description":"Heavy rain"}]`:                                         "Heavy rain",
		`[{"language":"ja-JP","description":""}]`:                                                "",
		`[]`: "",
	} {
		var got alertText
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s: got %q, want %q", body, got, want)
		}
	}
	var bad alertText
	if err := json.Unmarshal([]byte(`{"x":1}`), &bad); err == nil {
		t.Error("an object decoded without error")
	}

	// Stored back as a plain string, which older snapshots also hold.
	encoded, _ := json.Marshal(alertDetail{ID: "a", Description: "大雨"})
	var back alertDetail
	if err := json.Unmarshal(encoded, &back); err != nil || back.Description != "大雨" {
		t.Errorf("round trip: %s -> %+v, %v", encoded, back, err)
	}
}

func TestAlertEventName(t *testing.T) {
	for _, c := range []struct {
		id   string
		tags []string
		want string
	}{
		{"VPWW54_JPTK_150758_02_202611150758060_001_95105:15:1310800:0:0:50e8", []string{"Wind"}, "強風注意報"},
		{"VPWW54_JPTK_150758_02_202611150758060_001_95105:16:1310800:0:0:4919", []string{"Coastal event", "Marine event"}, "波浪注意報"},
		{"VPWW54_JPTK_x:33:1310800:0:0:h", nil, "大雨特別警報"},
		{"VPWW54_JPTK_x:99:1310800:0:0:h", []string{"Wind"}, "気象情報 (Wind)"},
		{"other-issuer-1", nil, "気象情報"},
	} {
		if got := alertEventName(c.id, c.tags); got != c.want {
			t.Errorf("%s: got %q, want %q", c.id, got, c.want)
		}
	}
}
