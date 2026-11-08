package journal

import (
	"encoding/json"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

// SlotSpan is a scored plan slot as the journal keeps it: the plan's
// average battery power over the slot as asked for, the delivered average
// as actual, and the energy behind both.
func SlotSpan(d control.SlotDelivery) state.ControlSpan {
	severity := "info"
	if d.Verdict == "under_delivered" || d.Verdict == "sign_mismatch" {
		severity = "warning"
	}
	sp := state.ControlSpan{
		Subject: "slot:battery", Kind: "slot", Status: d.Verdict, Severity: severity, Mode: string(d.Mode),
		StartMs: d.SlotStart.UnixMilli(), EndMs: d.SlotEnd.UnixMilli(), UpdatedMs: d.SlotEnd.UnixMilli(), Samples: 1,
	}
	if hours := d.SlotEnd.Sub(d.SlotStart).Hours(); hours > 0 {
		planned, actual := d.PlannedWh/hours, d.ActualWh/hours
		sp.Requested.Add(&planned)
		sp.Actual.Add(&actual)
	}
	sp.First, _ = json.Marshal(map[string]any{
		"planned_wh": d.PlannedWh, "actual_wh": d.ActualWh, "ratio": d.Ratio, "replans": d.Replans,
	})
	sp.Last = sp.First
	return sp
}
