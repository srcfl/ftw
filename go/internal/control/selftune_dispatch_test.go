package control

import (
	"math"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// selfTuneSite is idleSite outside idle: the mode a site is normally in when
// the operator starts a self-tune run.
func selfTuneSite(gridW float64, bats []idleBattery) (*telemetry.Store, *State, map[string]float64) {
	store, st, capacities := idleSite(gridW, bats)
	st.Mode = ModeSelfConsumption
	return store, st, capacities
}

// A self-tune step goes to the controllable battery pool and nowhere else. A
// charger, a PV inverter and a telemetry-only battery are registered drivers
// too, but each refuses a battery command, and every refusal costs the site a
// default-mode write and eventually a fault on that driver.
func TestSelfTuneCommandsOnlyTheControllableBatteries(t *testing.T) {
	store, st, capacities := selfTuneSite(500, []idleBattery{
		{name: "tuned", currentW: 0, soc: 0.50, online: true},
		{name: "sibling", currentW: 800, soc: 0.50, online: true},
	})
	store.Update("easee", telemetry.DerEV, 7000, nil, nil)
	store.DriverHealthMut("easee").RecordSuccess()
	store.Update("pv", telemetry.DerPV, -3000, nil, nil)
	store.DriverHealthMut("pv").RecordSuccess()
	zapSoC := 0.60
	store.Update("zap", telemetry.DerBattery, 0, &zapSoC, nil)
	store.DriverHealthMut("zap").RecordSuccess()

	targets := SelfTuneDispatch(store, st, capacities, 11040, "tuned", 1000)

	if len(targets) != 2 {
		t.Fatalf("self-tune issued %d targets %v, want the tuned battery and its sibling only", len(targets), targets)
	}
	if got := targetFor(t, targets, "tuned").TargetW; math.Abs(got-1000) > 0.01 {
		t.Errorf("tuned target = %.2f W, want the 1000 W step", got)
	}
	if got := targetFor(t, targets, "sibling").TargetW; math.Abs(got) > 0.01 {
		t.Errorf("sibling target = %.2f W, want it held at 0 W", got)
	}
}

// The +3000 W step is still an import. Near the breaker the fuse guard has to
// shrink it exactly as it would a planned charge.
func TestSelfTuneStepYieldsToTheFuseGuard(t *testing.T) {
	store, st, capacities := selfTuneSite(10000, []idleBattery{
		{name: "tuned", currentW: 0, soc: 0.50, online: true},
	})

	targets := SelfTuneDispatch(store, st, capacities, 11040, "tuned", 3000)

	tg := targetFor(t, targets, "tuned")
	// 10 kW now + 3 kW step would import 13 kW through an 11.04 kW fuse.
	if predicted := 10000 + tg.TargetW; predicted > 11040+1 {
		t.Fatalf("tuned target = %.0f W predicts %.0f W import past the 11040 W fuse", tg.TargetW, predicted)
	}
	if !tg.Clamped {
		t.Error("a fuse-guarded step must be marked clamped")
	}
}

// Holding the other batteries at 0 W is the tune's preference, not a rule the
// fuse-saver must obey. An unplanned load past the breaker still discharges a
// held sibling.
func TestSelfTuneHoldStillYieldsToTheFuseSaver(t *testing.T) {
	store, st, capacities := selfTuneSite(14000, []idleBattery{
		{name: "tuned", currentW: 0, soc: 0.50, online: true},
		{name: "sibling", currentW: 0, soc: 0.60, online: true},
	})

	targets := SelfTuneDispatch(store, st, capacities, 11040, "tuned", 1000)

	sibling := targetFor(t, targets, "sibling")
	if sibling.TargetW >= 0 {
		t.Fatalf("sibling target = %.0f W — the fuse-saver must reach past the self-tune hold", sibling.TargetW)
	}
	var sum float64
	for _, tg := range targets {
		sum += tg.TargetW
	}
	if predicted := 14000 + sum; predicted > 11040+1 {
		t.Errorf("targets %v leave %.0f W import past the 11040 W fuse", targets, predicted)
	}
}

// The step stays inside the battery's configured power and the empty-pack
// floor.
func TestSelfTuneStepRespectsPowerLimitsAndEmptyPack(t *testing.T) {
	store, st, capacities := selfTuneSite(0, []idleBattery{
		{name: "tuned", currentW: 0, soc: 0.50, online: true},
	})
	st.DriverLimits = map[string]PowerLimits{"tuned": {MaxChargeW: 2000, MaxDischargeW: 2000}}

	if got := targetFor(t, SelfTuneDispatch(store, st, capacities, 11040, "tuned", 3000), "tuned").TargetW; math.Abs(got-2000) > 0.01 {
		t.Errorf("+3000 W step with a 2000 W charge limit = %.0f W, want 2000 W", got)
	}
	if got := targetFor(t, SelfTuneDispatch(store, st, capacities, 11040, "tuned", -3000), "tuned").TargetW; math.Abs(got+2000) > 0.01 {
		t.Errorf("-3000 W step with a 2000 W discharge limit = %.0f W, want -2000 W", got)
	}

	empty := 0.03
	store.Update("tuned", telemetry.DerBattery, 0, &empty, nil)
	if got := targetFor(t, SelfTuneDispatch(store, st, capacities, 11040, "tuned", -3000), "tuned").TargetW; got < 0 {
		t.Errorf("-3000 W step on an empty pack = %.0f W, want no discharge", got)
	}
}

// An offline tuned battery is owed its autonomous default, not the step.
func TestSelfTuneDoesNotCommandAnOfflineTunedBattery(t *testing.T) {
	store, st, capacities := selfTuneSite(500, []idleBattery{
		{name: "tuned", currentW: 0, soc: 0.50, online: false},
		{name: "sibling", currentW: 0, soc: 0.50, online: true},
	})

	targets := SelfTuneDispatch(store, st, capacities, 11040, "tuned", 1000)

	if len(targets) != 1 || targets[0].Driver != "sibling" {
		t.Fatalf("self-tune issued %v, want only the online sibling held at 0 W", targets)
	}
}
