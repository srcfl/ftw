package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/loadpoint"
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
		{"missing hold_s", `{"power_w":1380}`, http.StatusBadRequest},
		{"zero hold_s", `{"power_w":1380,"hold_s":0}`, http.StatusBadRequest},
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
