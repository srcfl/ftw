package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// PV manual-hold endpoint tests. Validation, scoping, pct→W
// conversion, and the install → read → clear lifecycle.

func newPVHoldServer(t *testing.T) (*Server, *control.State, *sync.Mutex, *telemetry.Store) {
	t.Helper()
	st := control.NewState(0, 50, "ferroamp")
	st.SupportsPVCurtail = map[string]bool{
		"solaredge": true,
		"sungrow":   true,
	}
	mu := &sync.Mutex{}
	tel := telemetry.NewStore()
	// Emit live PV so pct conversion has a basis.
	tel.DriverHealthMut("solaredge").RecordSuccess()
	tel.Update("solaredge", telemetry.DerPV, -3000, nil, nil)
	tel.DriverHealthMut("sungrow").RecordSuccess()
	tel.Update("sungrow", telemetry.DerPV, -1000, nil, nil)
	srv := New(&Deps{Ctrl: st, CtrlMu: mu, Tel: tel})
	return srv, st, mu, tel
}

func TestPVHoldUnavailableWithoutCtrl(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"limit_w":500,"hold_s":60}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestPVHoldValidation(t *testing.T) {
	srv, _, _, _ := newPVHoldServer(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing hold_s", `{"limit_w":500}`, http.StatusBadRequest},
		{"zero hold_s", `{"limit_w":500,"hold_s":0}`, http.StatusBadRequest},
		{"hold_s too large", `{"limit_w":500,"hold_s":99999}`, http.StatusBadRequest},
		{"missing both knobs", `{"hold_s":60}`, http.StatusBadRequest},
		{"both knobs", `{"limit_w":500,"limit_pct":50,"hold_s":60}`, http.StatusBadRequest},
		{"negative limit_w", `{"limit_w":-1,"hold_s":60}`, http.StatusBadRequest},
		{"pct over 100", `{"limit_pct":150,"hold_s":60}`, http.StatusBadRequest},
		{"unsupported driver", `{"driver":"easee","limit_w":500,"hold_s":60}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("body=%q: status=%d want=%d (body=%s)",
					tc.body, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestPVHoldInstallSiteAggregate(t *testing.T) {
	srv, st, mu, _ := newPVHoldServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"limit_w":1500,"hold_s":60}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp pvManualHoldResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Active || resp.LimitW != 1500 || resp.Driver != "" {
		t.Errorf("resp = %+v", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if st.ManualPVHold.LimitW != 1500 || st.ManualPVHold.Driver != "" {
		t.Errorf("state.ManualPVHold = %+v", st.ManualPVHold)
	}
}

func TestPVHoldInstallDriverScoped(t *testing.T) {
	srv, st, mu, _ := newPVHoldServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"driver":"solaredge","limit_w":750,"hold_s":60}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if st.ManualPVHold.Driver != "solaredge" || st.ManualPVHold.LimitW != 750 {
		t.Errorf("state.ManualPVHold = %+v", st.ManualPVHold)
	}
}

// 50% of 3000 W live |PV| on solaredge → 1500 W cap.
func TestPVHoldPctConversionDriverScoped(t *testing.T) {
	srv, st, mu, _ := newPVHoldServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"driver":"solaredge","limit_pct":50,"hold_s":60}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got := st.ManualPVHold.LimitW; got < 1499 || got > 1501 {
		t.Errorf("limit_w from 50%% of 3000 W: got %.2f, want ≈1500", got)
	}
}

// 25% of (3000 + 1000) W aggregate live |PV| → 1000 W cap.
func TestPVHoldPctConversionSiteAggregate(t *testing.T) {
	srv, st, mu, _ := newPVHoldServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"limit_pct":25,"hold_s":60}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got := st.ManualPVHold.LimitW; got < 999 || got > 1001 {
		t.Errorf("limit_w from 25%% of 4000 W: got %.2f, want ≈1000", got)
	}
}

func TestPVHoldLifecycle(t *testing.T) {
	srv, _, _, _ := newPVHoldServer(t)

	// Install.
	postReq := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"limit_w":500,"hold_s":60}`))
	postReq.Header.Set("Content-Type", "application/json")
	postRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(postRR, postReq)
	if postRR.Code != http.StatusOK {
		t.Fatalf("install: status=%d", postRR.Code)
	}

	// GET → active.
	getRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRR, httptest.NewRequest(http.MethodGet, "/api/pv/manual_hold", nil))
	var got pvManualHoldResponse
	if err := json.Unmarshal(getRR.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Active || got.LimitW != 500 {
		t.Errorf("get after install: %+v", got)
	}

	// DELETE → cleared.
	delRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(delRR, httptest.NewRequest(http.MethodDelete, "/api/pv/manual_hold", nil))
	if delRR.Code != http.StatusOK {
		t.Fatalf("delete: status=%d", delRR.Code)
	}

	// GET → inactive.
	getRR2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRR2, httptest.NewRequest(http.MethodGet, "/api/pv/manual_hold", nil))
	var got2 pvManualHoldResponse
	if err := json.Unmarshal(getRR2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got2.Active {
		t.Errorf("get after delete should report inactive: %+v", got2)
	}
}

