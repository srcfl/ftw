package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// #1674: switching the import ceiling on applied 3,000 W that one browser
// had kept from a test, not the 7,500 W the owner meant. The box keeps the
// last ceiling that was on and serves it, so a client never supplies it.

func newCeilingServer(t *testing.T, st *state.Store, ctrl *control.State) *Server {
	t.Helper()
	return New(&Deps{State: st, Tel: telemetry.NewStore(), Ctrl: ctrl, CtrlMu: &sync.Mutex{}, CapMu: &sync.RWMutex{}, CfgMu: &sync.RWMutex{}, Cfg: &config.Config{}})
}

func postCeiling(t *testing.T, srv *Server, w string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/peak_import_ceiling", strings.NewReader(`{"peak_import_ceiling_w":`+w+`}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST %s W: status %d, body %s", w, rr.Code, rr.Body.String())
	}
}

// ceilingStatus returns the ceiling and the last one that was on, as
// /api/status serves them; nil stands for JSON null.
func ceilingStatus(t *testing.T, srv *Server) (any, any) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var body map[string]any
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &body) != nil {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	last, ok := body["peak_import_ceiling_last_w"]
	if !ok {
		t.Fatal("status has no peak_import_ceiling_last_w")
	}
	return body["peak_import_ceiling_w"], last
}

func TestPeakImportCeilingKeepsTheLastOneThatWasOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := newCeilingServer(t, st, &control.State{})

	if now, last := ceilingStatus(t, srv); now != 0.0 || last != nil {
		t.Fatalf("fresh box: ceiling %v, last %v; want 0 and null", now, last)
	}
	postCeiling(t, srv, "7500")
	if now, last := ceilingStatus(t, srv); now != 7500.0 || last != 7500.0 {
		t.Fatalf("on: ceiling %v, last %v; want 7,500 each", now, last)
	}
	postCeiling(t, srv, "0")
	postCeiling(t, srv, "0")
	if now, last := ceilingStatus(t, srv); now != 0.0 || last != 7500.0 {
		t.Fatalf("off: ceiling %v, last %v; want 0 and 7,500", now, last)
	}

	// The record outlives a restart, as the ceiling itself does.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv = newCeilingServer(t, st, &control.State{})
	if now, last := ceilingStatus(t, srv); now != 0.0 || last != 7500.0 {
		t.Fatalf("after restart: ceiling %v, last %v; want 0 and 7,500", now, last)
	}
	if v, ok := st.LoadConfig("peak_import_ceiling_w"); !ok || v != "0.0" {
		t.Fatalf("stored ceiling %q (%v), want 0.0", v, ok)
	}
}

// A ceiling switched on before this release was stored without a last
// value. Switching it off still keeps it.
func TestPeakImportCeilingOffKeepsACeilingSetBeforeTheRecord(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := newCeilingServer(t, st, &control.State{PeakImportCeilingW: 6000})

	postCeiling(t, srv, "0")
	if now, last := ceilingStatus(t, srv); now != 0.0 || last != 6000.0 {
		t.Fatalf("off: ceiling %v, last %v; want 0 and 6,000", now, last)
	}
}

// A ceiling the box cannot save is not applied: a restart would bring the
// old one back, and the caller has to learn that the change did not hold.
func TestPeakImportCeilingNotAppliedWhenTheSaveFails(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctrl := &control.State{PeakImportCeilingW: 6000}
	srv := newCeilingServer(t, st, ctrl)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/peak_import_ceiling", strings.NewReader(`{"peak_import_ceiling_w":0}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rr.Code, rr.Body.String())
	}
	if ctrl.PeakImportCeilingW != 6000 {
		t.Fatalf("ceiling = %.0f W after a failed save, want 6,000", ctrl.PeakImportCeilingW)
	}
}

// A new ceiling changes what the plan may deliver, so the plan follows it at
// once instead of at the next quarter.
func TestPeakImportCeilingChangeReplans(t *testing.T) {
	_, _, svc := newScheduleServer(t)
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := newCeilingServer(t, st, &control.State{})
	srv.deps.MPC = svc

	postCeiling(t, srv, "4500")
	waitForSchedulePlan(t, svc)
	if _, reason := svc.LastReplanInfo(); reason != "import_ceiling_changed" {
		t.Fatalf("replan reason = %q, want import_ceiling_changed", reason)
	}
}
