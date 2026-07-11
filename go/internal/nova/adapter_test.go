package nova

import (
	"encoding/json"
	"testing"
)

func p(v float64) *float64 { return &v }
func bp(b bool) *bool      { return &b }

func TestEncode_UnifiedIsVerbatim(t *testing.T) {
	in := &DerTelemetry{
		Type: KindBattery, Make: "ferroamp", Serial: "ES9234",
		HardwareID: "ferroamp:ES9234", TimestampMs: 1_713_610_245_123,
		W:   1500, // +W = charging, site convention
		SoC: p(0.65), DCV: p(48.5), DCA: p(10.2),
	}
	got, err := Encode(in, SchemaUnified)
	if err != nil {
		t.Fatal(err)
	}
	var out DerTelemetry
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if out.W != 1500 || out.Type != "battery" {
		t.Fatalf("unified round-trip lost data: %+v", out)
	}
	if out.SoC == nil || *out.SoC != 0.65 {
		t.Fatalf("SoC dropped: %+v", out.SoC)
	}
}

func TestEncode_LegacyBatterySignFlips(t *testing.T) {
	// Site convention: +W = charging. Nova legacy: −W = charging.
	// The adapter must flip only the sign, nothing else.
	in := &DerTelemetry{
		Type: KindBattery, Make: "ferroamp",
		TimestampMs: 1_713_610_245_123,
		W:           1500, // charging at 1.5 kW in site convention
		SoC:         p(0.65),
	}
	raw, err := Encode(in, SchemaLegacy)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "battery" {
		t.Fatalf("type: got %v, want battery", m["type"])
	}
	if w, _ := m["W"].(float64); w != -1500 {
		t.Fatalf("battery W sign not flipped: got %v, want -1500", m["W"])
	}
	if soc, _ := m["SoC_nom_fract"].(float64); soc != 0.65 {
		t.Fatalf("SoC_nom_fract: got %v, want 0.65", m["SoC_nom_fract"])
	}
	// Must not leak the clean snake_case keys.
	for _, bad := range []string{"w", "soc", "temp_c"} {
		if _, ok := m[bad]; ok {
			t.Fatalf("legacy output leaked clean key %q", bad)
		}
	}
}

func TestEncode_LegacyMeterDoesNotFlipSign(t *testing.T) {
	// Grid import is + in both conventions. No flip expected.
	in := &DerTelemetry{
		Type: KindMeter, TimestampMs: 1_713_610_245_123,
		W:      2000, // importing 2 kW
		FreqHz: p(50.02),
		L1V:    p(232.5),
		L1A:    p(8.6),
	}
	raw, _ := Encode(in, SchemaLegacy)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if w, _ := m["W"].(float64); w != 2000 {
		t.Fatalf("meter W must not flip: got %v", w)
	}
	if hz, _ := m["Hz"].(float64); hz != 50.02 {
		t.Fatalf("freq_hz→Hz failed: got %v", hz)
	}
	if l1v, _ := m["L1_V"].(float64); l1v != 232.5 {
		t.Fatalf("l1_v→L1_V failed: got %v", l1v)
	}
}

func TestEncode_LegacyPVTypeAndGeneration(t *testing.T) {
	in := &DerTelemetry{
		Type: KindPV, TimestampMs: 1_713_610_245_123,
		W:                 -3200, // generating (negative per site conv)
		MPPT1V:            p(420.0),
		MPPT1A:            p(7.8),
		TempC:             p(38.0),
		TotalGenerationWh: p(1_234_000),
	}
	raw, _ := Encode(in, SchemaLegacy)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["type"] != "solar" {
		t.Fatalf("pv→solar vocabulary swap failed: %v", m["type"])
	}
	if w, _ := m["W"].(float64); w != -3200 {
		t.Fatalf("pv W must keep negative sign for generation: got %v", w)
	}
	if m["mppt1_V"].(float64) != 420.0 {
		t.Fatalf("mppt1_v→mppt1_V failed")
	}
	if m["heatsink_C"].(float64) != 38.0 {
		t.Fatalf("temp_c→heatsink_C failed")
	}
}

