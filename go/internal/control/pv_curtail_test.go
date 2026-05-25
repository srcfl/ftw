package control

import (
	"sort"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// stubSlotDirective returns a fixed directive whenever called. Lets us
// pin PVLimitW for the test without standing up a full mpc.Service.
func stubSlotDirective(d SlotDirective) SlotDirectiveFunc {
	return func(now time.Time) (SlotDirective, bool) {
		return d, true
	}
}

func staleSlotDirective() SlotDirectiveFunc {
	return func(now time.Time) (SlotDirective, bool) {
		return SlotDirective{}, false
	}
}

// emitPV pushes a fresh pv reading into the store via the public Update
// API, matching the path the lua host uses. RawW must be site-signed:
// negative = generation.
func emitPV(t *testing.T, s *telemetry.Store, driver string, w float64) {
	t.Helper()
	s.DriverHealthMut(driver).RecordSuccess()
	s.Update(driver, telemetry.DerPV, w, nil, nil)
}

// emitBattery pushes a battery reading with an explicit SoC%. Used by
// the live-curtail tests to vary absorption headroom.
func emitBattery(t *testing.T, s *telemetry.Store, driver string, w, soc float64) {
	t.Helper()
	s.DriverHealthMut(driver).RecordSuccess()
	s.Update(driver, telemetry.DerBattery, w, &soc, nil)
}

// emitMeter pushes a site-meter reading. Positive = importing, negative
// = exporting. Used by the live-curtail tests to vary load.
func emitMeter(t *testing.T, s *telemetry.Store, driver string, w float64) {
	t.Helper()
	s.DriverHealthMut(driver).RecordSuccess()
	s.Update(driver, telemetry.DerMeter, w, nil, nil)
}

// findCurtail returns the per-driver LimitW from a CurtailTarget slice
// for stable assertions independent of slice order.
func findCurtail(targets []CurtailTarget) map[string]float64 {
	out := map[string]float64{}
	for _, t := range targets {
		out[t.Driver] = t.LimitW
	}
	return out
}

func TestComputePVCurtail_NoDirective_DoesNothing(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = staleSlotDirective()
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -5000)
	if got := ComputePVCurtail(st, store); got != nil {
		t.Errorf("expected no targets when plan is stale; got %+v", got)
	}
}

func TestComputePVCurtail_LimitZero_DoesNothing(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0})
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -5000)
	if got := ComputePVCurtail(st, store); got != nil {
		t.Errorf("expected no targets when PVLimitW=0; got %+v", got)
	}
}

func TestComputePVCurtail_AllocatesLimitProportionally(t *testing.T) {
	// Two PV drivers: sungrow producing 6 kW, ferroamp producing 4 kW.
	// Total = 10 kW; plan caps at 1500 W. Expect:
	//   sungrow  → 1500 × 6/10 = 900 W
	//   ferroamp → 1500 × 4/10 = 600 W
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 1500})
	st.SupportsPVCurtail = map[string]bool{
		"sungrow":  true,
		"ferroamp": true,
	}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -6000)
	emitPV(t, store, "ferroamp", -4000)

	got := findCurtail(ComputePVCurtail(st, store))
	if len(got) != 2 {
		t.Fatalf("want 2 targets, got %d: %+v", len(got), got)
	}
	if abs(got["sungrow"]-900) > 1e-3 {
		t.Errorf("sungrow limit: want 900, got %.2f", got["sungrow"])
	}
	if abs(got["ferroamp"]-600) > 1e-3 {
		t.Errorf("ferroamp limit: want 600, got %.2f", got["ferroamp"])
	}
}

func TestComputePVCurtail_SkipsNonSupportingDriver(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 1000})
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -3000)
	emitPV(t, store, "easee", -2000) // hypothetical PV reading on a non-curtail driver

	got := findCurtail(ComputePVCurtail(st, store))
	if len(got) != 1 {
		t.Fatalf("only sungrow should be curtailed; got %+v", got)
	}
	if abs(got["sungrow"]-1000) > 1e-3 {
		// 100% of limit (only one supporting driver in the pool).
		t.Errorf("sungrow limit: want 1000, got %.2f", got["sungrow"])
	}
}

