package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestPhysicalStorageAllocationSurvivesDispatch(t *testing.T) {
	now := time.Now()
	d := mpc.SlotDirective{DecisionID: "plan", SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 500,
		StorageEnergyWh: map[string]float64{"a": 400, "b": 100}, Strategy: mpc.ModeArbitrage, GridW: 2500}
	dir := SlotDirectiveFromMPC(d)
	d.StorageEnergyWh["a"] = 0
	store := seedStore(500, []struct {
		name          string
		currentW, soc float64
	}{{"a", 0, .5}, {"b", 0, .5}})
	s := newStateWithEnergyDispatch(dir, "ferroamp")
	s.clock = func() time.Time { return now }
	// b sits on a smaller inverter, so the plan's split is physical.
	s.DriverLimits = map[string]PowerLimits{"b": {MaxChargeW: 1500, MaxDischargeW: 1500, MaxChargeWSet: true, MaxDischargeWSet: true}}
	out := ComputeDispatch(store, s, caps(map[string]float64{"a": 10000, "b": 10000}), 11040)
	want := map[string]float64{"a": 1600, "b": 400}
	if len(out) != 2 {
		t.Fatalf("targets=%+v", out)
	}
	for _, v := range out {
		if math.Abs(v.TargetW-want[v.Driver]) > 1e-6 {
			t.Fatalf("targets=%+v", out)
		}
	}
}

func TestHeterogeneousStorageSharesReachFinalDispatch(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		now := time.Now()
		dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 500,
			StorageEnergyWh: map[string]float64{"a": 25, "b": 475}, PlannedGridW: 2500, HasPlannedGridW: true}
		store := seedStore(500, []struct {
			name          string
			currentW, soc float64
		}{{"a", 0, .5}, {"b", 0, .5}})
		if blocked {
			soc := .5
			store.Update("a", telemetry.DerBattery, 0, &soc, json.RawMessage(`{"charge_capable":false}`))
		}
		s := newStateWithEnergyDispatch(dir, "ferroamp")
		s.clock = func() time.Time { return now }
		out := ComputeDispatch(store, s, caps(map[string]float64{"a": 1000, "b": 19000}), 11040)
		if len(out) != 2 {
			t.Fatalf("targets=%+v", out)
		}
		for _, v := range out {
			want := 1900.
			if v.Driver == "a" {
				want = 100
				if blocked {
					want = 0
				}
			}
			if math.Abs(v.TargetW-want) > 1e-6 {
				t.Fatalf("blocked=%v targets=%+v", blocked, out)
			}
		}
	}
}

func TestPhysicalStorageDeliveryAndSiteClamp(t *testing.T) {
	start := time.Now()
	now := start
	dir := SlotDirective{DecisionID: "first", SlotStart: start, SlotEnd: start.Add(time.Hour), BatteryEnergyWh: 2000, StorageEnergyWh: map[string]float64{"a": 1500, "b": 500}}
	s := newStateWithEnergyDispatch(dir, "site")
	s.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	s.clock = func() time.Time { return now }
	bats := []batteryInfo{{driver: "a", capacityWh: 10000, soc: .5},
		{driver: "b", capacityWh: 10000, soc: .5, maxChargeW: 1000, maxDischargeW: 1000, maxChargeWSet: true, maxDischargeWSet: true}}
	out, ok := distributePlannedStorages(s, bats, 1000, false)
	if !ok || out[0].TargetW != 750 || out[1].TargetW != 250 {
		t.Fatalf("site clamp=%+v", out)
	}
	// Only a really delivered energy; b's budget is not credited from a.
	bats[0].currentW = 1500
	now = now.Add(time.Minute)
	out, _ = distributePlannedStorages(s, bats, 10000, false)
	if math.Abs(out[0].TargetW-1500) > 1e-6 || math.Abs(out[1].TargetW-500*60./59) > 1e-6 {
		t.Fatalf("measured delivery=%+v", out)
	}
	bats[1].chargeBlocked = true
	out, _ = distributePlannedStorages(s, bats, 10000, false)
	if out[0].TargetW != 1500 || out[1].TargetW != 0 {
		t.Fatalf("reassigned blocked share=%+v", out)
	}
	dir.DecisionID = "smaller"
	dir.StorageEnergyWh = map[string]float64{"a": 10, "b": 0}
	out, _ = distributePlannedStorages(s, bats, 10000, false)
	if out[0].TargetW != 0 || out[1].TargetW != 0 {
		t.Fatalf("completed budget reversed=%+v", out)
	}
	if _, ok := distributePlannedStorages(s, bats, 1000, true); ok {
		t.Fatal("planner overrode manual hold")
	}
	s.PlanStale = true
	if _, ok := distributePlannedStorages(s, bats, 1000, false); ok {
		t.Fatal("stale plan allocated storage")
	}
}

