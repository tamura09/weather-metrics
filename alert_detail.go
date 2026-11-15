package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// alertText is an alert's description. One Call 4.0's /alert/{id} returns it
// as a list of translations, [{"language":"ja-JP","description":"..."}],
// while the stored snapshots and history (and the 3.0 API) hold a plain
// string, so both are read and it is always written back as a string.
type alertText string

func (t *alertText) UnmarshalJSON(data []byte) error {
	var plain string
	if err := json.Unmarshal(data, &plain); err == nil {
		*t = alertText(plain)
		return nil
	}
	var translations []struct {
		Language    string `json:"language"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &translations); err != nil {
		return fmt.Errorf("alert description is neither a string nor a list of translations: %w", err)
	}
	*t = ""
	for _, translation := range translations {
		if translation.Description == "" {
			continue
		}
		// Japanese if there is one, since that is what the dashboard shows;
		// otherwise the first that says anything.
		if strings.HasPrefix(strings.ToLower(translation.Language), "ja") {
			*t = alertText(translation.Description)
			return nil
		}
		if *t == "" {
			*t = alertText(translation.Description)
		}
	}
	return nil
}

// jmaWarningNames are the 気象警報・注意報 codes of the JMA's VPWW54 bulletin.
// One Call 4.0 relays JMA warnings with an empty event and description, and
// the only place the kind of warning survives is the ID, which is built from
// the bulletin: "VPWW54_<office>_..._<serial>:<code>:<area>:...:<hash>".
var jmaWarningNames = map[string]string{
	"02": "暴風雪警報",
	"03": "大雨警報",
	"04": "洪水警報",
	"05": "暴風警報",
	"06": "大雪警報",
	"07": "波浪警報",
	"08": "高潮警報",
	"10": "大雨注意報",
	"12": "大雪注意報",
	"13": "風雪注意報",
	"14": "雷注意報",
	"15": "強風注意報",
	"16": "波浪注意報",
	"17": "融雪注意報",
	"18": "洪水注意報",
	"19": "高潮注意報",
	"20": "濃霧注意報",
	"21": "乾燥注意報",
	"22": "なだれ注意報",
	"23": "低温注意報",
	"24": "霜注意報",
	"25": "着氷注意報",
	"26": "着雪注意報",
	"32": "暴風雪特別警報",
	"33": "大雨特別警報",
	"35": "暴風特別警報",
	"36": "大雪特別警報",
	"37": "波浪特別警報",
	"38": "高潮特別警報",
}

// alertEventName is the name to file an alert under when the API gave none:
// the JMA warning its ID encodes, else its tags, so the timeline still has a
// row to put it on.
func alertEventName(id string, tags []string) string {
	if strings.HasPrefix(id, "VPWW54_") {
		if fields := strings.Split(id, ":"); len(fields) > 1 {
			if name, ok := jmaWarningNames[fields[1]]; ok {
				return name
			}
		}
	}
	if len(tags) > 0 {
		return "気象情報 (" + strings.Join(tags, ", ") + ")"
	}
	return "気象情報"
}
