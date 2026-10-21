package main

import (
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
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
// meter itself is stale. Its phase currents are optional; once a reading
// reports any, every configured phase is a required fuse-safety input. They
// come with the reading fuse protection reads, so they are as fresh as it.
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
	if phaseCount <= 0 {
		phaseCount = 3
	}
	if _, present, valid := control.SitePhaseCurrents(meter, phaseCount); present && !valid {
		return siteDispatchDecision{Reason: siteDispatchPhaseCurrentsStale}
	}
	return siteDispatchDecision{}
}

// siteDispatchNow evaluates the tick's pre-dispatch gate on demand, with the
// tick's inputs and watchdog timeout, for operator setpoints that reach
// hardware outside the tick.
func siteDispatchNow(
	tel *telemetry.Store,
	cfg *config.Config,
	cfgMu *sync.RWMutex,
	ctrl *control.State,
	ctrlMu *sync.Mutex,
	now time.Time,
) siteDispatchDecision {
	cfgMu.RLock()
	timeout := time.Duration(cfg.Site.WatchdogTimeoutS) * time.Second
	cfgMu.RUnlock()
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctrlMu.Lock()
	siteMeter := ctrl.SiteMeterDriver
	fuseAmps := ctrl.SiteFuseAmps
	phases := ctrl.SiteFusePhases
	ctrlMu.Unlock()
	return evaluateSiteDispatchFreshnessAt(tel, siteMeter, fuseAmps, phases, timeout, now)
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