// Field report, 23 Oct 2026: two identical Pixii batteries took turns. The
// planner is indifferent between them, so Energyplan put a whole slot on one
// battery and the next slot on the other; Core followed each battery's
// share and the SoCs drifted apart (74 % against 60 %). Now both charge in
// every slot, the emptier one faster (room 26 % against 40 %).
func TestInterchangeableStoragesShareThePlanTotal(t *testing.T) {
	start := time.Now()
	now := start
	dir := SlotDirective{DecisionID: "p", SlotStart: start, SlotEnd: start.Add(15 * time.Minute), BatteryEnergyWh: 500,
		StorageEnergyWh: map[string]float64{"a": 500, "b": 0}}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{{"a", 0, .74}, {"b", 0, .60}})
	s := newStateWithEnergyDispatch(dir, "ferroamp")
	s.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	s.clock = func() time.Time { return now }
	for slot, plan := range []map[string]float64{{"a": 500, "b": 0}, {"a": 0, "b": 500}} {
		dir.SlotStart, dir.SlotEnd = now, now.Add(15*time.Minute)
		dir.StorageEnergyWh = plan
		out := ComputeDispatch(store, s, caps(map[string]float64{"a": 10000, "b": 10000}), 11040)
		if len(out) != 2 {
			t.Fatalf("slot %d targets=%+v", slot, out)
		}
		want := map[string]float64{"a": 2000 * 26. / 66, "b": 2000 * 40. / 66}
		for _, v := range out {
			if math.Abs(v.TargetW-want[v.Driver]) > 1e-6 {
				t.Fatalf("slot %d: targets=%+v, want %v", slot, out, want)
			}
		}
		now = now.Add(15 * time.Minute)
	}
}

// A battery that cannot charge hands its share to its twin instead of
// sending it to the grid. The plan named the blocked battery.
func TestInterchangeableStoragesHandOverABlockedShare(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{DecisionID: "p", SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 500,
		StorageEnergyWh: map[string]float64{"a": 500, "b": 0}}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{{"a", 0, .5}, {"b", 0, .5}})
	soc := .5
	store.Update("a", telemetry.DerBattery, 0, &soc, json.RawMessage(`{"charge_capable":false}`))
	s := newStateWithEnergyDispatch(dir, "ferroamp")
	s.clock = func() time.Time { return now }
	out := ComputeDispatch(store, s, caps(map[string]float64{"a": 10000, "b": 10000}), 11040)
	want := map[string]float64{"a": 0, "b": 2000}
	if len(out) != 2 {
		t.Fatalf("targets=%+v", out)
	}
	for _, v := range out {
		if math.Abs(v.TargetW-want[v.Driver]) > 1e-6 {
			t.Fatalf("targets=%+v, want %v", out, want)
		}
	}
}

// The same two batteries on a cover-load slot: the plan put −564 W on one,
// the house drew 3 kW. Both batteries discharge, more than the planned
// 564 W in total, instead of one battery at its planned rate with the rest
// from the grid. The fuller one gives more: 69 % above the floor against 55 %.
func TestInterchangeableStoragesShareALargeLoad(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{DecisionID: "p", SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: -141,
		StorageEnergyWh: map[string]float64{"a": -141, "b": 0}, PlannedGridW: 0, HasPlannedGridW: true}
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{{"a", 0, .74}, {"b", 0, .60}})
	s := newStateWithEnergyDispatch(dir, "ferroamp")
	s.clock = func() time.Time { return now }
	out := ComputeDispatch(store, s, caps(map[string]float64{"a": 10000, "b": 10000}), 11040)
	if len(out) != 2 {
		t.Fatalf("targets=%+v", out)
	}
	w := map[string]float64{}
	for _, v := range out {
		w[v.Driver] = v.TargetW
	}
	if w["b"] >= 0 || math.Abs(w["a"]/w["b"]-69./55) > 1e-6 || w["a"]+w["b"] > -564-1e-6 {
		t.Fatalf("both batteries should share more than the planned 564 W, got %+v", out)
	}
}

