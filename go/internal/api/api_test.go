package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

func TestParseRangeSupports48h(t *testing.T) {
	const want = 48 * 60 * 60 * 1000
	if got := parseRange("48h"); got != want {
		t.Fatalf("parseRange(48h) = %d, want %d", got, want)
	}
}

// handleEVCommand rejects anything not in the allowlist with 400. The Lua
// driver's command hook silently returns nil for unknown actions, so
// without this gate the API would 200-OK typos.
func TestHandleEVCommandRejectsUnknownActions(t *testing.T) {
	srv := New(&Deps{}) // registry/tel unset — the 400 branches run first

	cases := []struct {
		name string
		body string
		want int
	}{
		{"unknown action", `{"action":"ev_nuke"}`, http.StatusBadRequest},
		{"empty action", `{"action":""}`, http.StatusBadRequest},
		{"missing action field", `{}`, http.StatusBadRequest},
		{"malformed json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/ev/command", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("body=%s: got status %d, want %d (body: %s)", tc.body, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// Allowlisted actions pass the action-validation gate. With a nil
// Registry they then short-circuit to 503, which confirms they got past
// the 400 branch without us needing a real driver registry.
func TestHandleEVCommandAcceptsAllowlistedActions(t *testing.T) {
	srv := New(&Deps{})

	for action := range validEVActions {
		t.Run(action, func(t *testing.T) {
			body := `{"action":"` + action + `"}`
			req := httptest.NewRequest(http.MethodPost, "/api/ev/command", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("action=%q: got status %d, want 503 (body: %s)", action, rr.Code, rr.Body.String())
			}
		})
	}
}

// Unknown driver in the JSON body must 404 before we touch the registry —
// otherwise the UI would silently fan a command to whichever charger
// happened to be first in the readings slice (the pre-fix behavior).
func TestHandleEVCommandRejectsUnknownDriver(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("easee-1", telemetry.DerEV, 0, nil, nil)
	srv := New(&Deps{Tel: tel}) // Registry nil — known driver would 503

	req := httptest.NewRequest(http.MethodPost, "/api/ev/command",
		strings.NewReader(`{"action":"ev_start","driver":"does-not-exist"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown driver: got %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
}

// Unknown driver on GET /api/ev/status must 404 — multi-EV UI needs to
// surface the mismatch rather than silently fall back to readings[0].
func TestHandleEVStatusRejectsUnknownDriver(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("easee-1", telemetry.DerEV, 0, nil, nil)
	srv := New(&Deps{Tel: tel})

	req := httptest.NewRequest(http.MethodGet, "/api/ev/status?driver=does-not-exist", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown driver: got %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestHandlePostConfigDoesNotPersistEVPasswordOnInvalidConfig(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig(evPasswordKey, "old-secret"); err != nil {
		t.Fatalf("seed password: %v", err)
	}

	cfg := &config.Config{
		Site:      config.Site{SmoothingAlpha: 0.3},
		Fuse:      config.Fuse{MaxAmps: 16},
		EVCharger: &config.EVCharger{Provider: "easee", Username: "old@example.com", Password: "old-secret"},
	}
	srv := New(&Deps{
		State:  st,
		CfgMu:  &sync.RWMutex{},
		Cfg:    cfg,
		CtrlMu: &sync.Mutex{},
		Ctrl:   control.NewState(0, 50, ""),
	})

	body, err := json.Marshal(config.Config{
		Site:      config.Site{SmoothingAlpha: 2},
		Fuse:      config.Fuse{MaxAmps: 16},
		EVCharger: &config.EVCharger{Provider: "easee", Username: "new@example.com", Password: "new-secret"},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	got, ok := st.LoadConfig(evPasswordKey)
	if !ok {
		t.Fatalf("password key missing after invalid config")
	}
	if got != "old-secret" {
		t.Fatalf("persisted password = %q, want old-secret", got)
	}
}

func TestHandleStatusIgnoresOfflineDERInLiveBalance(t *testing.T) {
	tel := telemetry.NewStore()
	ctrl := &control.State{SiteMeterDriver: "site"}

	tel.Update("site", telemetry.DerMeter, 8000, nil, nil)
	tel.DriverHealthMut("site").RecordSuccess()

	tel.Update("pv-online", telemetry.DerPV, -2000, nil, nil)
	tel.DriverHealthMut("pv-online").RecordSuccess()
	tel.Update("pv-offline", telemetry.DerPV, -9000, nil, nil)
	tel.DriverHealthMut("pv-offline").SetOffline()

	onlineSoC := 0.8
	offlineSoC := 0.1
	tel.Update("bat-online", telemetry.DerBattery, -1000, &onlineSoC, nil)
	tel.DriverHealthMut("bat-online").RecordSuccess()
	tel.Update("bat-offline", telemetry.DerBattery, -6000, &offlineSoC, nil)
	tel.DriverHealthMut("bat-offline").SetOffline()

	tel.Update("charger", telemetry.DerEV, 1000, nil, nil)
	tel.DriverHealthMut("charger").RecordSuccess()

	srv := New(&Deps{
		Tel:        tel,
		Ctrl:       ctrl,
		CtrlMu:     &sync.Mutex{},
		CapMu:      &sync.RWMutex{},
		Capacities: map[string]float64{"bat-online": 10_000, "bat-offline": 10_000},
		CfgMu:      &sync.RWMutex{},
		Cfg:        &config.Config{},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		GridW   float64                   `json:"grid_w"`
		PVW     float64                   `json:"pv_w"`
		BatW    float64                   `json:"bat_w"`
		EVW     float64                   `json:"ev_w"`
		LoadW   float64                   `json:"load_w"`
		BatSoC  float64                   `json:"bat_soc"`
		Drivers map[string]map[string]any `json:"drivers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	want := map[string]float64{
		"grid_w":  8000,
		"pv_w":    -2000,
		"bat_w":   -1000,
		"ev_w":    1000,
		"load_w":  10_000,
		"bat_soc": 0.8,
	}
	got := map[string]float64{
		"grid_w":  resp.GridW,
		"pv_w":    resp.PVW,
		"bat_w":   resp.BatW,
		"ev_w":    resp.EVW,
		"load_w":  resp.LoadW,
		"bat_soc": resp.BatSoC,
	}
	for k, wantV := range want {
		if got[k] != wantV {
			t.Fatalf("%s = %v, want %v", k, got[k], wantV)
		}
	}
	if resp.Drivers["pv-offline"]["status"] != "offline" || resp.Drivers["pv-offline"]["pv_w"] != -9000.0 {
		t.Fatalf("offline driver details not preserved: %#v", resp.Drivers["pv-offline"])
	}
}

func TestLoadResearchDumpExportsHouseLoadWithEVSplit(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC()
	ts := now.Add(-time.Hour).UnixMilli()
	js := `{"drivers":{"charger":{"ev_w":300}}}`
	if err := st.RecordHistory(state.HistoryPoint{
		TsMs:   ts,
		GridW:  1300,
		PVW:    -500,
		BatW:   0,
		LoadW:  1800, // legacy whole-site load: grid - bat - pv
		BatSoC: 55,
		JSON:   js,
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	cfg := &config.Config{
		Fuse:       config.Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Price:      &config.Price{Provider: "none", Zone: "SE3"},
		Loadpoints: []config.Loadpoint{{ID: "ev", DriverName: "charger"}},
	}
	srv := New(&Deps{State: st, Cfg: cfg, Version: "test"})

	req := httptest.NewRequest(http.MethodGet, "/api/research/load/dump?days=1", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	files := readTarGz(t, rr.Body.Bytes())
	csv := string(files["timeseries_15m.csv"])
	if !strings.Contains(csv, ",300,1500,1800,") {
		t.Fatalf("timeseries does not carry ev_w=300, house_load_w=1500, recorded_load_w=1800:\n%s", csv)
	}
	siteJSON := string(files["site.json"])
	if !strings.Contains(siteJSON, `"has_ev": true`) {
		t.Fatalf("site.json does not mark EV presence:\n%s", siteJSON)
	}
}

func readTarGz(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		parts := strings.SplitN(hdr.Name, "/", 2)
		if len(parts) == 2 {
			files[parts[1]] = b
		}
	}
	return files
}

// With no state backing, /api/energy/daily returns an empty payload at
// 200 (the "history is optional" branch). Anything else would break
// dev/test harnesses that run without a DB.
func TestHandleEnergyDailyNoState(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?days=7", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("nil state: got %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v (body: %s)", err, rr.Body.String())
	}
	days, ok := body["days"].([]any)
	if !ok {
		t.Fatalf("missing days array: %#v", body)
	}
	if len(days) != 0 {
		t.Fatalf("expected empty days, got %d entries", len(days))
	}
}

// An empty history DB must still return N pre-seeded day buckets with
// zero-valued fields — that's what lets the UI distinguish "no data
// yet" (zeros) from a backend failure (500).
func TestHandleEnergyDailyEmptyDBReturnsZeroedDays(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st})

	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?days=5", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Days []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(body.Days) != 5 {
		t.Fatalf("want 5 day buckets, got %d", len(body.Days))
	}
	for i, d := range body.Days {
		if d["day"] == "" {
			t.Errorf("day[%d] missing date", i)
		}
		for _, f := range []string{"import_wh", "export_wh", "pv_wh", "bat_charged_wh", "bat_discharged_wh", "load_wh"} {
			if v, _ := d[f].(float64); v != 0 {
				t.Errorf("day[%d].%s = %v, want 0", i, f, v)
			}
		}
	}
}

// Dropping real history into a few buckets and confirming the
// integration math lands the expected Wh in the expected day buckets.
// This is the site-convention regression net: any future sign flip
// inside the driver layer will show up here.
func TestHandleEnergyDailyBucketsByLocalDay(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Drop two samples inside today, separated by `gap`, both strictly
	// between todayMidnight and now. The two-sample slice integrates to
	// GridW * gap == 1000 * gapHours Wh of import attributed to today.
	// Sizing the gap off `elapsed` keeps the test robust when CI runs
	// early in the morning (e.g. 01:46 local — the original hard-coded
	// "now - 1h, now - 2h" scheme fell before midnight and got filtered
	// out by LoadHistory's [firstDayStart, now] range).
	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	elapsed := now.Sub(todayMidnight)
	if elapsed < 15*time.Minute {
		t.Skip("too close to local midnight; skipping bucket test")
	}
	gap := elapsed / 3
	t0 := todayMidnight.Add(gap)
	t1 := t0.Add(gap)
	gapHours := gap.Seconds() / 3600.0
	expectedImport := 1000.0 * gapHours
	for _, p := range []state.HistoryPoint{
		{TsMs: t0.UnixMilli(), GridW: 1000},
		{TsMs: t1.UnixMilli(), GridW: 1000},
	} {
		if err := st.RecordHistory(p); err != nil {
			t.Fatalf("RecordHistory: %v", err)
		}
	}

	srv := New(&Deps{State: st})
	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?days=3", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Days []map[string]any `json:"days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(body.Days) != 3 {
		t.Fatalf("want 3 buckets, got %d", len(body.Days))
	}
	// Today is the last entry; the earlier two must be zero.
	for i, d := range body.Days[:2] {
		if v, _ := d["import_wh"].(float64); v != 0 {
			t.Errorf("day[%d].import_wh = %v, want 0 (older bucket should be empty)", i, v)
		}
	}
	todayImport, _ := body.Days[2]["import_wh"].(float64)
	// Allow 1% slop: SQLite ms-precision vs Go's time.Sub can differ.
	tolerance := 0.01 * expectedImport
	if tolerance < 1 {
		tolerance = 1
	}
	if diff := todayImport - expectedImport; diff < -tolerance || diff > tolerance {
		t.Errorf("today import_wh = %v, want ~%v (gap=%v)", todayImport, expectedImport, gap)
	}
}

func TestCurrentGridEnergySlotUsesFixedQuarterWindow(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 5, 23, 14, 7, 0, 0, time.Local)
	slotStart := time.Date(2026, 5, 23, 14, 0, 0, 0, time.Local)
	if err := st.RecordHistory(state.HistoryPoint{TsMs: slotStart.Add(time.Minute).UnixMilli(), GridW: 1200}); err != nil {
		t.Fatalf("RecordHistory import: %v", err)
	}
	if err := st.RecordHistory(state.HistoryPoint{TsMs: slotStart.Add(4 * time.Minute).UnixMilli(), GridW: 1200}); err != nil {
		t.Fatalf("RecordHistory import 2: %v", err)
	}
	if err := st.RecordHistory(state.HistoryPoint{TsMs: slotStart.Add(6 * time.Minute).UnixMilli(), GridW: -600}); err != nil {
		t.Fatalf("RecordHistory export: %v", err)
	}
	// Previous-slot noise must not leak into the current 14:00-14:15 bucket.
	if err := st.RecordHistory(state.HistoryPoint{TsMs: slotStart.Add(-time.Minute).UnixMilli(), GridW: 9000}); err != nil {
		t.Fatalf("RecordHistory previous slot: %v", err)
	}

	got, err := currentGridEnergySlot(st, now)
	if err != nil {
		t.Fatalf("currentGridEnergySlot: %v", err)
	}
	if got["slot_start_ms"].(int64) != slotStart.UnixMilli() {
		t.Fatalf("slot_start_ms = %v, want %v", got["slot_start_ms"], slotStart.UnixMilli())
	}
	if got["slot_end_ms"].(int64) != slotStart.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("slot_end_ms = %v, want %v", got["slot_end_ms"], slotStart.Add(15*time.Minute).UnixMilli())
	}
	// DailyEnergy uses the current row's grid_w over (prev_ts, ts]:
	// +1200 W for 3 min = 60 Wh, then -600 W for 2 min = 20 Wh export.
	if math.Abs(got["import_wh"].(float64)-60) > 0.01 {
		t.Fatalf("import_wh = %v, want 60", got["import_wh"])
	}
	if math.Abs(got["export_wh"].(float64)-20) > 0.01 {
		t.Fatalf("export_wh = %v, want 20", got["export_wh"])
	}
	if math.Abs(got["net_wh"].(float64)-40) > 0.01 {
		t.Fatalf("net_wh = %v, want 40", got["net_wh"])
	}
}

// Closing the state store mid-flight turns LoadHistory into an error
// path. The old handler silently returned zeroed days (indistinguishable
// from a real 0 kWh day); the new handler returns 500 so operators see
// the failure.
func TestHandleEnergyDailyReturns500OnDBError(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	_ = st.Close() // force subsequent LoadHistory to fail
	srv := New(&Deps{State: st})

	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?days=3", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 (body: %s)", rr.Code, rr.Body.String())
	}
}