// 100% should mean "100% of nominal_w" when configured — not 100% of
// the live |PV| (which is typically much lower than nominal and would
// make the slider effectively meaningless above current production).
func TestPVHoldPctUsesNominalWhenConfigured(t *testing.T) {
	st := control.NewState(0, 50, "ferroamp")
	st.SupportsPVCurtail = map[string]bool{"solaredge": true}
	mu := &sync.Mutex{}
	tel := telemetry.NewStore()
	// Live PV is small (cloudy moment).
	tel.DriverHealthMut("solaredge").RecordSuccess()
	tel.Update("solaredge", telemetry.DerPV, -256, nil, nil)
	cfgMu := &sync.RWMutex{}
	cfg := &config.Config{Drivers: []config.Driver{{
		Name:   "solaredge",
		Config: map[string]any{"nominal_w": 8000},
	}}}
	srv := New(&Deps{Ctrl: st, CtrlMu: mu, Tel: tel, Cfg: cfg, CfgMu: cfgMu})

	req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"driver":"solaredge","limit_pct":100,"hold_s":60}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got := st.ManualPVHold.LimitW; got != 8000 {
		t.Errorf("100%% with nominal=8000 W: got %.2f, want 8000", got)
	}
}

// Site-aggregate pct sums nominal_w across curtail-supporting drivers.
func TestPVHoldPctAggregateUsesNominalSum(t *testing.T) {
	st := control.NewState(0, 50, "ferroamp")
	st.SupportsPVCurtail = map[string]bool{"solaredge": true, "sungrow": true}
	mu := &sync.Mutex{}
	tel := telemetry.NewStore()
	cfgMu := &sync.RWMutex{}
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "solaredge", Config: map[string]any{"nominal_w": 8000}},
		{Name: "sungrow", Config: map[string]any{"nominal_w": 5000}},
	}}
	srv := New(&Deps{Ctrl: st, CtrlMu: mu, Tel: tel, Cfg: cfg, CfgMu: cfgMu})

	req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"limit_pct":50,"hold_s":60}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got := st.ManualPVHold.LimitW; got != 6500 {
		t.Errorf("50%% of nominal sum (13000 W): got %.2f, want 6500", got)
	}
}

func TestPVHoldNoSupportingDriversConfigured(t *testing.T) {
	st := control.NewState(0, 50, "meter")
	st.SupportsPVCurtail = map[string]bool{} // explicitly empty
	mu := &sync.Mutex{}
	srv := New(&Deps{Ctrl: st, CtrlMu: mu, Tel: telemetry.NewStore()})
	req := httptest.NewRequest(http.MethodPost, "/api/pv/manual_hold",
		strings.NewReader(`{"limit_w":500,"hold_s":60}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (no curtail-capable drivers)", rr.Code)
	}
}
