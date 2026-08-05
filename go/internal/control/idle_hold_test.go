package control

// What idle means.
//
// A battery holds the last setpoint it accepted until something gives it
// another one, so a mode that sends nothing does not stop a fleet — it
// abandons it at whatever the previous mode was asking for. What happens
// after that is the vendor's decision, and the vendors disagree: Ferroamp's
// forced mode expires and the EnergyHub returns to its own self-consumption
// (2026-06-10: 2.6 kW charged from the grid while FTW believed it was
// idling), Sungrow holds until told otherwise. One operator action, two
// outcomes, neither of them "stopped".
//
// So idle commands 0 W, to every battery it may command, on every tick. These
// tests pin the four things that has to mean, because each of them is a way
// the fix could be true in name only:
//
//  1. the zero is issued, not implied;
//  2. it is re-issued every tick, so it cannot expire;
//  3. protection still overrides it — the fuse-saver reaches past the hold;
//  4. it reaches the fleet under control and no further: an offline or
//     faulted driver is owed its autonomous default, not a setpoint.

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// idleSite wires a meter and a set of batteries, each of which can be online,
// offline or faulted, and returns everything ComputeDispatch needs.
type idleBattery struct {
	name     string
	currentW float64
	soc      float64
	online   bool
	fault    bool
}

func idleSite(gridW float64, bats []idleBattery) (*telemetry.Store, *State, map[string]float64) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	capacities := map[string]float64{}
	for _, b := range bats {
		soc := b.soc
		s.Update(b.name, telemetry.DerBattery, b.currentW, &soc, nil)
		h := s.DriverHealthMut(b.name)
		switch {
		case b.fault:
			h.RecordSuccess()
			h.SetDeviceFault(true, "idle test: faulted")
		case b.online:
			h.RecordSuccess()
		default:
			h.SetOffline()
		}
		capacities[b.name] = 15200
	}
	st := NewState(0, 50, "meter")
	st.Mode = ModeIdle
	st.MinDispatchIntervalS = 5
	return s, st, capacities
}

func targetFor(t *testing.T, targets []DispatchTarget, driver string) DispatchTarget {
	t.Helper()
	for _, tg := range targets {
		if tg.Driver == driver {
			return tg
		}
	}
	t.Fatalf("no target for %q in %v", driver, targets)
	return DispatchTarget{}
}

// 1. The zero is issued. A battery charging at 5 kW when the operator selects
// idle is the whole case: under the old contract it kept charging at 5 kW,
// because "no dispatch" left the setpoint it had already accepted in place.
func TestIdleCommandsZeroToARunningBattery(t *testing.T) {
	store, st, capacities := idleSite(5200, []idleBattery{
		{name: "ferroamp", currentW: 5000, soc: 0.55, online: true},
	})

	targets := ComputeDispatch(store, st, capacities, 11040)

	if len(targets) != 1 {
		t.Fatalf("idle issued %d targets %v, want a held zero for the one battery", len(targets), targets)
	}
	if got := targetFor(t, targets, "ferroamp").TargetW; math.Abs(got) > 0.01 {
		t.Errorf("ferroamp target = %.2f W, want 0 W — a battery at 5 kW stays at 5 kW until told otherwise", got)
	}
}

// The hold covers a whole fleet, not just the first battery found.
func TestIdleHoldsEveryBatteryInTheFleet(t *testing.T) {
	store, st, capacities := idleSite(0, []idleBattery{
		{name: "ferroamp", currentW: 3000, soc: 0.55, online: true},
		{name: "pixii", currentW: -1200, soc: 0.40, online: true},
		{name: "sungrow", currentW: 0, soc: 0.80, online: true},
	})

	targets := ComputeDispatch(store, st, capacities, 11040)

	if len(targets) != 3 {
		t.Fatalf("idle issued %d targets %v, want one per online battery", len(targets), targets)
	}
	for _, tg := range targets {
		if math.Abs(tg.TargetW) > 0.01 {
			t.Errorf("%s target = %.2f W, want 0 W", tg.Driver, tg.TargetW)
		}
	}
}

