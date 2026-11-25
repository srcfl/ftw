package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A Pixii stops charging at its own ceiling, 90 % on the site in
// srcfl/device-drivers#175, while Core had no soc_max configured. The driver
// reports that ceiling as max_soc_fract; dispatch stops charge there too.
func deviceCappedBatterySite(soc float64, data string) (*telemetry.Store, *State, map[string]float64) {
	tel, st, capacities := fullBatterySite(soc, 0)
	tel.Update("battery", telemetry.DerBattery, 0, &soc, json.RawMessage(data))
	st.PlanTarget = func(time.Time) (string, float64, string, bool) {
		return string(ModeCharge), 2000, "charge", true
	}
	return tel, st, capacities
}

func TestDeviceSoCMaxStopsChargingWithoutConfiguredCeiling(t *testing.T) {
	for _, mode := range AllModes() {
		t.Run(string(mode), func(t *testing.T) {
			tel, st, capacities := deviceCappedBatterySite(.9, `{"max_soc_fract":0.9}`)
			st.Mode = mode
			got := ComputeDispatch(tel, st, capacities, 13800)
			if len(got) != 1 || got[0].TargetW > 0 || !st.BatteryChargePaused("battery") {
				t.Fatalf("device ceiling of 90%% did not stop charging: %+v", got)
			}
			if resume := st.BatteryChargeResumeSoC("battery"); resume < .889 || resume > .891 {
				t.Fatalf("resume level = %v, want 0.89 under the device ceiling", resume)
			}
		})
	}
}

func TestDeviceSoCMaxAbsentOrHigherKeepsConfiguredCeiling(t *testing.T) {
	for _, data := range []string{`{}`, `{"max_soc_fract":0}`, `{"max_soc_fract":90}`, `{"max_soc_fract":1}`} {
		tel, st, capacities := deviceCappedBatterySite(.9, data)
		got := ComputeDispatch(tel, st, capacities, 13800)
		if len(got) != 1 || got[0].TargetW <= 0 || st.BatteryChargePaused("battery") {
			t.Fatalf("%s stopped charging at 90%% with no ceiling below it: %+v", data, got)
		}
	}
	// The configured ceiling still applies when it is the lower one.
	tel, st, capacities := deviceCappedBatterySite(.86, `{"max_soc_fract":0.95}`)
	st.DriverLimits = map[string]PowerLimits{"battery": {SoCMax: .85}}
	got := ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW > 0 || !st.BatteryChargePaused("battery") {
		t.Fatalf("a higher device ceiling lifted the configured 85%%: %+v", got)
	}
}

func TestDeviceSoCMaxFollowsTheLatestReading(t *testing.T) {
	tel, st, capacities := deviceCappedBatterySite(.9, `{"max_soc_fract":0.9}`)
	ComputeDispatch(tel, st, capacities, 13800)
	if !st.BatteryChargePaused("battery") {
		t.Fatal("device ceiling did not pause charging")
	}
	// The owner raises the Pixii's limit; the next reading carries it.
	tel.Update("battery", telemetry.DerBattery, 0, ptrF64(.9), json.RawMessage(`{"max_soc_fract":0.95}`))
	got := ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW <= 0 || st.BatteryChargePaused("battery") {
		t.Fatalf("raising the device ceiling did not release the stop: %+v", got)
	}
	// A driver that stops reporting the ceiling falls back to the configured one.
	tel.Update("battery", telemetry.DerBattery, 0, ptrF64(.95), json.RawMessage(`{"max_soc_fract":0.95}`))
	ComputeDispatch(tel, st, capacities, 13800)
	tel.Update("battery", telemetry.DerBattery, 0, ptrF64(.95), json.RawMessage(`{}`))
	got = ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW <= 0 {
		t.Fatalf("a ceiling the driver no longer reports still stops charging: %+v", got)
	}
}

func TestDeviceSoCMaxClosesTheFuseChargeBound(t *testing.T) {
	soc := .9
	r := &telemetry.DerReading{SoC: &soc, Data: json.RawMessage(`{"max_soc_fract":0.9}`)}
	lim := PowerLimits{MaxChargeW: 5000, MaxDischargeW: 5000}
	if lower, upper := fuseTargetBounds(r, lim); upper != 0 || lower != -5000 {
		t.Fatalf("bounds at the device ceiling = %v..%v, want -5000..0", lower, upper)
	}
	r.Data = json.RawMessage(`{}`)
	if _, upper := fuseTargetBounds(r, lim); upper != 5000 {
		t.Fatalf("charge bound without a device ceiling = %v, want 5000", upper)
	}
}