func TestComputePVCurtail_SkipsDriverNotGenerating(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 800})
	st.SupportsPVCurtail = map[string]bool{
		"sungrow":  true,
		"ferroamp": true,
	}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -2000)
	emitPV(t, store, "ferroamp", 0) // night, not generating

	got := findCurtail(ComputePVCurtail(st, store))
	if _, ok := got["ferroamp"]; ok {
		t.Errorf("ferroamp not generating; should not receive curtail: %+v", got)
	}
	if abs(got["sungrow"]-800) > 1e-3 {
		t.Errorf("sungrow should get full limit; got %.2f", got["sungrow"])
	}
}

// Regression: when the plan stops asking for curtailment, the driver
// that was previously curtailed must receive a one-shot LimitW=0
// (translated to `curtail_disable` by main.go) — otherwise the cap
// stays applied silently after the slot rolls over.
func TestComputePVCurtail_ReleasesPreviouslyCurtailedDriver(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -3000)

	// Tick 1: plan caps PV at 1000 W → sungrow gets curtailed.
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 1000})
	tick1 := findCurtail(ComputePVCurtail(st, store))
	if abs(tick1["sungrow"]-1000) > 1e-3 {
		t.Fatalf("tick1: want sungrow=1000, got %+v", tick1)
	}
	if !st.LastCurtailedDrivers["sungrow"] {
		t.Fatalf("tick1: state should remember sungrow as curtailed")
	}

	// Tick 2: plan no longer caps (slot rolled over). Expect a
	// release target for sungrow with LimitW=0.
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0})
	tick2 := ComputePVCurtail(st, store)
	if len(tick2) != 1 {
		t.Fatalf("tick2: want one release target, got %+v", tick2)
	}
	if tick2[0].Driver != "sungrow" || tick2[0].LimitW != 0 {
		t.Errorf("tick2: want {sungrow, 0}, got %+v", tick2[0])
	}
	if len(st.LastCurtailedDrivers) != 0 {
		t.Errorf("tick2: state should have cleared LastCurtailedDrivers; got %+v",
			st.LastCurtailedDrivers)
	}

	// Tick 3: still no curtail, no driver to release. Expect nil.
	if got := ComputePVCurtail(st, store); got != nil {
		t.Errorf("tick3: want nil (idempotent release), got %+v", got)
	}
}

// Sanity: target slice is deterministic enough that callers can sort
// by driver name without relying on map iteration order.
func TestComputePVCurtail_DeterministicSortable(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 1000})
	st.SupportsPVCurtail = map[string]bool{"a": true, "b": true, "c": true}
	store := telemetry.NewStore()
	emitPV(t, store, "a", -1000)
	emitPV(t, store, "b", -1000)
	emitPV(t, store, "c", -1000)
	got := ComputePVCurtail(st, store)
	if len(got) != 3 {
		t.Fatalf("want 3 targets, got %d", len(got))
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Driver < got[j].Driver })
	for i, d := range []string{"a", "b", "c"} {
		if got[i].Driver != d {
			t.Errorf("idx %d: want driver %q, got %q", i, d, got[i].Driver)
		}
	}
}

// ---- Manual hold ----

// An active aggregate hold overrides whatever the planner directive
// says — including taking effect when the planner isn't curtailing at
// all. Used by the operator-side verification flow.
func TestComputePVCurtail_ManualHoldOverridesPlanner(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0}) // planner not curtailing
	st.SupportsPVCurtail = map[string]bool{"sungrow": true, "solaredge": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -2000)
	emitPV(t, store, "solaredge", -2000)

	st.SetPVManualHold(PVManualHold{LimitW: 1000, ExpiresAt: time.Now().Add(time.Minute)})

	got := findCurtail(ComputePVCurtail(st, store))
	if len(got) != 2 {
		t.Fatalf("want 2 targets (aggregate hold split across both), got %+v", got)
	}
	// 1000 W cap split proportionally over equal-output drivers → 500/500.
	if abs(got["sungrow"]-500) > 1e-3 {
		t.Errorf("sungrow: want 500, got %.2f", got["sungrow"])
	}
	if abs(got["solaredge"]-500) > 1e-3 {
		t.Errorf("solaredge: want 500, got %.2f", got["solaredge"])
	}
}

