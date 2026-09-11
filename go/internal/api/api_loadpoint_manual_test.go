package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Manual-hold endpoint tests. Validation, route wiring, and the full
// install → read → clear lifecycle.

func newManualHoldServer(t *testing.T) (*Server, *loadpoint.Controller) {
	t.Helper()
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{
		ID:         "garage",
		DriverName: "easee",
		MinChargeW: 1380,
		MaxChargeW: 11000,
	}})
	ctrl := loadpoint.NewController(mgr, nil, nil, nil)
	return New(&Deps{Loadpoints: mgr, LoadpointCtrl: ctrl}), ctrl
}

func TestManualHoldUnavailableWithoutController(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee"}})
	srv := New(&Deps{Loadpoints: mgr}) // no LoadpointCtrl wired
	body := `{"power_w":1380,"hold_s":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/manual_hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (controller missing)", rr.Code)
	}
}

func TestManualHold404OnUnknownLoadpoint(t *testing.T) {
	srv, _ := newManualHoldServer(t)
	body := `{"power_w":1380,"hold_s":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/ghost/manual_hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestManualHoldValidatesBody(t *testing.T) {
	srv, _ := newManualHoldServer(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		// hold_s == 0 (or omitted) now means a persistent override
		// (operator "Start" / amp slider) — accepted, not rejected.
		{"missing hold_s is persistent", `{"power_w":1380}`, http.StatusOK},
		{"zero hold_s is persistent", `{"power_w":1380,"hold_s":0}`, http.StatusOK},
		{"negative hold_s", `{"power_w":1380,"hold_s":-1}`, http.StatusBadRequest},
		{"hold_s too large", `{"power_w":1380,"hold_s":99999}`, http.StatusBadRequest},
		{"negative power", `{"power_w":-1,"hold_s":30}`, http.StatusBadRequest},
		{"bad phase_mode", `{"power_w":1380,"hold_s":30,"phase_mode":"5p"}`, http.StatusBadRequest},
		{"malformed json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/manual_hold", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("body=%s: status = %d, want %d", tc.body, rr.Code, tc.want)
			}
		})
	}
}

