package api

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// CloudStatistics contains only the fields named by the versioned project
// statistics terms. Configuration and ledger ids never cross this boundary.
func (s *Server) CloudStatistics(ctx context.Context, now time.Time) (cloudwire.Stats, bool) {
	hour := now.UTC().Truncate(time.Hour).Add(-time.Hour)
	out := cloudwire.Stats{HourMS: hour.UnixMilli(), Metrics: map[string]int64{}}
	if s.deps == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil {
		return out, false
	}
	s.deps.CfgMu.RLock()
	cfg := s.deps.Cfg
	// A partner-controlled site's contract must allow statistics first.
	if cfg.Nova != nil && cfg.Nova.Enabled {
		s.deps.CfgMu.RUnlock()
		return out, false
	}
	put := func(key string, value float64) {
		if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1_000_000 {
			out.Metrics[key] = int64(math.Round(value/10)) * 10
		}
	}
	batteryWh, flexibleW, powerKnown := 0.0, 0.0, true
	groups := map[string]float64{}
	drivers := map[string]bool{}
	for _, d := range cfg.Drivers {
		if d.Disabled {
			continue
		}
		drivers[d.Name] = !d.ObserveOnly && !d.BatteryTelemetryOnly
		if d.BatteryCapacityWh > 0 {
			batteryWh += d.BatteryCapacityWh
			if !drivers[d.Name] {
				continue
			}
			cap := math.Max(d.MaxChargeW, d.MaxDischargeW)
			if cap <= 0 || math.IsNaN(cap) || math.IsInf(cap, 0) {
				powerKnown = false
				continue
			}
			group := d.InverterGroup
			if group == "" {
				group = d.Name
			}
			groups[group] = math.Max(groups[group], cap)
		}
	}
	for _, cap := range groups {
		flexibleW += cap
	}
	chargers := map[string]float64{}
	for _, lp := range cfg.Loadpoints {
		if !drivers[lp.DriverName] {
			continue
		}
		if lp.MaxChargeW <= 0 || math.IsNaN(lp.MaxChargeW) || math.IsInf(lp.MaxChargeW, 0) {
			powerKnown = false
			continue
		}
		chargers[lp.DriverName] = math.Max(chargers[lp.DriverName], lp.MaxChargeW)
	}
	for _, cap := range chargers {
		flexibleW += cap
	}
	put("battery_capacity_wh", batteryWh)
	if powerKnown {
		put("flexible_power_w", flexibleW)
	}
	if cfg.Weather != nil {
		rated := cfg.Weather.PVRatedW
		if len(cfg.Weather.PVArrays) > 0 {
			rated = 0
			for _, array := range cfg.Weather.PVArrays {
				rated += array.RatedWatts()
			}
		}
		if rated > 0 {
			put("pv_rated_w", rated)
		}
	}
	s.deps.CfgMu.RUnlock()
	if s.deps.State == nil {
		return out, true
	}
	points, truncated, err := s.deps.State.LoadEnergyHistoryContext(ctx, state.EnergyHistoryQuery{
		SinceMS: hour.UnixMilli(), UntilMS: hour.Add(time.Hour).UnixMilli(), BucketMS: cloudwire.StatsHourMS, Limit: 1,
	})
	if err != nil || truncated {
		return out, true
	}
	totals := map[string]float64{}
	for _, point := range points {
		if point.BucketStartMS != hour.UnixMilli() || point.BucketLenMS != cloudwire.StatsHourMS ||
			(point.Quality != "measured" && point.Quality != "estimated" && point.Quality != "recovered") {
			continue
		}
		key := string(point.Flow) + "_wh"
		for _, allowed := range cloudwire.StatsMetricKeys {
			if key == allowed {
				if point.EnergyWh >= 0 && !math.IsNaN(point.EnergyWh) && !math.IsInf(point.EnergyWh, 0) {
					totals[key] += point.EnergyWh
				}
			}
		}
	}
	for key, value := range totals {
		put(key, value)
	}
	return out, true
}

// The owner reads this before POST /api/cloud/stats with this policy id.
func (s *Server) handleCloudStatsPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.cloudGate(w, r, false) {
		return
	}
	available := s.deps != nil && s.deps.Cloud != nil && s.deps.Cloud.Status().StatisticsReady
	writeJSON(w, http.StatusOK, map[string]any{
		"available":                  available,
		"policy":                     cloudwire.StatsPolicy,
		"cloud_scopes":               protocol.CloudScopes,
		"cloud_policy":               cloudwire.CloudTermsPolicy,
		"cloud_includes_statistics":  true,
		"standalone":                 true,
		"purpose":                    "Publish group totals for FTW use and energy; improve planning. No ads or household decisions.",
		"fields":                     cloudwire.StatsMetricKeys,
		"meaning":                    "One reporting installation, configured flexible power and solar ratings, battery capacity, and recorded closed-hour energy. Configured limits do not prove physical control. Missing readings are unknown.",
		"resolution":                 "Closed UTC hours, energy rounded to 10 Wh, capacities and limits rounded to 10 Wh or W. No names, addresses, device ids or location.",
		"retention":                  "48 hours. Leaving Cloud or stopping local sharing deletes unpublished household rows; published group totals remain.",
		"publication_minimum_homes":  cloudwire.StatsMinimumHomes,
		"publication_max_home_share": 0.3,
		"controller":                 "sourceful", "local_default": "on", "local_optional": true, "cloud_required": true,
	})
}
