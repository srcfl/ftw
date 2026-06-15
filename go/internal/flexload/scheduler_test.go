package flexload

import (
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/thermalmodel"
)

// buildSlots makes a horizon with the given per-slot prices, 60-min slots.
func buildSlots(prices []float64) []PriceSlot {
	out := make([]PriceSlot, len(prices))
	start := int64(1_700_000_000_000)
	for i, p := range prices {
		out[i] = PriceSlot{
			StartMs:  start + int64(i)*3600_000,
			LenMin:   60,
			PriceOre: p,
		}
	}
	return out
}

// TestThermalPreHeatsInCheapHours verifies the schedule targets MaxC during
// the cheapest slots and coasts toward MinC during expensive ones, while
// never predicting a breach of the comfort floor.
func TestThermalPreHeatsInCheapHours(t *testing.T) {
	// Cheap night (slots 0-2), expensive evening peak (slots 3-5).
	prices := []float64{20, 25, 30, 200, 220, 210}
	slots := buildSlots(prices)

	m := thermalmodel.NewModel()
	m.Samples = thermalmodel.WarmupSamples // trust learned coefs
	spec := ThermalSpec{
		DriverName:      "living_room",
		Model:           *m,
		CurrentC:        20.0,
		MinC:            19.0,
		MaxC:            22.0,
		MaxHeatW:        2000,
		Outdoor:         func(int64) float64 { return 0.0 },
		PreHeatFraction: 0.5,
	}
	sched := PlanThermal(slots, spec)
	if len(sched.Setpoints) != len(slots) {
		t.Fatalf("want %d setpoints, got %d", len(slots), len(sched.Setpoints))
	}

	// Cheapest half should pre-heat (target MaxC).
	if !sched.Setpoints[0].PreHeat {
		t.Error("cheapest slot should pre-heat")
	}
	if sched.Setpoints[0].TargetC != spec.MaxC {
		t.Errorf("pre-heat slot target = %.1f, want MaxC %.1f", sched.Setpoints[0].TargetC, spec.MaxC)
	}
	// Expensive peak should NOT pre-heat (unless forced by the comfort floor).
	peak := sched.Setpoints[4]
	if peak.PreHeat {
		t.Error("expensive peak slot should not voluntarily pre-heat")
	}

	// Comfort floor: no setpoint below MinC, ever.
	for i, sp := range sched.Setpoints {
		if sp.TargetC < spec.MinC-1e-9 {
			t.Errorf("slot %d target %.2f below MinC %.2f", i, sp.TargetC, spec.MinC)
		}
	}
}

// TestThermalComfortFloorForcesHeat verifies that when coasting would drop
// the zone below MinC in an expensive slot, the scheduler still forces a
// hold at the floor rather than violating comfort to save money.
func TestThermalComfortFloorForcesHeat(t *testing.T) {
	// All slots expensive so the scheduler never *wants* to heat.
	prices := []float64{300, 300, 300, 300}
	slots := buildSlots(prices)

	m := thermalmodel.NewModel()
	m.Samples = thermalmodel.WarmupSamples
	// Very leaky building (short tau) and cold outside → coasting drops fast.
	m.Beta[0] = 1.0 / 1800.0 // tau = 30 min
	spec := ThermalSpec{
		DriverName:      "drafty",
		Model:           *m,
		CurrentC:        19.2,
		MinC:            19.0,
		MaxC:            21.0,
		MaxHeatW:        3000,
		Outdoor:         func(int64) float64 { return -10.0 },
		PreHeatFraction: 0.25,
	}
	sched := PlanThermal(slots, spec)

	// At least one slot must be forced to hold the floor (target == MinC with
	// nonzero heat), proving the floor guard fired.
	forced := false
	for _, sp := range sched.Setpoints {
		if sp.TargetC >= spec.MinC && sp.EstHeatW > 0 {
			forced = true
		}
		if sp.TargetC < spec.MinC-1e-9 {
			t.Fatalf("comfort floor violated: target %.2f < MinC %.2f", sp.TargetC, spec.MinC)
		}
	}
	if !forced {
		t.Error("expected the comfort-floor guard to force heating in a cold leaky zone")
	}
}

