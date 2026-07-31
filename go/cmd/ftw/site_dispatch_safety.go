package main

import (
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

const (
	siteDispatchReady              = ""
	siteDispatchMeterStale         = "site_meter_stale"
	siteDispatchPhaseCurrentsStale = "site_phase_currents_stale"
)

// siteDispatchDecision is the single pre-dispatch freshness verdict shared
// by loadpoint and storage dispatch. An empty Reason means commands may be
// emitted; otherwise only explicit standdown/default commands are allowed.
type siteDispatchDecision struct {
	Reason string
}

func (d siteDispatchDecision) Allowed() bool { return d.Reason == siteDispatchReady }

// evaluateSiteDispatchFreshnessAt rejects physical dispatch when the site
// meter itself is stale. Per-phase currents are optional until the configured
// meter has emitted at least one of them; once available, every configured
// phase becomes a required fuse-safety input and must remain fresh.
func evaluateSiteDispatchFreshnessAt(
	tel *telemetry.Store,
	siteMeter string,
	fuseAmps float64,
	phaseCount int,
	timeout time.Duration,
	now time.Time,
) siteDispatchDecision {
	if tel == nil || siteMeter == "" {
		return siteDispatchDecision{}
	}
	meter := tel.Get(siteMeter, telemetry.DerMeter)
	if meter == nil || now.Sub(meter.UpdatedAt) > timeout {
		return siteDispatchDecision{Reason: siteDispatchMeterStale}
	}
	if fuseAmps <= 0 {
		return siteDispatchDecision{}
	}
	_, available, fresh := sitePhaseCurrentsAt(tel, siteMeter, phaseCount, timeout, now)
	if available && !fresh {
		return siteDispatchDecision{Reason: siteDispatchPhaseCurrentsStale}
	}
	return siteDispatchDecision{}
}

// sitePhaseCurrentsAt returns the configured phases in L1/L2/L3 order.
// available is false when this meter has never exposed phase-current
// telemetry, preserving the documented optional-per-phase behavior. If any
// phase has appeared, fresh is true only when all configured phases exist and
// are within the same watchdog window as the aggregate meter.
func sitePhaseCurrentsAt(
	tel *telemetry.Store,
	siteMeter string,
	phaseCount int,
	timeout time.Duration,
	now time.Time,
) (amps [3]float64, available, fresh bool) {
	if tel == nil || siteMeter == "" {
		return amps, false, false
	}
	return phaseCurrentsFromSnapshots(
		tel.LatestMetricsByDriver(siteMeter), phaseCount, timeout, now,
	)
}

func phaseCurrentsFromSnapshots(
	metrics []telemetry.MetricSnapshot,
	phaseCount int,
	timeout time.Duration,
	now time.Time,
) (amps [3]float64, available, fresh bool) {
	if phaseCount <= 0 {
		phaseCount = 3
	}
	if phaseCount > len(amps) {
		phaseCount = len(amps)
	}

	byName := make(map[string]telemetry.MetricSnapshot, len(metrics))
	for _, metric := range metrics {
		byName[metric.Name] = metric
	}
	phaseMetricNames := [...]string{"meter_l1_a", "meter_l2_a", "meter_l3_a"}
	fresh = true
	for i := 0; i < phaseCount; i++ {
		metric, ok := byName[phaseMetricNames[i]]
		if !ok {
			fresh = false
			continue
		}
		available = true
		amps[i] = metric.Value
		if now.Sub(metric.UpdatedAt) > timeout {
			fresh = false
		}
	}
	if !available {
		return amps, false, false
	}
	return amps, true, fresh
}

// staleSiteDefaultTracker emits one DefaultMode request per registered driver
// on entry to a stale state. Drivers added while the gate remains closed are
// defaulted once when first observed. Recovery clears the latch so a future
// stale transition is protected again without command spam every control tick.
type staleSiteDefaultTracker struct {
	blocked   bool
	siteMeter string
	defaulted map[string]struct{}
}

func (t *staleSiteDefaultTracker) update(blocked bool, siteMeter string, names []string) (pending []string, entered, recovered bool) {
	if !blocked {
		recovered = t.blocked
		t.blocked = false
		t.siteMeter = siteMeter
		t.defaulted = nil
		return nil, false, recovered
	}

	if !t.blocked || t.siteMeter != siteMeter {
		entered = true
		t.defaulted = make(map[string]struct{}, len(names))
	}
	t.blocked = true
	t.siteMeter = siteMeter
	if t.defaulted == nil {
		t.defaulted = make(map[string]struct{}, len(names))
	}
	for _, name := range names {
		if _, ok := t.defaulted[name]; ok {
			continue
		}
		t.defaulted[name] = struct{}{}
		pending = append(pending, name)
	}
	return pending, entered, false
}
