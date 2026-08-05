package appproto

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
)

func samplePlan(generatedAt time.Time) *mpc.Plan {
	slot := generatedAt.UnixMilli()
	return &mpc.Plan{
		GeneratedAtMs: slot,
		Mode:          mpc.ModePassiveArbitrage,
		Actions: []mpc.Action{
			// Cheap night hour, charging off the meter.
			{SlotStartMs: slot, SlotLenMin: 15, PriceOre: 30, PVW: 0, LoadW: 500, BatteryW: 3000, GridW: 3500, SoCPct: 40},
			// Midday, PV covers the house and fills the battery.
			{SlotStartMs: slot + 900_000, SlotLenMin: 15, PriceOre: 60, PVW: -5000, LoadW: 800, BatteryW: 3500, GridW: -700, SoCPct: 60},
			// Evening peak, battery covers the house.
			{SlotStartMs: slot + 1_800_000, SlotLenMin: 15, PriceOre: 180, PVW: 0, LoadW: 4000, BatteryW: -3500, GridW: 500, SoCPct: 45},
			// Nothing to do.
			{SlotStartMs: slot + 2_700_000, SlotLenMin: 15, PriceOre: 55, PVW: 0, LoadW: 600, BatteryW: 0, GridW: 600, SoCPct: 45},
		},
	}
}

func TestPlanGetAnswersOnTheBulkLaneAndEchoesTheRequestId(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)
	box.plan = samplePlan(clock.now)

	deliver(t, h, MsgPlanGet, ptrU32(4), nil)

	f := rec.only(t, MsgPlan)
	if f.lane != LaneBulk {
		t.Fatalf("plan went out on lane %d, want bulk", f.lane)
	}
	if f.env.ID == nil || *f.env.ID != 4 {
		t.Fatalf("plan did not echo the request id: %v", f.env.ID)
	}

	p := body[Plan](t, f)
	if len(p.Slots) != 4 {
		t.Fatalf("plan has %d slots, want 4", len(p.Slots))
	}
	if p.Slots[0].DurationMs != 900_000 {
		t.Fatalf("slot duration = %d ms, want 900000", p.Slots[0].DurationMs)
	}
	if p.Slots[0].PriceMinor == nil || *p.Slots[0].PriceMinor != 30 {
		t.Fatalf("priceMinor = %v, want 30", p.Slots[0].PriceMinor)
	}
	if p.Stale {
		t.Fatal("a plan generated a moment ago was reported stale")
	}
}

// Reason codes are stable machine strings. The box has no idea what language
// anyone reads, and the app owns every word the user sees.
func TestPlanReasonsAreStableCodes(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)
	box.plan = samplePlan(clock.now)

	deliver(t, h, MsgPlanGet, ptrU32(1), nil)
	p := body[Plan](t, rec.only(t, MsgPlan))

	want := []PlanReason{
		ReasonCheapImport,
		ReasonSolarSurplus,
		ReasonExpensiveImport,
		ReasonIdle,
	}
	for i, w := range want {
		if p.Slots[i].Reason != w {
			t.Fatalf("slot %d reason = %q, want %q", i, p.Slots[i].Reason, w)
		}
	}

	known := map[PlanReason]bool{
		ReasonCheapImport: true, ReasonExpensiveImport: true, ReasonSolarSurplus: true,
		ReasonPeakShaving: true, ReasonReserveHeld: true, ReasonExportPaid: true,
		ReasonIdle: true,
	}
	for i, s := range p.Slots {
		if !known[s.Reason] {
			t.Fatalf("slot %d carried %q, which is not one of the stable codes", i, s.Reason)
		}
	}
}

// A ceiling is a hardware limit and a price is an opinion, so the ceiling
// outranks the price bands when both could explain a slot.
func TestDefendingACeilingReadsAsPeakShaving(t *testing.T) {
	ceiling := int64(9000)
	a := mpc.Action{PriceOre: 50, PVW: 0, LoadW: 12000, BatteryW: -3000, GridW: 9000, SoCPct: 50}
	if got := reasonCode(a, 100, 40, &ceiling); got != ReasonPeakShaving {
		t.Fatalf("reason = %q, want %q", got, ReasonPeakShaving)
	}
}

func TestDischargingIntoAnExportingMeterReadsAsPaidExport(t *testing.T) {
	a := mpc.Action{PriceOre: 200, PVW: 0, LoadW: 500, BatteryW: -4000, GridW: -3500, SoCPct: 50}
	if got := reasonCode(a, 100, 20, nil); got != ReasonExportPaid {
		t.Fatalf("reason = %q, want %q", got, ReasonExportPaid)
	}
}

// Idle at its deepest planned point during an expensive hour is the planner
// refusing to spend the reserve, which is worth naming rather than calling
// nothing.
func TestHoldingTheReserveIsNotTheSameAsIdle(t *testing.T) {
	held := mpc.Action{PriceOre: 200, PVW: 0, LoadW: 600, BatteryW: 0, GridW: 600, SoCPct: 10}
	if got := reasonCode(held, 100, 10, nil); got != ReasonReserveHeld {
		t.Fatalf("reason = %q, want %q", got, ReasonReserveHeld)
	}

	// The same slot with the battery well above its floor is simply idle.
	idle := held
	idle.SoCPct = 70
	if got := reasonCode(idle, 100, 10, nil); got != ReasonIdle {
		t.Fatalf("reason = %q, want %q", got, ReasonIdle)
	}
}

// "Nothing is scheduled" and "we do not know what is scheduled" are different
// sentences, and only one of them is honest when the planner could not run.
func TestNoPlanIsStaleRatherThanEmpty(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)
	box.plan = nil

	deliver(t, h, MsgPlanGet, ptrU32(1), nil)
	p := body[Plan](t, rec.only(t, MsgPlan))

	if !p.Stale {
		t.Fatal("a box with no plan claimed it knew the schedule")
	}
	if len(p.Slots) != 0 {
		t.Fatalf("a stale plan carried %d slots", len(p.Slots))
	}
}

func TestAPlanOlderThanTheControlLoopWillActOnIsStale(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)
	box.plan = samplePlan(clock.now.Add(-mpc.MaxPlanAge - time.Minute))

	deliver(t, h, MsgPlanGet, ptrU32(1), nil)
	p := body[Plan](t, rec.only(t, MsgPlan))

	if !p.Stale {
		t.Fatal("a plan the control loop would not act on was reported fresh")
	}
	if len(p.Slots) == 0 {
		t.Fatal("a stale plan should still show what the box last decided")
	}
}

func TestPlanGetWhileBootingIsRefusedAgainstItsOwnRequest(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)
	box.boot = &BootProgress{Phase: BootPhaseDrivers, Pct: 80}

	deliver(t, h, MsgPlanGet, ptrU32(11), nil)

	f := rec.only(t, MsgError)
	if f.env.ID == nil || *f.env.ID != 11 {
		t.Fatal("the error did not carry the request id, so it would raise a session-wide banner")
	}
	if e := body[ErrorBody](t, f); e.Code != ErrBooting {
		t.Fatalf("code = %q", e.Code)
	}
}

// Slot lengths are not guaranteed equal, and an unweighted mean would let a
// stray short slot drag the reference every reason code is judged against.
func TestMeanPriceIsWeightedBySlotLength(t *testing.T) {
	actions := []mpc.Action{
		{SlotLenMin: 60, PriceOre: 100},
		{SlotLenMin: 15, PriceOre: 200},
	}
	got := meanPriceOre(actions)
	want := (100*60.0 + 200*15.0) / 75.0
	if got != want {
		t.Fatalf("mean = %v, want %v", got, want)
	}
}