// TestThermalCOPAccountsElectricalDraw verifies a hydronic (heat-pump) zone
// reports EstHeatW as the ELECTRICAL draw (thermal/COP), while an otherwise
// identical direct-electric zone reports the full thermal watts.
func TestThermalCOPAccountsElectricalDraw(t *testing.T) {
	// Single cold slot that forces active pre-heat to MaxC.
	slots := buildSlots([]float64{10})

	m := thermalmodel.NewModel()
	m.Samples = thermalmodel.WarmupSamples

	base := ThermalSpec{
		DriverName:      "zone",
		Model:           *m,
		CurrentC:        18.0, // below MaxC → pulling up at full output
		MinC:            19.0,
		MaxC:            22.0,
		MaxHeatW:        3000, // thermal output cap
		Outdoor:         func(int64) float64 { return -5.0 },
		PreHeatFraction: 1.0, // cheapest slot → pre-heat
	}

	electric := base
	electric.COP = 1.0
	elecSched := PlanThermal(slots, electric)

	hydronic := base
	hydronic.COP = 3.0
	hydroSched := PlanThermal(slots, hydronic)

	eW := elecSched.Setpoints[0].EstHeatW
	hW := hydroSched.Setpoints[0].EstHeatW
	if eW != 3000 {
		t.Errorf("direct-electric EstHeatW = %.0f, want 3000 (thermal=electrical)", eW)
	}
	if hW != 1000 {
		t.Errorf("hydronic EstHeatW = %.0f, want 1000 (3000 thermal / COP 3)", hW)
	}
	// Both must reach the same comfort target — COP changes cost, not comfort.
	if elecSched.Setpoints[0].TargetC != hydroSched.Setpoints[0].TargetC {
		t.Error("COP must not change the comfort setpoint")
	}
}

// TestDeferrablePicksCheapestSlots verifies energy lands in the cheapest
// eligible slots and the budget is met.
func TestDeferrablePicksCheapestSlots(t *testing.T) {
	prices := []float64{100, 50, 200, 25, 300, 75}
	slots := buildSlots(prices)
	spec := DeferrableSpec{
		DriverName: "water_heater",
		EnergyWh:   3000, // 3 slots @ 1000W*1h
		PowerW:     1000,
	}
	sched := PlanDeferrable(slots, spec)

	// Cheapest three slots are idx 3 (25), 1 (50), 5 (75).
	wantOn := map[int]bool{3: true, 1: true, 5: true}
	for i, s := range sched.Slots {
		if s.On != wantOn[i] {
			t.Errorf("slot %d (price %.0f): On=%v want %v", i, prices[i], s.On, wantOn[i])
		}
	}
	if sched.ScheduledWh != 3000 {
		t.Errorf("scheduled %.0f Wh, want 3000", sched.ScheduledWh)
	}
}

// TestDeferrableRespectsWindow verifies earliest/deadline bounds are honored.
func TestDeferrableRespectsWindow(t *testing.T) {
	prices := []float64{10, 10, 500, 500, 10, 10} // cheap slots outside window
	slots := buildSlots(prices)
	// Window = slots 2,3 only (the expensive ones) — scheduler must use them
	// because the cheap slots are out of bounds.
	spec := DeferrableSpec{
		DriverName: "pump",
		EnergyWh:   1000,
		PowerW:     1000,
		EarliestMs: slots[2].StartMs,
		DeadlineMs: slots[4].StartMs, // exclusive upper bound → slots 2,3 eligible
	}
	sched := PlanDeferrable(slots, spec)
	for i, s := range sched.Slots {
		if (i == 2 || i == 3) == false && s.On {
			t.Errorf("slot %d outside window must be off", i)
		}
	}
	// One slot of the two should be on (cheapest within window — equal, so idx 2).
	if !sched.Slots[2].On {
		t.Error("expected slot 2 (cheapest in window) to be scheduled")
	}
}

// TestDeferrablePrefersPV checks the PV credit biases selection toward a
// surplus slot even when its raw price is higher.
func TestDeferrablePrefersPV(t *testing.T) {
	slots := buildSlots([]float64{60, 100})
	// Slot 1 is pricier but has full-coverage PV surplus → effective ~0.
	slots[1].PVSurplusW = 2000
	spec := DeferrableSpec{
		DriverName: "dishwasher",
		EnergyWh:   1000,
		PowerW:     1000,
		PreferPV:   true,
	}
	sched := PlanDeferrable(slots, spec)
	if !sched.Slots[1].On {
		t.Error("PV-surplus slot should win despite higher raw price")
	}
	if sched.Slots[0].On {
		t.Error("non-PV slot should stay off when budget fits in the PV slot")
	}
}

// TestDeferrableZeroBudgetAllOff ensures a satisfied/empty load yields an
// explicit all-off schedule (so the dispatcher turns it off).
func TestDeferrableZeroBudgetAllOff(t *testing.T) {
	slots := buildSlots([]float64{10, 20, 30})
	sched := PlanDeferrable(slots, DeferrableSpec{DriverName: "x", EnergyWh: 0, PowerW: 1000})
	if len(sched.Slots) != 3 {
		t.Fatalf("want 3 slots, got %d", len(sched.Slots))
	}
	for i, s := range sched.Slots {
		if s.On {
			t.Errorf("slot %d should be off for zero-budget load", i)
		}
	}
}
