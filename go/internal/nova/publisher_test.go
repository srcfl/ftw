package nova

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/driverinventory"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestDriverInventoryContentSHAIgnoresGeneratedAt(t *testing.T) {
	first := driverinventory.Snapshot{
		SchemaVersion: driverinventory.SchemaVersion,
		GeneratedAt:   time.Unix(1, 0),
		Host: driverinventory.Host{
			Product: "ftw", Version: "1.5.0-beta.1", UpdateChannel: "beta",
			Target: "ftw-core", RuntimeABI: driverinventory.RuntimeABI, HostAPI: driverinventory.HostAPI,
		},
		Drivers: []driverinventory.Driver{{
			DriverID: "sdm630", Version: "1.1.1", Source: "bundled",
			SourceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ControlClass: "read_only", ConfiguredInstances: 1, RunningInstances: 1,
			Health: driverinventory.Health{OK: 1},
		}},
	}
	second := first
	second.GeneratedAt = time.Unix(2, 0)
	firstSHA, err := driverInventoryContentSHA(first)
	if err != nil {
		t.Fatal(err)
	}
	secondSHA, err := driverInventoryContentSHA(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstSHA != secondSHA {
		t.Fatal("generated_at changed content identity")
	}
	if got := driverInventoryTopic("f42w-gw-1"); got != "gateways/f42w-gw-1/inventory/drivers/json/v1" {
		t.Fatalf("topic = %q", got)
	}
}

// TestAssemble_PicksUpClean Snake CaseFromLuaEmit confirms that
// arbitrary fields a Lua driver emits inside host.emit() flow into
// the clean payload unmodified — because the emit convention and
// the clean schema share snake_case.
func TestAssemble_PicksUpCleanFieldsFromLuaEmit(t *testing.T) {
	// This is the JSON shape that host.emit("meter", {…}) produces.
	data := json.RawMessage(`{
		"w": 1800,
		"l1_v": 232.0, "l2_v": 231.5, "l3_v": 232.2,
		"l1_a": 3.2, "l2_a": 3.1, "l3_a": 3.3,
		"freq_hz": 49.97,
		"total_import_wh": 4500000,
		"total_export_wh": 1200000
	}`)
	r := &telemetry.DerReading{
		Driver:    "p1-meter",
		DerType:   telemetry.DerMeter,
		RawW:      1800,
		SmoothedW: 1810,
		Data:      data,
		UpdatedAt: time.UnixMilli(1713610245123),
	}
	dev := state.Device{DeviceID: "p1:SN42", Make: "landis+gyr", Serial: "SN42"}
	got := assemble(r, dev, 1713610245123)
	if got.Type != "meter" || got.W != 1800 || got.HardwareID != "p1:SN42" {
		t.Fatalf("envelope wrong: %+v", got)
	}
	if got.L1V == nil || *got.L1V != 232.0 {
		t.Fatalf("l1_v did not flow through: %v", got.L1V)
	}
	if got.FreqHz == nil || *got.FreqHz != 49.97 {
		t.Fatalf("freq_hz did not flow through: %v", got.FreqHz)
	}
	if got.TotalImportWh == nil || *got.TotalImportWh != 4500000 {
		t.Fatalf("total_import_wh did not flow through: %v", got.TotalImportWh)
	}
}

func TestAssemble_BatterySoCFromDerReading(t *testing.T) {
	// Battery drivers sometimes emit w only and set SoC via the
	// separate argument — the telemetry.Store carries SoC on the
	// DerReading even when the emit table didn't include it (e.g.
	// Ferroamp ESO publishes SoC less frequently than power flow).
	soc := 0.65
	r := &telemetry.DerReading{
		Driver:    "ferroamp",
		DerType:   telemetry.DerBattery,
		RawW:      1500, // charging (+ in site convention)
		SoC:       &soc,
		Data:      json.RawMessage(`{"w": 1500, "dc_v": 48.5, "dc_a": 30.9, "temp_c": 25.5}`),
		UpdatedAt: time.UnixMilli(1713610245123),
	}
	dev := state.Device{DeviceID: "ferroamp:ES9234", Make: "ferroamp", Serial: "ES9234"}
	got := assemble(r, dev, 1713610245123)
	if got.SoC == nil || *got.SoC != 0.65 {
		t.Fatalf("SoC not picked up: %+v", got.SoC)
	}
	if got.DCV == nil || *got.DCV != 48.5 {
		t.Fatalf("dc_v missing: %v", got.DCV)
	}
	if got.TempC == nil || *got.TempC != 25.5 {
		t.Fatalf("temp_c missing: %v", got.TempC)
	}
	if got.W != 1500 {
		t.Fatalf("W: got %v, want 1500 (site conv — flip happens in adapter, not assemble)", got.W)
	}
}

func TestAssemble_V2XVehicleSoCFromDerReading(t *testing.T) {
	soc := 0.58
	r := &telemetry.DerReading{
		Driver:    "ambibox",
		DerType:   telemetry.DerV2X,
		RawW:      3200,
		SoC:       &soc,
		Data:      json.RawMessage(`{"w": 3200}`),
		UpdatedAt: time.UnixMilli(1713610245123),
	}
	dev := state.Device{DeviceID: "ambibox:V2X", Make: "Ambibox", Serial: "V2X"}
	got := assemble(r, dev, 1713610245123)
	if got.VehicleSoC == nil || *got.VehicleSoC != 0.58 {
		t.Fatalf("vehicle_soc not picked up for V2X: %+v", got.VehicleSoC)
	}
	if got.SoC != nil {
		t.Fatalf("V2X vehicle SoC should not be encoded as battery soc: %+v", got.SoC)
	}
}

func TestAssemble_EmptyDataDoesNotPanic(t *testing.T) {
	r := &telemetry.DerReading{
		Driver: "x", DerType: telemetry.DerPV, RawW: -2500,
	}
	dev := state.Device{DeviceID: "mac:aabb", Make: "solis"}
	got := assemble(r, dev, 1_000)
	if got.Type != "pv" || got.W != -2500 {
		t.Fatalf("nil Data path broken: %+v", got)
	}
}

// End-to-end through Encode: assemble → adapter → Nova legacy JSON.
// Catches any drift between the clean schema and the legacy wire format.
func TestAssemblePlusEncode_BatterySignFlipsExactlyOnce(t *testing.T) {
	r := &telemetry.DerReading{
		Driver: "ferroamp", DerType: telemetry.DerBattery,
		RawW: 1500, // + = charging (site conv)
		Data: json.RawMessage(`{"w": 1500}`),
	}
	dev := state.Device{DeviceID: "ferroamp:ES9234", Make: "ferroamp"}
	clean := assemble(r, dev, 1_713_610_245_123)
	raw, err := Encode(clean, SchemaLegacy)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if w, _ := m["W"].(float64); w != -1500 {
		t.Fatalf("end-to-end battery W must be -1500 in legacy wire format, got %v", w)
	}
}
