package drivers

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// The catalog's spellings were silently dropped by nova.DerTelemetry, so a
// site running a catalog driver reported no energy counters and no frequency.
func TestCatalogSpellingsReachTheNovaNames(t *testing.T) {
	in := []byte(`{"type":"meter","w":1000,"hz":50.1,"import_wh":4500,"export_wh":120}`)
	got := decode(t, normalizeTelemetryKeys(in))

	for name, want := range map[string]float64{
		"freq_hz":         50.1,
		"total_import_wh": 4500,
		"total_export_wh": 120,
	} {
		if got[name] != want {
			t.Errorf("%s: got %v, want %v", name, got[name], want)
		}
	}
	// The driver's own keys stay, because the UI shows a reading verbatim.
	if got["hz"] != 50.1 || got["import_wh"] != float64(4500) {
		t.Error("original keys must be preserved")
	}
}

func TestBatteryAndPvCountersReachTheNovaNames(t *testing.T) {
	in := []byte(`{"type":"battery","w":-500,"charge_wh":1200,"discharge_wh":900}`)
	got := decode(t, normalizeTelemetryKeys(in))
	if got["total_charge_wh"] != float64(1200) || got["total_discharge_wh"] != float64(900) {
		t.Errorf("battery counters not mapped: %v", got)
	}

	in = []byte(`{"type":"pv","w":-2000,"lifetime_wh":30000}`)
	got = decode(t, normalizeTelemetryKeys(in))
	if got["total_generation_wh"] != float64(30000) {
		t.Errorf("lifetime_wh not mapped: %v", got)
	}
}

// srcfl/device-drivers is converting to these; they must land on the same
// internal names so no second host change is needed.
func TestCanonicalSpellingsReachTheNovaNames(t *testing.T) {
	in := []byte(`{"type":"meter","W":1234,"Hz":50,"L1_V":230.5,"total_import_Wh":99}`)
	got := decode(t, normalizeTelemetryKeys(in))

	for name, want := range map[string]float64{
		"w":               1234,
		"freq_hz":         50,
		"l1_v":            230.5,
		"total_import_wh": 99,
	} {
		if got[name] != want {
			t.Errorf("%s: got %v, want %v", name, got[name], want)
		}
	}
}

// A driver that already sets the target name must keep its own value; the
// alias must never overwrite it.
func TestExistingTargetKeyWins(t *testing.T) {
	in := []byte(`{"type":"meter","w":1,"import_wh":5,"total_import_wh":7}`)
	got := decode(t, normalizeTelemetryKeys(in))
	if got["total_import_wh"] != float64(7) {
		t.Errorf("alias overwrote the driver's own value: %v", got["total_import_wh"])
	}
}

// FTW's own drivers emit both names already. Normalising must be a no-op for
// them rather than reordering or dropping anything.
func TestBothNamesPresentIsUnchanged(t *testing.T) {
	in := []byte(`{"type":"meter","w":1,"import_wh":5,"total_import_wh":5,"hz":50,"freq_hz":50}`)
	if string(normalizeTelemetryKeys(in)) != string(in) {
		t.Error("payload that already carries both names should pass through untouched")
	}
}

func TestMalformedInputIsReturnedUnchanged(t *testing.T) {
	in := []byte(`not json`)
	if string(normalizeTelemetryKeys(in)) != string(in) {
		t.Error("a parse failure must never lose the reading")
	}
}

// Rated AC power arrives under four spellings and none of them was mapped, so
// it never reached Nova from any driver — including our own zap.lua.
func TestRatedPowerReachesTheNovaName(t *testing.T) {
	for _, spelling := range []string{"rated_W", "rated_power_W", "rated_w"} {
		in := []byte(`{"type":"pv","w":-2000,"` + spelling + `":5000}`)
		got := decode(t, normalizeTelemetryKeys(in))
		if got["rated_power_w"] != float64(5000) {
			t.Errorf("%s did not reach rated_power_w: %v", spelling, got)
		}
	}

	// The name DerTelemetry reads is left alone when the driver uses it.
	in := []byte(`{"type":"pv","w":-1,"rated_power_w":7,"rated_W":9}`)
	got := decode(t, normalizeTelemetryKeys(in))
	if got["rated_power_w"] != float64(7) {
		t.Errorf("alias overwrote the driver's own value: %v", got["rated_power_w"])
	}
}