// Two equal batteries follow their targets for five hours of discharge, and
// five of charge. The SoC gap shrinks on every tick, neither battery passes
// the floor or full, and both end there together.
func TestInterchangeableStoragesConverge(t *testing.T) {
	for _, tc := range []struct {
		name            string
		socA, socB, end float64
		desiredW        float64
	}{
		{"discharge from 74 % and 60 %", .74, .60, dischargeFloorSoC, -3000},
		{"charge from 30 % and 50 %", .30, .50, 1, 3000},
	} {
		now := time.Now()
		s := NewState(0, 0, "site")
		s.Mode = ModePlannerArbitrage
		s.clock = func() time.Time { return now }
		// The plan puts every slot on a; Core still shares it.
		wh := tc.desiredW / 4
		s.SlotDirective = func(at time.Time) (SlotDirective, bool) {
			return SlotDirective{DecisionID: "p", SlotStart: at, SlotEnd: at.Add(15 * time.Minute), BatteryEnergyWh: wh,
				StorageEnergyWh: map[string]float64{"a": wh, "b": 0}}, true
		}
		bats := []batteryInfo{{driver: "a", capacityWh: 10000, soc: tc.socA}, {driver: "b", capacityWh: 10000, soc: tc.socB}}
		gap := math.Abs(tc.socA - tc.socB)
		for tick := 0; tick < 3600; tick++ {
			out, ok := distributePlannedStorages(s, bats, tc.desiredW, false)
			if !ok || len(out) != 2 {
				t.Fatalf("%s tick %d: targets=%+v", tc.name, tick, out)
			}
			for i := range bats {
				bats[i].soc += out[i].TargetW * 5 / 3600 / bats[i].capacityWh
				bats[i].currentW = out[i].TargetW
			}
			now = now.Add(5 * time.Second)
			next := math.Abs(bats[0].soc - bats[1].soc)
			if next > gap+1e-12 || (next >= gap && next > 1e-9) {
				t.Fatalf("%s tick %d: gap %.4f did not shrink from %.4f", tc.name, tick, next, gap)
			}
			gap = next
			// One 5 s tick at 3 kW moves a 10 kWh battery 0.04 %.
			const tickSoC = 3000. * 5 / 3600 / 10000
			for _, b := range bats {
				if b.soc < dischargeFloorSoC-tickSoC || b.soc > 1+tickSoC {
					t.Fatalf("%s tick %d: %s at %.4f", tc.name, tick, b.driver, b.soc)
				}
			}
		}
		if math.Abs(bats[0].soc-tc.end) > .002 || math.Abs(bats[1].soc-tc.end) > .002 {
			t.Fatalf("%s: ended at %.4f and %.4f", tc.name, bats[0].soc, bats[1].soc)
		}
	}
}

func TestShareByAvailableEnergyStaysWithinLimits(t *testing.T) {
	bats := []batteryInfo{{driver: "a", capacityWh: 10000, soc: .80}, {driver: "b", capacityWh: 10000, soc: .10}}
	// By energy a would give 5,625 W of 6 kW; it stops at 5 kW and b gives the rest.
	out := shareByAvailableEnergy(bats, -6000)
	if out[0].TargetW != -5000 || math.Abs(out[1].TargetW+1000) > 1e-6 {
		t.Fatalf("targets=%+v", out)
	}
	// More than the fleet can give: both at their limits.
	out = shareByAvailableEnergy(bats, -12000)
	if out[0].TargetW != -5000 || out[1].TargetW != -5000 {
		t.Fatalf("targets=%+v", out)
	}
	// A battery at the floor gives nothing.
	bats[1].soc = dischargeFloorSoC
	out = shareByAvailableEnergy(bats, -3000)
	if out[0].TargetW != -3000 || out[1].TargetW != 0 {
		t.Fatalf("targets=%+v", out)
	}
	// Full batteries take nothing.
	bats[0].soc, bats[1].soc = 1, 1
	out = shareByAvailableEnergy(bats, 3000)
	if out[0].TargetW != 0 || out[1].TargetW != 0 {
		t.Fatalf("targets=%+v", out)
	}
}

func TestShareByAvailableEnergyCharge(t *testing.T) {
	// Three batteries charging. c is blocked, a nearly full; b would take
	// 6.4 kW of 8 kW by energy, stops at 5 kW, and a takes the rest.
	bats := []batteryInfo{
		{driver: "a", capacityWh: 10000, soc: .80},
		{driver: "b", capacityWh: 10000, soc: .20},
		{driver: "c", capacityWh: 10000, soc: .10, chargeBlocked: true},
	}
	out := shareByAvailableEnergy(bats, 8000)
	want := []DispatchTarget{{Driver: "a", TargetW: 3000}, {Driver: "b", TargetW: 5000, Clamped: true}, {Driver: "c", Clamped: true}}
	for i := range want {
		if math.Abs(out[i].TargetW-want[i].TargetW) > 1e-6 || out[i].Clamped != want[i].Clamped {
			t.Fatalf("targets=%+v, want %+v", out, want)
		}
	}
	// Nothing limits a small charge, so nothing reads as clamped.
	out = shareByAvailableEnergy(bats, 1000)
	if math.Abs(out[0].TargetW-200) > 1e-6 || math.Abs(out[1].TargetW-800) > 1e-6 || out[0].Clamped || out[1].Clamped || !out[2].Clamped {
		t.Fatalf("targets=%+v", out)
	}
}

