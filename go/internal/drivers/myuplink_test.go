package drivers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// myuplinkDriverPath resolves drivers/myuplink.lua from the repo root regardless
// of the test's working directory (tests run in go/internal/drivers/).
func myuplinkDriverPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = <repo>/go/internal/drivers/myuplink_test.go → up 3 to <repo>.
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(repo, "drivers", "myuplink.lua")
}

// TestMyUplinkEmitsTelemetry loads the real driver against a fake MyUplink
// server and asserts the compressor-power metric lands in the telemetry store.
func TestMyUplinkEmitsTelemetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-token", "expires_in": 3600,
			})
		default:
			// /v2/devices/{id}/points
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"parameterId": "10012", "value": "1500", "unit": "W"},
				{"parameterId": "40013", "value": "520"}, // 52.0 °C (×10 encoding)
			})
		}
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("myuplink", tel).WithHTTP()

	d, err := NewLuaDriver(myuplinkDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	cfg := map[string]any{
		"client_id":     "cid",
		"client_secret": "csecret",
		"device_id":     "DEV1",
		"base_url":      srv.URL,
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if v, _, ok := tel.LatestMetric("myuplink", "hp_power_w"); !ok || v != 1500 {
		t.Errorf("hp_power_w = %v (ok=%v), want 1500", v, ok)
	}
	if v, _, ok := tel.LatestMetric("myuplink", "hp_hw_top_temp_c"); !ok || v != 52 {
		t.Errorf("hp_hw_top_temp_c = %v (ok=%v), want 52", v, ok)
	}
}
