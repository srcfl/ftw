package cars

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

// fallback is what the planner's reserve gives the Easee while it has no
// plan it accepts (#1657): whether it keeps a goal charging, and how hard.
func fallback(t *testing.T, h *harness) (ok bool, maxW float64) {
	t.Helper()
	svc := &mpc.Service{LoadpointStates: h.mgr.States}
	// Ask at the goal's deadline: before its latest start a goal with
	// slack holds 0 W (#1718).
	at := h.now
	if st, ok := h.mgr.State("easee"); ok && st.TargetTime.After(at) {
		at = st.TargetTime
	}
	d, _ := svc.SlotDirectiveAt(at)
	return d.LoadpointFallback["easee"], d.LoadpointMaxPowerW["easee"]
}

// reserveNeed is the reserve's need for a session on the charger's own
// table, as fallback's service holds it.
func reserveNeed(st loadpoint.State) (energyWh, powerW float64, ok bool) {
	minW, maxW, steps := loadpoint.LimitPower(st, st.AllowedStepsW)
	return loadpoint.DeadlineNeed(st, minW, maxW, steps)
}

// A failed replan keeps the session car's goal charging, held to that car's
// own charger and level, the moment the box knows the car; never a guess for
// a car it does not know.
func TestAFailedReplanKeepsTheSessionCarsGoal(t *testing.T) {
	volvoGoal := &protocol.CarGoal{TargetPermille: 600, ReadyByMin: 390, Days: 0x7f}
	twoCars := func(t *testing.T) (*harness, protocol.Car, protocol.Car) {
		h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
		h.start()
		tesla := h.only()
		volvo := h.add(protocol.Car{Name: "Volvo", CapacityWh: 40000, MaxW: 7400, Goal: volvoGoal})
		return h, tesla, volvo
	}

	t.Run("a single car at 80 % by 07:00", func(t *testing.T) {
		h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
		h.start()
		h.plug("easee", "s1")
		if ok, maxW := fallback(t, h); !ok || maxW != 11000 {
			t.Fatalf("fallback %v at %v W", ok, maxW)
		}
	})
	t.Run("asking", func(t *testing.T) {
		h, _, _ := twoCars(t)
		h.plug("easee", "s1")
		h.tick(61 * time.Second)
		if st := h.state("easee"); st.CarIdentifiedBy != protocol.CarIdentifiedByAsking {
			t.Fatalf("not asking: %s", st.CarIdentifiedBy)
		}
		if ok, _ := fallback(t, h); ok {
			t.Fatal("a car the box asks about got the reserve")
		}
	})
	t.Run("a visitor with no goal", func(t *testing.T) {
		h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
		h.start()
		if _, err := h.rt.UpdateCar(h.only().ID, Update{Identifiers: &[]string{"rfid-7"}}); err != nil {
			t.Fatal(err)
		}
		h.rt.ChargerIdentified("easee", "rfid-guest")
		h.plug("easee", "s1")
		if ok, _ := fallback(t, h); ok {
			t.Fatal("a visitor without a goal got the reserve")
		}
	})

	h, tesla, volvo := twoCars(t)
	if _, err := h.rt.SetLevel(volvo.ID, .3); err != nil {
		t.Fatal(err)
	}
	h.plug("easee", "s1")
	h.tick(61 * time.Second)

	// (d) A tap, and no control tick to roll the schedules after it.
	if _, err := h.tap("easee", volvo.ID); err != nil {
		t.Fatal(err)
	}
	if ok, maxW := fallback(t, h); !ok || maxW <= 0 || maxW > 7400 {
		t.Fatalf("after the tap: fallback %v at %v W, want at most the Volvo's 7400", ok, maxW)
	}

	// (e) From the Tesla to the Volvo: the Volvo's capacity and level.
	if _, err := h.tap("easee", tesla.ID); err != nil {
		t.Fatal(err)
	}
	if ok, maxW := fallback(t, h); !ok || maxW != 11000 {
		t.Fatalf("the Tesla: fallback %v at %v W", ok, maxW)
	}
	if _, err := h.tap("easee", volvo.ID); err != nil {
		t.Fatal(err)
	}
	st := h.state("easee")
	need, peak, ok := reserveNeed(st)
	want := (0.6 - 0.3) * 40000 / loadpoint.DefaultChargeEfficiency
	if !ok || st.VehicleCapacityWh != 40000 || math.Abs(st.CurrentSoC-.3) > 1e-9 || math.Abs(need-want) > 1 || peak > 7400 {
		t.Fatalf("the Volvo's need %v (want %v) at %v W, capacity %v, level %v", need, want, peak, st.VehicleCapacityWh, st.CurrentSoC)
	}

	// (f) To a visitor: no reserve, in the same tick.
	if _, err := h.tap("easee", ""); err != nil {
		t.Fatal(err)
	}
	if ok, _ := fallback(t, h); ok {
		t.Fatal("a visitor kept the Volvo's reserve")
	}

	// (g) A visitor's goal: met, not surplus only, planned from empty, and
	// the level still unknown.
	if _, err := h.rt.SetSessionGoal("easee", &protocol.CarGoal{TargetPermille: 500, ReadyByMin: 420}, false, nil); err != nil {
		t.Fatal(err)
	}
	st = h.state("easee")
	if st.SurplusOnly || !st.SoCUnknown {
		t.Fatalf("a visitor's goal: surplus only %v, unknown level %v", st.SurplusOnly, st.SoCUnknown)
	}
	if ok, _ := fallback(t, h); !ok {
		t.Fatal("a visitor's goal got no reserve")
	}
	if need, _, _ := reserveNeed(st); math.Abs(need-0.5*76000/loadpoint.DefaultChargeEfficiency) > 1 {
		t.Fatalf("a visitor's need %v, want it from empty", need)
	}
	h.tick(time.Second)
	if h.state("easee").SurplusOnly {
		t.Fatal("the next tick put surplus only back")
	}
	if _, err := h.rt.SetSessionGoal("easee", nil, true, nil); err != nil {
		t.Fatal(err)
	}
	if st := h.state("easee"); !st.SurplusOnly {
		t.Fatal("a visitor without a goal charges from surplus only again")
	}
}