// 2. It is re-issued every tick. This is the property that makes the hold
// ours instead of the vendor's: ferroamp.lua's zero branch re-publishes
// forced idle on every command it receives, and it can only do that if
// commands keep arriving. The holdoff window is the specific thing that must
// not swallow them — idle runs at control cadence, well inside
// MinDispatchIntervalS.
func TestIdleRepublishesTheZeroOnEveryTick(t *testing.T) {
	store, st, capacities := idleSite(400, []idleBattery{
		{name: "ferroamp", currentW: 2000, soc: 0.55, online: true},
	})
	now := time.Now()
	st.clock = func() time.Time { return now }

	for tick := 1; tick <= 5; tick++ {
		targets := ComputeDispatch(store, st, capacities, 11040)
		if len(targets) != 1 {
			t.Fatalf("tick %d: idle issued %d targets %v — a skipped tick lets a forced mode expire",
				tick, len(targets), targets)
		}
		if got := targetFor(t, targets, "ferroamp").TargetW; math.Abs(got) > 0.01 {
			t.Fatalf("tick %d: ferroamp target = %.2f W, want 0 W", tick, got)
		}
		now = now.Add(time.Second)
	}
}

// 3. Protection overrides the hold. The tooltip promises an operator that a
// battery may still discharge during "stop" when the breaker is threatened,
// and this is that promise: 14 kW through an 11.04 kW fuse with the fleet
// held at zero.
func TestIdleHoldStillYieldsToTheFuseSaver(t *testing.T) {
	store, st, capacities := idleSite(14000, []idleBattery{
		{name: "ferroamp", currentW: 0, soc: 0.60, online: true},
	})
	st.DriverLimits = map[string]PowerLimits{"ferroamp": {MaxChargeW: 10000, MaxDischargeW: 10000}}

	targets := ComputeDispatch(store, st, capacities, 11040)

	tg := targetFor(t, targets, "ferroamp")
	if tg.TargetW >= 0 {
		t.Fatalf("ferroamp target = %.0f W — the fuse-saver must reach past idle's hold", tg.TargetW)
	}
	if !tg.Clamped {
		t.Error("a fuse-saver discharge must be marked clamped so the dispatch trace names it")
	}
	// Predicted overage: 14000 − 11040 = 2960 W.
	if want := -2960.0; math.Abs(tg.TargetW-want) > 1 {
		t.Errorf("ferroamp target = %.0f W, want %.0f W", tg.TargetW, want)
	}
}

// 4. The hold reaches the fleet under control and no further. A driver that
// is offline or has reported a device fault is owed its autonomous default
// mode by the tracker in cmd/ftw; a setpoint from here would be core writing
// to a device it has already concluded it cannot drive.
func TestIdleDoesNotCommandOfflineOrFaultedDrivers(t *testing.T) {
	store, st, capacities := idleSite(1000, []idleBattery{
		{name: "ferroamp", currentW: 1500, soc: 0.55, online: true},
		{name: "pixii", currentW: 900, soc: 0.50, online: false},
		{name: "sungrow", currentW: 700, soc: 0.50, online: true, fault: true},
	})

	targets := ComputeDispatch(store, st, capacities, 11040)

	if len(targets) != 1 {
		t.Fatalf("idle issued %d targets %v, want only the online healthy battery", len(targets), targets)
	}
	if targets[0].Driver != "ferroamp" {
		t.Errorf("idle commanded %q; offline and faulted drivers take their own default mode instead", targets[0].Driver)
	}
}

// A battery with no telemetry is not known to be a battery. sungrow.lua
// rejects a `battery` command outright on a string inverter, and a refused
// dispatch command counts against the driver — so the hold must not
// manufacture one for a driver that has never emitted a battery reading.
func TestIdleDoesNotCommandADriverWithNoBatteryReading(t *testing.T) {
	store, st, capacities := idleSite(1000, []idleBattery{
		{name: "ferroamp", currentW: 1500, soc: 0.55, online: true},
	})
	store.Update("string-inverter", telemetry.DerPV, -2000, nil, nil)
	store.DriverHealthMut("string-inverter").RecordSuccess()
	capacities["string-inverter"] = 9600

	targets := ComputeDispatch(store, st, capacities, 11040)

	for _, tg := range targets {
		if tg.Driver == "string-inverter" {
			t.Errorf("idle commanded %q, which has never reported a battery", tg.Driver)
		}
	}
	if len(targets) != 1 {
		t.Fatalf("idle issued %d targets %v, want one", len(targets), targets)
	}
}

