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
	bats := []batteryInfo{{driver: "a", capacityWh: 10000, soc: .5}, {driver: "b", capacityWh: 10000, soc: .5}}
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