// An idle slot keeps the local controller, with its inverter-group routing.
// So do batteries whose SoC Core does not know: room cannot be weighed.
func TestInterchangeableStoragesKeepTheLocalControllerWhenIdleOrBlind(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{DecisionID: "p", SlotStart: now, SlotEnd: now.Add(15 * time.Minute), StorageEnergyWh: map[string]float64{"a": 0, "b": 0}}
	s := newStateWithEnergyDispatch(dir, "site")
	s.Mode = ModePlannerSelf
	s.clock = func() time.Time { return now }
	bats := []batteryInfo{{driver: "a", capacityWh: 10000, soc: .5}, {driver: "b", capacityWh: 10000, soc: .5}}
	if _, ok := distributePlannedStorages(s, bats, 3000, false); ok {
		t.Fatal("idle slot left the local controller")
	}
	dir.StorageEnergyWh = map[string]float64{"a": 500, "b": 0}
	s.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	bats[1].socMissing, bats[1].soc, bats[1].dischargeBlocked = true, 0, true
	if _, ok := distributePlannedStorages(s, bats, 2000, false); ok {
		t.Fatal("a battery without SoC was weighed as empty")
	}
}

// A battery on a hybrid inverter with a small limit is physically different:
// splitting 6 kW by capacity would ask it for 3 kW and lose 1.5 kW. The plan
// already put the energy where each inverter can deliver it, so Core keeps
// following each battery's planned energy.
func TestPhysicallyDifferentStoragesFollowThePlan(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{DecisionID: "p", SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 1500,
		StorageEnergyWh: map[string]float64{"a": 1125, "b": 375}}
	store := seedStore(500, []struct {
		name          string
		currentW, soc float64
	}{{"a", 0, .5}, {"b", 0, .5}})
	s := newStateWithEnergyDispatch(dir, "ferroamp")
	s.clock = func() time.Time { return now }
	s.DriverLimits = map[string]PowerLimits{
		"a": {MaxChargeW: 5000, MaxDischargeW: 5000, MaxChargeWSet: true, MaxDischargeWSet: true},
		"b": {MaxChargeW: 1500, MaxDischargeW: 1500, MaxChargeWSet: true, MaxDischargeWSet: true},
	}
	out := ComputeDispatch(store, s, caps(map[string]float64{"a": 10000, "b": 10000}), 11040)
	want := map[string]float64{"a": 4500, "b": 1500}
	if len(out) != 2 {
		t.Fatalf("targets=%+v", out)
	}
	for _, v := range out {
		if math.Abs(v.TargetW-want[v.Driver]) > 1e-6 {
			t.Fatalf("targets=%+v, want %v", out, want)
		}
	}
}

func TestInterchangeableStoragesRule(t *testing.T) {
	limit := func(charge, discharge float64) batteryInfo {
		return batteryInfo{capacityWh: 10000, maxChargeW: charge, maxDischargeW: discharge, maxChargeWSet: true, maxDischargeWSet: true}
	}
	for _, tc := range []struct {
		name string
		bats []batteryInfo
		want bool
	}{
		{"identical defaults", []batteryInfo{{capacityWh: 10000}, {capacityWh: 10000}}, true},
		{"twice the capacity and twice the power", []batteryInfo{limit(3000, 3000), {capacityWh: 20000, maxChargeW: 6000, maxDischargeW: 6000, maxChargeWSet: true, maxDischargeWSet: true}}, true},
		{"4 % apart", []batteryInfo{limit(5000, 5000), limit(4800, 4800), limit(5000, 5000)}, true},
		{"6 % apart", []batteryInfo{limit(5000, 5000), limit(4700, 5000)}, false},
		{"spread across three", []batteryInfo{limit(4850, 5000), limit(5000, 5000), limit(5150, 5000)}, false},
		{"smaller inverter", []batteryInfo{limit(5000, 5000), limit(1500, 1500)}, false},
		{"same power, smaller pack", []batteryInfo{{capacityWh: 1000}, {capacityWh: 19000}}, false},
		{"charge disabled on one", []batteryInfo{limit(5000, 5000), limit(0, 5000)}, false},
		{"unknown capacity", []batteryInfo{{capacityWh: 10000}, {}}, false},
	} {
		if got := interchangeableStorages(tc.bats); got != tc.want {
			t.Errorf("%s: interchangeable=%v, want %v", tc.name, got, tc.want)
		}
	}
}
