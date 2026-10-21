package loadpoint

import (
	"math"
	"sort"
	"time"
)

type fuseRequest struct {
	cfg      Config
	deadline time.Time
	wantW    float64
	goal     bool
}

// freezeFuseBudget splits the EV fuse cap between cars for the whole tick,
// before any command, in priority order. A later callback or goal update
// cannot give a second car the first car's share. The cap is the only fuse
// limit here: control.EVFuseBudget reads the site meter for the phases that
// FuseConfigs names.
func (c *Controller) freezeFuseBudget(now time.Time, samples map[string]EVSample) {
	c.tickFuseCaps = nil
	if c.fuseEVMax == nil {
		return
	}
	total, ok := c.fuseEVMax()
	if !ok || total < 0 {
		return
	}
	if !finite(total) {
		total = 0
	}
	d, _ := c.currentDirective(now)
	var requests []fuseRequest
	for _, cfg := range c.manager.Configs() {
		sample := samples[cfg.DriverName]
		st, _ := c.manager.State(cfg.ID)
		request := fuseRequest{cfg: c.fuseConfig(cfg, c.tickChoices[cfg.ID].phaseMode), deadline: st.TargetTime}
		if sample.Connected && !sample.ConnectionUnknown && c.driverCanDispatch(cfg.DriverName) {
			if hold, held := c.GetManualHold(cfg.ID, now); held {
				request.wantW = clampManualPower(cfg, hold, c.siteFuse())
			} else {
				request.wantW, _, _ = c.computeCommandForBudget(now, cfg, sample.PowerW, false)
			}
			request.goal = request.wantW > 0 && !cfg.SurplusOnly
			if _, held := c.GetManualHold(cfg.ID, now); !held {
				c.batSoCArmedMu.Lock()
				armed := c.batSoCArmed[cfg.ID]
				c.batSoCArmedMu.Unlock()
				if cfg.SurplusOnly || armed || c.wakeKickActive(cfg.ID, now) || (st.FinishAtVehicleLimit && !st.GoalComplete) {
					request.wantW = cfg.MaxChargeW
				}
				if d.LoadpointFallback[cfg.ID] && sample.PowerUnavailable {
					request.wantW = 0
				}
			}
		}
		requests = append(requests, request)
	}
	sort.SliceStable(requests, func(i, j int) bool {
		a, b := requests[i], requests[j]
		if a.goal != b.goal {
			return a.goal // Required charging precedes a speculative PV/wake offer.
		}
		if a.deadline.IsZero() != b.deadline.IsZero() {
			return !a.deadline.IsZero()
		}
		if !a.deadline.Equal(b.deadline) {
			return a.deadline.Before(b.deadline)
		}
		return a.cfg.ID < b.cfg.ID
	})
	c.tickFuseCaps = make(map[string]float64, len(requests))
	remaining := total
	for _, request := range requests {
		cap := floorChargeW(math.Min(request.wantW, remaining), request.cfg.MinChargeW, request.cfg.MaxChargeW, request.cfg.AllowedStepsW)
		c.tickFuseCaps[request.cfg.ID] = cap
		remaining -= cap
	}
}

// FuseConfigs returns the loadpoints with the phase mode each command names
// this tick. control.EVFuseBudget measures room on those phases, so an auto
// or 1p offer never gets three phases of room. Outside a tick, and for a
// loadpoint that gets no command, the configured mode stands.
func (c *Controller) FuseConfigs() []Config {
	configs := c.manager.Configs()
	for i := range configs {
		if mode := c.tickChoices[configs[i].ID].phaseMode; mode != "" {
			configs[i].PhaseMode = mode
		}
	}
	return configs
}

// fusePhase names the phases a command uses. Empty is the driver's locked 3p.
func fusePhase(mode string) string {
	if mode == "" {
		return "3p"
	}
	return mode
}

// fuseConfig snaps a fuse cap to the smallest legal step on the phases the
// command names. A 3p label on a 1p-sized offer is below 6 A and never charges.
func (c *Controller) fuseConfig(cfg Config, phaseMode string) Config {
	cfg.MinChargeW = phaseMinChargeW(cfg, phaseMode, c.siteFuse())
	return cfg
}

func (c *Controller) currentDirective(now time.Time) (Directive, bool) {
	if c.tickDirectiveActive {
		return c.tickDirective, c.tickDirectiveOK
	}
	if c.plan == nil {
		return Directive{}, false
	}
	return c.plan(now)
}

func (c *Controller) fuseCapFor(id string) (float64, bool) {
	if c.tickFuseCaps != nil {
		return c.tickFuseCaps[id], true
	}
	if c.tickDirectiveActive {
		return 0, false // The tick's one callback read found no constraint.
	}
	if c.fuseEVMax != nil {
		return c.fuseEVMax()
	}
	return 0, false
}
