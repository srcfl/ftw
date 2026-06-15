package flexload

import (
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/thermalmodel"
)

func trainedModel() thermalmodel.Model {
	m := thermalmodel.NewModel()
	m.Samples = thermalmodel.WarmupSamples
	return *m
}

// TestSimpleBlocksWhenBufferCovers verifies the simple controller blocks
// heating during an expensive period when the building's inertia keeps the
// target for the block horizon — and heats otherwise.
func TestSimpleBlocksWhenBufferCovers(t *testing.T) {
	m := trainedModel() // τ = 4h prior → slow decay

	// 2.5°C margin over target at a mild 15°C outdoor → ~1.6h of coast with
	// the τ=4h prior, comfortably over the 1h block horizon.
	expensive := SimpleSpec{
		Model: m, CurrentC: 22.5, TargetC: 20.0, MinC: 18.0,
		Outdoor: 15.0, PriceNow: 300, PriceThreshold: 150,
		BlockHorizon: time.Hour, MaxHeatW: 2000, COP: 1,
		Confidence: 1.0, // trained model → blocking permitted
	}
	d := EvaluateSimple(expensive)
	if d.Heat {
		t.Errorf("expected block (coast %.1fh ≥ 1h, price 300>150): %s", d.CoastHours, d.Reason)
	}
	if d.SetpointC != expensive.MinC {
		t.Errorf("blocked zone setpoint = %.1f, want MinC %.1f", d.SetpointC, expensive.MinC)
	}

	// Cheap price → heat to target even though buffer exists.
	cheap := expensive
	cheap.PriceNow = 50
	if dd := EvaluateSimple(cheap); !dd.Heat {
		t.Error("cheap price should heat to target")
	}
}

// TestSimpleNeverBlocksUntrainedModel verifies the core safety guarantee:
// even when it's expensive and the (prior-only) model claims a big buffer,
// a zone whose model isn't trained is NOT blocked — it maintains target.
func TestSimpleNeverBlocksUntrainedModel(t *testing.T) {
	m := *thermalmodel.NewModel() // fresh → Quality()==0
	spec := SimpleSpec{
		Model: m, CurrentC: 22.5, TargetC: 20.0, MinC: 18.0,
		Outdoor: 15.0, PriceNow: 300, PriceThreshold: 150,
		BlockHorizon: time.Hour, MaxHeatW: 2000, COP: 1,
		Confidence: m.Quality(), // 0 → below MinBlockConfidence
	}
	d := EvaluateSimple(spec)
	if !d.Heat {
		t.Error("untrained model must never trigger a block — maintain target")
	}
}

// TestSimpleProtectsFloor verifies a zone at/below the floor always heats,
// regardless of price.
func TestSimpleProtectsFloor(t *testing.T) {
	m := trainedModel()
	spec := SimpleSpec{
		Model: m, CurrentC: 17.9, TargetC: 20.0, MinC: 18.0,
		Outdoor: -10.0, PriceNow: 999, PriceThreshold: 100,
		BlockHorizon: time.Hour, MaxHeatW: 3000, COP: 1,
	}
	d := EvaluateSimple(spec)
	if !d.Heat {
		t.Error("zone below floor must heat regardless of price")
	}
}

// TestSimpleHeatsWhenBufferInsufficient verifies that even at a high price,
// a zone close to its target (little buffer) heats to protect comfort.
func TestSimpleHeatsWhenBufferInsufficient(t *testing.T) {
	m := trainedModel()
	m.Beta[0] = 1.0 / 1200.0 // τ = 20min, very leaky → tiny buffer
	spec := SimpleSpec{
		Model: m, CurrentC: 20.1, TargetC: 20.0, MinC: 18.0,
		Outdoor: -15.0, PriceNow: 400, PriceThreshold: 100,
		BlockHorizon: time.Hour, MaxHeatW: 3000, COP: 1,
	}
	d := EvaluateSimple(spec)
	if !d.Heat {
		t.Errorf("leaky zone with <1h buffer should heat despite high price (coast %.2fh)", d.CoastHours)
	}
}

// TestArbitrateBlocksHighestPowerFirst verifies that under a shared power
// budget, the highest-power zone that can afford to coast is blocked first.
func TestArbitrateBlocksHighestPowerFirst(t *testing.T) {
	m := trainedModel()
	// Two zones, both want to heat, both have ample buffer. Budget only
	// fits one. The 3 kW zone should be blocked before the 1 kW zone.
	specs := []SimpleSpec{
		{Model: m, CurrentC: 21.5, TargetC: 20, MinC: 18, Outdoor: 5, MaxHeatW: 1000, COP: 1, BlockHorizon: time.Hour},
		{Model: m, CurrentC: 21.5, TargetC: 20, MinC: 18, Outdoor: 5, MaxHeatW: 3000, COP: 1, BlockHorizon: time.Hour},
	}
	decisions := []SimpleDecision{
		{Heat: true, SetpointC: 20, EstHeatW: 1000, CoastHours: 5},
		{Heat: true, SetpointC: 20, EstHeatW: 3000, CoastHours: 5},
	}
	ArbitrateSimple(decisions, specs, 1500) // budget fits the 1kW zone only
	if !decisions[0].Heat {
		t.Error("1 kW zone should keep heating under budget")
	}
	if decisions[1].Heat {
		t.Error("3 kW zone should be blocked first (highest power, comfort allows)")
	}
}

// TestArbitrateProtectsZoneNeedingHeat verifies arbitration never blocks a
// zone that lacks the buffer to coast, even if it's the highest power.
func TestArbitrateProtectsZoneNeedingHeat(t *testing.T) {
	m := trainedModel()
	specs := []SimpleSpec{
		{Model: m, CurrentC: 20.05, TargetC: 20, MinC: 18, Outdoor: 5, MaxHeatW: 3000, COP: 1, BlockHorizon: time.Hour},
		{Model: m, CurrentC: 22.0, TargetC: 20, MinC: 18, Outdoor: 5, MaxHeatW: 1000, COP: 1, BlockHorizon: time.Hour},
	}
	decisions := []SimpleDecision{
		{Heat: true, SetpointC: 20, EstHeatW: 3000, CoastHours: 0.1}, // no buffer
		{Heat: true, SetpointC: 20, EstHeatW: 1000, CoastHours: 6},   // ample buffer
	}
	ArbitrateSimple(decisions, specs, 1000)
	if !decisions[0].Heat {
		t.Error("3 kW zone with no buffer must not be blocked")
	}
	if decisions[1].Heat {
		t.Error("1 kW zone with buffer should yield instead")
	}
}

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
