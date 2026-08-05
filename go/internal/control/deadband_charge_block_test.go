package control

// What an early exit owes a battery whose charge direction is closed.
//
// floorBlockedCharge (#809) runs inside applyDispatchSafetyPipeline. The
// three early exits — idle, holdoff, reactive deadband — return through
// fuseSaverEarlyExit and never reach it. An exit that issues no target
// withdraws nothing: main.go only sends commands for the targets
// ComputeDispatch returned, and a driver holds its last accepted setpoint
// until it gets another one.
//
// Only the deadband exit turns that into a trap that does not end, because
// its own condition can be satisfied BY the violation: the battery absorbing
// the surplus is what keeps the meter near target. The tests below split into
// three groups — the deadband cases the fix withdraws, the quiet ticks it
// must leave quiet, and the two exits deliberately left alone with the
// evidence for why.

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type deadbandBattery struct {
	name          string
	currentW, soc float64
	capability    string
}

func seedDeadbandSite(gridW float64, bats []deadbandBattery) *telemetry.Store {
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

// passiveArbitrageIdleState builds a site on a passive-arbitrage idle slot —
// the slot whose live-export gate closes the charge direction site-wide.
func passiveArbitrageIdleState(now time.Time) *State {
	st := NewState(0, 60, "meter")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{
			SlotStart:       now.Add(-7 * time.Minute),
			SlotEnd:         now.Add(8 * time.Minute),
			BatteryEnergyWh: 0,
			Strategy:        "passive_arbitrage",
		}, true
	}
	return st
}

// ---- The deadband cases the fix withdraws ----

// The reproduction, from the P2 raised on #809. An idle arbitrage slot over a
// 2 kW PV surplus: the battery eats the surplus, so the meter reads -50 W and
// the grid error sits inside the 60 W deadband. The tick that would have
// stopped the charge walks away because the charge is what makes the error
// small — the violation sustains the condition that hides it, tick after
// tick, for as long as the sun holds. Nothing downstream withdraws the
// earlier charge command, so the surplus the slot exists to export goes into
// the pack instead.
func TestDeadbandExitMayNotStrandBlockedCharge(t *testing.T) {
	now := time.Now()
	store := seedDeadbandSite(-50, []deadbandBattery{{"ferroamp", 2000, 0.55, ""}})
	st := passiveArbitrageIdleState(now)

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Fatalf("no target issued — the tick left the battery charging at +2000 W into a closed charge direction")
	}
	got := targetsByDriver(targets)
	if math.Abs(got["ferroamp"].TargetW) > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W, want 0 W — an idle arbitrage slot over an exporting baseline forbids charge",
			got["ferroamp"].TargetW)
	}
}

// Same law through the second site-wide authority: planner_self with no fresh
// plan is discharge-only. The battery is charging at +2500 W and the meter is
// 40 W inside the deadband because of it.
func TestDeadbandExitMayNotStrandStalePlanChargeBlock(t *testing.T) {
	store := seedDeadbandSite(-40, []deadbandBattery{{"ferroamp", 2500, 0.60, ""}})
	st := NewState(0, 60, "meter")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	st.PlanTarget = func(time.Time) (string, float64, bool) { return "", 0, false }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Fatalf("no target issued — a stale plan left the battery charging at +2500 W")
	}
	got := targetsByDriver(targets)
	if math.Abs(got["ferroamp"].TargetW) > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W, want 0 W — planner_self on a stale plan may not charge",
			got["ferroamp"].TargetW)
	}
	if !st.PlanStale {
		t.Errorf("scenario did not arm the stale-plan gate — the test proves nothing")
	}
}

// The per-driver authority: the driver itself reported charge_capable=false
// while measured mid-charge. Same deadband, same silence, and the earlier
// charge command keeps running into hardware that has said it cannot take it.
func TestDeadbandExitMayNotStrandChargeBlockedDriver(t *testing.T) {
	store := seedDeadbandSite(-30, []deadbandBattery{
		{"ferroamp", 2000, 0.50, `{"discharge_capable":true,"charge_capable":false}`},
	})
	st := NewState(0, 60, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Fatalf("no target issued — the driver said it cannot charge and was left charging at +2000 W")
	}
	got := targetsByDriver(targets)
	if math.Abs(got["ferroamp"].TargetW) > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W, want 0 W — the driver reported charge_capable=false",
			got["ferroamp"].TargetW)
	}
}

