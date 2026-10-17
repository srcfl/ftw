package control

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type evBatteryPowerSeries struct {
	reading *telemetry.DerReading
	current telemetry.ControlObservation
	latest  float64
	points  []telemetry.ControlObservation
}

// clampEVBatteryHeadroom reserves EV offers that the site sample does not
// yet contain. A newer charger reading must not be subtracted from an older
// grid sample to fund optional battery charge. Commands that reduce another
// battery's charge or increase its discharge do not create measured room.
func clampEVBatteryHeadroom(targets []DispatchTarget, store *telemetry.Store, state *State, fuseMaxW float64) []DispatchTarget {
	if store == nil || state == nil || len(targets) == 0 || fuseMaxW <= 0 {
		return targets
	}
	now := state.now()
	windows := store.ConfirmedControlWindows(now.Add(-telemetry.ControlWindowDuration), now)
	need := make(map[string]float64, len(state.evFuseDemands))
	phaseMode := make(map[string]string, len(state.evFuseDemands))
	for _, demand := range state.evFuseDemands {
		if demand.powerW > 0 && finiteFuseValue(demand.powerW) {
			need[demand.driverName] = math.Max(need[demand.driverName], demand.powerW)
			phaseMode[demand.driverName] = demand.phaseMode
		}
	}
	series := make(map[string]evBatteryPowerSeries)
	// Unmanaged live chargers stay separate. Their measured power cannot
	// finance a different car's new offer, but an unobserved start also
	// needs room before an optional battery increase.
	for _, kind := range []telemetry.DerType{telemetry.DerEV, telemetry.DerV2X} {
		for _, reading := range store.ReadingsByType(kind) {
			health := store.DriverHealth(reading.Driver)
			observation, ok := qualifiedFuseObservation(reading, now)
			if !ok || health == nil || !health.TelemetryLive() || observation.PowerW <= 0 {
				continue
			}
			need[reading.Driver] = math.Max(need[reading.Driver], observation.PowerW)
			points := windows[reading.Driver+":"+kind.String()].Points
			series[reading.Driver] = evBatteryPowerSeries{reading: reading, current: observation, latest: observation.PowerW, points: append(points, observation)}
		}
	}
	if len(need) == 0 {
		return targets
	}
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	site, known := qualifiedFuseObservation(meter, now)
	if !known || !finiteFuseValue(fuseMaxW) {
		return floorPositiveEVBatteryTargets(targets)
	}
	baselines := make(map[string]float64, len(targets))
	latestBaselines := make(map[string]float64, len(targets))
	batteryReadings := make(map[string]*telemetry.DerReading, len(targets))
	byDriver := make(map[string]DispatchTarget, len(targets))
	drivers := make(map[string]bool, len(targets))
	for driver := range state.DriverLimits {
		drivers[driver] = true
	}
	for _, reading := range store.ReadingsByType(telemetry.DerBattery) {
		drivers[reading.Driver] = true
	}
	for _, target := range targets {
		if _, duplicate := byDriver[target.Driver]; duplicate {
			return floorPositiveEVBatteryTargets(targets)
		}
		byDriver[target.Driver] = target
		drivers[target.Driver] = true
	}
	uppers := make(map[string]float64, len(drivers))
	coherentBatteries := make(map[string]bool, len(targets))
	for driver := range drivers {
		reading := store.Get(driver, telemetry.DerBattery)
		if reading != nil {
			batteryReadings[driver] = reading
		}
		if current, fresh := qualifiedFuseObservation(reading, now); fresh {
			uppers[driver] = current.PowerW
			coherentBatteries[driver] = true
		}
		if _, targeted := byDriver[driver]; targeted {
			coherentBatteries[driver] = true
		}
		if upper, known := batteryOrderUpperW(store, state, driver); known {
			if !finiteFuseValue(upper) {
				return floorPositiveEVBatteryTargets(targets)
			}
			if observed, known := uppers[driver]; known {
				upper = math.Max(upper, observed)
			}
			uppers[driver] = upper
			coherentBatteries[driver] = true
		}
	}
	at, gridW := coherentEVBatterySite(store, windows, state, site, meter.RawW, series, coherentBatteries, now)
	var evIncreaseW, latestEVIncreaseW, latestEVIncreaseA float64
	for driver, watts := range need {
		var credit, latestCredit float64
		if power, ok := series[driver]; ok {
			if baseline, ok := bracketedEVPower(power.points, at, power.latest); ok {
				credit = baseline
			}
			latestCredit = evPowerAtLatestSite(power.points, site.Seen(), power.current)
		}
		evIncreaseW += math.Max(0, watts-credit)
		latestIncrease := math.Max(0, watts-latestCredit)
		latestEVIncreaseW += latestIncrease
		phases := 1 // No phase placement promise for 1p or auto chargers.
		if phaseMode[driver] == "3p" {
			phases = 3
		}
		if state.SiteFuseVoltage > 0 {
			latestEVIncreaseA += latestIncrease / (state.SiteFuseVoltage * float64(phases))
		}
	}
	for driver := range drivers {
		baseline, reading, _ := evBatteryBaseline(store, windows, state, driver, at, now)
		baselines[driver] = baseline
		if reading != nil {
			batteryReadings[driver] = reading
		}
		latestBaselines[driver] = -state.DriverLimits[driver].dischargeCap()
		if current, fresh := qualifiedFuseObservation(reading, now); fresh {
			if upper, known := uppers[driver]; !known || current.PowerW > upper {
				uppers[driver] = current.PowerW
			}
			points := append(windows[driver+":"+telemetry.DerBattery.String()].Points, current)
			latestBaselines[driver] = batteryPowerAtLatestSite(points, site.Seen(), current, latestBaselines[driver])
		}
	}

	// Keep the common measured balance separate from the newest meter.
	// Combining a newer total with an older low battery baseline would
	// reserve an already observed increase twice and cause oscillation.
	spare := state.effectiveImportCeilingW(fuseMaxW) - gridW - evIncreaseW
	out := capBatteryOffersToHeadroom(targets, baselines, uppers, spare)
	latestSpare := state.effectiveImportCeilingW(fuseMaxW) - meter.RawW - latestEVIncreaseW
	// During measured export overflow, the existing fuse pipeline may
	// require charge to reduce export. Magnitudes alone cannot label that
	// worst phase as import; leave this direction to the existing guard.
	if meter.RawW >= -state.effectiveExportCeilingW(fuseMaxW) {
		if phaseSpare, known := evBatteryPhaseSpareW(meter, state, latestEVIncreaseA); known {
			latestSpare = math.Min(latestSpare, phaseSpare)
		}
	}
	latestOut := capBatteryOffersToHeadroom(targets, latestBaselines, uppers, latestSpare)
	for i := range out {
		if latestOut[i].TargetW < out[i].TargetW {
			out[i].TargetW, out[i].Clamped = latestOut[i].TargetW, true
		}
	}
	// Charge needed to bring measured export inside its hard ceiling is
	// not optional. Preserve that part of the already validated offer;
	// this stage never invents or increases an original battery command.
	var offeredCharge, finalCharge float64
	for i := range targets {
		offeredCharge += math.Max(0, targets[i].TargetW)
		finalCharge += math.Max(0, out[i].TargetW)
	}
	exportFloor := math.Min(offeredCharge, math.Max(0, -meter.RawW-state.effectiveExportCeilingW(fuseMaxW)))
	if exportFloor > finalCharge && offeredCharge > finalCharge {
		scale := (exportFloor - finalCharge) / (offeredCharge - finalCharge)
		for i := range out {
			if targets[i].TargetW > out[i].TargetW && targets[i].TargetW > 0 {
				out[i].TargetW += (targets[i].TargetW - out[i].TargetW) * scale
			}
		}
	}
	// A racing poll invalidates the captured balance; retry next tick.
	if current := store.Get(state.SiteMeterDriver, telemetry.DerMeter); current == nil || !current.UpdatedAt.Equal(meter.UpdatedAt) {
		return floorPositiveEVBatteryTargets(targets)
	}
	for driver, reading := range batteryReadings {
		if current := store.Get(driver, telemetry.DerBattery); current == nil || !current.UpdatedAt.Equal(reading.UpdatedAt) {
			return floorPositiveEVBatteryTargets(targets)
		}
	}
	for _, power := range series {
		if current := store.Get(power.reading.Driver, power.reading.DerType); current == nil || !current.UpdatedAt.Equal(power.reading.UpdatedAt) {
			return floorPositiveEVBatteryTargets(targets)
		}
	}
	return out
}

