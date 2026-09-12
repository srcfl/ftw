package control

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Last-known −4 kW PV from a watchdog-offline driver must not inflate
// household load. Mirrors TestSumOnlineEVWSkipsOfflineDrivers: live PV
// plus a corpse reading used to skip DC-link export protection.
func TestSiteLoadWIgnoresOfflinePV(t *testing.T) {
	st := NewState(0, 100, "meter")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -5500)
	emitPV(t, store, "live-pv", -6000)
	emitPV(t, store, "dead-pv", -4000)
	store.DriverHealthMut("dead-pv").SetOffline()

	got := siteLoadW(st, store)
	if math.Abs(got-500) > 1 {
		t.Errorf("siteLoadW = %.1f, want 500 (offline −4 kW PV ignored)", got)
	}
}

func TestSiteLoadWIgnoresOfflineBattery(t *testing.T) {
	st := NewState(0, 100, "meter")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -5500)
	emitPV(t, store, "pv", -6000)
	emitBattery(t, store, "dead-bat", -5000, 0.5)
	store.DriverHealthMut("dead-bat").SetOffline()

	got := siteLoadW(st, store)
	if math.Abs(got-500) > 1 {
		t.Errorf("siteLoadW = %.1f, want 500 (offline battery ignored)", got)
	}
}

func TestProtectivePVCurtailIgnoresOfflinePV(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("ferroamp", telemetry.DerPV, -6000, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	store.Update("dead-pv", telemetry.DerPV, -4000, nil, nil)
	store.DriverHealthMut("dead-pv").SetOffline()
	soc := 0.85
	store.Update("ferroamp", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	store.Update("ferroamp", telemetry.DerMeter, -5500, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()

	st := NewState(0, 0, "ferroamp")
	st.SupportsPVCurtail = map[string]bool{"ferroamp": true}
	st.DCLinkProtectionEnabled = true
	st.DCLinkProtectionSoCThreshold = 0.80
	st.DCLinkProtectionMarginW = 1000

	limit, ok := protectiveCurtailLimitW(st, store)
	if !ok {
		t.Fatal("protection must still engage; offline PV must not inflate load")
	}
	if math.Abs(limit-1500) > 50 {
		t.Errorf("protective limit = %f, want ≈ 1500", limit)
	}
}

func TestMissingSoCDoesNotDischarge(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("ferroamp", telemetry.DerMeter, 2000, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	store.Update("ferroamp", telemetry.DerBattery, 0, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()

	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	for _, tg := range ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040) {
		if tg.TargetW < 0 {
			t.Errorf("missing SoC must not discharge, got %+v", tg)
		}
	}
}

func TestMissingSoCAllowsChargeFromSurplus(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("ferroamp", telemetry.DerMeter, -2000, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	store.Update("ferroamp", telemetry.DerBattery, 0, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()

	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 || targets[0].TargetW <= 0 {
		t.Errorf("missing SoC should still allow charge-from-surplus, got %+v", targets)
	}
}

func TestMissingSoCDischargeStaysBlockedThroughSafety(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gridW    float64
		batteryW float64
		mode     Mode
		holdoff  bool
	}{
		{name: "slew from autonomous discharge", gridW: 2000, batteryW: -1000, mode: ModeSelfConsumption},
		{name: "fuse relief from idle", gridW: 15000, mode: ModeIdle},
		{name: "fuse relief cancels charge", gridW: 15000, batteryW: 1000, mode: ModeSelfConsumption},
		{name: "fuse relief during dispatch holdoff", gridW: 15000, mode: ModeSelfConsumption, holdoff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := telemetry.NewStore()
			emitMeter(t, store, "meter", tc.gridW)
			store.Update("battery", telemetry.DerBattery, tc.batteryW, nil, nil)
			store.DriverHealthMut("battery").RecordSuccess()
			st := NewState(0, 50, "meter")
			st.Mode = tc.mode
			st.SlewRateW = 500
			st.MinDispatchIntervalS = 0
			if tc.holdoff {
				now := st.now()
				st.LastDispatch = &now
				st.MinDispatchIntervalS = 60
			}
			targets := ComputeDispatch(store, st, caps(map[string]float64{"battery": 15200}), 11040)
			if !tc.holdoff && len(targets) != 1 {
				t.Fatalf("expected a battery command, got %+v", targets)
			}
			for _, target := range targets {
				if target.TargetW < 0 {
					t.Errorf("unknown SoC cannot authorize discharge: %+v", target)
				}
			}
			for _, target := range st.LastTargets {
				if target.TargetW < 0 {
					t.Errorf("recorded a discharge command without SoC: %+v", target)
				}
			}
		})
	}
}

func TestMissingSoCFuseReliefUsesKnownBattery(t *testing.T) {
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", 15000)
	store.Update("unknown", telemetry.DerBattery, 0, nil, nil)
	store.DriverHealthMut("unknown").RecordSuccess()
	emitBattery(t, store, "known", 0, 0.5)
	st := NewState(0, 50, "meter")
	st.Mode = ModeIdle
	st.MinDispatchIntervalS = 0
	targets := ComputeDispatch(store, st, caps(map[string]float64{"unknown": 15200, "known": 15200}), 11040)
	knownDischarges := false
	for _, target := range targets {
		if target.Driver == "unknown" && target.TargetW < 0 {
			t.Errorf("unknown battery received fuse discharge: %+v", target)
		}
		if target.Driver == "known" && target.TargetW < 0 {
			knownDischarges = true
		}
	}
	if !knownDischarges {
		t.Fatalf("known battery should still provide fuse relief, got %+v", targets)
	}
}

func TestLiveCurtailLimitWStaleMeter(t *testing.T) {
	st := NewState(0, 100, "meter")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -2000)
	emitPV(t, store, "sungrow", -4000)
	if _, ok := liveCurtailLimitW(st, store); !ok {
		t.Fatal("fresh meter should yield a live cap")
	}
	store.Get("meter", telemetry.DerMeter).UpdatedAt = st.now().Add(-2 * time.Minute)
	if _, ok := liveCurtailLimitW(st, store); ok {
		t.Fatal("stale UpdatedAt on DerMeter must not yield a live cap")
	}
}

func TestLiveCurtailLimitWMissingMeterDoesNotUseBattery(t *testing.T) {
	st := NewState(0, 100, "meter")
	store := telemetry.NewStore()
	emitBattery(t, store, "ferroamp", 5000, 0.5)
	emitPV(t, store, "sungrow", -4000)
	if limit, ok := liveCurtailLimitW(st, store); ok {
		t.Fatalf("missing meter must not treat battery watts as grid (ok=true, limit=%.1f)", limit)
	}
}

func TestLiveCurtailLimitWOfflineMeter(t *testing.T) {
	st := NewState(0, 100, "meter")
	store := telemetry.NewStore()
	emitMeter(t, store, "meter", -2000)
	store.DriverHealthMut("meter").SetOffline()
	if _, ok := liveCurtailLimitW(st, store); ok {
		t.Fatal("watchdog-offline meter must not yield a live cap")
	}
}