// ---- The quiet ticks the fix must leave quiet ----

// The guard that bounds the whole change: a deadband tick with nothing
// blocked issues nothing, exactly as before. A battery measured charging is
// not by itself a reason to speak — most of a sunny day looks like this, and
// turning those ticks into commands would put a driver refusal counter (#800)
// behind every one of them.
func TestDeadbandExitStaysQuietWhenNothingIsBlocked(t *testing.T) {
	store := seedDeadbandSite(-30, []deadbandBattery{{"ferroamp", 2000, 0.50, ""}})
	st := NewState(0, 60, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("deadband tick issued %d target(s) %v — no gate closed charge here, the tick must stay quiet",
			len(targets), targets)
	}
}

// The other half of the guard: the block is armed, but no battery is charging
// against it, so there is nothing to withdraw and the tick stays quiet. This
// is what keeps the predicate "a blocked battery is charging" rather than the
// much broader "a block exists" — the latter would speak on every deadband
// tick of an idle arbitrage slot, all night.
func TestDeadbandExitStaysQuietWhenTheBlockedBatteryIsIdle(t *testing.T) {
	now := time.Now()
	store := seedDeadbandSite(-30, []deadbandBattery{{"ferroamp", 0, 0.55, ""}})
	st := passiveArbitrageIdleState(now)

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("deadband tick issued %d target(s) %v — the charge block had nothing to withdraw",
			len(targets), targets)
	}
}

// A blocked battery measured DISCHARGING is not charging against its block
// either. The charge floor is one-sided and so is the exit that feeds it.
func TestDeadbandExitStaysQuietWhenTheBlockedBatteryIsDischarging(t *testing.T) {
	now := time.Now()
	store := seedDeadbandSite(30, []deadbandBattery{{"ferroamp", -2000, 0.55, ""}})
	st := passiveArbitrageIdleState(now)

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("deadband tick issued %d target(s) %v — a discharging battery is not charging against a charge block",
			len(targets), targets)
	}
}

// The discharge-side clause on the line above the new one still behaves the
// same way. It was the precedent for this fix; a regression in it would make
// the argument for the fix false.
func TestDeadbandDischargeCarveOutStillFires(t *testing.T) {
	now := time.Now()
	store := seedDeadbandSite(30, []deadbandBattery{{"ferroamp", -2000, 0.55, ""}})
	st := NewState(0, 60, "meter")
	st.Mode = ModePlannerCheap
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	st.PlanTarget = func(time.Time) (string, float64, bool) { return "charge", 0, true }
	_ = now

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Fatalf("no target issued — a charge slot with the battery discharging must not take the deadband exit")
	}
}

// ---- The two exits left alone, and the evidence ----

// The holdoff exit reaches no charge block either, and does not need to. It
// only arms after a dispatch actually happened, and fuseSaverEarlyExit
// refreshes LastDispatch only when the fuse-saver fires, so the window is
// bounded by MinDispatchIntervalS and then the normal path runs. This test
// pins that bound: identical inputs, quiet inside the window, withdrawn the
// moment it expires. Reaching the block from the holdoff exit would mean
// hoisting the meter read, the battery gather and the gate computation above
// it — and the two planner_self gates that ARE known there without the
// arbitrage one are exactly the partial enumeration #809 argued against.
func TestHoldoffDelaysTheChargeBlockButDoesNotDefeatIt(t *testing.T) {
	now := time.Now()
	caps := caps(map[string]float64{"ferroamp": 15200})

	inWindow := passiveArbitrageIdleState(now)
	inWindow.MinDispatchIntervalS = 5
	recent := now.Add(-1 * time.Second)
	inWindow.LastDispatch = &recent
	held := ComputeDispatch(seedDeadbandSite(-50, []deadbandBattery{{"ferroamp", 2000, 0.55, ""}}), inWindow, caps, 11040)
	if len(held) != 0 {
		t.Errorf("holdoff tick issued %d target(s) %v — the holdoff window suppresses re-dispatch", len(held), held)
	}

	expired := passiveArbitrageIdleState(now)
	expired.MinDispatchIntervalS = 5
	stale := now.Add(-30 * time.Second)
	expired.LastDispatch = &stale
	after := ComputeDispatch(seedDeadbandSite(-50, []deadbandBattery{{"ferroamp", 2000, 0.55, ""}}), expired, caps, 11040)
	if len(after) == 0 {
		t.Fatalf("holdoff expired and the charge block still issued nothing — the delay became a defeat")
	}
	if got := targetsByDriver(after); math.Abs(got["ferroamp"].TargetW) > 0.01 {
		t.Errorf("ferroamp TargetW = %.1f W, want 0 W once the holdoff window has passed", got["ferroamp"].TargetW)
	}
}

