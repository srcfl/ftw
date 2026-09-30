package control

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func fullBatterySite(soc, power float64) (*telemetry.Store, *State, map[string]float64) {
	tel := seedStore(-1000, []struct {
		name          string
		currentW, soc float64
	}{{"battery", power, soc}})
	st := NewState(0, 50, "ferroamp")
	st.MinDispatchIntervalS = 0
	st.Mode = ModeCharge
	return tel, st, map[string]float64{"battery": 9600}
}

func TestFullBatteryStopsChargingAcrossModes(t *testing.T) {
	for _, mode := range AllModes() {
		t.Run(string(mode), func(t *testing.T) {
			tel, st, capacities := fullBatterySite(1, 2000)
			st.Mode = mode
			st.UseEnergyDispatch = true
			st.SlewEnabled, st.SlewRateW = true, 10
			st.SlotDirective = func(now time.Time) (SlotDirective, bool) {
				return SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 500, Strategy: "arbitrage"}, true
			}
			st.PlanTarget = func(time.Time) (string, float64, string, bool) { return string(ModeCharge), 2000, "test", true }
			got := ComputeDispatch(tel, st, capacities, 13800)
			if len(got) == 0 {
				t.Fatal("full battery still charging: no stop command")
			}
			for _, target := range got {
				if target.TargetW > 0 {
					t.Fatalf("charge survived full stop: %+v", got)
				}
			}
		})
	}
}

func TestFullBatteryManualStopDischargeAndSibling(t *testing.T) {
	tel, st, capacities := fullBatterySite(1, 0)
	tel.Update("other", telemetry.DerBattery, 0, ptrF64(.5), nil)
	tel.RecordDriverSuccess("other")
	capacities["other"] = 9600
	st.SlewEnabled = false
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 3000, ExpiresAt: time.Now().Add(time.Hour)})
	got := ComputeDispatch(tel, st, capacities, 13800)
	for _, target := range got {
		if target.Driver == "battery" && target.TargetW != 0 {
			t.Fatalf("full battery received charge: %+v", got)
		}
		if target.Driver == "other" && target.TargetW < 2900 {
			t.Fatalf("capable sibling lost charge: %+v", got)
		}
	}
	st.SetBatteryManualHold(BatteryManualHold{PowerW: -2000, ExpiresAt: time.Now().Add(time.Hour)})
	got = ComputeDispatch(tel, st, capacities, 13800)
	for _, target := range got {
		if target.TargetW >= 0 {
			t.Fatalf("full stop prevented discharge: %+v", got)
		}
	}
}

func TestFullBatteryHysteresisAndMissingSoC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel, st, capacities := fullBatterySite(.999, 0)
		check := func(wantPaused bool) {
			t.Helper()
			got := ComputeDispatch(tel, st, capacities, 13800)
			if st.BatteryChargePaused("battery") != wantPaused || len(got) != 1 || (got[0].TargetW == 0) != wantPaused {
				t.Fatalf("paused=%v targets=%+v want paused=%v", st.BatteryChargePaused("battery"), got, wantPaused)
			}
		}
		check(false) // A near-full reading alone does not prove full.
		for _, soc := range []float64{1, .999, .995, 1, .991} {
			tel.Update("battery", telemetry.DerBattery, 0, &soc, nil)
			check(true)
		}
		time.Sleep(6 * time.Minute)
		tel.Update("battery", telemetry.DerBattery, 0, nil, nil)
		tel.RecordDriverSuccess("battery")
		check(true) // Missing SoC cannot release a known full stop.
		tel.Update("battery", telemetry.DerBattery, 0, ptrF64(.99), nil)
		check(false)
	})
}

func TestFullBatteryStopsPreviousChargeDuringHoldoffAndDeadband(t *testing.T) {
	for _, holdoff := range []bool{false, true} {
		tel, st, capacities := fullBatterySite(1, 0)
		st.Mode = ModeSelfConsumption
		tel.Update("ferroamp", telemetry.DerMeter, 0, nil, nil)
		// The BMS has already stopped power, but the previous command is still positive.
		st.PrevTargets["battery"] = 530
		st.LastTargets = []DispatchTarget{{Driver: "battery", TargetW: 530}}
		if holdoff {
			now := time.Now()
			st.LastDispatch = &now
			st.MinDispatchIntervalS = 60
		}
		got := ComputeDispatch(tel, st, capacities, 13800)
		if len(got) != 1 || got[0].TargetW != 0 || st.LastTargets[0].TargetW != 0 {
			t.Fatalf("holdoff=%v left prior charge active: %+v", holdoff, got)
		}
	}
}

func TestFullBatteryStillProvidesFuseRelief(t *testing.T) {
	tel, st, capacities := fullBatterySite(1, 0)
	tel = seedStore(16000, []struct {
		name          string
		currentW, soc float64
	}{{"battery", 0, 1}})
	st.Mode = ModeIdle
	got := ComputeDispatch(tel, st, capacities, 13800)
	if len(got) != 1 || got[0].TargetW >= 0 {
		t.Fatalf("full stop blocked fuse relief: %+v", got)
	}
}
