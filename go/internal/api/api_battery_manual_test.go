package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Battery manual-hold endpoint tests. Validation, route wiring, and
// the install → read → clear lifecycle.

func newBatteryHoldServer(t *testing.T) (*Server, *control.State, *sync.Mutex) {
	t.Helper()
	st := control.NewState(0, 50, "ferroamp")
	mu := &sync.Mutex{}
	srv := New(&Deps{Ctrl: st, CtrlMu: mu})
	return srv, st, mu
}

func newScopedBatteryHoldServer(t *testing.T) (*Server, *control.State) {
	t.Helper()
	srv, st, _ := newBatteryHoldServer(t)
	soc := 0.5
	tel := telemetry.NewStore()
	tel.Update("bat_a", telemetry.DerBattery, 0, &soc, nil)
	tel.DriverHealthMut("bat_a").RecordSuccess()
	srv.deps.Tel = tel
	srv.deps.CapMu = &sync.RWMutex{}
	srv.deps.Capacities = map[string]float64{"bat_a": 10000}
	srv.deps.BatteryIdentity = func(name string) (string, bool) {
		return "maker:serial-a", name == "bat_a"
	}
	return srv, st
}

func TestBatteryHoldUnavailableWithoutCtrl(t *testing.T) {
	srv := New(&Deps{}) // no Ctrl/CtrlMu
	body := `{"direction":"charge","power_w":3000,"hold_s":60}`
	req := httptest.NewRequest(http.MethodPost, "/api/battery/manual_hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestBatteryHoldValidation(t *testing.T) {
	srv, _, _ := newBatteryHoldServer(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing hold_s", `{"direction":"charge","power_w":3000}`, http.StatusBadRequest},
		{"zero hold_s", `{"direction":"charge","power_w":3000,"hold_s":0}`, http.StatusBadRequest},
		{"hold_s too large", `{"direction":"charge","power_w":3000,"hold_s":99999}`, http.StatusBadRequest},
		{"negative power", `{"direction":"charge","power_w":-1,"hold_s":60}`, http.StatusBadRequest},
		{"unknown direction", `{"direction":"explode","power_w":3000,"hold_s":60}`, http.StatusBadRequest},
		{"empty direction", `{"power_w":3000,"hold_s":60}`, http.StatusBadRequest},
		{"malformed json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/battery/manual_hold", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("body=%s: status = %d, want %d (resp %s)",
					tc.body, rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestBatteryHoldInstallChargeAndReadBack(t *testing.T) {
	srv, st, _ := newBatteryHoldServer(t)
	body := `{"direction":"charge","power_w":3000,"hold_s":60}`
	req := httptest.NewRequest(http.MethodPost, "/api/battery/manual_hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", rr.Code, rr.Body.String())
	}
	h, active := st.GetBatteryManualHold(time.Now())
	if !active {
		t.Fatalf("hold not installed in State")
	}
	if h.PowerW != 3000 {
		t.Errorf("PowerW=%v, want 3000 (charge → site-signed +)", h.PowerW)
	}

	// GET round-trip.
	getReq := httptest.NewRequest(http.MethodGet, "/api/battery/manual_hold", nil)
	getRR := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET status=%d", getRR.Code)
	}
	var got batteryManualHoldResponse
	if err := json.Unmarshal(getRR.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Active || got.Direction != "charge" || got.PowerW != 3000 {
		t.Errorf("GET body mismatch: %+v", got)
	}
}

func TestBatteryHoldDischargeSignsNegative(t *testing.T) {
	srv, st, _ := newBatteryHoldServer(t)
	body := `{"direction":"discharge","power_w":2500,"hold_s":120}`
	req := httptest.NewRequest(http.MethodPost, "/api/battery/manual_hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", rr.Code, rr.Body.String())
	}
	h, active := st.GetBatteryManualHold(time.Now())
	if !active {
		t.Fatalf("hold not installed")
	}
	if h.PowerW != -2500 {
		t.Errorf("PowerW=%v, want -2500 (discharge → site-signed −)", h.PowerW)
	}
}

func TestBatteryHoldIdleSetsZero(t *testing.T) {
	srv, st, _ := newBatteryHoldServer(t)
	body := `{"direction":"idle","power_w":1234,"hold_s":60}`
	req := httptest.NewRequest(http.MethodPost, "/api/battery/manual_hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", rr.Code, rr.Body.String())
	}
	h, active := st.GetBatteryManualHold(time.Now())
	if !active {
		t.Fatalf("hold not installed")
	}
	// Idle ignores any provided magnitude — power_w is zeroed.
	if h.PowerW != 0 {
		t.Errorf("idle direction should zero PowerW, got %v", h.PowerW)
	}
}

func TestBatteryHoldDelete(t *testing.T) {
	srv, st, _ := newBatteryHoldServer(t)
	st.SetBatteryManualHold(control.BatteryManualHold{
		PowerW:    -2000,
		ExpiresAt: time.Now().Add(60 * time.Second),
	})
	if _, ok := st.GetBatteryManualHold(time.Now()); !ok {
		t.Fatalf("precondition: hold should be active")
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/battery/manual_hold", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d", rr.Code)
	}
	if _, ok := st.GetBatteryManualHold(time.Now()); ok {
		t.Errorf("DELETE should clear the hold")
	}
}

func TestBatteryHoldGetReturnsInactiveWhenNoHold(t *testing.T) {
	srv, _, _ := newBatteryHoldServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/battery/manual_hold", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status=%d", rr.Code)
	}
	var got batteryManualHoldResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Active {
		t.Errorf("GET on empty state should report inactive, got %+v", got)
	}
}

func TestBatteryHoldInstallsHardwareBoundDriverScope(t *testing.T) {
	srv, st := newScopedBatteryHoldServer(t)
	body := `{"direction":"charge","power_w":3000,"hold_s":60,"driver":"bat_a"}`
	req := httptest.NewRequest(http.MethodPost, "/api/battery/manual_hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", rr.Code, rr.Body.String())
	}
	hold, active := st.GetBatteryManualHold(time.Now())
	if !active || hold.Driver != "bat_a" || hold.DeviceID != "maker:serial-a" {
		t.Fatalf("scoped hold = %+v, active=%v", hold, active)
	}
	var got batteryManualHoldResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Driver != hold.Driver {
		t.Fatalf("response = %+v, hold = %+v", got, hold)
	}
}

func TestBatteryHoldDriverScopeFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Server)
		want   int
	}{
		{"unknown driver", func(*Server) {}, http.StatusBadRequest},
		{"offline driver", func(s *Server) { s.deps.Capacities["ghost"] = 10000 }, http.StatusConflict},
		{"missing identity", func(s *Server) {
			s.deps.Capacities["ghost"] = 10000
			soc := 0.5
			s.deps.Tel.Update("ghost", telemetry.DerBattery, 0, &soc, nil)
			s.deps.Tel.DriverHealthMut("ghost").RecordSuccess()
		}, http.StatusConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newScopedBatteryHoldServer(t)
			tc.mutate(srv)
			body := `{"direction":"charge","power_w":3000,"hold_s":60,"driver":"ghost"}`
			req := httptest.NewRequest(http.MethodPost, "/api/battery/manual_hold", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status=%d, want %d body=%s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}
