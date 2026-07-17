package drivers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type zapAPIStub struct {
	crypto    any
	devices   any
	snapshots map[string]any
	posts     *atomic.Int32
}

func (z zapAPIStub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost && z.posts != nil {
		z.posts.Add(1)
	}
	switch {
	case r.URL.Path == "/api/crypto":
		if z.crypto == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(z.crypto)
	case r.URL.Path == "/api/devices":
		_ = json.NewEncoder(w).Encode(z.devices)
	case strings.HasPrefix(r.URL.Path, "/api/devices/") && strings.HasSuffix(r.URL.Path, "/data/json"):
		sn := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/devices/"), "/data/json")
		payload, ok := z.snapshots[sn]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	default:
		http.NotFound(w, r)
	}
}

func loadZapForTest(t *testing.T, stub zapAPIStub, configOverrides map[string]any) (*telemetry.Store, *HostEnv, *LuaDriver) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(stub.serveHTTP))
	t.Cleanup(srv.Close)

	tel := telemetry.NewStore()
	env := NewHostEnv("sourceful-zap", tel).WithHTTP()
	driver, err := NewLuaDriver("../../../drivers/zap.lua", env)
	if err != nil {
		t.Fatalf("load Zap driver: %v", err)
	}
	t.Cleanup(driver.Cleanup)

	cfg := map[string]any{"host": strings.TrimPrefix(srv.URL, "http://")}
	for key, value := range configOverrides {
		cfg[key] = value
	}
	if err := driver.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init Zap driver: %v", err)
	}
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("poll Zap driver: %v", err)
	}
	return tel, env, driver
}

func readingData(t *testing.T, reading *telemetry.DerReading) map[string]any {
	t.Helper()
	if reading == nil {
		t.Fatal("missing telemetry reading")
	}
	var data map[string]any
	if err := json.Unmarshal(reading.Data, &data); err != nil {
		t.Fatalf("decode telemetry: %v", err)
	}
	return data
}

