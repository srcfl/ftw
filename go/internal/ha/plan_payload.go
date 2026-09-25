package ha

import (
	"encoding/json"
	"math"
	"sort"
	"time"
)

// Home Assistant's recorder refuses attributes above this size and logs a
// warning on every state change instead (#1296). The Plan sensor carries the
// schedule as attributes, so its payload must stay below it.
const haMaxAttributesBytes = 16384

// planAttributesBudget leaves room for Home Assistant's own serialisation of
// the same attributes.
const planAttributesBudget = 14 * 1024

// planSlot is one schedule entry in the Plan sensor attributes: what a
// dashboard or automation needs per slot, rounded to one decimal. The full
// slot detail goes to plan_schedule_json.
type planSlot struct {
	Start    string  `json:"start"`
	Action   string  `json:"action"`
	BatteryW float64 `json:"battery_w"`
	GridW    float64 `json:"grid_w"`
	SoCPct   float64 `json:"soc_pct"`
	PriceOre float64 `json:"price_ore"`
}

// planScheduleSlot is one entry of plan_schedule_json, the full schedule for
// MQTT consumers. It is not attached to a Home Assistant entity because it
// can exceed the recorder limit.
type planScheduleSlot struct {
	Start    string  `json:"start"`
	End      string  `json:"end"`
	Action   string  `json:"action"`
	BatteryW float64 `json:"battery_w"`
	GridW    float64 `json:"grid_w"`
	SoCPct   float64 `json:"soc_pct"`
	PVW      float64 `json:"pv_w,omitempty"`
	LoadW    float64 `json:"load_w,omitempty"`
	PriceOre float64 `json:"price_ore,omitempty"`
	SpotOre  float64 `json:"spot_ore,omitempty"`
	CostOre  float64 `json:"cost_ore,omitempty"`
	Reason   string  `json:"reason,omitempty"`
	EMSMode  string  `json:"ems_mode,omitempty"`
}

// planSnapshot is everything publishPlan publishes about the plan.
type planSnapshot struct {
	Action        string     // current slot label, or "unavailable"
	Current       PlanAction // current slot; zero when no slot covers now
	Attributes    []byte     // plan_json, bounded by planAttributesBudget
	Schedule      []byte     // plan_schedule_json, full detail
	ScheduleSlots int        // slots kept in Attributes
	Truncated     bool       // Attributes hold fewer slots than the 24 h horizon
}

func buildPlanSnapshot(actions []PlanAction, now time.Time) planSnapshot {
	nowMs := now.UnixMilli()
	horizon := nowMs + 24*60*60*1000
	sorted := append([]PlanAction(nil), actions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SlotStartMs < sorted[j].SlotStartMs })

	snap := planSnapshot{Action: "unavailable"}
	var curStart, curEnd string
	compact := []planSlot{}
	full := []planScheduleSlot{}
	for _, a := range sorted {
		endMs := a.SlotStartMs + int64(a.SlotLenMin)*60*1000
		if endMs <= nowMs {
			continue // past slot
		}
		if a.SlotStartMs > horizon {
			break // beyond 24 h
		}
		label := planActionLabel(a.BatteryW)
		start := time.UnixMilli(a.SlotStartMs).UTC().Format(time.RFC3339)
		end := time.UnixMilli(endMs).UTC().Format(time.RFC3339)
		if a.SlotStartMs <= nowMs && nowMs < endMs {
			snap.Action, snap.Current, curStart, curEnd = label, a, start, end
		}
		compact = append(compact, planSlot{
			Start: start, Action: label,
			BatteryW: round1(a.BatteryW), GridW: round1(a.GridW), SoCPct: round1(a.SoCPct),
			PriceOre: round1(a.PriceOre),
		})
		full = append(full, planScheduleSlot{
			Start: start, End: end, Action: label,
			BatteryW: a.BatteryW, GridW: a.GridW, SoCPct: a.SoCPct, PVW: a.PVW, LoadW: a.LoadW,
			PriceOre: a.PriceOre, SpotOre: a.SpotOre, CostOre: a.CostOre,
			Reason: a.Reason, EMSMode: a.EMSMode,
		})
	}

	cur := snap.Current
	attrs := map[string]any{
		"action":     snap.Action,
		"battery_w":  cur.BatteryW,
		"grid_w":     cur.GridW,
		"soc_pct":    cur.SoCPct,
		"slot_start": curStart,
		"slot_end":   curEnd,
		"price_ore":  cur.PriceOre,
		"spot_ore":   cur.SpotOre,
		"cost_ore":   cur.CostOre,
		"reason":     cur.Reason,
		"ems_mode":   cur.EMSMode,
	}
	snap.Attributes, snap.ScheduleSlots, snap.Truncated = marshalPlanAttributes(attrs, compact, planAttributesBudget)
	if d, err := json.Marshal(map[string]any{
		"action": snap.Action, "slot_start": curStart, "slot_end": curEnd, "schedule": full,
	}); err == nil {
		snap.Schedule = d
	}
	return snap
}

// marshalPlanAttributes keeps the attributes within budget. When the
// schedule does not fit, the farthest quarter of the horizon is dropped and
// the result is marked truncated, so a 5-minute plan still publishes the
// hours that matter instead of an attribute set Home Assistant discards.
func marshalPlanAttributes(attrs map[string]any, slots []planSlot, budget int) ([]byte, int, bool) {
	truncated := false
	for {
		attrs["schedule"] = slots
		attrs["schedule_slots"] = len(slots)
		attrs["schedule_truncated"] = truncated
		d, err := json.Marshal(attrs)
		if err != nil {
			return nil, 0, truncated
		}
		if len(d) <= budget || len(slots) == 0 {
			return d, len(slots), truncated
		}
		slots = slots[:len(slots)*3/4]
		truncated = true
	}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
