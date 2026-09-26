package ha

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func planFixture(now time.Time, slotMin, slots int) []PlanAction {
	actions := make([]PlanAction, 0, slots)
	for i := 0; i < slots; i++ {
		actions = append(actions, PlanAction{
			SlotStartMs: now.Add(time.Duration(i*slotMin) * time.Minute).UnixMilli(),
			SlotLenMin:  slotMin,
			BatteryW:    -3000.123456789 + float64(i),
			GridW:       1234.56789 * float64(i%7),
			SoCPct:      45.678901 + float64(i)/4,
			PriceOre:    123.456789 + float64(i),
			SpotOre:     80.12345,
			CostOre:     -12.3456,
			Reason:      "cheap hour: charge before the evening peak while PV output is still available",
			EMSMode:     "planner_arbitrage",
			PVW:         -2000.5,
			LoadW:       800.25,
		})
	}
	return actions
}

// Home Assistant's recorder discards attributes above 16 KB and logs it on
// every plan update (#1296). A full day of 15-minute slots must fit without
// truncation, and the full detail must still be available on its own topic.
func TestPlanAttributesStayUnderHomeAssistantLimit(t *testing.T) {
	now := time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
	snap := buildPlanSnapshot(planFixture(now, 15, 96), now.Add(5*time.Minute))
	if len(snap.Attributes) > planAttributesBudget || len(snap.Attributes) > haMaxAttributesBytes {
		t.Fatalf("plan attributes = %d bytes, budget %d", len(snap.Attributes), planAttributesBudget)
	}
	if snap.Truncated || snap.ScheduleSlots != 96 {
		t.Fatalf("24 h of 15-minute slots must fit untruncated: slots=%d truncated=%v bytes=%d", snap.ScheduleSlots, snap.Truncated, len(snap.Attributes))
	}
	if snap.Action != "discharge" || snap.Current.SlotStartMs != now.UnixMilli() {
		t.Fatalf("current slot = %q %+v", snap.Action, snap.Current)
	}

	var attrs map[string]any
	if err := json.Unmarshal(snap.Attributes, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["action"] != "discharge" || attrs["reason"] == "" || attrs["schedule_truncated"] != false {
		t.Fatalf("attributes = %v", attrs)
	}
	schedule, _ := attrs["schedule"].([]any)
	if len(schedule) != 96 {
		t.Fatalf("schedule entries = %d", len(schedule))
	}
	first, _ := schedule[0].(map[string]any)
	for _, key := range []string{"start", "action", "battery_w", "grid_w", "soc_pct", "price_ore"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("compact slot missing %s: %v", key, first)
		}
	}
	if _, ok := first["reason"]; ok {
		t.Fatalf("compact slot must not carry the reason text: %v", first)
	}
	if fmt.Sprint(first["battery_w"]) != "-3000.1" {
		t.Fatalf("compact values must be rounded: %v", first["battery_w"])
	}

	var full struct {
		Schedule []map[string]any `json:"schedule"`
	}
	if err := json.Unmarshal(snap.Schedule, &full); err != nil {
		t.Fatal(err)
	}
	if len(full.Schedule) != 96 || full.Schedule[0]["reason"] == nil || full.Schedule[0]["end"] == nil {
		t.Fatalf("full schedule lost detail: %d entries, first=%v", len(full.Schedule), full.Schedule[0])
	}
}

// Finer slots do not fit a day into the budget. The horizon is shortened and
// the attributes say so; the limit is never exceeded.
func TestPlanAttributesShortenTheHorizonRatherThanExceedTheLimit(t *testing.T) {
	now := time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
	snap := buildPlanSnapshot(planFixture(now, 5, 288), now)
	if len(snap.Attributes) > planAttributesBudget {
		t.Fatalf("plan attributes = %d bytes, budget %d", len(snap.Attributes), planAttributesBudget)
	}
	if !snap.Truncated || snap.ScheduleSlots == 0 || snap.ScheduleSlots >= 288 {
		t.Fatalf("expected a shortened horizon: slots=%d truncated=%v", snap.ScheduleSlots, snap.Truncated)
	}
	var attrs map[string]any
	if err := json.Unmarshal(snap.Attributes, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["schedule_truncated"] != true || attrs["schedule_slots"] != float64(snap.ScheduleSlots) {
		t.Fatalf("truncation not reported: %v %v", attrs["schedule_truncated"], attrs["schedule_slots"])
	}
	var full struct {
		Schedule []map[string]any `json:"schedule"`
	}
	if err := json.Unmarshal(snap.Schedule, &full); err != nil || len(full.Schedule) != 288 {
		t.Fatalf("full schedule must keep every slot: %d %v", len(full.Schedule), err)
	}
}

func TestPlanSnapshotWithoutCurrentSlotIsUnavailable(t *testing.T) {
	now := time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
	snap := buildPlanSnapshot(planFixture(now.Add(time.Hour), 15, 4), now)
	if snap.Action != "unavailable" || snap.ScheduleSlots != 4 {
		t.Fatalf("snapshot = %q slots=%d", snap.Action, snap.ScheduleSlots)
	}
	var attrs map[string]any
	if err := json.Unmarshal(snap.Attributes, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["slot_start"] != "" || attrs["battery_w"] != float64(0) {
		t.Fatalf("no current slot must publish empty current fields: %v", attrs)
	}
}
