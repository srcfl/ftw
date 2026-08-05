package control

// The slew limiter may not re-open a charge direction another stage closed.
//
// Every test here runs at a slew rate a site actually runs (250-1500 W, next
// to NewState's 500 W default) with the battery MEASURED mid-charge. That
// combination is the whole bug: the limiter anchors on measured power, so a
// battery physically charging drags its own command back up regardless of
// what the tick decided. Pinning these at SlewRateW = 100000, the way most of
// the older dispatch tests do, would make all of them pass against the broken
// code.

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// chargingBattery seeds one battery reporting live charge power, optionally
// with a capability block, plus the site meter.
func seedChargeFloorSite(t *testing.T, gridW float64, bats []struct {
	name          string
	currentW, soc float64
	capability    string
}) *telemetry.Store {
	t.Helper()
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	for _, b := range bats {
		soc := b.soc
		var data json.RawMessage
		if b.capability != "" {
			data = json.RawMessage(b.capability)
		}
		s.Update(b.name, telemetry.DerBattery, b.currentW, &soc, data)
		s.DriverHealthMut(b.name).RecordSuccess()
	}
	return s
}

func idleArbitrageSlot(now time.Time, strategy string) SlotDirective {
	return SlotDirective{
		SlotStart:       now.Add(-7 * time.Minute),
		SlotEnd:         now.Add(8 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        strategy,
	}
}

// The reproduction from the harvest, watt for watt: a passive-arbitrage idle
// slot with the meter exporting 2000 W and the battery measured at +2000 W.
// The slot's charge block pins the fleet total to 0; before the post-slew
// floor the limiter walked that back to +1500 W, so the site charged on a
// tick whose whole purpose was to let the surplus reach the meter.
func TestSlewMayNotReopenArbitrageIdleChargeBlock(t *testing.T) {
	now := time.Now()
	store := seedChargeFloorSite(t, -2000, []struct {
		name          string
		currentW, soc float64
		capability    string
	}{
		{"ferroamp", 2000, 0.55, ""},
	})
	st := NewState(0, 60, "meter")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return idleArbitrageSlot(now, "passive_arbitrage"), true
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targetsByDriver(targets)
	if got["ferroamp"].TargetW > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W — an idle arbitrage slot over an exporting meter forbids charge; slew re-opened it",
			got["ferroamp"].TargetW)
	}
}

// Same law, planner_arbitrage rather than passive: the gate covers the whole
// arbitrage family.
func TestSlewMayNotReopenPlannerArbitrageIdleChargeBlock(t *testing.T) {
	now := time.Now()
	store := seedChargeFloorSite(t, -2000, []struct {
		name          string
		currentW, soc float64
		capability    string
	}{
		{"ferroamp", 2000, 0.55, ""},
	})
	st := NewState(0, 60, "meter")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return idleArbitrageSlot(now, "arbitrage"), true
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targetsByDriver(targets)
	if got["ferroamp"].TargetW > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W — idle arbitrage slot over an exporting meter must not charge", got["ferroamp"].TargetW)
	}
}

// planner_self with no fresh plan is discharge-only: it may cover live import
// but may not buy or absorb into the pack until a plan arrives. The stale-plan
// block is the second of the two charge gates the slew loop's snap-to-zero
// carve-out never learned about.
func TestSlewMayNotReopenStalePlanChargeBlock(t *testing.T) {
	store := seedChargeFloorSite(t, -1500, []struct {
		name          string
		currentW, soc float64
		capability    string
	}{
		{"ferroamp", 2500, 0.60, ""},
	})
	st := NewState(0, 60, "meter")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	st.PlanTarget = func(time.Time) (string, float64, bool) { return "", 0, false }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targetsByDriver(targets)
	if got["ferroamp"].TargetW > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W — planner_self on a stale plan is discharge-only; slew re-opened the charge block",
			got["ferroamp"].TargetW)
	}
	if !st.PlanStale {
		t.Errorf("scenario did not arm the stale-plan gate — the test proves nothing")
	}
}

