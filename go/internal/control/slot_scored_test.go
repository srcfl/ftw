package control

import (
	"math"
	"testing"
	"time"
)

// A scored slot reaches SlotScored with its numbers and verdict, so the
// control journal can keep it after the log line has rolled away.
func TestSlotScoredReportsEachScoredSlot(t *testing.T) {
	now := time.Now()
	slot1Start := now.Add(-1 * time.Minute)
	slot1 := SlotDirective{SlotStart: slot1Start, SlotEnd: slot1Start.Add(15 * time.Minute), BatteryEnergyWh: 1250}
	slot2 := SlotDirective{SlotStart: slot1.SlotEnd, SlotEnd: slot1.SlotEnd.Add(15 * time.Minute), BatteryEnergyWh: 1250}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{{"ferroamp", 1200, 0.2}})
	active := slot1
	st := makeSlotMetricsState("ferroamp", func(time.Time) (SlotDirective, bool) { return active, true })
	var got []SlotDelivery
	st.SlotScored = func(d SlotDelivery) { got = append(got, d) }

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 9600}), 11040)
	st.slotPlannedPastWh = 1250
	st.slotActualWh = 296 // 06:15 on 8 November 2026: 24 % of the plan

	active = slot2
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 9600}), 11040)
	if len(got) != 1 {
		t.Fatalf("want one scored slot, got %+v", got)
	}
	d := got[0]
	if d.Verdict != "under_delivered" || math.Abs(d.PlannedWh-1250) > 0.01 || math.Abs(d.ActualWh-296) > 0.01 ||
		math.Abs(d.Ratio-0.2368) > 1e-4 || !d.SlotEnd.Equal(slot1.SlotEnd) || d.SlotStart.IsZero() {
		t.Fatalf("scored slot: %+v", d)
	}
}
