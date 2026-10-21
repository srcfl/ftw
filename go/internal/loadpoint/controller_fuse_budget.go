package loadpoint

import (
	"math"
	"sort"
	"time"
)

type fuseRequest struct {
	cfg      Config
	phase    string
	deadline time.Time
	wantW    float64
	baseW    float64
	goal     bool
}

// freezeFuseBudget divides one cap for the whole tick, before any command.
// A later callback or goal update cannot give a second car the first car's
// already offered share. Metered reductions earn new room only next tick.
// Each car is budgeted on the phases its command will name.
func (c *Controller) freezeFuseBudget(now time.Time, samples map[string]EVSample, choices map[string]tickChoice) {
	c.tickFuseCaps = nil
	if c.fuseEVMax == nil {
		clear(c.fuseResumeTicks)
		return
	}
	total, ok := c.fuseEVMax()
	if !ok || total < 0 {
		clear(c.fuseResumeTicks)
		return
	}
	if !finite(total) {
		total = 0
	}
	d, _ := c.currentDirective(now)
	var requests []fuseRequest
	var baseTotal float64
	for _, cfg := range c.manager.Configs() {
		sample := samples[cfg.DriverName]
		st, _ := c.manager.State(cfg.ID)
		st.PluggedIn = sample.Connected && !sample.ConnectionUnknown
		phase := choices[cfg.ID].phaseMode
		request := fuseRequest{cfg: c.fuseConfig(cfg, phase), phase: fusePhase(phase), deadline: st.TargetTime}
		// The meter may still show a car's highest recent offer on the same
		// phases. Up to that is not an increase; the car's own readings,
		// late or lone, never count.
		if peak := c.offerPeaks[cfg.ID]; st.PluggedIn && peak.phase == request.phase {
			request.baseW = peak.w
		}
		baseTotal += request.baseW
		if st.PluggedIn && c.driverCanDispatch(cfg.DriverName) {
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
	remaining, increase := total, math.Max(0, total-baseTotal)
	if phaseIncrease, known := c.sharedPhaseIncreaseW(requests); known {
		increase = math.Max(0, math.Min(increase, phaseIncrease))
	}
	for _, request := range requests {
		target := floorChargeW(math.Min(request.wantW, remaining), request.cfg.MinChargeW, request.cfg.MaxChargeW, request.cfg.AllowedStepsW)
		remaining -= target
		// Another car's curtailment is spent only once it has settled.
		want := math.Min(target, request.baseW+increase)
		cap := floorChargeW(want, request.cfg.MinChargeW, request.cfg.MaxChargeW, request.cfg.AllowedStepsW)
		cap = c.recoverFuseCap(request.cfg.ID, request.wantW, cap)
		c.tickFuseCaps[request.cfg.ID] = cap
		increase -= math.Max(0, cap-request.baseW)
	}
}

type offerPeak struct {
	w     float64
	phase string
	at    time.Time
}

// recordOfferPeak keeps the highest offer the meter may still show. A new
// phase mode starts over: the same watts on other phases are other currents.
func (c *Controller) recordOfferPeak(now time.Time, id string, w float64, cmd map[string]any) string {
	mode, _ := cmd["phase_mode"].(string)
	phase := fusePhase(mode)
	if c.offerPeaks == nil {
		c.offerPeaks = map[string]offerPeak{}
	}
	if peak := c.offerPeaks[id]; peak.phase != phase || w >= peak.w || now.Sub(peak.at) >= CommandSettle {
		c.offerPeaks[id] = offerPeak{w: w, phase: phase, at: now}
	}
	return phase
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

func (c *Controller) recoverFuseCap(id string, wantW, capW float64) float64 {
	if wantW <= 0 {
		delete(c.fuseResumeTicks, id)
		return capW
	}
	if capW <= 0 {
		if c.fuseResumeTicks == nil {
			c.fuseResumeTicks = map[string]int{}
		}
		c.fuseResumeTicks[id] = 0
		return 0
	}
	if ready, paused := c.fuseResumeTicks[id]; paused {
		ready++
		if ready < fuseResumeSafeTicks {
			c.fuseResumeTicks[id] = ready
			return 0
		}
		delete(c.fuseResumeTicks, id)
	}
	return capW
}

// A new single-phase offer can land on the busiest phase, so an increase
// needs measured room there. Cutting an overage is the shared budget's job.
func (c *Controller) sharedPhaseIncreaseW(requests []fuseRequest) (float64, bool) {
	if c.perPhaseMeterAmps == nil {
		return 0, false
	}
	l1, l2, l3, ok := c.perPhaseMeterAmps()
	site := c.siteFuse()
	if !ok || !finite(site.MaxAmps) || !finite(site.Voltage) || site.MaxAmps <= 0 || site.Voltage <= 0 {
		return 0, false
	}
	if !finite(l1) || !finite(l2) || !finite(l3) {
		return 0, true
	}
	phaseCount := site.Phases()
	for _, request := range requests {
		if request.phase != "3p" {
			phaseCount = 1
		}
	}
	worst := math.Max(math.Abs(l1), math.Max(math.Abs(l2), math.Abs(l3)))
	return (site.MaxAmps - fusePhaseMarginA - worst) * site.Voltage * float64(phaseCount), true
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
