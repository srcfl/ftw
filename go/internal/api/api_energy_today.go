package api

import "github.com/srcfl/ftw/go/internal/state"

func energyTodayFields(d state.DayEnergy) map[string]any {
	if d.Intervals == 0 {
		return nil
	}
	out := map[string]any{"history_quality": d.Missing.Quality()}
	if d.Missing&state.EstimatedLoad != 0 {
		out["load_quality"] = "estimated"
	}
	if d.LoadUntilMS > 0 {
		out["load_until_ms"] = d.LoadUntilMS
	}
	if d.Missing&state.EndpointEstimatedPV != 0 {
		out["pv_quality"] = "estimated"
		if out["history_quality"] == "complete" {
			out["history_quality"] = "estimated"
		}
	}
	if d.PVUntilMS > 0 {
		out["pv_until_ms"] = d.PVUntilMS
	}
	measured := make(map[string]any)
	for key, field := range map[string]struct {
		flag state.HistoryMissing
		wh   float64
	}{
		"import_wh":         {state.MissingGrid, d.ImportWh},
		"export_wh":         {state.MissingGrid, d.ExportWh},
		"pv_wh":             {state.MissingPV, d.PVWh},
		"bat_charged_wh":    {state.MissingBattery, d.BatChargedWh},
		"bat_discharged_wh": {state.MissingBattery, d.BatDischargedWh},
		"load_wh":           {state.MissingLoad, d.LoadWh},
	} {
		// Existing clients still receive null for an incomplete total.
		out[key] = d.Missing.Value(field.flag, field.wh)
		measured[key] = d.MeasuredValue(field.flag, field.wh)
	}
	if d.Missing&state.EstimatedLoad != 0 {
		// The load total includes inferred intervals. It cannot also appear
		// in the measured-only fallback used by clients after a data gap.
		measured["load_wh"] = nil
	}
	if d.Missing&state.EndpointEstimatedPV != 0 {
		measured["pv_wh"] = nil
	}
	out["measured"] = measured
	return out
}