// Both source-time bounds share this allocation. A reduction at one battery
// never funds an increase at another, and a lost discharge also needs room.
func capBatteryOffersToHeadroom(targets []DispatchTarget, baselines, uppers map[string]float64, spare float64) []DispatchTarget {
	if !finiteFuseValue(spare) {
		return floorPositiveEVBatteryTargets(targets)
	}
	byDriver := make(map[string]DispatchTarget, len(targets))
	allocationBases := make(map[string]float64, len(targets))
	var positiveBase, dischargeLoss, pendingExtra, pendingOutsidePositive float64
	for _, target := range targets {
		byDriver[target.Driver] = target
		baseline := baselines[target.Driver]
		if !finiteFuseValue(baseline) {
			return floorPositiveEVBatteryTargets(targets)
		}
		allocationBases[target.Driver] = math.Max(0, baseline)
		if upper, known := uppers[target.Driver]; known {
			allocationBases[target.Driver] = math.Max(allocationBases[target.Driver], upper)
		}
		if target.TargetW > 0 {
			positiveBase += math.Max(0, baseline)
		}
		dischargeLoss += math.Max(0, math.Min(0, target.TargetW)-math.Min(0, baseline))
	}
	for driver, upper := range uppers {
		floor := baselines[driver]
		target, targeted := byDriver[driver]
		if targeted {
			floor = math.Max(floor, math.Min(0, target.TargetW))
		}
		pending := math.Max(0, upper-floor)
		pendingExtra += pending
		if !targeted || target.TargetW <= 0 {
			pendingOutsidePositive += pending
		}
	}
	spare -= dischargeLoss
	out := append([]DispatchTarget(nil), targets...)
	var wantedIncrease float64
	for _, target := range out {
		if target.TargetW > 0 {
			wantedIncrease += math.Max(0, target.TargetW-allocationBases[target.Driver])
		}
	}
	increaseScale := 1.0
	if wantedIncrease > 0 {
		increaseScale = math.Min(1, math.Max(0, spare-pendingExtra)/wantedIncrease)
	}
	var positiveTotal float64
	for i := range out {
		if out[i].TargetW > 0 {
			baseline := allocationBases[out[i].Driver]
			capped := math.Min(out[i].TargetW, baseline) + math.Max(0, out[i].TargetW-baseline)*increaseScale
			if capped < out[i].TargetW {
				out[i].TargetW, out[i].Clamped = capped, true
			}
			positiveTotal += capped
		}
	}
	capW := math.Max(0, positiveBase+spare-pendingOutsidePositive)
	if positiveTotal > capW && positiveTotal > 0 {
		for i := range out {
			if out[i].TargetW > 0 {
				out[i].TargetW *= capW / positiveTotal
				out[i].Clamped = true
			}
		}
	}
	// A partial positive reduction also leaves its old higher order pending.
	// Bound every offer against the same snapshot of its siblings; one
	// commanded reduction must not fund another offer in this pass.
	covered := make([]float64, len(out))
	var coveredTotal float64
	for i, target := range out {
		if target.TargetW > 0 {
			covered[i] = math.Max(target.TargetW, math.Max(0, uppers[target.Driver]))
			coveredTotal += covered[i]
		}
	}
	for i := range out {
		if out[i].TargetW > 0 {
			room := math.Max(0, capW-(coveredTotal-covered[i]))
			if out[i].TargetW > room {
				out[i].TargetW, out[i].Clamped = room, true
			}
		}
	}
	return out
}

