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

func TestZapReadsP1MeterOnly(t *testing.T) {
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
				},
				"battery": map[string]any{
					"W": 500, "rated_power_W": 5000, "SoC_nom_fract": 0.75,
				},
			},
			"INV-2": map[string]any{"pv": map[string]any{
				"W": -1250, "rated_power_W": 6000, "total_generation_Wh": 20000,
			}},
			"V2X-1": map[string]any{"v2x_charger": map[string]any{
				"W": -3000, "vehicle_soc_fract": 0.60,
			}},
		},
	}

	tel, env, _ := loadZapForTest(t, stub, nil)
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

	if got := tel.Get("sourceful-zap", telemetry.DerPV); got != nil {
		t.Fatalf("Zap must not ingest PV from attached inverters: %+v", got)
	}
	if got := tel.Get("sourceful-zap", telemetry.DerBattery); got != nil {
		t.Fatalf("Zap must not ingest battery from attached inverters: %+v", got)
	}
	if got := tel.Get("sourceful-zap", telemetry.DerV2X); got != nil {
		t.Fatalf("Zap must not ingest V2X from attached chargers: %+v", got)
	}

	count, _, ok := tel.LatestMetric("sourceful-zap", "other_resources")
	if !ok || count != 3 {
		t.Fatalf("other_resources = %v %v, want 3 (PV, battery, charger)", count, ok)
	}
}

func mixedZapSite() zapAPIStub {
	return zapAPIStub{
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
				},
				"battery": map[string]any{
					"W": 500, "rated_power_W": 5000, "SoC_nom_fract": 0.75,
				},
			},
			"INV-2": map[string]any{"pv": map[string]any{
				"W": -1250, "rated_power_W": 6000, "total_generation_Wh": 20000,
			}},
			"V2X-1": map[string]any{"v2x_charger": map[string]any{
				"W": -3000, "vehicle_soc_fract": 0.60,
			}},
		},
	}
}

func TestZapReadsPVWhenOptedIn(t *testing.T) {
	tel, _, _ := loadZapForTest(t, mixedZapSite(), map[string]any{"read_pv": true})

	meter := tel.Get("sourceful-zap", telemetry.DerMeter)
	if meter == nil || meter.RawW != -33 {
		t.Fatalf("meter = %+v, want -33W export", meter)
	}

	pv := tel.Get("sourceful-zap", telemetry.DerPV)
	if pv == nil {
		t.Fatal("expected aggregated PV from Zap-listed inverters")
	}
	if pv.RawW != -3750 {
		t.Fatalf("pv = %+v, want -3750W (INV-1 + INV-2)", pv)
	}
	pvData := readingData(t, pv)
	if pvData["rated_w"] != float64(14000) {
		t.Fatalf("pv rated_w = %+v, want 14000", pvData["rated_w"])
	}
	if pvData["lifetime_wh"] != float64(30000) {
		t.Fatalf("pv lifetime_wh = %+v, want 30000", pvData["lifetime_wh"])
	}

	if got := tel.Get("sourceful-zap", telemetry.DerBattery); got != nil {
		t.Fatalf("read_pv must not ingest battery: %+v", got)
	}
	if got := tel.Get("sourceful-zap", telemetry.DerV2X); got != nil {
		t.Fatalf("Zap must not ingest V2X: %+v", got)
	}

	count, _, ok := tel.LatestMetric("sourceful-zap", "other_resources")
	if !ok || count != 2 {
		t.Fatalf("other_resources = %v %v, want 2 (battery, charger)", count, ok)
	}
}

func TestZapReadsBatteryWhenOptedIn(t *testing.T) {
	tel, _, _ := loadZapForTest(t, mixedZapSite(), map[string]any{"read_battery": true})

	bat := tel.Get("sourceful-zap", telemetry.DerBattery)
	if bat == nil {
		t.Fatal("expected battery telemetry from Zap-listed inverter")
	}
	if bat.RawW != 500 {
		t.Fatalf("battery W = %+v, want 500", bat)
	}
	data := readingData(t, bat)
	if data["soc"] != 0.75 {
		t.Fatalf("battery soc = %+v, want 0.75 fraction", data["soc"])
	}

	if got := tel.Get("sourceful-zap", telemetry.DerPV); got != nil {
		t.Fatalf("read_battery must not ingest PV: %+v", got)
	}
	if got := tel.Get("sourceful-zap", telemetry.DerV2X); got != nil {
		t.Fatalf("Zap must not ingest V2X: %+v", got)
	}
}

func TestZapReadsPVFromInverterOnlySiteWhenOptedIn(t *testing.T) {
	stub := zapAPIStub{
		devices: map[string]any{"devices": []any{map[string]any{
			"type": "modbus_tcp", "device_type": "inverter", "sn": "PV-ONLY",
			"ders": []any{map[string]any{"type": "pv", "enabled": false, "rated_power": 5000}},
		}}},
		snapshots: map[string]any{"PV-ONLY": map[string]any{"pv": map[string]any{
			"W": -2400, "rated_power_W": 5000,
		}}},
	}
	tel, _, _ := loadZapForTest(t, stub, map[string]any{"read_pv": true})
	pv := tel.Get("sourceful-zap", telemetry.DerPV)
	if pv == nil || pv.RawW != -2400 {
		t.Fatalf("inverter-only Zap with read_pv = %+v, want -2400W", pv)
	}
	if got := tel.Get("sourceful-zap", telemetry.DerMeter); got != nil {
		t.Fatalf("unexpected synthetic meter on inverter-only Zap: %+v", got)
	}
}

func TestZapDoesNotProxyInverterWithoutMeter(t *testing.T) {
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
	if got := tel.Get("sourceful-zap", telemetry.DerPV); got != nil {
		t.Fatalf("inverter-only Zap must not emit PV: %+v", got)
	}
	if got := tel.Get("sourceful-zap", telemetry.DerMeter); got != nil {
		t.Fatalf("unexpected synthetic meter on inverter-only Zap: %+v", got)
	}
	count, _, ok := tel.LatestMetric("sourceful-zap", "other_resources")
	if !ok || count != 1 {
		t.Fatalf("other_resources = %v %v, want 1", count, ok)
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
