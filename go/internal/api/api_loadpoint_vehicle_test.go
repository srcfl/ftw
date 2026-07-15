package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// POST /api/loadpoints/{id}/force_start — handler coverage.

// newForceStartServer builds a Server with a Manager + Controller
// and pre-installs a stub vehicleStatus + sender. Pass overrides to
// switch the stubs per-test (e.g. for the "send fails" case).
func newForceStartServer(
	t *testing.T,
	vehicleStatus func(string) (string, string, bool),
	send loadpoint.SenderFunc,
) *Server {
	t.Helper()
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{
		ID:         "garage",
		DriverName: "easee",
		MinChargeW: 1380,
		MaxChargeW: 11000,
	}})
	ctrl := loadpoint.NewController(mgr, nil, nil, send)
	if vehicleStatus != nil {
		ctrl.SetVehicleStatus(vehicleStatus)
	}
	return New(&Deps{Loadpoints: mgr, LoadpointCtrl: ctrl})
}

func TestForceStart_503WhenControllerMissing(t *testing.T) {
	s := New(&Deps{})
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/force_start", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestForceStart_503WhenControllerNotReady(t *testing.T) {
	// Controller exists but vehicleStatus / send wiring is missing.
	s := newForceStartServer(t, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/force_start", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (controller not ready), body=%s", rec.Code, rec.Body.String())
	}
}

func TestForceStart_404WhenLoadpointMissing(t *testing.T) {
	s := newForceStartServer(t,
		func(string) (string, string, bool) { return "tesla-vehicle", "Stopped", true },
		func(context.Context, string, []byte) error { return nil },
	)
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/no-such-lp/force_start", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["loadpoint_id"] != "no-such-lp" {
		t.Errorf("response should echo the requested loadpoint_id, got %q", body["loadpoint_id"])
	}
}

func TestForceStart_422WhenNoVehicleBound(t *testing.T) {
	s := newForceStartServer(t,
		func(string) (string, string, bool) { return "", "", false }, // LP exists, no vehicle bound
		func(context.Context, string, []byte) error { return nil },
	)
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/force_start", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 422 {
		t.Fatalf("status = %d, want 422 (no vehicle bound), body=%s", rec.Code, rec.Body.String())
	}
}

func TestForceStart_502OnDriverSendError(t *testing.T) {
	s := newForceStartServer(t,
		func(string) (string, string, bool) { return "tesla-vehicle", "Stopped", true },
		func(context.Context, string, []byte) error {
			// Driver-internal error strings (URLs, IPs) must NOT leak
			// through to the HTTP response body — asserted below.
			return errors.New("http://192.168.1.223:8080/api/1/vehicles/SECRET/wake: connection refused")
		},
	)
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/force_start", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); containsAny(got, "192.168.1.223", "SECRET", "connection refused") {
		t.Errorf("502 body leaked driver-internal error string: %s", got)
	}
}

func TestForceStart_200OnSuccess(t *testing.T) {
	var sentDriver string
	var sentPayload []byte
	s := newForceStartServer(t,
		func(string) (string, string, bool) { return "tesla-vehicle", "Stopped", true },
		func(_ context.Context, driver string, payload []byte) error {
			sentDriver = driver
			sentPayload = payload
			return nil
		},
	)
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/force_start", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if sentDriver != "tesla-vehicle" {
		t.Errorf("sent to driver %q, want tesla-vehicle", sentDriver)
	}
	if !containsAny(string(sentPayload), `"action":"charge_start"`) {
		t.Errorf("payload did not include the charge_start action: %s", sentPayload)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["ok"] != true {
		t.Errorf("response ok=%v, want true", body["ok"])
	}
	if body["vehicle_driver"] != "tesla-vehicle" {
		t.Errorf("response vehicle_driver=%v, want tesla-vehicle", body["vehicle_driver"])
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if idx := indexOf(haystack, n); idx >= 0 {
			return true
		}
	}
	return false
}

// indexOf is a local strings.Contains-ish helper to keep this file
// from importing strings just for one call.
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