// The reserve stops at the car's own charge limit while its level is its own
// exact reading, as the plan does.
func TestTheReserveStopsAtTheCarsChargeLimit(t *testing.T) {
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
	h.start()
	car := h.only()
	if _, err := h.rt.UpdateCar(car.ID, Update{Sources: &[]string{"tesla-ha"}}); err != nil {
		t.Fatal(err)
	}
	h.plug("easee", "s1")
	h.obs["tesla-ha"] = telemetry.VehicleObservation{Driver: "tesla-ha", SoC: .5, ChargeLimit: .7, At: h.now, ReportedAt: h.now}
	h.tick(time.Second)
	st := h.state("easee")
	need, _, ok := reserveNeed(st)
	if want := (0.7 - 0.5) * 76000 / loadpoint.DefaultChargeEfficiency; !ok || st.CarChargeLimit != .7 || math.Abs(need-want) > 50 {
		t.Fatalf("need %v (want about %v), limit %v", need, want, st.CarChargeLimit)
	}
}

// A steady session does not replan every tick: the runtime replans when the
// car, its level source, its goal or its choices change.
func TestASteadySessionDoesNotReplanEachTick(t *testing.T) {
	h := newHarness(t, homeConfig(), map[string]loadpoint.Schedule{"easee": homeGoal(t)}, nil)
	replans := 0
	h.rt.deps.Replan = func(string) { replans++ }
	h.start()
	h.plug("easee", "s1")
	h.tick(time.Second)
	before := replans
	for range 10 {
		h.tick(time.Second)
	}
	if replans != before {
		t.Fatalf("%d replans in ten quiet ticks", replans-before)
	}
}
