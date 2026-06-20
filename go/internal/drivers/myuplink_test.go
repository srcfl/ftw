package drivers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
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
// server and asserts (1) it authenticates with the refresh_token grant the
// MyUplink portal actually supports (NOT client_credentials, which produced
// the invalid_client failure in #496), (2) the compressor-power + temperature
// metrics land in the telemetry store, and (3) a rotated refresh_token is
// persisted via host.persist_secret so it survives a restart.
func TestMyUplinkEmitsTelemetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "grant_type=refresh_token") {
				t.Errorf("expected refresh_token grant, got body %q", string(body))
			}
			if !strings.Contains(string(body), "refresh_token=RT-initial") {
				t.Errorf("expected the configured refresh_token in the request, got %q", string(body))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "test-token",
				"expires_in":    3600,
				"refresh_token": "RT-rotated", // Azure B2C rotates on refresh
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
	persisted := map[string]string{}
	env := NewHostEnv("myuplink", tel).WithHTTP()
	env.PersistSecret = func(k, v string) error { persisted[k] = v; return nil }

	d, err := NewLuaDriver(myuplinkDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	cfg := map[string]any{
		"client_id":     "cid",
		"client_secret": "csecret",
		"refresh_token": "RT-initial",
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
	if persisted["refresh_token"] != "RT-rotated" {
		t.Errorf("rotated refresh_token not persisted: got %q, want RT-rotated", persisted["refresh_token"])
	}
}

// TestMyUplinkNoRefreshTokenIdles verifies the driver degrades gracefully
// when it has not been connected yet (no refresh_token in config): init must
// not error or crash, and a poll must not panic — it simply emits nothing.
func TestMyUplinkNoRefreshTokenIdles(t *testing.T) {
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
		// no refresh_token — user hasn't completed the OAuth connect yet
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init should not error when not yet connected: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll should not error when not yet connected: %v", err)
	}
	if _, _, ok := tel.LatestMetric("myuplink", "hp_power_w"); ok {
		t.Errorf("expected no telemetry before OAuth connect")
	}
}
