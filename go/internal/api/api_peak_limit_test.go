package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// POST /api/peak_limit is one of the two operator paths into the
// peak-shaving threshold (the other is the Home Assistant number). Both
// share control.State.SetPeakLimit; these tests pin that the endpoint
// actually routes through it and reports a rejection instead of storing
// a limit the fuse guard would make meaningless.

// 16 A × 230 V × 3 phases = 11040 W, less the 0.5 A margin = 10695 W.
func newPeakLimitServer(t *testing.T) (*Server, *control.State) {
	t.Helper()
	st := control.NewState(0, 50, "ferroamp")
	st.SiteFuseAmps = 16
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	st.SiteFuseSafetyA = 0.5
	srv := New(&Deps{Ctrl: st, CtrlMu: &sync.Mutex{}, Tel: telemetry.NewStore()})
	return srv, st
}

func postPeakLimit(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/peak_limit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestPeakLimitEndpointAcceptsValueUnderTheFuse(t *testing.T) {
	srv, st := newPeakLimitServer(t)
	if rr := postPeakLimit(t, srv, `{"peak_limit_w":7000}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if st.PeakLimitW != 7000 {
		t.Errorf("PeakLimitW = %.0f, want 7000", st.PeakLimitW)
	}
}

func TestPeakLimitEndpointRejectsValueAboveTheFuse(t *testing.T) {
	srv, st := newPeakLimitServer(t)
	rr := postPeakLimit(t, srv, `{"peak_limit_w":20000}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	// The caller has to learn why, here and not from dispatch later.
	if !strings.Contains(rr.Body.String(), "10695") {
		t.Errorf("response must name the ceiling that beat it, got %s", rr.Body.String())
	}
	if st.PeakLimitW != 5000 {
		t.Errorf("rejected value must not land: PeakLimitW = %.0f, want the untouched 5000", st.PeakLimitW)
	}
}

func TestPeakLimitEndpointRejectsNegative(t *testing.T) {
	srv, st := newPeakLimitServer(t)
	if rr := postPeakLimit(t, srv, `{"peak_limit_w":-2000}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	if st.PeakLimitW != 5000 {
		t.Errorf("rejected value must not land: PeakLimitW = %.0f, want 5000", st.PeakLimitW)
	}
}

// Zero is a real threshold for peak shaving ("shave everything above
// 0 W"), not the disabled sentinel PeakImportCeilingW uses. The endpoint
// takes it.
func TestPeakLimitEndpointAcceptsZero(t *testing.T) {
	srv, st := newPeakLimitServer(t)
	if rr := postPeakLimit(t, srv, `{"peak_limit_w":0}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if st.PeakLimitW != 0 {
		t.Errorf("PeakLimitW = %.0f, want 0", st.PeakLimitW)
	}
}

// A site whose fuse the operator never described keeps the old
// permissive behaviour rather than inheriting an invented breaker.
func TestPeakLimitEndpointWithoutFuseAcceptsAnyNonNegative(t *testing.T) {
	st := control.NewState(0, 50, "ferroamp")
	srv := New(&Deps{Ctrl: st, CtrlMu: &sync.Mutex{}, Tel: telemetry.NewStore()})
	if rr := postPeakLimit(t, srv, `{"peak_limit_w":20000}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if st.PeakLimitW != 20000 {
		t.Errorf("PeakLimitW = %.0f, want 20000", st.PeakLimitW)
	}
}