// A source sample before this site sample may hold its current level. A
// later start needs bracketed replacement credit before it can fund change.
func evPowerAtLatestSite(points []telemetry.ControlObservation, at time.Time, current telemetry.ControlObservation) float64 {
	if !current.Seen().After(at) && finiteFuseValue(current.PowerW) {
		return math.Max(0, current.PowerW)
	}
	baseline, _ := bracketedEVPower(points, at, current.PowerW)
	return baseline
}

func batteryPowerAtLatestSite(points []telemetry.ControlObservation, at time.Time, current telemetry.ControlObservation, fallback float64) float64 {
	if !finiteFuseValue(current.PowerW) {
		return fallback
	}
	if !current.Seen().After(at) {
		return current.PowerW
	}
	if baseline, known := bracketedBatteryPower(points, at, current.PowerW); known {
		return baseline
	}
	baseline := math.Min(0, current.PowerW)
	for _, point := range points {
		if finiteFuseValue(point.PowerW) {
			baseline = math.Min(baseline, point.PowerW)
		}
	}
	return baseline
}

// Use one common source instant when fixed poll order leaves the latest
// site sample ahead of a battery. The separate latest-site bound handles
// later grid and phase increases without counting DER increases twice.
func coherentEVBatterySite(store *telemetry.Store, windows map[string]telemetry.ControlBaseline, state *State, current telemetry.ControlObservation, gridW float64, evs map[string]evBatteryPowerSeries, batteries map[string]bool, now time.Time) (time.Time, float64) {
	current.At = current.Seen()
	sites := append(windows[state.SiteMeterDriver+":"+telemetry.DerMeter.String()].Points, current)
	sort.SliceStable(sites, func(i, j int) bool { return sites[i].At.After(sites[j].At) })
	for _, site := range sites {
		if site.At.Before(now.Add(-telemetry.ControlWindowDuration)) || site.At.After(now) || !finiteFuseValue(site.PowerW) {
			continue
		}
		coherent := true
		for _, ev := range evs {
			if _, known := bracketedEVPower(ev.points, site.At, ev.latest); !known {
				coherent = false
				break
			}
		}
		if coherent {
			for driver := range batteries {
				if _, _, known := evBatteryBaseline(store, windows, state, driver, site.At, now); !known {
					coherent = false
					break
				}
			}
		}
		if coherent {
			return site.At, site.PowerW
		}
	}
	return current.At, gridW
}