func TestZapOfficialLocalAPIMapsAllDERs(t *testing.T) {
	stub := zapAPIStub{
		crypto: map[string]any{
			"deviceName":   "software_zap",
			"serialNumber": "zap-04772a97",
			"publicKey":    "04a1b2c3",
		},
		devices: map[string]any{"count": 4, "devices": []any{
			map[string]any{
				"type": "p1_uart", "device_type": "energy_meter", "sn": "p1-main",
				"ders": []any{map[string]any{"type": "meter", "enabled": false}},
			},
			map[string]any{
				"type": "modbus_tcp", "device_type": "inverter", "sn": "INV-1",
				"ders": []any{
					map[string]any{"type": "pv", "enabled": false, "rated_power": 8000},
					map[string]any{"type": "battery", "enabled": false, "rated_power": 5000, "capacity": 10000},
				},
			},
			map[string]any{
				"type": "modbus_tcp", "device_type": "inverter", "sn": "INV-2",
				"ders": []any{map[string]any{"type": "pv", "enabled": false, "rated_power": 6000}},
			},
			map[string]any{
				"type": "mqtt", "device_type": "v2x_charger", "sn": "V2X-1",
				"ders": []any{map[string]any{"type": "v2x_charger", "enabled": false, "capacity": 77000}},
			},
		}},
		snapshots: map[string]any{
			"p1-main": map[string]any{"meter": map[string]any{
				"W": -33, "L1_W": 208, "L2_W": -62, "L3_W": -179,
				"L1_V": 230.1, "L2_V": 229.9, "L3_V": 230.4,
				"L1_A": 1.1, "L2_A": 0.8, "L3_A": 0.9, "Hz": 50.01,
				"total_import_Wh": 123456, "total_export_Wh": 65432,
			}},
			"INV-1": map[string]any{
				"pv": map[string]any{
					"W": -2500, "rated_power_W": 8000, "total_generation_Wh": 10000,
					"mppt1_V": 410.2, "mppt1_A": -6.1, "heatsink_C": 42.5,
				},
				"battery": map[string]any{
					"W": 500, "rated_power_W": 5000, "SoC_nom_fract": 0.75,
					"V": 48.2, "A": 10.4, "heatsink_C": 25.0,
					"lower_limit_W": -4500, "upper_limit_W": 5000,
					"total_charge_Wh": 8000, "total_discharge_Wh": 7200,
				},
			},
			"INV-2": map[string]any{"pv": map[string]any{
				"W": -1250, "rated_power_W": 6000, "total_generation_Wh": 20000,
			}},
			"V2X-1": map[string]any{"v2x_charger": map[string]any{
				"W": -3000, "ac_W": -3000, "rated_power_W": 11000, "vehicle_soc_fract": 0.60,
				"status": "discharging", "protocol": "ISO_15118_20", "control_mode": "dynamic_bpt",
				"connector_status": "occupied", "charging_state": "discharging",
				"plug_connected": true, "V": 230.5, "A": -13.0, "Hz": 49.98,
				"L1_V": 232.8, "L1_A": -13.0, "L1_W": -3026,
				"L2_V": 230.9, "L2_A": 0.0, "L2_W": 0,
				"L3_V": 230.2, "L3_A": 0.0, "L3_W": 0,
				"dc_W": -3150, "dc_V": 400, "dc_A": -7.875,
				"ev_target_energy_req_Wh": 5300, "ev_max_energy_req_Wh": 18100,
				"ev_min_energy_req_Wh": -25800, "session_charge_Wh": 50,
				"session_discharge_Wh": 1250, "total_charge_Wh": 142000,
				"total_discharge_Wh": 5100,
			}},
		},
	}

	tel, env, _ := loadZapForTest(t, stub, nil)
	if !env.BatteryTelemetryOnly {
		t.Fatal("Zap read_only catalog metadata must automatically admit battery telemetry")
	}
	makeName, serial := env.Identity()
	if makeName != "Sourceful" || serial != "zap-04772a97" {
		t.Fatalf("identity = %q/%q, want Sourceful/zap-04772a97", makeName, serial)
	}

	meter := tel.Get("sourceful-zap", telemetry.DerMeter)
	if meter == nil || meter.RawW != -33 {
		t.Fatalf("meter = %+v, want -33W export", meter)
	}
	meterData := readingData(t, meter)
	if meterData["l1_w"] != float64(208) || meterData["freq_hz"] != 50.01 {
		t.Fatalf("meter phase/frequency mapping = %+v", meterData)
	}
	if meterData["total_import_wh"] != float64(123456) || meterData["import_wh"] != float64(123456) {
		t.Fatalf("meter energy aliases = %+v", meterData)
	}

	pv := tel.Get("sourceful-zap", telemetry.DerPV)
	if pv == nil || pv.RawW != -3750 {
		t.Fatalf("PV = %+v, want aggregate -3750W", pv)
	}
	pvData := readingData(t, pv)
	if pvData["lifetime_wh"] != float64(30000) || pvData["total_generation_wh"] != float64(30000) {
		t.Fatalf("PV lifetime = %+v, want 30000Wh", pvData)
	}
	if pvData["rated_power_w"] != float64(14000) {
		t.Fatalf("PV aggregate rating = %v, want 14000W", pvData["rated_power_w"])
	}

	battery := tel.Get("sourceful-zap", telemetry.DerBattery)
	if battery == nil || battery.RawW != 500 || battery.SoC == nil || *battery.SoC != 0.75 {
		t.Fatalf("battery = %+v, want +500W charge at 75%%", battery)
	}
	batteryData := readingData(t, battery)
	if batteryData["discharge_capable"] != true || batteryData["charge_capable"] != true {
		t.Fatalf("battery capability mapping = %+v", batteryData)
	}
	if batteryData["capacity_wh"] != float64(10000) || batteryData["total_charge_wh"] != float64(8000) {
		t.Fatalf("battery capacity/energy mapping = %+v", batteryData)
	}

	v2x := tel.Get("sourceful-zap", telemetry.DerV2X)
	if v2x == nil || v2x.RawW != -3000 || v2x.SoC == nil || *v2x.SoC != 0.60 {
		t.Fatalf("V2X = %+v, want -3000W at 60%%", v2x)
	}
	v2xData := readingData(t, v2x)
	if v2xData["connected"] != true || v2xData["dc_w"] != float64(-3150) {
		t.Fatalf("V2X mapping = %+v", v2xData)
	}
	if v2xData["protocol"] != "ISO_15118_20" || v2xData["control_mode"] != "dynamic_bpt" ||
		v2xData["connector_status"] != "occupied" || v2xData["charging_state"] != "discharging" ||
		v2xData["ac_w"] != float64(-3000) || v2xData["ac_v"] != 230.5 ||
		v2xData["l1_w"] != float64(-3026) {
		t.Fatalf("V2X electrical/protocol mapping = %+v", v2xData)
	}
	if v2xData["ev_target_energy_req_wh"] != float64(5300) ||
		v2xData["ev_min_energy_req_wh"] != float64(-25800) ||
		v2xData["capacity_wh"] != float64(77000) {
		t.Fatalf("V2X energy/capacity mapping = %+v", v2xData)
	}

	// Zap's `enabled` flag is a Novacore publish switch, not a local-read
	// switch. All four DER kinds above deliberately use enabled=false.
	if _, _, ok := tel.LatestMetric("sourceful-zap", "pv_w_inv_1"); !ok {
		t.Fatal("expected per-inverter PV diagnostic for multi-PV Zap")
	}
	if value, _, ok := tel.LatestMetric("sourceful-zap", "v2x_ev_min_energy_req_wh"); !ok || value != -25800 {
		t.Fatalf("V2X signed energy diagnostic = %v %v, want -25800 Wh", value, ok)
	}
}

