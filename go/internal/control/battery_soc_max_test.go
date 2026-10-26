package control

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func cappedBatterySite(soc, power float64) (*telemetry.Store, *State, map[string]float64) {
	tel, st, capacities := fullBatterySite(soc, power)
	st.DriverLimits = map[string]PowerLimits{"battery": {SoCMax: .9, MaxChargeW: 10000}}
	return tel, st, capacities
}

func TestSoCMaxStopsChargingInEveryMode(t *testing.T) {
	for _, mode := range AllModes() {
		for _, energy := range []bool{false, true} {
			t.Run(string(mode)+"/energy="+map[bool]string{false: "off", true: "on"}[energy], func(t *testing.T) {
				tel, st, capacities := cappedBatterySite(.9, 2000)
				st.Mode, st.UseEnergyDispatch = mode, energy
				st.SlewEnabled, st.SlewRateW = true, 10
				st.SlotDirective = func(now time.Time) (SlotDirective, bool) {
					return SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 500}, true
				}
				st.PlanTarget = func(time.Time) (string, float64, string, bool) {
					return string(ModeCharge), 2000, "charge", true
				}
				got := ComputeDispatch(tel, st, capacities, 13800)
				if len(got) != 1 || got[0].TargetW > 0 || !st.BatteryChargePaused("battery") {
					t.Fatalf("90%% ceiling did not stop charging: %+v", got)
				}
			})
		}
	}
}

func TestSoCMaxManualHoldAndSibling(t *testing.T) {
	tel, st, capacities := cappedBatterySite(.9, 0)
	tel.Update("other", telemetry.DerBattery, 0, ptrF64(.8), nil)
	tel.RecordDriverSuccess("other")
	capacities["other"] = 9600
	st.DriverLimits["other"] = PowerLimits{SoCMax: .95}
	st.SlewEnabled = false
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 3000, ExpiresAt: time.Now().Add(time.Hour)})
	got := ComputeDispatch(tel, st, capacities, 13800)
	if targetFor(t, got, "battery").TargetW != 0 || targetFor(t, got, "other").TargetW < 2900 {
		t.Fatalf("charge must go to the battery below its own ceiling: %+v", got)
	}
	st.SetBatteryManualHold(BatteryManualHold{PowerW: -2000, ExpiresAt: time.Now().Add(time.Hour)})
	got = ComputeDispatch(tel, st, capacities, 13800)
	if targetFor(t, got, "battery").TargetW >= 0 {
		t.Fatalf("charge ceiling blocked discharge: %+v", got)
	}
	st.BatteryHoldTargetValid = func(driver, id string) bool { return driver == "battery" && id == "hardware" }
	st.SetBatteryManualHold(BatteryManualHold{Driver: "battery", DeviceID: "hardware", PowerW: 3000, ExpiresAt: time.Now().Add(time.Hour)})
	got = ComputeDispatch(tel, st, capacities, 13800)
	if targetFor(t, got, "battery").TargetW != 0 || targetFor(t, got, "other").TargetW != 0 {
		t.Fatalf("scoped hold bypassed its ceiling or charged another battery: %+v", got)
	}
}

func TestSoCMaxStopsSelfTuneWithoutPriorControlTick(t *testing.T) {
	tel, st, capacities := cappedBatterySite(.9, 1000)
	got := SelfTuneDispatch(tel, st, capacities, 13800, "battery", 3000)
	if len(got) != 1 || got[0].TargetW != 0 || !got[0].Clamped {
		t.Fatalf("self-tune crossed the charge ceiling: %+v", got)
	}
}

func TestSoCMaxStopsPreviousOrderDuringQuietOrHeldTick(t *testing.T) {
	for _, holdoff := range []bool{false, true} {
		tel, st, capacities := cappedBatterySite(.9, 0)
		st.Mode = ModeSelfConsumption
		tel.Update("ferroamp", telemetry.DerMeter, 0, nil, nil)
		st.PrevTargets["battery"] = 10000
		st.LastTargets = []DispatchTarget{{Driver: "battery", TargetW: 10000}}
		if holdoff {
			now := time.Now()
			st.LastDispatch, st.MinDispatchIntervalS = &now, 60
		}
		got := ComputeDispatch(tel, st, capacities, 13800)
		if len(got) != 1 || got[0].TargetW != 0 || st.LastTargets[0].TargetW != 0 || st.PrevTargets["battery"] != 0 {
			t.Fatalf("holdoff=%v left the prior charge active: %+v", holdoff, got)
		}
	}
}

func TestSoCMaxHysteresisAndMissingLevel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel, st, capacities := cappedBatterySite(.899, 0)
		check := func(paused bool) {
			t.Helper()
			got := ComputeDispatch(tel, st, capacities, 13800)
			if len(got) != 1 || st.BatteryChargePaused("battery") != paused || (got[0].TargetW == 0) != paused {
				t.Fatalf("paused=%v targets=%+v, want paused=%v", st.BatteryChargePaused("battery"), got, paused)
			}
		}
		check(false)
		for _, level := range []float64{.9, .92, .899, .891} {
			tel.Update("battery", telemetry.DerBattery, 0, &level, nil)
			check(true)
		}
		time.Sleep(6 * time.Minute)
		tel.Update("battery", telemetry.DerBattery, 0, nil, nil)
		tel.RecordDriverSuccess("battery")
		check(true)
		tel.Update("battery", telemetry.DerBattery, 0, ptrF64(.89), nil)
		check(false)
	})
}

func TestSoCMaxReloadTakesEffectOnNextTick(t *testing.T) {
	tel, st, capacities := cappedBatterySite(.9, 0)
	ComputeDispatch(tel, st, capacities, 13800)
	st.DriverLimits["battery"] = PowerLimits{SoCMax: .95}
	got := ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW <= 0 {
		t.Fatalf("raising the ceiling did not release the stop: %+v", got)
	}
	st.DriverLimits["battery"] = PowerLimits{SoCMax: .85}
	got = ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW != 0 {
		t.Fatalf("lowering the ceiling did not stop charging: %+v", got)
	}
}

func TestSoCMaxStillAllowsFuseRelief(t *testing.T) {
	tel, st, capacities := cappedBatterySite(.9, 0)
	tel.Update("ferroamp", telemetry.DerMeter, 16000, nil, nil)
	st.Mode = ModeIdle
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases = 20, 230, 3
	got := ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW >= 0 {
		t.Fatalf("charge ceiling blocked fuse relief: %+v", got)
	}
}

func TestSoCMaxExportGuardCannotRestorePausedCharge(t *testing.T) {
	tel, st, capacities := cappedBatterySite(.9, 2000)
	st.Mode = ModeIdle
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases = 20, 230, 3
	st.batteryCommands = map[string]commandLog{"battery": commandLog{}.add(st.now(), 2000)}
	st.updateBatteryChargeFull(tel, capacities)
	// The stop remains active below the ceiling until the resume level. Even
	// an export overage must not restore the charge that the stop removed.
	tel.Update("battery", telemetry.DerBattery, 2000, ptrF64(.895), nil)
	tel.Update("ferroamp", telemetry.DerMeter, -20000, nil, nil)
	got := ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW != 0 {
		t.Fatalf("export guard restored paused charging: %+v", got)
	}
}