func TestManualHoldInstallsAndReadsBack(t *testing.T) {
	srv, ctrl := newManualHoldServer(t)
	body := `{"power_w":1380,"phase_mode":"1p","voltage":230,"max_amps_per_phase":16,"hold_s":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/manual_hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	// Verify it's in the controller.
	h, active := ctrl.GetManualHold("garage", time.Now())
	if !active {
		t.Fatalf("hold not installed in controller")
	}
	if h.PowerW != 1380 || h.PhaseMode != "1p" || h.Voltage != 230 {
		t.Errorf("controller hold mismatch: %+v", h)
	}
	// GET should return it.
	getReq := httptest.NewRequest(http.MethodGet, "/api/loadpoints/garage/manual_hold", nil)
	getRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getRR.Code)
	}
	var got manualHoldResponse
	if err := json.Unmarshal(getRR.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Active {
		t.Errorf("GET reports not active")
	}
	if got.PowerW != 1380 || got.PhaseMode != "1p" {
		t.Errorf("GET body mismatch: %+v", got)
	}
}

// Per Copilot review: POST/DELETE/GET must all 404 when Loadpoints is
// nil or when the id isn't configured. Earlier behaviour allowed a
// hold to be installed on an arbitrary id when Loadpoints was nil.
func TestManualHold404WhenLoadpointsNil(t *testing.T) {
	mgr := loadpoint.NewManager()
	ctrl := loadpoint.NewController(mgr, nil, nil, nil)
	srv := New(&Deps{LoadpointCtrl: ctrl}) // intentionally Loadpoints=nil

	cases := []struct {
		name, method, path, body string
	}{
		{"POST", http.MethodPost, "/api/loadpoints/garage/manual_hold", `{"power_w":1380,"hold_s":30}`},
		{"DELETE", http.MethodDelete, "/api/loadpoints/garage/manual_hold", ""},
		{"GET", http.MethodGet, "/api/loadpoints/garage/manual_hold", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			var req *http.Request
			if body != nil {
				req = httptest.NewRequest(tc.method, tc.path, body)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404", tc.name, rr.Code)
			}
		})
	}
}

func TestManualHoldDeleteAndGet404OnUnknownLoadpoint(t *testing.T) {
	srv, _ := newManualHoldServer(t)
	for _, m := range []string{http.MethodDelete, http.MethodGet} {
		req := httptest.NewRequest(m, "/api/loadpoints/ghost/manual_hold", nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s ghost: status = %d, want 404", m, rr.Code)
		}
	}
}

func TestManualHoldDeleteClears(t *testing.T) {
	srv, ctrl := newManualHoldServer(t)
	ctrl.SetManualHold("garage", loadpoint.ManualHold{
		PowerW:    1380,
		ExpiresAt: time.Now().Add(time.Minute),
	})
	if _, active := ctrl.GetManualHold("garage", time.Now()); !active {
		t.Fatalf("setup: hold not installed")
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/loadpoints/garage/manual_hold", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", rr.Code)
	}
	if _, active := ctrl.GetManualHold("garage", time.Now()); active {
		t.Errorf("hold still active after DELETE")
	}
}

// A "charge now → X %" hold whose target the SoC estimate already meets
// is refused, not installed-then-released: the operator who pressed
// Start five times against an estimate sitting on the schedule target
// got "active" for one tick each time and no charge. Omitting the
// target (or setting it above the estimate) still installs.
func TestManualHoldRefusesReleaseTargetAlreadyMet(t *testing.T) {
	srv, ctrl := newManualHoldServer(t)
	// Plugged in; the estimate re-anchored to 80 %.
	srv.deps.Loadpoints.Observe("garage", true, 0, 0, true)
	if !srv.deps.Loadpoints.SetCurrentSoC("garage", 0.8) {
		t.Fatalf("SetCurrentSoC failed")
	}
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/manual_hold", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	if rr := post(`{"power_w":11040,"hold_s":0,"release_at_soc_pct":80}`); rr.Code != http.StatusConflict {
		t.Fatalf("target == estimate: status = %d, want 409 (body: %s)", rr.Code, rr.Body.String())
	} else if !strings.Contains(rr.Body.String(), "already at 80 %") {
		t.Errorf("409 body should name the estimate: %s", rr.Body.String())
	}
	if _, active := ctrl.GetManualHold("garage", time.Now()); active {
		t.Fatalf("refused hold must not be installed")
	}
	if rr := post(`{"power_w":11040,"hold_s":0,"release_at_soc_pct":70}`); rr.Code != http.StatusConflict {
		t.Errorf("target below estimate: status = %d, want 409", rr.Code)
	}
	if rr := post(`{"power_w":11040,"hold_s":0,"release_at_soc_pct":90}`); rr.Code != http.StatusOK {
		t.Errorf("target above estimate: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	ctrl.ClearManualHold("garage")
	if rr := post(`{"power_w":11040,"hold_s":0}`); rr.Code != http.StatusOK {
		t.Errorf("no target: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if h, active := ctrl.GetManualHold("garage", time.Now()); !active || h.ReleaseAtSoC != 0 {
		t.Errorf("no-target hold should be installed without a SoC release, got active=%v %+v", active, h)
	}
}

// After Charge now the loadpoint carries a live account of the hold: what
// was ordered, since when, and what the charger did with it (#1002). The
// manual tab renders this instead of a sentence written at click time.
func TestLoadpointsCarryManualStatus(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11000}})
	ctrl := loadpoint.NewController(mgr, func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false }, func(string) (loadpoint.EVSample, bool) {
		return loadpoint.EVSample{Connected: true, RequestActive: true}, true
	}, nil)
	tel := telemetry.NewStore()
	srv := New(&Deps{Loadpoints: mgr, LoadpointCtrl: ctrl, Tel: tel})

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/manual_hold", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST status = %d: %s", rr.Code, rr.Body.String())
		}
		var resp manualHoldResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.StartedAtMs == 0 {
			t.Error("POST response must carry started_at_ms")
		}
	}
	manual := func() loadpoint.ManualStatus {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /api/loadpoints = %d: %s", rr.Code, rr.Body.String())
		}
		var got struct {
			Loadpoints []loadpoint.State `json:"loadpoints"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Loadpoints) != 1 {
			t.Fatalf("loadpoints = %d, want 1", len(got.Loadpoints))
		}
		return got.Loadpoints[0].Manual
	}

	if m := manual(); m.Active {
		t.Fatalf("no hold yet, got %+v", m)
	}

	// 6 A on three phases at 230 V; the charger has not answered yet.
	post(`{"power_w":4140,"hold_s":0}`)
	m := manual()
	if !m.Active || m.State != loadpoint.ManualSent || m.RequestedA != 6 || m.StartedAtMs == 0 {
		t.Fatalf("after Charge now: %+v", m)
	}
	first := m.StartedAtMs

	// The Easee echoes the limit: accepted, waiting for the car.
	tel.Update("easee", telemetry.DerEV, 0, nil, json.RawMessage(`{"max_a":6,"charging":false,"reason_no_current_label":"car not drawing current"}`))
	if m = manual(); m.State != loadpoint.ManualSent {
		t.Fatalf("charger echo cannot confirm an unprocessed request: %+v", m)
	}
	ctrl.Tick(context.Background(), time.Now())
	// Confirmation must arrive after dispatch; the pre-tick echo above is
	// deliberately too early, even when both fall in the same millisecond.
	tel.Update("easee", telemetry.DerEV, 0, nil, json.RawMessage(`{"max_a":6,"charging":false,"reason_no_current_label":"car not drawing current"}`))
	m = manual()
	if m.State != loadpoint.ManualAccepted || !m.ChargerLimitKnown || m.ChargerLimitA != 6 || m.ChargerReason != "car not drawing current" {
		t.Fatalf("after the charger took the limit: %+v", m)
	}

	// An Update of the amps keeps the first press as the start.
	post(`{"power_w":11040,"hold_s":0}`)
	if m = manual(); m.StartedAtMs != first || m.RequestedA != 16 {
		t.Fatalf("after Update: %+v (first start %d)", m, first)
	}

	// The controller processes the new choice before its charger response.
	ctrl.Tick(context.Background(), time.Now())

	// The charger says the command stalled.
	tel.Update("easee", telemetry.DerEV, 0, nil, json.RawMessage(`{"max_a":16,"charging":false,"reason_no_current_label":"EV not accepting current","command_stalled":true}`))
	if m = manual(); m.State != loadpoint.ManualStalled || m.ChargerReason != "EV not accepting current" {
		t.Fatalf("after a stall: %+v", m)
	}

	// Power flows.
	tel.Update("easee", telemetry.DerEV, 10800, nil, json.RawMessage(`{"max_a":16,"charging":true}`))
	if m = manual(); m.State != loadpoint.ManualCharging {
		t.Fatalf("while charging: %+v", m)
	}

	// Stop clears the account.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/loadpoints/garage/manual_hold", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE = %d", rr.Code)
	}
	if m = manual(); m.Active || m.State != "" {
		t.Fatalf("after Stop: %+v", m)
	}
}

