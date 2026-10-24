package api

import (
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func dailyEnergyCacheReady(dayStart, now time.Time) bool {
	return !now.Before(dayStart.AddDate(0, 0, 1).Add(state.LoadEstimateRecoveryWindow + time.Minute))
}

func (s *Server) qualifyStatusHistory(resp map[string]any, siteMeter string) {
	var opts telemetry.ForecastOptions
	if s.deps.HistoryMeasurementSources != nil {
		opts = s.deps.HistoryMeasurementSources()
	}
	r := s.deps.Tel.LiveMeasurementNow(siteMeter, opts)
	var missing state.HistoryMissing
	for kind, flag := range map[telemetry.DerType]state.HistoryMissing{telemetry.DerMeter: state.MissingGrid, telemetry.DerPV: state.MissingPV, telemetry.DerBattery: state.MissingBattery, telemetry.DerEV: state.MissingEV, telemetry.DerV2X: state.MissingV2X} {
		if !r.Known[kind] {
			missing |= flag
		}
	}
	if !r.Valid {
		missing |= state.MissingLoad
	}
	if opts.PVInvalidReason != "" {
		missing |= state.MissingPV
	}
	if r.Reason == "invalid_balance" || strings.HasPrefix(r.Reason, "duplicate_") || strings.HasPrefix(r.Reason, "invalid_sign:") || r.Reason == "time_skew" {
		missing |= state.BalanceMismatch
	}
	resp["grid_w"] = missing.Value(state.MissingGrid, r.GridW)
	// Live power is not history: a dark inverter is 0 W and Home use stays
	// known while the site meter is read (load_quality says how).
	if r.PVLiveKnown && opts.PVInvalidReason == "" {
		resp["pv_w"] = r.PVW
	} else {
		resp["pv_w"] = nil
	}
	resp["bat_w"] = missing.Value(state.MissingBattery, r.BatteryW)
	resp["ev_w"] = missing.Value(state.MissingEV, r.EVW)
	if missing&state.MissingEV != 0 {
		resp["ev_charging_w"] = nil
	}
	resp["v2x_w"] = missing.Value(state.MissingV2X, r.V2XW)
	if r.LoadKnown {
		resp["load_w"] = r.LoadW
	} else {
		resp["load_w"] = nil
	}
	resp["load_quality"] = r.LoadQuality
	resp["load_reason"] = r.LoadReason
	resp["history_quality"] = missing.Quality()
	resp["history_reason"] = r.Reason
	drivers, _ := resp["drivers"].(map[string]any)
	for _, source := range r.Sources {
		if d, ok := drivers[source.Flow.Driver].(map[string]any); ok {
			field := map[telemetry.DerType]string{telemetry.DerMeter: "meter_w", telemetry.DerPV: "pv_w", telemetry.DerBattery: "bat_w", telemetry.DerEV: "ev_w", telemetry.DerV2X: "v2x_w"}[source.Flow.DerType]
			if field != "" {
				d[field] = source.PowerW
			}
			if source.Flow.DerType == telemetry.DerBattery {
				if source.SoC == nil {
					d["bat_soc"] = nil
				} else {
					d["bat_soc"] = *source.SoC
				}
			}
		}
	}
	var count int
	for _, source := range r.Sources {
		if source.Flow.DerType == telemetry.DerBattery {
			if source.SoC == nil {
				missing |= state.MissingSoC
			}
			count++
		}
	}
	if count == 0 {
		missing |= state.MissingSoC
	}
	if missing&state.MissingSoC != 0 {
		resp["bat_soc"] = nil
	}
	// handleStatus already weights known battery SoCs by capacity.
	// Qualification only clears that value when a required input is missing.
}
