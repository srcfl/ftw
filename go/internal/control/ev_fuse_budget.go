package control

import (
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// evHeadroomW is the shared grid balance for an EV pool. Power outside the
// pool stays in gridW. No unmeasured future battery relief earns headroom.
func evHeadroomW(ceiling, gridW, poolW float64) float64 {
	if !finiteFuseValue(ceiling) || !finiteFuseValue(gridW) || !finiteFuseValue(poolW) || ceiling <= 0 || poolW < 0 {
		return 0
	}
	return math.Max(0, ceiling-gridW+poolW)
}

func finiteFuseValue(w float64) bool { return !math.IsNaN(w) && !math.IsInf(w, 0) }

type evFuseBalance struct {
	gridW, poolW, pendingW      float64
	latestPoolW, latestPendingW float64
	latestGridW                 float64
	siteAt                      time.Time
	evEvidence                  []evFuseEvidence
}

// EVFuseBudget refreshes the common EV cap before a charger command, also
// without batteries. The shared freshness gate must already allow dispatch.
// Other EVs remain measured household demand. Callers hold the control lock.
// The controller also applies its final tick's phase choice before sending.
func EVFuseBudget(store *telemetry.Store, state *State, points []loadpoint.State, configs []loadpoint.Config, fuseMaxW float64) (float64, bool) {
	if store == nil || state == nil || !finiteFuseValue(fuseMaxW) || fuseMaxW <= 0 {
		return 0, true
	}
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if meter == nil || !finiteFuseValue(meter.RawW) {
		return 0, true
	}
	now := state.now()
	balance := coherentEVBalance(store, state, meter, points, now)
	// The common balance includes later observed DER increases. Keep the
	// newest site/phase read as an independent bound so the same increase is
	// not counted once in its grid watts and again as a pending command.
	commonCeiling := state.effectiveImportCeilingW(fuseMaxW)
	latestCeiling := state.phaseImportCeilingW(fuseMaxW, meter)
	cap := evHeadroomW(commonCeiling, balance.gridW+balance.pendingW, balance.poolW)
	latestCap := evHeadroomW(latestCeiling, meter.RawW+balance.latestPendingW, balance.latestPoolW)
	// Keep the newest site and phase bounds authoritative even when the
	// common historical balance has more replacement credit.
	phaseCap := latestCap
	phaseKnown := false
	if phaseW, known := evPhaseIncreaseW(meter, state, configs, balance.latestPoolW); known {
		phaseKnown = true
		phaseCap = math.Max(0, balance.latestPoolW+phaseW-balance.latestPendingW)
	}
	finalCap := math.Min(cap, math.Min(latestCap, phaseCap))
	minimumW := 0.0
	for _, cfg := range configs {
		for _, point := range points {
			if point.PluggedIn && point.ID == cfg.ID && cfg.MinChargeW > 0 && (minimumW == 0 || cfg.MinChargeW < minimumW) {
				minimumW = cfg.MinChargeW
			}
		}
	}
	if minimumW > 0 && finalCap < minimumW && (state.evFuseEvidenceAt.IsZero() || now.Before(state.evFuseEvidenceAt) || now.Sub(state.evFuseEvidenceAt) >= time.Minute) {
		state.evFuseEvidenceAt = now
		slog.Debug("EV fuse budget below charging minimum", "minimum_charge_w", minimumW,
			"common_site_at_ms", balance.siteAt.UnixMilli(), "latest_site_received_at_ms", meter.UpdatedAt.UnixMilli(),
			"physical_fuse_w", fuseMaxW, "configured_import_ceiling_w", state.PeakImportCeilingW,
			"common_ceiling_w", commonCeiling, "latest_ceiling_w", latestCeiling,
			"common_grid_w", balance.gridW, "common_ev_credit_w", balance.poolW, "common_pending_battery_w", balance.pendingW,
			"latest_grid_w", balance.latestGridW, "latest_ev_credit_w", balance.latestPoolW, "latest_pending_battery_w", balance.latestPendingW,
			"ev_credit_evidence", balance.evEvidence,
			"common_cap_w", cap, "latest_site_cap_w", latestCap, "phase_bound_known", phaseKnown, "phase_cap_w", phaseCap, "final_cap_w", finalCap)
	}
	return finalCap, true
}

// A managed EV earns replacement credit only across a site observation that
// its raw power brackets. This keeps normal asynchronous polls useful while
// preventing a later start or an earlier stop from inventing spare capacity.
// The existing bounded telemetry ring owns the samples; no second cache or
// filtered load estimate participates in the hard limit.
func coherentEVBalance(store *telemetry.Store, state *State, meter *telemetry.DerReading, points []loadpoint.State, now time.Time) evFuseBalance {
	balance := evFuseBalance{gridW: meter.RawW, siteAt: meter.UpdatedAt}
	balance.latestGridW = balance.gridW
	siteAt := meter.UpdatedAt
	from := now.Add(-telemetry.ControlWindowDuration)
	windows := store.ConfirmedControlWindows(from, now)
	batteries := pendingBatterySeriesForFuse(store, state, windows, now)
	sites := windows[state.SiteMeterDriver+":"+telemetry.DerMeter.String()].Points
	if current, ok := qualifiedFuseObservation(meter, now); ok {
		current.At = current.Seen()
		siteAt = current.At
		sites = append(sites, current)
	}
	type evSeries struct {
		latest  float64
		current telemetry.ControlObservation
		points  []telemetry.ControlObservation
	}
	var series []evSeries
	seen := map[string]bool{}
	for _, point := range points {
		if !point.PluggedIn || seen[point.DriverName] {
			continue
		}
		seen[point.DriverName] = true
		r := store.Get(point.DriverName, telemetry.DerEV)
		h := store.DriverHealth(point.DriverName)
		current, ok := qualifiedFuseObservation(r, now)
		evidence := evFuseEvidence{ControlPowerEvidence: telemetry.DescribeControlPower(point.DriverName, r, h, current, ok, now)}
		balance.evEvidence = append(balance.evEvidence, evidence)
		if !ok || h == nil || !h.TelemetryLive() || current.PowerW <= 0 {
			continue // Unknown EV power stays inside the measured site demand.
		}
		ps := windows[point.DriverName+":"+telemetry.DerEV.String()].Points
		ps = append(ps, current) // Includes explicit change-only confirmations.
		credit := evPowerAtLatestSite(ps, siteAt, current)
		balance.evEvidence[len(balance.evEvidence)-1].LatestCreditW = credit
		balance.evEvidence[len(balance.evEvidence)-1].Points = len(ps)
		if credit <= 0 {
			balance.evEvidence[len(balance.evEvidence)-1].Reason = "no_bracket_or_observed_stop"
		}
		series = append(series, evSeries{math.Min(current.PowerW, r.RawW), current, ps})
	}
	for _, ev := range series {
		balance.latestPoolW += evPowerAtLatestSite(ev.points, siteAt, ev.current)
	}
	balance.latestPendingW = pendingBatteryIncreaseAtLatestSite(batteries, siteAt)
	sort.SliceStable(sites, func(i, j int) bool { return sites[i].At.After(sites[j].At) })
	for _, site := range sites {
		if site.At.Before(from) || site.At.After(now) || !finiteFuseValue(site.PowerW) {
			continue
		}
		// Without a positive managed EV observation, an older high site
		// sample has no replacement credit to align. Use the latest site
		// fallback instead of retaining demand from a completed EV stop.
		if len(series) == 0 && !site.At.Equal(siteAt) {
			continue
		}
		credit, coherent := 0.0, true
		for _, ev := range series {
			w, ok := bracketedEVPowerAt(ev.points, site.At, ev.latest)
			if !ok {
				coherent = false
				break
			}
			credit += w
		}
		if coherent {
			balance.pendingW, coherent = pendingBatteryIncreaseAt(batteries, site.At)
		}
		if coherent {
			balance.gridW, balance.poolW, balance.siteAt = site.PowerW, credit, site.At
			return balance
		}
	}
	// No common bracket exists. Keep the EV credit already qualified at
	// this latest site instant; dropping it here would count the same EV
	// as household demand in a second bound on the very same site reading.
	balance.poolW = balance.latestPoolW
	// A battery already observed before this latest site read uses the
	// same baseline as the latest-site guard.
	// A first battery observation AFTER the site read cannot establish how
	// much earlier discharge the meter contains: retain its full fallback.
	balance.pendingW = 0
	for _, battery := range batteries {
		if battery.fresh && !battery.current.Seen().After(siteAt) {
			balance.pendingW += math.Max(0, battery.upperW-battery.current.PowerW)
		} else {
			reserve, _ := pendingBatteryIncreaseAt([]pendingBatterySeries{battery}, siteAt)
			balance.pendingW += reserve
		}
	}
	return balance
}

func qualifiedFuseObservation(r *telemetry.DerReading, now time.Time) (telemetry.ControlObservation, bool) {
	if r == nil || !finiteFuseValue(r.RawW) || r.UpdatedAt.After(now) || now.Sub(r.UpdatedAt) > telemetry.ControlPowerMaxAge(r.Data) {
		return telemetry.ControlObservation{}, false
	}
	p, ok := telemetry.ControlPowerObservation(r.RawW, r.Data, r.UpdatedAt)
	return p, ok && !p.Seen().After(now) && now.Sub(p.Seen()) <= telemetry.ControlWindowDuration
}

// bracketedEVPowerAt evaluates the historical site instant. Later EV
// stops are handled by the separate latest-site/phase bounds, not by
// retroactively subtracting them from this older grid observation.
func bracketedEVPowerAt(points []telemetry.ControlObservation, at time.Time, latest float64) (float64, bool) {
	var before, after telemetry.ControlObservation
	hasBefore, hasAfter := false, false
	for _, p := range points {
		if !finiteFuseValue(p.PowerW) || p.PowerW < 0 {
			continue
		}
		if !p.At.After(at) && (!hasBefore || p.At.After(before.At)) {
			before, hasBefore = p, true
		}
		if !p.Seen().Before(at) && (!hasAfter || p.At.Before(after.At)) {
			after, hasAfter = p, true
		}
	}
	if !hasBefore || !hasAfter {
		return 0, false
	}
	baseline := math.Max(0, math.Min(latest, math.Min(before.PowerW, after.PowerW)))
	return baseline, true
}

func bracketedEVPower(points []telemetry.ControlObservation, at time.Time, latest float64) (float64, bool) {
	baseline, known := bracketedEVPowerAt(points, at, latest)
	if !known {
		return 0, false
	}
	// Another slow device may force an older common site observation. A
	// later observed stop still removes its replacement credit, even if
	// the charger has since started again.
	for _, point := range points {
		if !point.Seen().Before(at) && finiteFuseValue(point.PowerW) && point.PowerW >= 0 {
			baseline = math.Min(baseline, point.PowerW)
		}
	}
	return baseline, true
}

// Phase-current magnitudes cannot tell us that an exporting phase makes
// room on an importing one. Bound the actual increase on the worst phase.
func evPhaseIncreaseW(meter *telemetry.DerReading, state *State, configs []loadpoint.Config, poolW float64) (float64, bool) {
	// DER data uses snake case; separate fields retain absent-vs-zero.
	var data struct {
		L1 *float64 `json:"l1_a"`
		L2 *float64 `json:"l2_a"`
		L3 *float64 `json:"l3_a"`
	}
	if json.Unmarshal(meter.Data, &data) != nil {
		return 0, false
	}
	amps := [...]*float64{data.L1, data.L2, data.L3}
	known := data.L1 != nil || data.L2 != nil || data.L3 != nil
	if !known {
		return 0, false
	}
	if !finiteFuseValue(state.SiteFuseAmps) || !finiteFuseValue(state.SiteFuseVoltage) || state.SiteFuseAmps <= 0 || state.SiteFuseVoltage <= 0 || state.SiteFusePhases < 1 || state.SiteFusePhases > 3 {
		return 0, true
	}
	worst := 0.0
	for _, a := range amps[:state.SiteFusePhases] {
		if a == nil || !finiteFuseValue(*a) {
			return 0, true
		}
		worst = math.Max(worst, math.Abs(*a))
	}
	phases := state.SiteFusePhases
	unknownPhase := false
	for _, cfg := range configs {
		// Empty retains the config's declared three-phase default. The
		// controller's final guard covers runtime auto/manual phase changes.
		if cfg.PhaseMode != "" && cfg.PhaseMode != "3p" {
			phases = 1 // No promise about an auto charger's current phase dwell.
			unknownPhase = true
		}
	}
	deltaA := state.SiteFuseAmps - 1 - worst
	if deltaA < 0 && unknownPhase {
		return -poolW, true // Cannot prove which single-phase car relieves it.
	}
	return deltaA * state.SiteFuseVoltage * float64(phases), true
}

func publishBatterylessEVFuseBudget(store *telemetry.Store, state *State, fuseMaxW float64) {
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	state.FuseEVMaxW, state.FuseSaturated = 0, false
	demand := math.Max(state.EVChargingW, scheduledEVPowerW(state))
	if meter == nil || demand <= 0 {
		return
	}
	cap := evHeadroomW(state.phaseImportCeilingW(fuseMaxW, meter), math.Max(meter.RawW, meter.SmoothedW), state.EVChargingW)
	state.FuseEVMaxW = math.Min(demand, cap)
	state.FuseSaturated = demand > cap
}