func evBatteryBaseline(store *telemetry.Store, windows map[string]telemetry.ControlBaseline, state *State, driver string, at, now time.Time) (float64, *telemetry.DerReading, bool) {
	reading := store.Get(driver, telemetry.DerBattery)
	observation, known := qualifiedFuseObservation(reading, now)
	if !known {
		return -state.DriverLimits[driver].dischargeCap(), reading, false
	}
	points := windows[driver+":"+telemetry.DerBattery.String()].Points
	latest := observation.PowerW // Qualified site-side AC, which can differ from raw DC.
	if baseline, known := bracketedBatteryPower(append(points, observation), at, latest); known {
		return baseline, reading, true
	}
	// No positive replacement credit without correspondence. Retain any
	// observed discharge as a possible import rise, even in a sparse ring.
	baseline := math.Min(0, latest)
	for _, point := range points {
		if finiteFuseValue(point.PowerW) {
			baseline = math.Min(baseline, point.PowerW)
		}
	}
	return baseline, reading, false
}

// Unlike an EV, a battery's baseline may be negative. Retaining the lower
// signed value also reserves a loss of discharge before a positive offer.
func bracketedBatteryPower(points []telemetry.ControlObservation, at time.Time, latest float64) (float64, bool) {
	var before, after telemetry.ControlObservation
	hasBefore, hasAfter := false, false
	for _, point := range points {
		if !finiteFuseValue(point.PowerW) {
			continue
		}
		if !point.At.After(at) && (!hasBefore || point.At.After(before.At)) {
			before, hasBefore = point, true
		}
		if !point.Seen().Before(at) && (!hasAfter || point.At.Before(after.At)) {
			after, hasAfter = point, true
		}
	}
	if !hasBefore || !hasAfter || !finiteFuseValue(latest) {
		return 0, false
	}
	baseline := math.Min(latest, math.Min(before.PowerW, after.PowerW))
	for _, point := range points {
		if !point.Seen().Before(at) && finiteFuseValue(point.PowerW) {
			baseline = math.Min(baseline, point.PowerW)
		}
	}
	return baseline, true
}

func evBatteryPhaseSpareW(meter *telemetry.DerReading, state *State, evIncreaseA float64) (float64, bool) {
	var data struct {
		L1 *float64 `json:"l1_a"`
		L2 *float64 `json:"l2_a"`
		L3 *float64 `json:"l3_a"`
	}
	if json.Unmarshal(meter.Data, &data) != nil || (data.L1 == nil && data.L2 == nil && data.L3 == nil) {
		return 0, false
	}
	if state.SiteFusePhases < 1 || state.SiteFusePhases > 3 || state.SiteFuseVoltage <= 0 || state.SiteFuseAmps <= 0 {
		return math.Inf(-1), true
	}
	worst := 0.0
	for _, amps := range []*float64{data.L1, data.L2, data.L3}[:state.SiteFusePhases] {
		if amps == nil || !finiteFuseValue(*amps) {
			return math.Inf(-1), true
		}
		worst = math.Max(worst, math.Abs(*amps))
	}
	margin := math.Max(1, state.SiteFuseSafetyA) // Same working margin as the EV phase guard.
	return (state.SiteFuseAmps - margin - worst - evIncreaseA) * state.SiteFuseVoltage * float64(state.SiteFusePhases), true
}

func floorPositiveEVBatteryTargets(targets []DispatchTarget) []DispatchTarget {
	out := append([]DispatchTarget(nil), targets...)
	for i := range out {
		if out[i].TargetW > 0 {
			out[i].TargetW, out[i].Clamped = 0, true
		}
	}
	return out
}