// The per-driver half. A battery that reports charge_capable=false is parked
// at 0 W by the distributor and its share handed to a capable sibling; the
// limiter then anchored it on its own live charge and commanded +1500 W into
// hardware that just said it cannot take it — while the sibling was already
// absorbing that same share.
func TestSlewMayNotReopenChargeBlockedBatterysParkedTarget(t *testing.T) {
	store := seedChargeFloorSite(t, -2500, []struct {
		name          string
		currentW, soc float64
		capability    string
	}{
		{"ferroamp", 2000, 0.50, `{"discharge_capable":true,"charge_capable":false}`},
		{"sungrow", 0, 0.40, ""},
	})
	st := NewState(0, 50, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)
	got := targetsByDriver(targets)
	if got["ferroamp"].TargetW > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W — the driver reported it cannot charge; slew re-opened its parked target",
			got["ferroamp"].TargetW)
	}
	// The capable sibling keeps its legitimate one-step ramp: it was measured
	// at 0 W and the limiter allows one rate's worth of charge per tick.
	if math.Abs(got["sungrow"].TargetW-500) > 0.01 {
		t.Errorf("sungrow TargetW = %.1f W, want 500 W — the capable sibling's ramp must survive the floor",
			got["sungrow"].TargetW)
	}
}

// The floor must not swallow charge nobody blocked. Same slew shape, plain
// self-consumption: the ramp toward the surplus is the correct answer and
// stays.
func TestChargeFloorLeavesUnblockedChargeAlone(t *testing.T) {
	store := seedChargeFloorSite(t, -2500, []struct {
		name          string
		currentW, soc float64
		capability    string
	}{
		{"ferroamp", 2000, 0.50, ""},
	})
	st := NewState(0, 50, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targetsByDriver(targets)
	if math.Abs(got["ferroamp"].TargetW-2500) > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W, want 2500 W — no gate closed charge here, the ramp must survive",
			got["ferroamp"].TargetW)
	}
}

// The floor is one-sided. On a tick that forbids charging, a battery measured
// mid-DISCHARGE still ramps back toward 0 at the slew rate rather than being
// snapped to it — the charge block says nothing about discharge, and covering
// live load must not become collateral damage.
func TestChargeFloorLeavesDischargeAlone(t *testing.T) {
	now := time.Now()
	store := seedChargeFloorSite(t, -2000, []struct {
		name          string
		currentW, soc float64
		capability    string
	}{
		{"ferroamp", -2000, 0.55, ""},
	})
	st := NewState(0, 60, "meter")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return idleArbitrageSlot(now, "passive_arbitrage"), true
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targetsByDriver(targets)
	if math.Abs(got["ferroamp"].TargetW-(-1500)) > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W, want -1500 W — the charge floor must not touch a discharging battery's ramp",
			got["ferroamp"].TargetW)
	}
}

// A direct unit test of the floor's contract, so the shape is pinned
// independently of everything ComputeDispatch does above it: it moves only
// positive targets, only for blocked drivers, and never away from zero.
func TestFloorBlockedChargeContract(t *testing.T) {
	in := []DispatchTarget{
		{Driver: "a", TargetW: 1500},
		{Driver: "b", TargetW: -1500},
		{Driver: "c", TargetW: 0},
	}
	out := floorBlockedCharge(append([]DispatchTarget(nil), in...), false, map[string]bool{"a": true})
	got := targetsByDriver(out)
	if got["a"].TargetW != 0 || !got["a"].Clamped {
		t.Errorf("blocked driver a = %.1f W (clamped=%v), want 0 W clamped", got["a"].TargetW, got["a"].Clamped)
	}
	if got["b"].TargetW != -1500 || got["b"].Clamped {
		t.Errorf("driver b = %.1f W (clamped=%v), want -1500 W untouched", got["b"].TargetW, got["b"].Clamped)
	}
	if got["c"].TargetW != 0 || got["c"].Clamped {
		t.Errorf("driver c = %.1f W (clamped=%v), want 0 W unclamped", got["c"].TargetW, got["c"].Clamped)
	}

	// The site-wide flag covers every driver, blocked list or not.
	all := floorBlockedCharge(append([]DispatchTarget(nil), in...), true, nil)
	gotAll := targetsByDriver(all)
	if gotAll["a"].TargetW != 0 || gotAll["c"].TargetW != 0 {
		t.Errorf("noSelfCharge left charge standing: a=%.1f c=%.1f", gotAll["a"].TargetW, gotAll["c"].TargetW)
	}
	if gotAll["b"].TargetW != -1500 {
		t.Errorf("noSelfCharge moved a discharge target: b=%.1f", gotAll["b"].TargetW)
	}
}
