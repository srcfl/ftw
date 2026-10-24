package loadpoint

import (
	"math"
	"sort"
	"time"
)

// fuseRoom is what one charger may use this tick, whatever it wants. What no
// charger took goes to the first charger with a share, so two chargers are
// never given the same room. capAmpsPerPhase reports it to the driver.
type fuseRoom struct {
	w          float64 // share of the EV cap, in watts
	phaseA     float64 // room on the busiest phase above its last command; with phaseKnown
	phaseKnown bool    // the site meter reports phase currents
}

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
	c.tickFuseCaps, c.tickFuseRooms, c.tickFuseReason = nil, nil, ""
	if c.fuseEVMax == nil {
		return
	}
	total, ok := c.fuseEVMax()
	if !ok || total < 0 {
		return
	}
	c.tickFuseReason = c.capReason()
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
				request.wantW, _, _ = c.computeCommandForBudget(now, request.cfg, sample.PowerW, false)
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
	roomA, perPhase := 0.0, false
	if c.fusePhaseRoomA != nil {
		roomA, perPhase = c.fusePhaseRoomA()
		perPhase = perPhase && finite(roomA)
	}
	c.tickFuseCaps = make(map[string]float64, len(requests))
	c.tickFuseRooms = make(map[string]fuseRoom, len(requests))
	remaining := total
	for _, request := range requests {
		want := math.Min(request.wantW, remaining)
		mode := c.tickChoices[request.cfg.ID].phaseMode
		split := c.commandSplitW(now, request.cfg)
		if perPhase {
			want = math.Min(want, c.phaseRoomCapW(request.cfg.ID, mode, want, split, roomA))
		}
		cap := floorChargeW(want, request.cfg.MinChargeW, request.cfg.MaxChargeW, request.cfg.AllowedStepsW)
		c.tickFuseCaps[request.cfg.ID] = cap
		remaining -= cap
		room := fuseRoom{w: cap, phaseKnown: perPhase}
		if perPhase {
			// A cut frees room only once the meter shows it.
			room.phaseA = math.Max(0, c.phaseRiseA(request.cfg.ID, mode, cap, split))
			roomA -= room.phaseA
		}
		c.tickFuseRooms[request.cfg.ID] = room
	}
	for _, request := range requests {
		room := c.tickFuseRooms[request.cfg.ID]
		if room.w <= 0 {
			continue
		}
		room.w += math.Max(0, remaining)
		room.phaseA += math.Max(0, roomA)
		c.tickFuseRooms[request.cfg.ID] = room
		break
	}
}

// capAmpsPerPhase is the most current per phase Core allows this charger on
// the phases its positive order names: its fuse room after the split between
// chargers, the import limit, max_amps_per_phase and the charger's maximum.
// It is not limited by what the plan or a hold wants, so it may exceed the
// order. Unknown without a frozen budget or a voltage.
func (c *Controller) capAmpsPerPhase(cfg Config, cmd map[string]any) (float64, bool) {
	room, ok := c.tickFuseRooms[cfg.ID]
	w, _ := cmd["power_w"].(float64)
	v, _ := cmd["voltage"].(float64)
	if !ok || !finite(room.w) || !finite(w) || w <= 0 || !finite(v) || v <= 0 {
		return 0, false
	}
	mode, _ := cmd["phase_mode"].(string)
	split, _ := cmd["phase_split_w"].(float64)
	phases := c.commandPhases(mode, w, split)
	capA := room.w / (v * float64(phases))
	if room.phaseKnown {
		capA = math.Min(capA, c.creditA(cfg.ID, phases)+room.phaseA)
	}
	if cfg.MaxChargeW > 0 {
		capA = math.Min(capA, cfg.MaxChargeW/(v*float64(phases)))
	}
	if !finite(capA) {
		return 0, false
	}
	// Two decimals, rounded down; the nudge absorbs float error at a step.
	capA = math.Floor(math.Max(0, capA)*100+1e-6) / 100
	if limit, known := cmd["max_amps_per_phase"].(float64); known && finite(limit) && limit > 0 {
		capA = math.Min(capA, limit)
	}
	return capA, true
}

// commandPhases is where a command of w lands: 1 or 3 phases, as the
// driver resolves its phase_mode and split. A 1-phase site has one.
func (c *Controller) commandPhases(mode string, w, split float64) int {
	if c.siteFuse().Phases() == 1 {
		return 1
	}
	if split <= 0 {
		split = defaultPhaseSplitW
	}
	return PhaseFor(mode, w, split)
}

// commandSplitW is the 1p/3p split this tick's command names.
func (c *Controller) commandSplitW(now time.Time, cfg Config) float64 {
	if hold, held := c.GetManualHold(cfg.ID, now); held && hold.PhaseSplitW > 0 {
		return hold.PhaseSplitW
	}
	return cfg.PhaseSplitW
}

// phaseRiseA is how much a command of w adds to the busiest phase over the
// loadpoint's last command. A single-phase command may land on any phase,
// so the last command's current comes off only where both use the same
// phases or the last one used all three.
func (c *Controller) phaseRiseA(id, mode string, w, split float64) float64 {
	site := c.siteFuse()
	if w <= 0 || site.Voltage <= 0 {
		return 0
	}
	phases := c.commandPhases(mode, w, split)
	return w/(site.Voltage*float64(phases)) - c.creditA(id, phases)
}

func (c *Controller) creditA(id string, phases int) float64 {
	st, _ := c.manager.State(id)
	site := c.siteFuse()
	if !st.CommandedKnown || st.CommandedW <= 0 || site.Voltage <= 0 || (st.CommandedPhases != 3 && st.CommandedPhases != phases) {
		return 0
	}
	return st.CommandedW / (site.Voltage * float64(min(st.CommandedPhases, site.Phases())))
}

// phaseRoomCapW is the most a command may offer when it can add at most
// roomA to the busiest phase. An auto command below the split lands on one
// phase. Room limits rises only: keeping the last command is never a cut,
// since the meter or the pending count already holds it, and only a
// measured overage cuts (EVFuseBudget). The same watts on other phases are
// a rise on some of them, so the last command is kept only when the order
// it allows lands on the same phases.
func (c *Controller) phaseRoomCapW(id, mode string, wantW, split, roomA float64) float64 {
	site := c.siteFuse()
	if site.Voltage <= 0 {
		return wantW
	}
	roomA = math.Max(0, roomA)
	phases := c.commandPhases(mode, wantW, split)
	capW := (c.creditA(id, phases) + roomA) * site.Voltage * float64(phases)
	if phases == 3 && c.commandPhases(mode, capW, split) == 1 {
		capW = math.Min(capW, (c.creditA(id, 1)+roomA)*site.Voltage)
	}
	if st, ok := c.manager.State(id); ok && st.CommandedKnown && st.CommandedPhases > 0 && st.CommandedW > capW &&
		c.commandPhases(mode, math.Min(wantW, st.CommandedW), split) == st.CommandedPhases {
		capW = st.CommandedW
	}
	return capW
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
