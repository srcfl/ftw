package control

import (
	"encoding/json"
	"testing"
	"testing/synctest"
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

func updateDeviceBattery(tel *telemetry.Store, soc float64, data string) {
	tel.Update("battery", telemetry.DerBattery, 0, &soc, json.RawMessage(data))
	tel.RecordDriverSuccess("battery")
	tel.Update("ferroamp", telemetry.DerMeter, -1000, nil, nil)
	tel.RecordDriverSuccess("ferroamp")
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

// A ceiling at or below the SoC floor, or below the trusted minimum, must
// not mark a half-empty battery full.
func TestDeviceSoCMaxAtOrBelowTheFloorIsNotUsed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		soc   float64
		data  string
		floor float64
	}{
		{"below the trusted minimum", .5, `{"max_soc_fract":0.05}`, .10},
		{"at or below the battery's floor", .58, `{"max_soc_fract":0.55}`, .60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel, st, capacities := deviceCappedBatterySite(tc.soc, tc.data)
			st.DriverLimits = map[string]PowerLimits{"battery": {SoCMin: tc.floor, MaxChargeW: 5000}}
			for range 2 {
				got := ComputeDispatch(tel, st, capacities, 13800)
				if len(got) != 1 || got[0].TargetW <= 0 || st.BatteryChargePaused("battery") {
					t.Fatalf("an unusable device ceiling stopped charging at %.0f%%: %+v", tc.soc*100, got)
				}
			}
			if c := st.chargeCeiling("battery"); c != 1 {
				t.Fatalf("charge ceiling = %v, want the configured 1", c)
			}
			r := tel.Get("battery", telemetry.DerBattery)
			if _, upper := fuseTargetBounds(r, st.DriverLimits["battery"]); upper != 5000 {
				t.Fatalf("fuse charge bound = %v, want 5000", upper)
			}
		})
	}
	// The floor case is logged once per value, not on every tick.
	tel, st, capacities := deviceCappedBatterySite(.58, `{"max_soc_fract":0.55}`)
	st.DriverLimits = map[string]PowerLimits{"battery": {SoCMin: .60}}
	ComputeDispatch(tel, st, capacities, 13800)
	if got := st.batteryDeviceSoCMax["battery"].belowFloor; got != .55 {
		t.Fatalf("below-floor ceiling recorded for its one log = %v, want 0.55", got)
	}
}

// One reading without the ceiling, from a limit read that failed, must not
// release a full battery. The ceiling holds for a bounded time.
func TestDeviceSoCMaxHoldsAcrossAMissedLimitRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel, st, capacities := deviceCappedBatterySite(.9, `{"max_soc_fract":0.9}`)
		ComputeDispatch(tel, st, capacities, 13800)
		if !st.BatteryChargePaused("battery") {
			t.Fatal("device ceiling did not pause charging")
		}
		updateDeviceBattery(tel, .9, `{}`)
		got := ComputeDispatch(tel, st, capacities, 13800)
		if len(got) != 1 || got[0].TargetW > 0 || !st.BatteryChargePaused("battery") {
			t.Fatalf("one reading without the ceiling released the stop: %+v", got)
		}
		for range 11 {
			time.Sleep(time.Minute)
			updateDeviceBattery(tel, .9, `{}`)
		}
		ComputeDispatch(tel, st, capacities, 13800)
		if st.BatteryChargePaused("battery") || st.chargeCeiling("battery") != 1 {
			t.Fatalf("a ceiling unreported for 11 minutes still applies: %v", st.chargeCeiling("battery"))
		}
	})
}

// The full-battery stop and the fuse bound read the same ceiling, so they
// agree on a reading that has stopped updating.
func TestDeviceSoCMaxDropsWithAStaleReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel, st, capacities := deviceCappedBatterySite(.9, `{"max_soc_fract":0.9}`)
		st.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 5000}}
		bound := func() float64 {
			_, upper := fuseTargetBounds(tel.Get("battery", telemetry.DerBattery), st.DriverLimits["battery"])
			return upper
		}
		st.updateBatteryChargeFull(tel, capacities)
		if c, upper := st.chargeCeiling("battery"), bound(); c != .9 || upper != 0 {
			t.Fatalf("fresh reading: ceiling %v, fuse charge bound %v; want 0.9 and 0", c, upper)
		}
		time.Sleep(telemetry.BatterySoCMaxAge + time.Second)
		st.updateBatteryChargeFull(tel, capacities)
		if c, upper := st.chargeCeiling("battery"), bound(); c != 1 || upper != 5000 {
			t.Fatalf("stale reading: ceiling %v, fuse charge bound %v; want 1 and 5000 from both paths", c, upper)
		}
	})
}

func TestDeviceSoCMaxFollowsTheLatestReading(t *testing.T) {
	tel, st, capacities := deviceCappedBatterySite(.9, `{"max_soc_fract":0.9}`)
	ComputeDispatch(tel, st, capacities, 13800)
	if !st.BatteryChargePaused("battery") {
		t.Fatal("device ceiling did not pause charging")
	}
	// The owner raises the Pixii's limit; the next reading carries it.
	updateDeviceBattery(tel, .9, `{"max_soc_fract":0.95}`)
	got := ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW <= 0 || st.BatteryChargePaused("battery") {
		t.Fatalf("raising the device ceiling did not release the stop: %+v", got)
	}
}

func TestDeviceSoCMaxClosesTheFuseChargeBound(t *testing.T) {
	soc, ceiling := .9, .9
	now := time.Now()
	r := &telemetry.DerReading{SoC: &soc, SoCCeiling: &ceiling, SoCCeilingAt: now, UpdatedAt: now}
	lim := PowerLimits{MaxChargeW: 5000, MaxDischargeW: 5000}
	if lower, upper := fuseTargetBounds(r, lim); upper != 0 || lower != -5000 {
		t.Fatalf("bounds at the device ceiling = %v..%v, want -5000..0", lower, upper)
	}
	r.SoCCeiling = nil
	if _, upper := fuseTargetBounds(r, lim); upper != 5000 {
		t.Fatalf("charge bound without a device ceiling = %v, want 5000", upper)
	}
}