// Driver-scoped hold caps that one driver only and leaves the others
// uncurtailed. Lets the operator verify a single inverter's curtail
// implementation against live hardware without throttling the rest.
func TestComputePVCurtail_ManualHoldScopedToOneDriver(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0})
	st.SupportsPVCurtail = map[string]bool{"sungrow": true, "solaredge": true}
	store := telemetry.NewStore()
	emitPV(t, store, "sungrow", -2000)
	emitPV(t, store, "solaredge", -2000)

	st.SetPVManualHold(PVManualHold{
		Driver:    "solaredge",
		LimitW:    800,
		ExpiresAt: time.Now().Add(time.Minute),
	})

	got := findCurtail(ComputePVCurtail(st, store))
	if _, ok := got["sungrow"]; ok {
		t.Errorf("sungrow is out of scope; should not be curtailed: %+v", got)
	}
	if abs(got["solaredge"]-800) > 1e-3 {
		t.Errorf("solaredge: want 800, got %.2f", got["solaredge"])
	}
}

// Expired hold falls back cleanly. If the planner is also not asking
// for curtailment, and a previous tick had curtailed the driver under
// the hold, the driver must be released with LimitW=0.
func TestComputePVCurtail_ExpiredHoldReleasesDriver(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0})
	st.SupportsPVCurtail = map[string]bool{"solaredge": true}
	store := telemetry.NewStore()
	emitPV(t, store, "solaredge", -2000)

	// Tick 1: hold active, driver capped.
	st.SetPVManualHold(PVManualHold{
		Driver:    "solaredge",
		LimitW:    500,
		ExpiresAt: time.Now().Add(100 * time.Millisecond),
	})
	t1 := findCurtail(ComputePVCurtail(st, store))
	if abs(t1["solaredge"]-500) > 1e-3 {
		t.Fatalf("tick1: want solaredge=500, got %+v", t1)
	}

	// Tick 2: simulate expiry by jumping past ExpiresAt.
	st.ManualPVHold.ExpiresAt = time.Now().Add(-time.Second)
	t2 := ComputePVCurtail(st, store)
	if len(t2) != 1 || t2[0].Driver != "solaredge" || t2[0].LimitW != 0 {
		t.Errorf("tick2: want a single release target {solaredge, 0}, got %+v", t2)
	}
	// And the hold itself should be evicted as a side effect of GetPVManualHold.
	if _, active := st.GetPVManualHold(time.Now()); active {
		t.Errorf("hold should be evicted after expiry")
	}
}

// Hold targeting a driver that doesn't advertise pv-curtail is silently
// skipped — ComputePVCurtail never dispatches to a driver that wouldn't
// know how to handle the payload.
func TestComputePVCurtail_ManualHoldOnUnsupportedDriverSkipped(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0})
	st.SupportsPVCurtail = map[string]bool{"sungrow": true}
	store := telemetry.NewStore()
	emitPV(t, store, "easee", -1000)

	st.SetPVManualHold(PVManualHold{
		Driver:    "easee",
		LimitW:    500,
		ExpiresAt: time.Now().Add(time.Minute),
	})

	if got := ComputePVCurtail(st, store); got != nil {
		t.Errorf("unsupported driver should be skipped, got %+v", got)
	}
}

// ---- Live-limit behavior ----
//
// Planner says "curtail this slot" (PVLimitW > 0), but the limit
// dispatch sends should track live conditions: rising load lifts the
// cap, battery SoC headroom lifts the cap, EV PV-charging demand lifts
// the cap.

// Battery with SoC headroom suppresses curtail entirely — its
// MaxChargeW (5 kW default) plus live load comfortably exceeds PV.
func TestComputePVCurtail_BatteryHeadroomLiftsCap(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 800}) // planner wants curtail
	st.SupportsPVCurtail = map[string]bool{"solaredge": true}
	store := telemetry.NewStore()
	emitPV(t, store, "solaredge", -3000)
	emitBattery(t, store, "pixii", 0, 60.0) // 60% SoC → 5 kW headroom available
	emitMeter(t, store, "meter", -2500)     // exporting 2.5 kW (load present but PV bigger)

	got := findCurtail(ComputePVCurtail(st, store))
	if len(got) != 0 {
		t.Errorf("battery headroom should suppress curtail (limit >> PV); got %+v", got)
	}
}