// Scope, half one: idle is a battery dispatch mode. PV curtailment is decided
// by the plan slot and dispatched on its own path, so selecting idle does not
// release a curtail the plan is still asking for. Pinned as an equality
// against self_consumption rather than a value, because the claim is that the
// curtail path cannot tell which battery mode is selected.
func TestIdleLeavesPVCurtailAlone(t *testing.T) {
	curtailFor := func(mode Mode) []CurtailTarget {
		store := telemetry.NewStore()
		store.Update("meter", telemetry.DerMeter, -4000, nil, nil)
		store.DriverHealthMut("meter").RecordSuccess()
		store.Update("sungrow", telemetry.DerPV, -6000, nil, nil)
		store.DriverHealthMut("sungrow").RecordSuccess()

		st := NewState(0, 50, "meter")
		st.Mode = mode
		st.SupportsPVCurtail = map[string]bool{"sungrow": true}
		st.SlotDirective = func(time.Time) (SlotDirective, bool) {
			return SlotDirective{
				SlotStart: st.now().Add(-5 * time.Minute),
				SlotEnd:   st.now().Add(10 * time.Minute),
				PVLimitW:  1500,
			}, true
		}
		return ComputePVCurtail(st, store)
	}

	idle := curtailFor(ModeIdle)
	self := curtailFor(ModeSelfConsumption)

	if len(idle) != 1 || idle[0].LimitW <= 0 {
		t.Fatalf("idle released the plan's PV curtail: got %v, want a cap still dispatched", idle)
	}
	if len(idle) != len(self) || idle[0].Driver != self[0].Driver ||
		math.Abs(idle[0].LimitW-self[0].LimitW) > 0.01 {
		t.Errorf("idle changed PV curtailment: idle=%v self_consumption=%v", idle, self)
	}
}

// Scope, half two: idle does not stop the EV. The loadpoint's own reserve is
// still read out of State, so a charging EV keeps its claim on the fuse
// budget while the batteries are held — bundling the two would make "stop
// batteries" silently mean "stop everything".
func TestIdleDoesNotWithdrawTheEVReserve(t *testing.T) {
	store, st, capacities := idleSite(7000, []idleBattery{
		{name: "ferroamp", currentW: 0, soc: 0.60, online: true},
	})
	st.EVChargingW = 4000

	targets := ComputeDispatch(store, st, capacities, 11040)

	if len(targets) != 1 {
		t.Fatalf("idle issued %d targets %v, want the held zero", len(targets), targets)
	}
	if got := targetFor(t, targets, "ferroamp").TargetW; math.Abs(got) > 0.01 {
		t.Errorf("ferroamp target = %.2f W with an EV drawing 4 kW under the fuse, want 0 W", got)
	}
}

// The dispatch payload a held zero produces, spelled out, because the
// Ferroamp incident turned on this exact byte: `power_w: 0` is the driver's
// forced-idle branch, and it is only a hold while it keeps arriving.
func TestIdleTargetMarshalsAsAZeroBatteryCommand(t *testing.T) {
	store, st, capacities := idleSite(0, []idleBattery{
		{name: "ferroamp", currentW: 4000, soc: 0.55, online: true},
	})

	targets := ComputeDispatch(store, st, capacities, 11040)
	if len(targets) != 1 {
		t.Fatalf("idle issued %d targets, want 1", len(targets))
	}
	payload, err := json.Marshal(map[string]any{"action": "battery", "power_w": targets[0].TargetW})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(payload), `{"action":"battery","power_w":0}`; got != want {
		t.Errorf("dispatch payload = %s, want %s", got, want)
	}
}