// days= parsing: garbage/0/negative fall through to default 7; >90 caps.
// Mirrors the silent-default convention used by parseRange elsewhere.
func TestHandleEnergyDailyDaysClamping(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st})

	cases := []struct {
		q    string
		want int
	}{
		{"", 7},
		{"abc", 7},
		{"-5", 7},
		{"0", 7},
		{"14", 14},
		{"150", 90},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			url := "/api/energy/daily"
			if tc.q != "" {
				url += "?days=" + tc.q
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("q=%q: got %d, want 200", tc.q, rr.Code)
			}
			var body struct {
				Days []map[string]any `json:"days"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("q=%q: invalid json: %v", tc.q, err)
			}
			if len(body.Days) != tc.want {
				t.Fatalf("q=%q: got %d days, want %d", tc.q, len(body.Days), tc.want)
			}
		})
	}
}

// passive_arbitrage merges planner_self + planner_cheap into one
// operator-facing mode. The API must accept the new value and propagate
// the corresponding mpc.Mode to the planner service.
func TestHandleSetModeAcceptsPassiveArbitrage(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctrl := control.NewState(0, 50, "meter")
	srv := New(&Deps{
		Ctrl:   ctrl,
		CtrlMu: &sync.Mutex{},
		State:  st,
		CfgMu:  &sync.RWMutex{},
		Cfg:    &config.Config{},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/mode",
		strings.NewReader(`{"mode":"planner_passive_arbitrage"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if ctrl.Mode != control.ModePlannerPassiveArbitrage {
		t.Errorf("ctrl.Mode = %q, want %q", ctrl.Mode, control.ModePlannerPassiveArbitrage)
	}
	if !ctrl.Mode.IsPlannerMode() {
		t.Errorf("passive_arbitrage must register as a planner mode for the dispatch energy-path")
	}
}

// 2026-05-24 evening regression: PI integrator state carried across an
// operator mode switch, so the new mode inherited a saturated integral
// from the previous mode's stuck-import accumulation and commanded
// wrong-direction battery moves for minutes after the switch (overnight
// the fleet drained to 7 %). Mode change is a discrete event; the PI
// integral has no meaning under the new control regime.
func TestHandleSetModeResetsPIIntegral(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctrl := control.NewState(0, 50, "meter")
	ctrl.Mode = control.ModeSelfConsumption
	// Wind PI integral to near saturation under a sustained positive
	// measurement (PI's internal err = setpoint - measurement = negative,
	// so integral drives negative).
	for i := 0; i < 200; i++ {
		ctrl.PI.Update(700)
	}
	if ctrl.PI.Integral() > -2900 {
		t.Fatalf("setup: expected integral pinned near -3000, got %f", ctrl.PI.Integral())
	}

	srv := New(&Deps{
		Ctrl:   ctrl,
		CtrlMu: &sync.Mutex{},
		State:  st,
		CfgMu:  &sync.RWMutex{},
		Cfg:    &config.Config{},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/mode",
		strings.NewReader(`{"mode":"self_consumption"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if got := ctrl.PI.Integral(); got != 0 {
		t.Errorf("PI integral after mode change = %f, want 0 (mode change must clear PI state)", got)
	}
}