// Idle is not a charge-block bypass, and the reason has not changed now that
// it commands a held zero rather than withholding commands: the site-wide
// block cannot be armed there at all, because effectiveMode reaches ModeIdle
// only from the operator's own idle mode or from a planner slot that took the
// branch where all three noSelfCharge gates are false. What remains is the
// per-driver report, and this test pins that idle treats it exactly as it
// treats an unblocked battery — 0 W either way, which is what a charge block
// asks for and what idle asks for.
//
// The note this test used to carry said that if somebody later decided idle
// must withdraw the previous command on entry, the test would move. It did.
func TestIdleHoldsBlockedAndUnblockedBatteriesAlike(t *testing.T) {
	caps := caps(map[string]float64{"ferroamp": 15200})

	blocked := NewState(0, 60, "meter")
	blocked.Mode = ModeIdle
	blocked.MinDispatchIntervalS = 0
	blockedOut := ComputeDispatch(
		seedDeadbandSite(-2000, []deadbandBattery{
			{"ferroamp", 2000, 0.50, `{"discharge_capable":true,"charge_capable":false}`},
		}), blocked, caps, 11040)

	unblocked := NewState(0, 60, "meter")
	unblocked.Mode = ModeIdle
	unblocked.MinDispatchIntervalS = 0
	unblockedOut := ComputeDispatch(
		seedDeadbandSite(-2000, []deadbandBattery{{"ferroamp", 2000, 0.50, ""}}),
		unblocked, caps, 11040)

	if len(blockedOut) != len(unblockedOut) {
		t.Errorf("idle treated a charge-blocked battery differently: blocked=%v unblocked=%v", blockedOut, unblockedOut)
	}
	if len(blockedOut) != 1 {
		t.Fatalf("idle issued %d target(s) %v — idle holds every battery it may command", len(blockedOut), blockedOut)
	}
	if math.Abs(blockedOut[0].TargetW) > 0.01 || math.Abs(unblockedOut[0].TargetW) > 0.01 {
		t.Errorf("idle must hold both at 0 W: blocked=%.2f W unblocked=%.2f W",
			blockedOut[0].TargetW, unblockedOut[0].TargetW)
	}
}

// The predicate's contract, pinned directly so the deadband condition cannot
// drift away from floorBlockedCharge's two authorities.
func TestAnyBlockedBatteryChargingContract(t *testing.T) {
	charging := []batteryInfo{{driver: "a", currentW: 2000}}
	idle := []batteryInfo{{driver: "a", currentW: 0}}
	discharging := []batteryInfo{{driver: "a", currentW: -2000}}
	chargingBlocked := []batteryInfo{{driver: "a", currentW: 2000, chargeBlocked: true}}
	mixed := []batteryInfo{
		{driver: "a", currentW: -2000, chargeBlocked: true},
		{driver: "b", currentW: 2000},
	}

	cases := []struct {
		name         string
		bats         []batteryInfo
		noSelfCharge bool
		want         bool
	}{
		{"site-wide block, charging", charging, true, true},
		{"site-wide block, idle", idle, true, false},
		{"site-wide block, discharging", discharging, true, false},
		{"no block, charging", charging, false, false},
		{"per-driver block, charging", chargingBlocked, false, true},
		{"per-driver block, only the unblocked one charges", mixed, false, false},
		{"site-wide block reaches the unblocked one", mixed, true, true},
		{"nothing at all", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := anyBlockedBatteryCharging(c.bats, c.noSelfCharge); got != c.want {
				t.Errorf("anyBlockedBatteryCharging = %v, want %v", got, c.want)
			}
		})
	}
}
