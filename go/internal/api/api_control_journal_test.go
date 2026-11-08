package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/journal"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The journal samples what the app shows: each controlled device's verdict,
// whether the site may dispatch (the app's "FTW is not in control"), the
// mode, and each source that has gone quiet (the app's "is not reporting").
func TestJournalVerdictsCoverDevicesSiteAndSources(t *testing.T) {
	tel := telemetry.NewStore()
	soc := 0.2
	tel.Update("sungrow", telemetry.DerMeter, 4240, nil, nil)
	tel.Update("sungrow", telemetry.DerBattery, 1250, &soc, nil)
	tel.RecordDriverSuccess("sungrow")
	tel.RecordDriverSuccess("myuplink")
	tel.DriverHealthMut("myuplink").Status = telemetry.StatusOffline
	ctrl := control.NewState(0, 42, "sungrow")
	ctrl.Mode = control.ModePlannerArbitrage
	ctrl.LastTargets = []control.DispatchTarget{{Driver: "sungrow", TargetW: 4250}}
	srv := New(&Deps{Tel: tel, Ctrl: ctrl, CtrlMu: &sync.Mutex{}, SiteDispatchBlocked: func() string { return "site_meter_stale" }})

	got := map[string]journal.Verdict{}
	for _, v := range srv.JournalVerdicts(time.Now()) {
		got[v.Subject] = v
	}
	if v, ok := got["battery:sungrow"]; !ok || v.Status == "" || len(v.Detail) == 0 {
		t.Fatalf("battery verdict: %+v", v)
	}
	if v := got["site:dispatch"]; v.Status != "blocked" || v.Reason != "site_meter_stale" || v.Severity != "warning" {
		t.Fatalf("dispatch verdict: %+v", v)
	}
	if v := got["site:mode"]; v.Status != "planner_arbitrage" {
		t.Fatalf("mode verdict: %+v", v)
	}
	if v := got["source:myuplink"]; v.Status != "offline" || v.Severity != "warning" {
		t.Fatalf("quiet source: %+v", v)
	}
	if v := got["source:sungrow"]; v.Status != "live" || v.Severity != "info" {
		t.Fatalf("live source: %+v", v)
	}
}

func TestControlJournalAnswersAWindow(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	t0 := time.Date(2026, 11, 8, 6, 0, 0, 0, time.Local).UnixMilli()
	minute := int64(60_000)
	limited := &state.ControlSpan{Subject: "battery:sungrow", Driver: "sungrow", Kind: "battery", Status: "limited",
		Reason: "power_below_target", Severity: "warning", StartMs: t0 + 14*minute, EndMs: t0 + 30*minute, UpdatedMs: t0 + 30*minute, Samples: 192,
		First: json.RawMessage(`{"requested_w":4298,"actual_w":1298}`)}
	for _, w := range []float64{4298, 4250, 4162} {
		limited.Requested.Add(&w)
	}
	actual := 1250.0
	limited.Actual.Add(&actual)
	following := &state.ControlSpan{Subject: "battery:sungrow", Driver: "sungrow", Kind: "battery", Status: "following",
		Reason: "idle", Severity: "info", StartMs: t0 + 30*minute, UpdatedMs: t0 + 40*minute, Samples: 120}
	if err := st.SaveControlSpans(context.Background(), []*state.ControlSpan{limited, following}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordNotification(state.NotificationEntry{TsMs: t0 + 20*minute, EventType: "control.not_following", Status: "sent"}); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{State: st})

	get := func(query string) (map[string]json.RawMessage, int) {
		rec := httptest.NewRecorder()
		srv.handleControlJournal(rec, httptest.NewRequest(http.MethodGet, "/api/control/journal?"+query, nil))
		var out map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out, rec.Code
	}
	out, code := get("since=" + msText(t0+15*minute) + "&until=" + msText(t0+20*minute) + "&severity=warning")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var spans []struct {
		Status     string                  `json:"status"`
		EndMs      *int64                  `json:"end_ms"`
		DurationS  float64                 `json:"duration_s"`
		RequestedW *struct{ Mean float64 } `json:"requested_w"`
		ActualW    *struct{ Max float64 }  `json:"actual_w"`
		First      map[string]float64      `json:"first"`
	}
	_ = json.Unmarshal(out["spans"], &spans)
	if len(spans) != 1 || spans[0].Status != "limited" || spans[0].EndMs == nil || spans[0].DurationS != 960 {
		t.Fatalf("spans: %s", out["spans"])
	}
	if spans[0].RequestedW == nil || spans[0].RequestedW.Mean < 4236 || spans[0].RequestedW.Mean > 4237 || spans[0].ActualW.Max != 1250 || spans[0].First["requested_w"] != 4298 {
		t.Fatalf("numbers: %s", out["spans"])
	}
	var notices []state.NotificationEntry
	_ = json.Unmarshal(out["notifications"], &notices)
	if len(notices) != 1 || notices[0].EventType != "control.not_following" {
		t.Fatalf("notifications: %s", out["notifications"])
	}

	// The open span runs to now and says so with a null end.
	out, _ = get("since=" + msText(t0+35*minute) + "&until=" + msText(t0+36*minute))
	_ = json.Unmarshal(out["spans"], &spans)
	if len(spans) != 1 || spans[0].Status != "following" || spans[0].EndMs != nil {
		t.Fatalf("open span: %s", out["spans"])
	}
	if _, code := get("since=10&until=5"); code != http.StatusBadRequest {
		t.Fatalf("reversed window: status %d", code)
	}
}

func msText(v int64) string { return strconv.FormatInt(v, 10) }