// Battery essentially full (SoC >= ceiling) and no EV reserve →
// planner-warranted curtail goes through, capped at live load only.
func TestComputePVCurtail_FullBatteryNoHeadroomCurtails(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 500})
	st.SupportsPVCurtail = map[string]bool{"solaredge": true}
	store := telemetry.NewStore()
	emitPV(t, store, "solaredge", -3000)
	emitBattery(t, store, "pixii", 0, 99.5) // above ceiling — no headroom
	emitMeter(t, store, "meter", -2500)     // exporting 2.5 kW → live load = 500 W

	got := findCurtail(ComputePVCurtail(st, store))
	if abs(got["solaredge"]-500) > 1e-3 {
		t.Errorf("want solaredge capped at live load (500 W), got %.2f", got["solaredge"])
	}
}

// Load rising mid-slot lifts the cap — self-consumption preserved
// even when planner's stale forecast would have throttled PV.
func TestComputePVCurtail_RisingLoadLiftsCap(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 500}) // planner figured load ≈ 500 W
	st.SupportsPVCurtail = map[string]bool{"solaredge": true}
	store := telemetry.NewStore()
	emitPV(t, store, "solaredge", -3000)
	emitBattery(t, store, "pixii", 0, 99.5) // full — no battery headroom
	// Live: load actually became 2500 W (heater turned on). Meter
	// reports import = load - pv = 2500 - 3000 = -500 W (still
	// exporting 500 W). live_load = -500 - (-3000) - 0 = 2500 W.
	emitMeter(t, store, "meter", -500)

	got := findCurtail(ComputePVCurtail(st, store))
	if abs(got["solaredge"]-2500) > 1e-3 {
		t.Errorf("rising load should lift cap to live load (2500 W), got %.2f", got["solaredge"])
	}
}

// EV PV-charging demand (EVSurplusOnlyReserveW) lifts the cap too,
// even with a full battery and no live load.
func TestComputePVCurtail_EVReserveLiftsCap(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 500})
	st.SupportsPVCurtail = map[string]bool{"solaredge": true}
	st.EVSurplusOnlyReserveW = 3500 // wallbox wants 3.5 kW from PV
	store := telemetry.NewStore()
	emitPV(t, store, "solaredge", -3000)
	emitBattery(t, store, "pixii", 0, 99.5)
	emitMeter(t, store, "meter", -3000) // all PV currently exporting

	got := findCurtail(ComputePVCurtail(st, store))
	if len(got) != 0 {
		t.Errorf("EV PV reserve should suppress curtail (3.5 kW > 3 kW PV); got %+v", got)
	}
}

// EVCurtailHeadroomW lifts the cap even when EVSurplusOnlyReserveW is
// zero (plugged-but-stopped EV with SoC headroom). That's the case
// main.go fills with loadpoint.SurplusPotentialW.
func TestComputePVCurtail_EVCurtailHeadroomLiftsCap(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 500})
	st.SupportsPVCurtail = map[string]bool{"solaredge": true}
	st.EVSurplusOnlyReserveW = 0    // EV isn't drawing — dispatch reserve is 0
	st.EVCurtailHeadroomW = 11000   // but it COULD draw up to 11 kW if PV grew
	store := telemetry.NewStore()
	emitPV(t, store, "solaredge", -3000)
	emitBattery(t, store, "pixii", 0, 99.5)
	emitMeter(t, store, "meter", -3000)

	got := findCurtail(ComputePVCurtail(st, store))
	if len(got) != 0 {
		t.Errorf("stopped-EV-with-headroom should suppress curtail; got %+v", got)
	}
}

// Manual hold ignores all the live-limit logic — operator override
// must be verbatim, including when the limit would otherwise be lifted.
func TestComputePVCurtail_ManualHoldBypassesLiveLimit(t *testing.T) {
	st := NewState(0, 100, "meter")
	st.SlotDirective = stubSlotDirective(SlotDirective{PVLimitW: 0}) // planner not curtailing
	st.SupportsPVCurtail = map[string]bool{"solaredge": true}
	store := telemetry.NewStore()
	emitPV(t, store, "solaredge", -3000)
	emitBattery(t, store, "pixii", 0, 50.0)    // big headroom
	emitMeter(t, store, "meter", 0)
	st.SetPVManualHold(PVManualHold{
		Driver:    "solaredge",
		LimitW:    750,
		ExpiresAt: time.Now().Add(time.Minute),
	})

	got := findCurtail(ComputePVCurtail(st, store))
	if abs(got["solaredge"]-750) > 1e-3 {
		t.Errorf("manual hold must be verbatim, got %.2f want 750", got["solaredge"])
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
