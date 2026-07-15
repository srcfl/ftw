package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// mkESPHomeStub spins up an httptest.Server that mimics the
// ESPHome `web_server: version: 3` REST surface. Each entity is
// served at /sensor/<id> or /text_sensor/<id> using the JSON
// shape {"value": <v>, "id": "...", ...} that the live device
// returns. Unknown IDs return 404 — the driver treats this as
// "this entity isn't on this device" and falls back accordingly.
//
// values: maps "sensor/<id>" or "text_sensor/<id>" → JSON value
// payload (numeric or quoted string, formatted as it would appear
// inside the `"value"` field of the response).
func mkESPHomeStub(values map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		v, ok := values[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// `state` deliberately omitted — quoting it without
		// double-quoting text-sensor values would require
		// per-call book-keeping the driver doesn't need.
		fmt.Fprintf(w, `{"name_id":"%s","id":"%s","value":%s}`,
			path, strings.ReplaceAll(path, "/", "-"), v)
	}))
}

// loadESPHomeDriver builds a HostEnv with HTTP enabled (no
// allowlist — empty allowlist is "allow all" in HostEnv), loads
// the driver, runs init pointed at host (sans scheme), and runs
// one poll. Returns the telemetry store + env so callers can
// assert on telemetry + identity.
func loadESPHomeDriver(t *testing.T, host string, configOverrides map[string]any) (*telemetry.Store, *HostEnv) {
	t.Helper()
	tel := telemetry.NewStore()
	env := NewHostEnv("zap-p1", tel).WithHTTP()

	d, err := NewLuaDriver("../../../drivers/esphome_dsmr.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(d.Cleanup)

	cfg := map[string]any{"host": host, "poll_ms": 1000}
	for k, v := range configOverrides {
		cfg[k] = v
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	return tel, env
}

func TestESPHomeDSMR_ImportingSiteConvention(t *testing.T) {
	// power_consumed = 1.234 kW (importing), power_produced = 0.567 kW
	// → meter.w = (1.234 − 0.567) × 1000 = +667 W (site convention:
	// positive = importing). Per-phase split arbitrary but must sum
	// to the same value.
	srv := mkESPHomeStub(map[string]string{
		"sensor/power_consumed":                   "1.234",
		"sensor/power_produced":                   "0.567",
		"sensor/power_consumed_l1":                "0.500",
		"sensor/power_consumed_l2":                "0.400",
		"sensor/power_consumed_l3":                "0.334",
		"sensor/power_produced_l1":                "0.100",
		"sensor/power_produced_l2":                "0.200",
		"sensor/power_produced_l3":                "0.267",
		"sensor/voltage_l1":                       "230.1",
		"sensor/voltage_l2":                       "229.8",
		"sensor/voltage_l3":                       "230.5",
		"sensor/current_l1":                       "2.5",
		"sensor/current_l2":                       "1.8",
		"sensor/current_l3":                       "0.4",
		"sensor/energy_consumed":                  "1234.567", // kWh → 1,234,567 Wh
		"sensor/energy_produced":                  "56.789",   // kWh →    56,789 Wh
		"text_sensor/electric_meter_equipment_id": "\"\"",     // empty → fall through
		"text_sensor/meter_identification":        "\"LGF5E360\"",
	})
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tel, env := loadESPHomeDriver(t, host, nil)

	// Identity: make pinned to "ESPHome" by default; serial falls
	// through equipment_id (empty) → meter_identification "LGF5E360".
	mk, sn := env.Identity()
	if mk != "ESPHome" {
		t.Errorf("make = %q, want ESPHome", mk)
	}
	if sn != "LGF5E360" {
		t.Errorf("serial = %q, want LGF5E360 (fallback path)", sn)
	}

	m := tel.Get("zap-p1", telemetry.DerMeter)
	if m == nil {
		t.Fatal("expected meter telemetry")
	}
	if !near(m.RawW, 667) {
		t.Errorf("meter.w = %v, want +667 (consumed − produced × 1000)", m.RawW)
	}

	var data map[string]any
	if err := json.Unmarshal(m.Data, &data); err != nil {
		t.Fatalf("meter data: %v", err)
	}
	if !near(data["l1_w"].(float64), 400) {
		t.Errorf("l1_w = %v, want 400 (0.500−0.100)·1000", data["l1_w"])
	}
	if !near(data["l2_w"].(float64), 200) {
		t.Errorf("l2_w = %v, want 200", data["l2_w"])
	}
	// l3: 0.334 − 0.267 = 0.067 → 67 W (FP rounding gives 66.99…)
	if !near(data["l3_w"].(float64), 67) {
		t.Errorf("l3_w = %v, want 67", data["l3_w"])
	}
	if !near(data["l1_v"].(float64), 230.1) {
		t.Errorf("l1_v = %v, want 230.1", data["l1_v"])
	}
	if !near(data["l2_a"].(float64), 1.8) {
		t.Errorf("l2_a = %v, want 1.8", data["l2_a"])
	}
	if !near(data["import_wh"].(float64), 1234567) {
		t.Errorf("import_wh = %v, want 1234567", data["import_wh"])
	}
	if !near(data["export_wh"].(float64), 56789) {
		t.Errorf("export_wh = %v, want 56789", data["export_wh"])
	}
}

func TestESPHomeDSMR_ExportingSign(t *testing.T) {
	// Consumed=0, produced=0.5 kW → meter.w must be negative
	// (exporting). Phase L1 mirrors the total; L2/L3 zero.
	srv := mkESPHomeStub(map[string]string{
		"sensor/power_consumed":                   "0",
		"sensor/power_produced":                   "0.5",
		"sensor/power_consumed_l1":                "0",
		"sensor/power_consumed_l2":                "0",
		"sensor/power_consumed_l3":                "0",
		"sensor/power_produced_l1":                "0.5",
		"sensor/power_produced_l2":                "0",
		"sensor/power_produced_l3":                "0",
		"sensor/voltage_l1":                       "230",
		"sensor/voltage_l2":                       "230",
		"sensor/voltage_l3":                       "230",
		"sensor/current_l1":                       "2.2",
		"sensor/current_l2":                       "0",
		"sensor/current_l3":                       "0",
		"sensor/energy_consumed":                  "10",
		"sensor/energy_produced":                  "5",
		"text_sensor/electric_meter_equipment_id": "\"EM-12345\"",
		"text_sensor/meter_identification":        "\"LGF5E360\"",
	})
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tel, env := loadESPHomeDriver(t, host, nil)

	// Equipment_id non-empty wins over meter_identification.
	if _, sn := env.Identity(); sn != "EM-12345" {
		t.Errorf("serial = %q, want EM-12345 (equipment_id wins over identification)", sn)
	}

	m := tel.Get("zap-p1", telemetry.DerMeter)
	if m == nil {
		t.Fatal("expected meter telemetry")
	}
	if m.RawW >= 0 {
		t.Errorf("meter.w = %v, want negative (exporting)", m.RawW)
	}
	if !near(m.RawW, -500) {
		t.Errorf("meter.w = %v, want -500", m.RawW)
	}
	var data map[string]any
	_ = json.Unmarshal(m.Data, &data)
	if !near(data["l1_w"].(float64), -500) {
		t.Errorf("l1_w = %v, want -500 (export on L1 only)", data["l1_w"])
	}
}

func TestESPHomeDSMR_CustomMakeFromConfig(t *testing.T) {
	// Operator override: set make: "Sourceful" so the device shows
	// up under the right brand in the UI even though the driver is
	// generic ESPHome.
	srv := mkESPHomeStub(map[string]string{
		"sensor/power_consumed":                   "0",
		"sensor/power_produced":                   "0",
		"sensor/power_consumed_l1":                "0",
		"sensor/power_consumed_l2":                "0",
		"sensor/power_consumed_l3":                "0",
		"sensor/power_produced_l1":                "0",
		"sensor/power_produced_l2":                "0",
		"sensor/power_produced_l3":                "0",
		"sensor/voltage_l1":                       "230",
		"sensor/voltage_l2":                       "230",
		"sensor/voltage_l3":                       "230",
		"sensor/current_l1":                       "0",
		"sensor/current_l2":                       "0",
		"sensor/current_l3":                       "0",
		"sensor/energy_consumed":                  "0",
		"sensor/energy_produced":                  "0",
		"text_sensor/electric_meter_equipment_id": "\"\"",
		"text_sensor/meter_identification":        "\"\"",
	})
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	_, env := loadESPHomeDriver(t, host, map[string]any{"make": "Sourceful"})
	mk, sn := env.Identity()
	if mk != "Sourceful" {
		t.Errorf("make = %q, want Sourceful (config override)", mk)
	}
	if sn != "" {
		t.Errorf("serial = %q, want empty (no equipment_id, no identification)", sn)
	}
}

func TestESPHomeDSMROmitsFailedOptionalReadsAndRetriesSerial(t *testing.T) {
	values := map[string]string{
		"sensor/power_consumed": "1.2",
		"sensor/power_produced": "0.2",
		// Phase, current, voltage, energy, and serial endpoints are absent.
	}
	srv := mkESPHomeStub(values)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tel := telemetry.NewStore()
	env := NewHostEnv("zap-p1", tel).WithHTTP()
	d, err := NewLuaDriver("../../../drivers/esphome_dsmr.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(d.Cleanup)
	if err := d.Init(context.Background(), map[string]any{"host": host, "poll_ms": "1500"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	interval, err := d.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if interval.Milliseconds() != 1500 {
		t.Fatalf("string poll_ms interval = %d, want 1500", interval.Milliseconds())
	}

	reading := tel.Get("zap-p1", telemetry.DerMeter)
	if reading == nil {
		t.Fatal("missing mandatory meter reading")
	}
	var data map[string]any
	if err := json.Unmarshal(reading.Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"l1_a", "l1_w", "l1_v", "import_wh", "export_wh"} {
		if _, ok := data[field]; ok {
			t.Errorf("optional failed field %q was published as a synthetic value: %v", field, data[field])
		}
	}

	// Identity endpoints can be temporarily unavailable during startup. A
	// later poll must still anchor device_id when the serial appears.
	values["text_sensor/electric_meter_equipment_id"] = `"P1-LATE"`
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("recovery poll: %v", err)
	}
	if _, serial := env.Identity(); serial != "P1-LATE" {
		t.Fatalf("late serial = %q, want P1-LATE", serial)
	}
}

func TestESPHomeDSMR_BailsOnTotalReadFailure(t *testing.T) {
	// power_consumed missing (404) → driver logs warn and skips emit.
	// The test asserts the driver does NOT publish a meter reading
	// (silence is correct here — the watchdog flips the driver
	// offline upstream after `site.watchdog_timeout_s`).
	srv := mkESPHomeStub(map[string]string{
		// Note: power_consumed deliberately missing.
		"sensor/power_produced":                   "0.5",
		"text_sensor/electric_meter_equipment_id": "\"\"",
		"text_sensor/meter_identification":        "\"\"",
	})
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tel, _ := loadESPHomeDriver(t, host, nil)
	if got := tel.Get("zap-p1", telemetry.DerMeter); got != nil {
		t.Errorf("meter telemetry should NOT be emitted when total read fails: %+v", got)
	}
}

func TestESPHomeDSMR_ReactiveDiagnosticsEmitted(t *testing.T) {
	// Firmware exposes reactive — driver probes on first poll,
	// flags `has_reactive`, then emits the per-phase + total
	// reactive metrics into the TS DB.
	srv := mkESPHomeStub(map[string]string{
		"sensor/power_consumed":                   "1.0",
		"sensor/power_produced":                   "0.5",
		"sensor/power_consumed_l1":                "0",
		"sensor/power_consumed_l2":                "0",
		"sensor/power_consumed_l3":                "0",
		"sensor/power_produced_l1":                "0",
		"sensor/power_produced_l2":                "0",
		"sensor/power_produced_l3":                "0",
		"sensor/voltage_l1":                       "230",
		"sensor/voltage_l2":                       "230",
		"sensor/voltage_l3":                       "230",
		"sensor/current_l1":                       "0",
		"sensor/current_l2":                       "0",
		"sensor/current_l3":                       "0",
		"sensor/energy_consumed":                  "0",
		"sensor/energy_produced":                  "0",
		"sensor/reactive_power_imported":          "0.123",
		"sensor/reactive_power_exported":          "0.456",
		"sensor/reactive_power_imported_l1":       "0.04",
		"sensor/reactive_power_imported_l2":       "0.04",
		"sensor/reactive_power_imported_l3":       "0.043",
		"sensor/reactive_power_exported_l1":       "0.15",
		"sensor/reactive_power_exported_l2":       "0.15",
		"sensor/reactive_power_exported_l3":       "0.156",
		"text_sensor/electric_meter_equipment_id": "\"\"",
		"text_sensor/meter_identification":        "\"\"",
	})
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tel, _ := loadESPHomeDriver(t, host, nil)

	// Imported total: 0.123 kvar → 123 var.
	if v, _, ok := tel.LatestMetric("zap-p1", "meter_q_imp_var"); !ok || !near(v, 123) {
		t.Errorf("meter_q_imp_var: got %v ok=%v, want 123", v, ok)
	}
	if v, _, ok := tel.LatestMetric("zap-p1", "meter_q_exp_var"); !ok || !near(v, 456) {
		t.Errorf("meter_q_exp_var: got %v ok=%v, want 456", v, ok)
	}
	// L3 export reactive: 0.156 kvar → 156 var.
	if v, _, ok := tel.LatestMetric("zap-p1", "meter_q_exp_l3_var"); !ok || !near(v, 156) {
		t.Errorf("meter_q_exp_l3_var: got %v ok=%v, want 156", v, ok)
	}
}

func TestESPHomeDSMR_ReactiveSkippedOnFirmwareWithout(t *testing.T) {
	// Firmware lacks reactive-power entities. First poll probes,
	// finds 404, sets has_reactive=false. Driver should NOT emit
	// any q-metric rows. Verifies the capability gate works.
	srv := mkESPHomeStub(map[string]string{
		"sensor/power_consumed":    "0",
		"sensor/power_produced":    "0",
		"sensor/power_consumed_l1": "0",
		"sensor/power_consumed_l2": "0",
		"sensor/power_consumed_l3": "0",
		"sensor/power_produced_l1": "0",
		"sensor/power_produced_l2": "0",
		"sensor/power_produced_l3": "0",
		"sensor/voltage_l1":        "230",
		"sensor/voltage_l2":        "230",
		"sensor/voltage_l3":        "230",
		"sensor/current_l1":        "0",
		"sensor/current_l2":        "0",
		"sensor/current_l3":        "0",
		"sensor/energy_consumed":   "0",
		"sensor/energy_produced":   "0",
		// No reactive entries — the stub will 404.
		"text_sensor/electric_meter_equipment_id": "\"\"",
		"text_sensor/meter_identification":        "\"\"",
	})
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tel, _ := loadESPHomeDriver(t, host, nil)

	if _, _, ok := tel.LatestMetric("zap-p1", "meter_q_imp_var"); ok {
		t.Errorf("meter_q_imp_var should NOT be emitted on firmware without reactive")
	}
}

func TestESPHomeDSMR_BackoffOnConsecutiveFailures(t *testing.T) {
	// Pin a stub server that 404s the totals — driver must return
	// progressively longer poll intervals as failures accumulate.
	srv := mkESPHomeStub(map[string]string{
		// power_consumed missing → all polls fail.
		"text_sensor/electric_meter_equipment_id": "\"\"",
		"text_sensor/meter_identification":        "\"\"",
	})
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	// Use a small base poll so the backoff doublings stay
	// readable in test output (and the cap at 60 s isn't hit
	// across the 4 polls we run).
	tel := telemetry.NewStore()
	env := NewHostEnv("zap-p1", tel).WithHTTP()
	d, err := NewLuaDriver("../../../drivers/esphome_dsmr.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(d.Cleanup)
	if err := d.Init(context.Background(), map[string]any{
		"host": host, "poll_ms": 1000,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Run four polls; collect the next-poll-interval each returns.
	intervals := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		next, err := d.Poll(context.Background())
		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		intervals = append(intervals, next.Milliseconds())
	}

	// 1st failure → 1000·2 = 2000 ms.
	// 2nd failure → 1000·4 = 4000 ms.
	// 3rd failure → 1000·8 = 8000 ms.
	// 4th failure → 1000·16 = 16000 ms.
	// (Cap at 60000 ms doesn't kick in until ~6 failures.)
	want := []int64{2000, 4000, 8000, 16000}
	for i, w := range want {
		if intervals[i] != w {
			t.Errorf("poll %d backoff: got %d ms, want %d ms", i, intervals[i], w)
		}
	}
}

func TestESPHomeDSMR_BackoffResetsOnRecovery(t *testing.T) {
	// Start with totals missing (failure path), then "fix" the
	// device by having the stub start serving the entities, and
	// verify the next poll returns to the configured poll_ms
	// rather than the backed-off interval.
	values := map[string]string{
		// Initially: no power_consumed → fails.
		"text_sensor/electric_meter_equipment_id": "\"\"",
		"text_sensor/meter_identification":        "\"\"",
	}
	srv := mkESPHomeStub(values)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	tel := telemetry.NewStore()
	env := NewHostEnv("zap-p1", tel).WithHTTP()
	d, err := NewLuaDriver("../../../drivers/esphome_dsmr.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(d.Cleanup)
	if err := d.Init(context.Background(), map[string]any{
		"host": host, "poll_ms": 1000,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Two failures.
	for i := 0; i < 2; i++ {
		if _, err := d.Poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	// "Repair" the device by populating the stub. The handler
	// reads from the (shared) values map every request, so adding
	// keys here makes them visible on the next poll without
	// restarting the server.
	values["sensor/power_consumed"] = "0"
	values["sensor/power_produced"] = "0"
	values["sensor/power_consumed_l1"] = "0"
	values["sensor/power_consumed_l2"] = "0"
	values["sensor/power_consumed_l3"] = "0"
	values["sensor/power_produced_l1"] = "0"
	values["sensor/power_produced_l2"] = "0"
	values["sensor/power_produced_l3"] = "0"
	values["sensor/voltage_l1"] = "230"
	values["sensor/voltage_l2"] = "230"
	values["sensor/voltage_l3"] = "230"
	values["sensor/current_l1"] = "0"
	values["sensor/current_l2"] = "0"
	values["sensor/current_l3"] = "0"
	values["sensor/energy_consumed"] = "0"
	values["sensor/energy_produced"] = "0"

	next, err := d.Poll(context.Background())
	if err != nil {
		t.Fatalf("recovery poll: %v", err)
	}
	if next.Milliseconds() != 1000 {
		t.Errorf("recovery poll interval: got %d ms, want 1000 (back to poll_ms)", next.Milliseconds())
	}
}

func TestESPHomeDSMRCatalogEntry(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	var found *CatalogEntry
	for i, e := range entries {
		if e.ID == "esphome-dsmr" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("esphome-dsmr not in catalog")
	}
	if found.Manufacturer != "ESPHome" {
		t.Errorf("manufacturer = %q, want ESPHome", found.Manufacturer)
	}
	if len(found.Capabilities) != 1 || found.Capabilities[0] != "meter" {
		t.Errorf("capabilities = %v, want [meter]", found.Capabilities)
	}
}

// `near` is defined in solis_string_test.go (same package) — reusing
// it for sub-1 W power assertions where IEEE-754 dither would
// otherwise demand ulp-bookkeeping the test isn't testing.