func TestEncode_LegacyEVVocabularyAndStatus(t *testing.T) {
	cases := []struct {
		name       string
		connected  *bool
		charging   *bool
		wantStatus string
	}{
		{"disconnected", bp(false), nil, "disconnected"},
		{"connected_idle", bp(true), bp(false), "connected"},
		{"charging", bp(true), bp(true), "charging"},
		{"unknown", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &DerTelemetry{
				Type: KindEV, TimestampMs: 1_713_610_245_123,
				W:          3600,
				Connected:  tc.connected,
				Charging:   tc.charging,
				VehicleSoC: p(0.42),
				SessionWh:  p(7500),
			}
			raw, _ := Encode(in, SchemaLegacy)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if m["type"] != "ev_port" {
				t.Fatalf("ev→ev_port vocabulary swap failed: %v", m["type"])
			}
			if tc.wantStatus == "" {
				if _, has := m["status"]; has {
					t.Fatalf("expected no status, got %v", m["status"])
				}
			} else if m["status"] != tc.wantStatus {
				t.Fatalf("status: got %v, want %s", m["status"], tc.wantStatus)
			}
			if m["vehicle_soc_fract"].(float64) != 0.42 {
				t.Fatalf("vehicle_soc→vehicle_soc_fract failed")
			}
			if m["session_charge_Wh"].(float64) != 7500 {
				t.Fatalf("session_wh→session_charge_Wh failed")
			}
		})
	}
}

func TestEncode_TimestampRFC3339(t *testing.T) {
	in := &DerTelemetry{
		Type: KindMeter, TimestampMs: 1_713_610_245_123,
		W: 100,
	}
	raw, _ := Encode(in, SchemaLegacy)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	ts, _ := m["timestamp"].(string)
	if ts == "" || ts[len(ts)-1] != 'Z' {
		t.Fatalf("timestamp not RFC3339 UTC: %q", ts)
	}
	if rtm, _ := m["read_time_ms"].(float64); int64(rtm) != 1_713_610_245_123 {
		t.Fatalf("read_time_ms: got %v", rtm)
	}
}

func TestEncode_LegacyV2X(t *testing.T) {
	connected := true
	status := "discharging"
	mode := "manual"
	protocol := "mqtt"
	in := &DerTelemetry{
		Type:               KindV2X,
		TimestampMs:        1_713_610_245_123,
		W:                  -5000,
		Connected:          &connected,
		VehicleSoC:         p(0.64),
		DCW:                p(-5200),
		DCV:                p(400),
		DCA:                p(-13),
		SessionDischargeWh: p(1250),
		ChargePowerMinW:    p(1400),
		ChargePowerMaxW:    p(20000),
		DischargePowerMinW: p(1400),
		DischargePowerMaxW: p(15000),
		EVMaxEnergyReqWh:   p(50000),
		EVMinEnergyReqWh:   p(10000),
		RatedPowerW:        p(20000),
		Status:             &status,
		ControlMode:        &mode,
		Protocol:           &protocol,
	}
	raw, _ := Encode(in, SchemaLegacy)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["type"] != "v2x_charger" {
		t.Fatalf("v2x type: got %v", m["type"])
	}
	if m["W"].(float64) != -5000 {
		t.Fatalf("v2x W sign changed: %v", m["W"])
	}
	if m["vehicle_soc_fract"].(float64) != 0.64 {
		t.Fatalf("vehicle_soc mapping failed: %v", m["vehicle_soc_fract"])
	}
	if m["dc_W"].(float64) != -5200 || m["dc_V"].(float64) != 400 || m["dc_A"].(float64) != -13 {
		t.Fatalf("dc fields failed: %+v", m)
	}
	if m["session_discharge_Wh"].(float64) != 1250 {
		t.Fatalf("session discharge failed: %v", m["session_discharge_Wh"])
	}
	upper := m["upper_limit_W"].([]any)
	lower := m["lower_limit_W"].([]any)
	if upper[0].(float64) != 1400 || upper[2].(float64) != 20000 {
		t.Fatalf("upper limits failed: %+v", upper)
	}
	if lower[0].(float64) != -15000 || lower[2].(float64) != -1400 {
		t.Fatalf("lower limits failed: %+v", lower)
	}
	if m["ev_max_energy_req_Wh"].(float64) != 50000 || m["ev_min_energy_req_Wh"].(float64) != 10000 {
		t.Fatalf("energy request fields failed: %+v", m)
	}
	if m["status"] != "discharging" || m["control_mode"] != "manual" || m["protocol"] != "mqtt" {
		t.Fatalf("status/control fields failed: %+v", m)
	}
}