func TestLoadpointsReportChargerFreshnessWithoutManualHold(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee"}})
	tel := telemetry.NewStore()
	srv := New(&Deps{Loadpoints: mgr, Tel: tel})
	read := func() *loadpoint.ChargerStatus {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
		var body struct {
			Loadpoints []loadpoint.State `json:"loadpoints"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Loadpoints) != 1 || body.Loadpoints[0].Charger == nil {
			t.Fatalf("missing charger status: %s", rr.Body.String())
		}
		return body.Loadpoints[0].Charger
	}
	if got := read(); got.Known || got.Available {
		t.Fatalf("no reading must stay unknown: %+v", got)
	}
	tel.Update("easee", telemetry.DerEV, 10800, nil, json.RawMessage(`{"is_online":false,"charging":true,"max_a":16,"reason_no_current_label":"offline"}`))
	if got := read(); !got.Known || got.Available || got.UpdatedAtMs == 0 || got.Reason != "offline" {
		t.Fatalf("offline cached power became current: %+v", got)
	}
	tel.Update("easee", telemetry.DerEV, 10800, nil, json.RawMessage(`{"is_online":true,"charging":true,"max_a":16}`))
	if got := read(); !got.Available || got.LimitA == nil || *got.LimitA != 16 {
		t.Fatalf("fresh report did not recover: %+v", got)
	}
}
