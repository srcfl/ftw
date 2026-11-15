package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A tester asked why the battery emptied during the night. The support file
// covered the last hour and could not say. This seeds a week where, one
// night, the battery discharged 8 kWh while every plan slot asked it to
// hold, a charger lost contact and the fuse limited charging, and checks
// that the file answers from the box's own storage.
func TestSupportDumpAnswersForTheLastWeek(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	now := time.Now()
	y, m, d := now.AddDate(0, 0, -1).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.Local) // inside last night but one
	from, to := midnight.Add(-2*time.Hour), midnight.Add(6*time.Hour)

	var samples []state.Sample
	var history []state.HistoryPoint
	for ts := from; !ts.After(to); ts = ts.Add(time.Minute) {
		batW, soc := 0.0, 0.8
		switch {
		case !ts.Before(midnight) && ts.Before(midnight.Add(4*time.Hour)):
			batW = -2000
			soc = 0.8 - 0.5*ts.Sub(midnight).Hours()/4
		case !ts.Before(midnight.Add(4 * time.Hour)):
			soc = 0.3
		}
		ms := ts.UnixMilli()
		samples = append(samples,
			state.Sample{Driver: "pixii", Metric: "battery_w", TsMs: ms, Value: batW, Unit: "W"},
			state.Sample{Driver: "pixii", Metric: "battery_soc", TsMs: ms, Value: soc},
			state.Sample{Driver: "easee", Metric: "ev_w", TsMs: ms, Value: 0, Unit: "W"},
		)
		history = append(history, state.HistoryPoint{TsMs: ms, GridW: 400, BatW: batW, LoadW: 2400, BatSoC: soc, JSON: `{}`})
	}
	if err := st.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	if err := st.BulkRecordHistory(history); err != nil {
		t.Fatal(err)
	}

	// Every 15-minute slot that night asked the battery to hold.
	var spans []*state.ControlSpan
	for s := midnight; s.Before(midnight.Add(4 * time.Hour)); s = s.Add(15 * time.Minute) {
		first, _ := json.Marshal(map[string]any{"planned_wh": 0.0, "actual_wh": -500.0, "ratio": 0, "replans": 1})
		spans = append(spans, &state.ControlSpan{Subject: "slot:batteries", Kind: "slot", Status: "over_delivered",
			Severity: "info", StartMs: s.UnixMilli(), EndMs: s.Add(15 * time.Minute).UnixMilli(),
			UpdatedMs: s.Add(15 * time.Minute).UnixMilli(), Samples: 1, First: first, Last: first})
	}
	spans = append(spans,
		&state.ControlSpan{Subject: "source:easee", Driver: "easee", Kind: "source", Status: "offline",
			Reason: "telemetry_stale", Severity: "warning", StartMs: midnight.Add(time.Hour).UnixMilli(),
			EndMs: midnight.Add(3 * time.Hour).UnixMilli(), UpdatedMs: midnight.Add(3 * time.Hour).UnixMilli(), Samples: 1440},
		&state.ControlSpan{Subject: "ev:easee", Driver: "easee", Kind: "ev", Status: "limited",
			Reason: "fuse_limit", Severity: "info", StartMs: midnight.Add(-90 * time.Minute).UnixMilli(),
			EndMs: midnight.Add(-60 * time.Minute).UnixMilli(), UpdatedMs: midnight.Add(-60 * time.Minute).UnixMilli(), Samples: 360},
	)
	if err := st.SaveControlSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	for i, reason := range []string{"scheduled", "soc_drift", "soc_drift"} {
		js := `{"solver":{"engine":"energyplan","status":"optimal"}}`
		if i == 2 {
			js = `{"solver":{"engine":"core","backend":"dp","status":"fallback","fallback":true,"fallback_reason":"optimizer timed out"}}`
		}
		if err := st.SaveDiagnostic(midnight.Add(time.Duration(i)*time.Hour).UnixMilli(), reason, "SE3", 10, 96, js); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordNotification(state.NotificationEntry{TsMs: midnight.Add(time.Hour).UnixMilli(),
		EventType: "driver_offline", Driver: "easee", Title: "Charger offline", Body: "easee stopped answering", Status: "sent"}); err != nil {
		t.Fatal(err)
	}
	hour := midnight.Add(time.Hour).Truncate(time.Hour)
	if err := st.AddLogDigest(ctx, []state.LogDigest{{HourMs: hour.UnixMilli(), Level: "WARN", Driver: "easee",
		Pattern: "poll failed: HTTP #", Msg: "poll failed: HTTP 503", Count: 240,
		FirstMs: hour.UnixMilli(), LastMs: hour.Add(59 * time.Minute).UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordEvent("restart"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // events are keyed by the millisecond
	if err := st.RecordEvent("startup"); err != nil {
		t.Fatal(err)
	}

	srv := New(&Deps{
		Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{},
		Tel: telemetry.NewStore(), LogRing: telemetry.NewLogRing(), Version: "test", State: st,
	})
	files := readSupportZip(t, srv)

	for _, want := range []string{
		"week/summary.json", "week/series/pixii__battery_w.csv", "week/series/pixii__battery_soc.csv",
		"week/control-journal.json", "week/plans.json", "week/notifications.json", "week/events.json",
		"logs/warnings-week.log",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("archive is missing %s", want)
		}
	}
	var manifest struct {
		Contents []string `json:"contents"`
	}
	if err := json.Unmarshal([]byte(files["manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(manifest.Contents, " "), "week/series/<driver>__<metric>.csv") {
		t.Errorf("manifest does not list the week's series: %v", manifest.Contents)
	}

	// The series come from storage at five-minute resolution, far past the
	// last hour the old file stopped at.
	csv := strings.Split(strings.TrimSpace(files["week/series/pixii__battery_w.csv"]), "\n")
	if csv[0] != "ts_ms,v,min,max,n" {
		t.Errorf("series header = %q", csv[0])
	}
	if rows := len(csv) - 1; rows < 90 || rows > 100 {
		t.Errorf("eight hours of minute samples gave %d rows, want about 96 five-minute buckets", rows)
	}
	if !strings.Contains(files["week/series/pixii__battery_w.csv"], ",-2000,-2000,") {
		t.Error("series lost the discharge")
	}
	if !strings.Contains(files["week/control-journal.json"], "source:easee") {
		t.Error("control journal file lacks the charger's offline span")
	}
	if !strings.Contains(files["week/plans.json"], "optimizer timed out") {
		t.Error("plans file lacks the fallback reason")
	}
	if !strings.Contains(files["logs/warnings-week.log"], "poll failed: HTTP 503") || !strings.Contains(files["logs/warnings-week.log"], "×240") {
		t.Errorf("warnings-week.log = %q", files["logs/warnings-week.log"])
	}

	report := files["00-help-report.md"]
	week := sectionOf(report, "## What happened over the last week")
	if week == "" {
		t.Fatalf("report has no week section:\n%s", report)
	}
	for _, want := range []string{
		"discharged 8.00 kWh",   // the night's measured discharge
		"30–80%",                // its charge range
		"more than the plan",    // and that the plan did not ask for it
		"easee",                 // the charger that went offline
		"offline for 2.0 h",     // and for how long
		"fuse",                  // the fuse limit on charging
		"Core's own planner",    // the fallback plan
		"optimizer timed out",   // with its reason
		"restart",               // the box restarted
		"poll failed: HTTP 503", // the week's most repeated warning
		"| Period |",            // and the table behind the bullets
	} {
		if !strings.Contains(week, want) {
			t.Errorf("week section lacks %q:\n%s", want, week)
		}
	}
	if strings.Index(report, "## What happened over the last week") > strings.Index(report, "## Right now") {
		t.Error("the week section belongs before Right now, next to Findings")
	}
}

// Periods split the week at 06:00 and 22:00 household time, so a question
// about "last night" lands in one row. The window opens at the first
// boundary inside the last seven days and ends now.
func TestSupportWeekPeriods(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skip("no zone database")
	}
	now := time.Date(2026, 11, 15, 12, 54, 0, 0, loc)
	ps := supportWeekPeriods(now)
	if len(ps) != 14 {
		t.Fatalf("got %d periods, want 7 nights and 7 days", len(ps))
	}
	if !ps[0].Start.Equal(time.Date(2026, 11, 8, 22, 0, 0, 0, loc)) || !ps[0].Night {
		t.Errorf("first period = %+v, want the night from 8 Nov 22:00", ps[0])
	}
	last := ps[len(ps)-1]
	if last.Night || !last.Start.Equal(time.Date(2026, 11, 15, 6, 0, 0, 0, loc)) || !last.End.Equal(now) || !last.Partial {
		t.Errorf("last period = %+v, want today's day so far", last)
	}
	for i := 1; i < len(ps); i++ {
		if !ps[i].Start.Equal(ps[i-1].End) {
			t.Fatalf("gap between %v and %v", ps[i-1].End, ps[i].Start)
		}
	}
	if got := ps[0].Label(); got != "Sun 8 → Mon 9 Nov, night" {
		t.Errorf("night label = %q", got)
	}
	if got := ps[1].Label(); got != "Mon 9 Nov, day" {
		t.Errorf("day label = %q", got)
	}

	// The autumn clock change makes one night nine hours long.
	dst := supportWeekPeriods(time.Date(2026, 10, 27, 12, 0, 0, 0, loc))
	for _, p := range dst {
		if p.Night && p.Start.Day() == 24 {
			if h := p.End.Sub(p.Start).Hours(); h != 9 {
				t.Errorf("night over the clock change is %.1f h, want 9", h)
			}
		}
	}
}

func sectionOf(report, heading string) string {
	i := strings.Index(report, heading)
	if i < 0 {
		return ""
	}
	rest := report[i+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// A night that discharged as planned is still listed, because it is what
// people ask about; a start with no stop before it is a crash.
func TestWeekFindingsNightDischargeAndCrash(t *testing.T) {
	loc := time.FixedZone("test", 3600)
	start := time.Date(2026, 11, 13, 22, 0, 0, 0, loc)
	night := supportPeriod{Start: start, End: start.Add(8 * time.Hour), Night: true}
	out, soc := 3100.0, 20.0
	full := 90.0
	w := &supportWeek{
		Since: start.Add(-24 * time.Hour), Until: start.Add(24 * time.Hour),
		Periods: []supportPeriodSummary{{
			period: night, Label: night.Label(), Night: true, BatDischargedWh: &out,
			SoCMinPct: &soc, SoCMaxPct: &full, SlotsScored: 32, PlannedOutWh: 3000, ActualOutWh: 3100,
			Starts: 2, UncleanStarts: 1,
		}},
		Events: []state.Event{
			{TsMs: start.Add(time.Hour).UnixMilli(), Event: "startup"},
			{TsMs: start.Add(2 * time.Hour).UnixMilli(), Event: "startup"},
		},
	}
	var text strings.Builder
	writeFindingList(&text, weekFindings(w))
	got := text.String()
	for _, want := range []string{
		"NOTE — Night discharge.", "3.10 kWh (charge 20–90%), plan 3.00 kWh",
		"WARNING — Core stopped without a clean shutdown.", "Core started 2 times",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("findings lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "did not follow the plan") {
		t.Errorf("a night within 1 kWh of its plan is not a miss:\n%s", got)
	}
}

// A box installed today has one start and an hour of data. Its first start
// had no stop before it because there was no run before it, and the empty
// days before the install fold into one row.
func TestSupportWeekOnANewBox(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.RecordEvent("startup"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var samples []state.Sample
	for ts := now.Add(-time.Hour); ts.Before(now); ts = ts.Add(time.Minute) {
		samples = append(samples, state.Sample{Driver: "sungrow", Metric: "battery_soc", TsMs: ts.UnixMilli(), Value: 0.5})
	}
	if err := st.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{
		Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{},
		Tel: telemetry.NewStore(), LogRing: telemetry.NewLogRing(), Version: "test", State: st,
	})
	week := sectionOf(srv.buildSupportReport(context.Background(), now), "## What happened over the last week")
	if strings.Contains(week, "clean shutdown") {
		t.Errorf("a new box's first start is not a crash:\n%s", week)
	}
	if !strings.Contains(week, "no stored data") || !strings.Contains(week, "The stored week starts late") {
		t.Errorf("the empty days before the install should fold into one row:\n%s", week)
	}
	if rows := strings.Count(week, "\n| "); rows > 4 {
		t.Errorf("week table has %d rows for an hour of data:\n%s", rows, week)
	}
}