func TestZapPVOnlyWorksWithoutP1Meter(t *testing.T) {
	stub := zapAPIStub{
		devices: map[string]any{"devices": []any{map[string]any{
			"type": "modbus_tcp", "device_type": "inverter", "sn": "PV-ONLY",
			"ders": []any{map[string]any{"type": "pv", "enabled": false, "rated_power": 5000}},
		}}},
		snapshots: map[string]any{"PV-ONLY": map[string]any{"pv": map[string]any{
			"W": -2400, "rated_power_W": 5000,
		}}},
	}
	tel, _, _ := loadZapForTest(t, stub, nil)
	if got := tel.Get("sourceful-zap", telemetry.DerPV); got == nil || got.RawW != -2400 {
		t.Fatalf("PV-only Zap reading = %+v, want -2400W", got)
	}
	if got := tel.Get("sourceful-zap", telemetry.DerMeter); got != nil {
		t.Fatalf("unexpected synthetic meter on PV-only Zap: %+v", got)
	}
}

func TestZapDoesNotInventZeroForMissingRequiredPower(t *testing.T) {
	stub := zapAPIStub{
		devices: map[string]any{"devices": []any{map[string]any{
			"type": "p1_uart", "device_type": "energy_meter", "sn": "P1",
		}}},
		snapshots: map[string]any{"P1": map[string]any{"meter": map[string]any{
			"L1_W": 100, "L2_W": 200, "L3_W": 300,
		}}},
	}
	tel, _, _ := loadZapForTest(t, stub, nil)
	if got := tel.Get("sourceful-zap", telemetry.DerMeter); got != nil {
		t.Fatalf("missing meter.W must not become a synthetic zero: %+v", got)
	}
}

func TestZapRejectsPowerOverflowAgainstNameplate(t *testing.T) {
	stub := zapAPIStub{
		devices: map[string]any{"devices": []any{map[string]any{
			"type": "modbus_tcp", "device_type": "inverter", "sn": "INV-OFFLINE",
			"ders": []any{map[string]any{"type": "pv", "rated_power": 5000}},
		}}},
		snapshots: map[string]any{"INV-OFFLINE": map[string]any{"pv": map[string]any{
			"W": -65535, "rated_power_W": 5000,
		}}},
	}
	tel, _, _ := loadZapForTest(t, stub, nil)
	if got := tel.Get("sourceful-zap", telemetry.DerPV); got != nil {
		t.Fatalf("overflow sentinel must not reach site PV: %+v", got)
	}
}

func TestZapDisableFlagsAvoidDuplicateNativeDrivers(t *testing.T) {
	stub := zapAPIStub{
		devices: map[string]any{"devices": []any{map[string]any{
			"type": "modbus_tcp", "device_type": "inverter", "sn": "HYBRID",
			"ders": []any{
				map[string]any{"type": "pv", "rated_power": 5000},
				map[string]any{"type": "battery", "rated_power": 5000, "capacity": 10000},
			},
		}}},
		snapshots: map[string]any{"HYBRID": map[string]any{
			"pv":      map[string]any{"W": -1000, "rated_power_W": 5000},
			"battery": map[string]any{"W": 500, "rated_power_W": 5000, "SoC_nom_fract": 0.5},
		}},
	}
	tel, _, _ := loadZapForTest(t, stub, map[string]any{"disable_pv": true, "disable_battery": true})
	if got := tel.Get("sourceful-zap", telemetry.DerPV); got != nil {
		t.Fatalf("disable_pv still emitted %+v", got)
	}
	if got := tel.Get("sourceful-zap", telemetry.DerBattery); got != nil {
		t.Fatalf("disable_battery still emitted %+v", got)
	}
}

func TestZapDoesNotUseUnsafeLegacyRESTControl(t *testing.T) {
	var posts atomic.Int32
	stub := zapAPIStub{
		posts: &posts,
		devices: map[string]any{"devices": []any{map[string]any{
			"type": "modbus_tcp", "device_type": "inverter", "sn": "BAT-1",
			"ders": []any{map[string]any{"type": "battery", "rated_power": 5000, "capacity": 10000}},
		}}},
		snapshots: map[string]any{"BAT-1": map[string]any{"battery": map[string]any{
			"W": 0, "SoC_nom_fract": 0.5,
		}}},
	}

	_, _, driver := loadZapForTest(t, stub, nil)
	if err := driver.Command(context.Background(), []byte(`{"action":"battery","power_w":1000}`)); err == nil {
		t.Fatal("battery command must fail closed until Zap advertises safe leased local control")
	}
	if err := driver.DefaultMode(); err != nil {
		t.Fatalf("read-only default mode: %v", err)
	}
	if got := posts.Load(); got != 0 {
		t.Fatalf("read-only Zap driver made %d REST control writes, want 0", got)
	}
}
