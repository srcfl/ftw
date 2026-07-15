package drivers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// nibeLocalDriverPath resolves drivers/nibe_local.lua from the repo root
// regardless of the test's working directory (tests run in go/internal/drivers/).
func nibeLocalDriverPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(repo, "drivers", "nibe_local.lua")
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// TestNibeLocalEmitsTelemetry loads the real driver against a fake NIBE
// Local REST API and asserts: (1) it sends HTTP Basic auth, (2) it detects
// the serial from /api/v1/devices and sets the SN, (3) canonical headline
// metrics land with EXACT per-point divisor scaling (no °C×10 guessing),
// (4) the "not connected" s16 sentinel (-32768) is filtered out even though
// the API reports isOk=true, and (5) every other point auto-emits as a
// sanitized hp_<name> with soft hyphens stripped.
func TestNibeLocalEmitsTelemetry(t *testing.T) {
	const wantUser, wantPass = "localuser", "secret-pass"
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(wantUser+":"+wantPass))

	// Build the points map exactly as the pump returns it: object keyed by
	// variableId, each with metadata{divisor,unit,variableSize} + value.
	point := func(title, size, unit string, divisor, integerValue int) map[string]any {
		return map[string]any{
			"title": title,
			"metadata": map[string]any{
				"variableSize": size,
				"unit":         unit,
				"divisor":      divisor,
				"isWritable":   false,
			},
			"value": map[string]any{"type": "datavalue", "isOk": true, "integerValue": integerValue},
		}
	}
	points := map[string]any{
		"1801":  point("Compres­sor power input", "u16", "W", 1, 1500),             // hp_power_w
		"4":     point("Current outdoor temper­ature (BT1)", "s16", "°C", 10, 294), // 29.4
		"11":    point("Hot water top (BT7)", "s16", "°C", 10, 570),                // 57.0
		"8":     point("Supply line (BT2)", "s16", "°C", 10, 449),                  // auto → hp_supply_line_bt2 44.9
		"5":     point("Supply line (EP23-BT2)", "s16", "°C", 10, -32768),          // sentinel → skipped
		"28393": point("Tot. consump­tion", "u32", "kWh", 10, 53999),               // hp_energy_consumed_kwh 5399.9
		"9001":  point("Duplicate title", "u16", "", 1, 10),
		"9002":  point("Duplicate title", "u16", "", 1, 20),
	}

	// Attach a Modbus register id to each point's metadata (the pump reports
	// modbusRegisterID per point); the driver threads it into the live snapshot.
	// Injected here so the soft-hyphen titles above stay byte-for-byte untouched.
	for id, reg := range map[string]int{"1801": 1048, "4": 1, "11": 8, "8": 5, "28393": 2166} {
		points[id].(map[string]any)["metadata"].(map[string]any)["modbusRegisterID"] = reg
	}

	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/v1/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"devices": []map[string]any{
					{"product": map[string]any{
						"serialNumber": "06613225140002",
						"manufacturer": "NIBE",
						"firmwareId":   "nibe-n",
					}, "deviceIndex": 0, "aidMode": "off"},
				},
			})
		case "/api/v1/devices/06613225140002/points":
			_ = json.NewEncoder(w).Encode(points)
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("nibe", tel).WithHTTP()
	d, err := NewLuaDriver(nibeLocalDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	cfg := map[string]any{
		"username": wantUser,
		"password": wantPass,
		"base_url": srv.URL,
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if sawAuth != wantAuth {
		t.Errorf("Authorization header = %q, want %q", sawAuth, wantAuth)
	}
	if got := env.SN; got != "06613225140002" {
		t.Errorf("serial not anchored: SN=%q, want 06613225140002", got)
	}

	// Canonical headline metrics with exact scaling.
	wantMetric := map[string]float64{
		"hp_power_w":             1500,   // W, div 1
		"hp_outdoor_temp_c":      29.4,   // 294 / 10
		"hp_hw_top_temp_c":       57.0,   // 570 / 10
		"hp_energy_consumed_kwh": 5399.9, // 53999 / 10
		"hp_supply_line_bt2":     44.9,   // auto-named, 449 / 10
	}
	for name, want := range wantMetric {
		v, _, ok := tel.LatestMetric("nibe", name)
		if !ok {
			t.Errorf("metric %s missing", name)
			continue
		}
		if !approxEq(v, want) {
			t.Errorf("metric %s = %v, want %v", name, v, want)
		}
	}

	// The Modbus register id rides along into the live snapshot so the
	// per-driver "all signals" detail view can show each signal's source
	// register — for canonical aliases (hp_power_w) and auto-named metrics alike.
	regByName := map[string]string{}
	for _, m := range tel.LatestMetricsByDriver("nibe") {
		regByName[m.Name] = m.Register
	}
	for name, want := range map[string]string{
		"hp_power_w":         "1048", // canonical alias keeps its point's register
		"hp_supply_line_bt2": "5",    // auto-named metric
	} {
		if regByName[name] != want {
			t.Errorf("metric %s register = %q, want %q", name, regByName[name], want)
		}
	}

	// The disconnected supply-line sensor (-32768 sentinel, isOk=true) must
	// NOT be emitted — filtering by size is the whole point.
	if _, _, ok := tel.LatestMetric("nibe", "hp_supply_line_ep23_bt2"); ok {
		t.Error("hp_supply_line_ep23_bt2 was emitted; the -32768 sentinel should be filtered")
	}
	// Canonical ids must not ALSO leak under their auto-sanitized names.
	if _, _, ok := tel.LatestMetric("nibe", "hp_compressor_power_input"); ok {
		t.Error("compressor power leaked under auto name; canonical id should be emitted once as hp_power_w")
	}
	// Sanitized-title collisions must be deterministic: both colliding points
	// carry their id, independent of Lua table iteration order.
	for name, want := range map[string]float64{
		"hp_duplicate_title_9001": 10,
		"hp_duplicate_title_9002": 20,
	} {
		if v, _, ok := tel.LatestMetric("nibe", name); !ok || !approxEq(v, want) {
			t.Errorf("metric %s = %v (ok=%v), want %v", name, v, ok, want)
		}
	}
	if _, _, ok := tel.LatestMetric("nibe", "hp_duplicate_title"); ok {
		t.Error("colliding metric leaked under an unstable unsuffixed name")
	}

	// A second unchanged poll keeps headline resolution but does not enqueue
	// every non-headline point again. This bounds TS growth for the ~980-point
	// map while hp_poll_ok still keeps the watchdog fresh.
	_ = tel.FlushSamples()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	second := tel.FlushSamples()
	seenSecond := map[string]bool{}
	for _, s := range second {
		seenSecond[s.Metric] = true
	}
	if seenSecond["hp_supply_line_bt2"] || seenSecond["hp_duplicate_title_9001"] {
		t.Fatalf("unchanged non-headline metrics were re-emitted: %#v", seenSecond)
	}
	if !seenSecond["hp_power_w"] || !seenSecond["hp_poll_ok"] {
		t.Fatalf("second poll must retain headline + health heartbeat: %#v", seenSecond)
	}
}

// TestNibeLocalLive exercises the driver against a real NIBE pump. Skipped
// unless NIBE_LIVE=1; provide NIBE_HOST, NIBE_PORT (8443), NIBE_USER,
// NIBE_PASS, NIBE_PIN (the cert SHA-256 fingerprint).
func TestNibeLocalLive(t *testing.T) {
	if os.Getenv("NIBE_LIVE") != "1" {
		t.Skip("set NIBE_LIVE=1 + NIBE_USER/NIBE_PASS/NIBE_PIN to run against a real pump")
	}
	envOr := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	host := envOr("NIBE_HOST", "192.168.1.180")
	port := envOr("NIBE_PORT", "8443")

	env := NewHostEnv("nibe", telemetry.NewStore()).WithHTTP().
		WithHTTPAllowedHosts([]string{host + ":" + port}).
		WithHTTPTLSPin(os.Getenv("NIBE_PIN"))
	d, err := NewLuaDriver(nibeLocalDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	cfg := map[string]any{
		"host":     host,
		"port":     port,
		"username": os.Getenv("NIBE_USER"),
		"password": os.Getenv("NIBE_PASS"),
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if env.SN == "" {
		t.Fatal("no serial detected from the live pump")
	}
	t.Logf("live pump serial=%s", env.SN)
	for _, name := range []string{
		"hp_power_w", "hp_used_power_w", "hp_outdoor_temp_c",
		"hp_hw_top_temp_c", "hp_energy_consumed_kwh", "hp_energy_produced_kwh",
		"hp_degree_minutes",
	} {
		if v, _, ok := env.Telemetry.LatestMetric("nibe", name); ok {
			t.Logf("  %-24s = %v", name, v)
		} else {
			t.Logf("  %-24s = (absent)", name)
		}
	}
	if _, _, ok := env.Telemetry.LatestMetric("nibe", "hp_outdoor_temp_c"); !ok {
		t.Error("expected hp_outdoor_temp_c from a live pump")
	}
}

// TestNibeLocalHeadlineOverride proves per-model headline resolution: a
// config override (param_power_id) wins over the built-in profile default,
// the model name is captured, and the override survives the post-detection
// profile rebuild in try_setup. The overridden default id then falls through
// to its auto-generated name instead of vanishing.
func TestNibeLocalHeadlineOverride(t *testing.T) {
	point := func(title, size, unit string, divisor, integerValue int) map[string]any {
		return map[string]any{
			"title": title,
			"metadata": map[string]any{
				"variableSize": size, "unit": unit, "divisor": divisor, "isWritable": false,
			},
			"value": map[string]any{"type": "datavalue", "isOk": true, "integerValue": integerValue},
		}
	}
	points := map[string]any{
		"1801": point("Compressor power input", "u16", "W", 1, 1500),   // built-in default id
		"9999": point("Custom whole-house power", "u32", "W", 1, 2222), // override target
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"devices": []map[string]any{
					{"product": map[string]any{
						"serialNumber": "06613225140002", "manufacturer": "NIBE",
						"name": "NIBE S735", "firmwareId": "nibe-n",
					}},
				},
			})
		case "/api/v1/devices/06613225140002/points":
			_ = json.NewEncoder(w).Encode(points)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	d, err := NewLuaDriver(nibeLocalDriverPath(t), NewHostEnv("nibe", tel).WithHTTP())
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()
	cfg := map[string]any{
		"username":       "u",
		"password":       "p",
		"base_url":       srv.URL,
		"param_power_id": "9999", // override the default compressor-power id
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// hp_power_w must follow the override (id 9999 = 2222 W), not the default 1801.
	if v, _, ok := tel.LatestMetric("nibe", "hp_power_w"); !ok || !approxEq(v, 2222) {
		t.Errorf("hp_power_w = %v (ok=%v), want 2222 from override id 9999", v, ok)
	}
	// The now-unmapped default id 1801 should surface under its auto name.
	if v, _, ok := tel.LatestMetric("nibe", "hp_compressor_power_input"); !ok || !approxEq(v, 1500) {
		t.Errorf("overridden default id should fall through to hp_compressor_power_input=1500, got %v (ok=%v)", v, ok)
	}
}
