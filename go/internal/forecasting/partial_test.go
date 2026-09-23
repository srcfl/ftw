package forecasting

import (
	"encoding/json"
	"testing"
	"time"
)

func TestIssueBoundsPartialPredictionSupport(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).UnixMilli()
	origin := start + 7*60000
	for _, tc := range []struct {
		name            string
		predictionStart int64
		valid           bool
	}{
		{"remainder", origin, true},
		{"legacy record", 0, true},
		{"negative", -1, false},
		{"before slot", start - 1, false},
		{"same as slot", start, false},
		{"before origin", origin - 1, false},
		{"at end", start + 900000, false},
		{"after end", start + 900001, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			point := testPoint(start, start+900000, 1000, 500)
			point.PredictionStartMS = tc.predictionStart
			issue := testIssue("i", "cfg", "champion", origin, origin, []Point{point})
			if err := issue.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			data, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Issue
			if err = json.Unmarshal(data, &decoded); err != nil || decoded.Series[0].Points[0].PredictionStartMS != tc.predictionStart {
				t.Fatal("partial provenance lost on archive roundtrip")
			}
		})
	}
}
