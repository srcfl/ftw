package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func newBatteryBoostServer(t *testing.T) (*Server, *loadpoint.Controller) {
	t.Helper()
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger"}})
	mgr.Observe("garage", true, 2500, 1000, true)
	ctrl := loadpoint.NewController(mgr, nil, nil, nil)
	return New(&Deps{Loadpoints: mgr, LoadpointCtrl: ctrl}), ctrl
}

func batteryBoostAPIRequest(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestBatteryBoostAPIEnableStatusCancelLifecycle(t *testing.T) {
	srv, ctrl := newBatteryBoostServer(t)
	rr := batteryBoostAPIRequest(t, srv, http.MethodPost, "/api/loadpoints/garage/battery_boost",
		`{"duration_s":3600,"min_battery_soc_pct":30,"ev_target_soc_pct":80}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rr.Code, rr.Body.String())
	}
	var status loadpoint.BatteryBoostStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil || !status.Active || status.EVTargetSoCPct != 80 {
		t.Fatalf("enable status = %+v, err=%v", status, err)
	}

	rr = batteryBoostAPIRequest(t, srv, http.MethodGet, "/api/loadpoints/garage/battery_boost", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"active":true`) {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}

	rr = batteryBoostAPIRequest(t, srv, http.MethodDelete, "/api/loadpoints/garage/battery_boost", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"stop_reason":"cancelled"`) {
		t.Fatalf("cancel = %d: %s", rr.Code, rr.Body.String())
	}
	if _, got := ctrl.BatteryBoost("garage", time.Now()); got.Active {
		t.Fatal("lease remains active after DELETE")
	}
}

func TestBatteryBoostAPIMutationRejectsUnauthenticatedRemoteRequest(t *testing.T) {
	srv, ctrl := newBatteryBoostServer(t)
	srv.deps.MutationPolicy = MutationPolicy{
		RequireTokenForRemote: true,
		Token:                 testMutationToken,
	}
	req := httptest.NewRequest(http.MethodPost,
		"https://ftw.example.com/api/loadpoints/garage/battery_boost",
		strings.NewReader(`{"duration_s":3600,"min_battery_soc_pct":30}`))
	req.RemoteAddr = "203.0.113.10:43210"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://ftw.example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
	if _, status := ctrl.BatteryBoost("garage", time.Now()); status.Active {
		t.Fatal("remote request installed a boost lease without a token")
	}
}

func TestBatteryBoostAPIValidatesBoundedContract(t *testing.T) {
	srv, _ := newBatteryBoostServer(t)
	expires := time.Now().Add(time.Hour).UnixMilli()
	tests := []struct {
		name string
		body string
	}{
		{"missing time", `{"min_battery_soc_pct":30}`},
		{"two times", `{"duration_s":60,"expires_at_ms":` + jsonNumber(expires) + `,"min_battery_soc_pct":30}`},
		{"too short", `{"duration_s":30,"min_battery_soc_pct":30}`},
		{"too long", `{"duration_s":14401,"min_battery_soc_pct":30}`},
		{"bad reserve", `{"duration_s":3600,"min_battery_soc_pct":101}`},
		{"bad target", `{"duration_s":3600,"min_battery_soc_pct":30,"ev_target_soc_pct":101}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := batteryBoostAPIRequest(t, srv, http.MethodPost, "/api/loadpoints/garage/battery_boost", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestBatteryBoostAppearsOnLoadpointState(t *testing.T) {
	srv, _ := newBatteryBoostServer(t)
	rr := batteryBoostAPIRequest(t, srv, http.MethodPost, "/api/loadpoints/garage/battery_boost",
		`{"duration_s":3600,"min_battery_soc_pct":25}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rr.Code, rr.Body.String())
	}
	rr = batteryBoostAPIRequest(t, srv, http.MethodGet, "/api/loadpoints", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"battery_boost":{"state":"active","active":true`) {
		t.Fatalf("loadpoints = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestBatteryBoostAPIRejectsUnknownOrUnavailableLoadpoint(t *testing.T) {
	srv, _ := newBatteryBoostServer(t)
	rr := batteryBoostAPIRequest(t, srv, http.MethodGet, "/api/loadpoints/missing/battery_boost", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d", rr.Code)
	}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger"}})
	srv = New(&Deps{Loadpoints: mgr})
	rr = batteryBoostAPIRequest(t, srv, http.MethodGet, "/api/loadpoints/garage/battery_boost", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable = %d", rr.Code)
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
