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

// TestMyUplinkAutoDetectsDevice exercises the device auto-detection path
// (no device_id in config) against the REAL MyUplink response shape:
// GET /v2/systems/me returns {"systems":[{"devices":[{"id":...}]}]} — the
// top-level key is "systems", not "objects" (the bug behind "connects but
// finds nothing"). Points use "parameterUnit", not "unit".
func TestMyUplinkAutoDetectsDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "AT", "expires_in": 3600, "refresh_token": "RT",
			})
		case r.URL.Path == "/v2/systems/me":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"page": 1, "itemsPerPage": 10, "numItems": 1,
				"systems": []map[string]any{
					{"systemId": "sys-1", "name": "Home", "devices": []map[string]any{
						{"id": "DEV-AUTO"},
					}},
				},
			})
		default: // /v2/devices/DEV-AUTO/points
			if !strings.Contains(r.URL.Path, "DEV-AUTO") {
				t.Errorf("points fetched for wrong device: %s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"parameterId": "10012", "value": "2", "parameterUnit": "kW"}, // 2 kW → 2000 W
			})
		}
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("myuplink", tel).WithHTTP()
	env.PersistSecret = func(k, v string) error { return nil }

	d, err := NewLuaDriver(myuplinkDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	cfg := map[string]any{
		"client_id":     "cid",
		"client_secret": "csec",
		"refresh_token": "RT",
		// no device_id → must auto-detect via /v2/systems/me
		"base_url": srv.URL,
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// 2 kW must be converted to 2000 W via parameterUnit.
	if v, _, ok := tel.LatestMetric("myuplink", "hp_power_w"); !ok || v != 2000 {
		t.Errorf("hp_power_w = %v (ok=%v), want 2000 (2 kW via parameterUnit)", v, ok)
	}
}

// TestMyUplinkEmitsAllPointsWithUnits verifies the driver fetches the full
// points list and emits every numeric point as hp_<sanitized name> with its
// unit, while still emitting the four canonical headline metrics from their
// fixed parameter IDs (and not double-emitting those four as generic).
func TestMyUplinkEmitsAllPointsWithUnits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "AT", "expires_in": 3600, "refresh_token": "RT",
			})
			return
		}
		// /v2/devices/DEV1/points — full list (driver must NOT send ?parameters)
		if q := r.URL.RawQuery; q != "" {
			t.Errorf("expected unfiltered points fetch, got query %q", q)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"parameterId": "10012", "parameterName": "Compressor power", "value": "1500", "parameterUnit": "W"},    // canonical
			{"parameterId": "40004", "parameterName": "Outdoor temp (BT1)", "value": "226", "parameterUnit": "°C"},  // canonical
			{"parameterId": "40008", "parameterName": "Supply line (BT2)", "value": "455", "parameterUnit": "°C"},   // generic temp, ×10
			{"parameterId": "43136", "parameterName": "Compressor frequency", "value": "42", "parameterUnit": "Hz"}, // generic
			{"parameterId": "43005", "parameterName": "Degree minutes", "value": "-600", "parameterUnit": "GM"},     // generic, negative
		})
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("myuplink", tel).WithHTTP()
	env.PersistSecret = func(k, v string) error { return nil }

	d, err := NewLuaDriver(myuplinkDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()
	cfg := map[string]any{
		"client_id": "cid", "client_secret": "csec", "refresh_token": "RT",
		"device_id": "DEV1", "base_url": srv.URL,
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	byName := map[string]telemetry.MetricSnapshot{}
	for _, m := range tel.LatestMetricsByDriver("myuplink") {
		byName[m.Name] = m
	}
	// Canonical headline metrics still present.
	if m, ok := byName["hp_power_w"]; !ok || m.Value != 1500 {
		t.Errorf("hp_power_w = %+v, want value 1500", m)
	}
	if m, ok := byName["hp_outdoor_temp_c"]; !ok || m.Value != 22.6 {
		t.Errorf("hp_outdoor_temp_c = %+v, want 22.6", m)
	}
	// Generic points emitted with sanitized names + units.
	supply, ok := byName["hp_supply_line_bt2"]
	if !ok {
		t.Fatalf("missing generic metric hp_supply_line_bt2; got %v", keysOf(byName))
	}
	if supply.Unit != "°C" || supply.Value != 45.5 {
		t.Errorf("supply = %+v, want 45.5 °C (×10 decode for °C unit)", supply)
	}
	if hz, ok := byName["hp_compressor_frequency"]; !ok || hz.Unit != "Hz" || hz.Value != 42 {
		t.Errorf("compressor frequency = %+v, want 42 Hz (no scaling)", hz)
	}
	if dm, ok := byName["hp_degree_minutes"]; !ok || dm.Value != -600 {
		t.Errorf("degree minutes = %+v, want -600 (no scaling for non-°C)", dm)
	}
	// Canonical IDs must NOT be double-emitted under generic names.
	if _, ok := byName["hp_compressor_power"]; ok {
		t.Errorf("canonical PARAM_POWER (10012) was double-emitted as hp_compressor_power")
	}
	if _, ok := byName["hp_outdoor_temp_bt1"]; ok {
		t.Errorf("canonical PARAM_OUTDOOR (40004) was double-emitted as a generic name")
	}
}

func keysOf(m map[string]telemetry.MetricSnapshot) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestMyUplinkSelfHealsAfterTransientAuthFailure reproduces the "only works
// after a manual restart" bug: NIBE/MyUplink is touchy right after consent,
// so the first token request can fail. The driver must retry setup in
// driver_poll (with backoff) instead of idling forever on a nil device_id.
func TestMyUplinkSelfHealsAfterTransientAuthFailure(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			tokenCalls++
			if tokenCalls == 1 {
				// First attempt fails (transient, as NIBE does post-consent).
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "AT", "expires_in": 3600, "refresh_token": "RT",
			})
		case "/v2/systems/me":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"systems": []map[string]any{{"devices": []map[string]any{{"id": "DEV1"}}}},
			})
		default: // points
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"parameterId": "10012", "value": "900", "parameterUnit": "W"},
			})
		}
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("myuplink", tel).WithHTTP()
	env.PersistSecret = func(k, v string) error { return nil }

	d, err := NewLuaDriver(myuplinkDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()
	cfg := map[string]any{
		"client_id": "cid", "client_secret": "csec", "refresh_token": "RT",
		"base_url":       srv.URL,
		"setup_retry_ms": 0, // no backoff delay in the test
	}
	// Init's first auth fails — driver must NOT permanently give up.
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, _, ok := tel.LatestMetric("myuplink", "hp_power_w"); ok {
		t.Fatal("should not have telemetry yet (first auth failed)")
	}
	// A subsequent poll must retry setup (auth now succeeds) and recover —
	// no manual restart needed. May take two polls (retry, then emit).
	for i := 0; i < 3; i++ {
		if _, err := d.Poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if v, _, ok := tel.LatestMetric("myuplink", "hp_power_w"); !ok || v != 900 {
		t.Errorf("hp_power_w = %v (ok=%v), want 900 — driver should self-heal after the transient auth failure", v, ok)
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
