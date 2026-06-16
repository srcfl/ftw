package flexload

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
	"github.com/frahlg/forty-two-watts/go/internal/thermalmodel"
)

// Device is one configured flexible load (normalized from config.FlexLoad).
type Device struct {
	Type       string // "thermostat" | "deferrable"
	DriverName string
	Mode       string // "planner" (default) | "simple" — thermostats only

	// thermostat
	HeatingKind       string  // "electric" (default) | "hydronic"
	COP               float64 // electrical→thermal multiplier (1 electric, ~3 heat pump)
	FlowDriver        string  // optional: heat-pump driver exposing supply (flow) temp
	FlowMetric        string  // metric carrying flow temperature (°C)
	NominalFlowDeltaC float64 // design flow-above-room delta for a full heat charge (default 15)
	MinC            float64
	MaxC            float64
	MaxHeatW        float64 // zone thermal output cap (W)
	IndoorDriver    string  // optional: separate driver (temp sensor) for indoor temp
	IndoorMetric    string  // driver metric carrying measured indoor temp (°C)
	HeatMetric      string  // optional: metered heating power (W) for RC training
	SlabDriver      string  // optional: floor-probe / flow-temp driver (→ two-mass model)
	SlabMetric      string  // metric carrying slab/floor temperature (°C)
	SetpointAction  string  // driver command action that writes the setpoint
	PreHeatFraction float64

	// simple-mode
	TargetC           float64 // comfort target to maintain
	PriceThresholdOre float64 // "expensive" cutoff; 0 = derive from forecast
	BlockHorizonH     float64 // hours the target must hold to allow a block

	// deferrable
	PowerMetric  string // optional: plug power metric → learn load + energy
	EnergyWh     float64
	PowerW       float64
	OnAction     string
	OffAction    string
	PreferPV     bool
	EarliestHour int
	DeadlineHour int
}

// DispatchFunc sends a JSON command to a driver (typically registry.Send).
type DispatchFunc func(ctx context.Context, driver string, payload []byte) error

// Service runs the flex-load scheduler against live forecasts and dispatches
// per-slot directives to Matter drivers. It also trains a per-zone thermal
// RC model from telemetry when a metered heating signal is available.
//
// It is deliberately decoupled from the MPC: the caller supplies a Slots
// function (built from the MPC plan's price/PV curve) and an Outdoor
// temperature function, so flexload neither imports mpc nor duplicates the
// forecast plumbing.
type Service struct {
	Store    *state.Store
	Tele     *telemetry.Store
	Devices  []Device
	Slots    func() []PriceSlot              // current horizon price/PV slots
	Outdoor  func(slotStartMs int64) float64 // outdoor temp forecast (°C)
	Dispatch DispatchFunc
	// PriceAt returns the price (öre/kWh) at an arbitrary time, independently
	// of the MPC. Used for simple-mode "now" pricing AND for the reheat-cost
	// side of the economic pause calculation (price at the future moment the
	// zone will need to be reheated). Optional — when nil, simple mode
	// derives the current price from Slots() and the pause economics fall
	// back to "hold comfort" (never a speculative reduction).
	PriceAt func(t time.Time) (float64, bool)
	// FuseBudgetW is the shared electrical headroom (W) simple-mode zones
	// arbitrate under (block highest-power zones first when comfort allows).
	// 0 disables arbitration.
	FuseBudgetW float64

	SampleInterval time.Duration // thermal training cadence (default 60s)
	ReplanInterval time.Duration // schedule + dispatch cadence (default 5m)

	mu         sync.RWMutex
	thermal    map[string]*thermalmodel.Model    // by driver name (single-mass RC)
	twomass    map[string]*thermalmodel.TwoMass  // by driver name (slab+room, floor heating)
	lastIndoor map[string]tempSample             // last indoor reading for delta training
	lastSlab   map[string]tempSample             // last slab reading for two-mass training
	plug       map[string]*PlugProfile           // by driver name (deferrable load learning)
	stove      map[string]*ExternalHeatDetector  // by driver name (wood-stove / free-heat detection)

	stop chan struct{}
	done chan struct{}
}

type tempSample struct {
	c    float64
	tsMs int64
}

const thermalStateKeyPrefix = "flexload/thermal/"
const twoMassStateKeyPrefix = "flexload/twomass/"
const plugStateKeyPrefix = "flexload/plug/"
const stoveStateKeyPrefix = "flexload/stove/"

// NewService builds a flex-load service. Returns nil if there are no
// devices configured, so the caller can skip starting it entirely.
func NewService(st *state.Store, tel *telemetry.Store, devices []Device) *Service {
	if len(devices) == 0 {
		return nil
	}
	s := &Service{
		Store:          st,
		Tele:           tel,
		Devices:        devices,
		SampleInterval: 60 * time.Second,
		ReplanInterval: 5 * time.Minute,
		thermal:        map[string]*thermalmodel.Model{},
		twomass:        map[string]*thermalmodel.TwoMass{},
		lastIndoor:     map[string]tempSample{},
		lastSlab:       map[string]tempSample{},
		plug:           map[string]*PlugProfile{},
		stove:          map[string]*ExternalHeatDetector{},
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	for _, d := range devices {
		switch d.Type {
		case "thermostat":
			// Restore or initialize a thermal model per thermostat.
			m := thermalmodel.NewModel()
			if st != nil {
				if js, ok := st.LoadConfig(thermalStateKeyPrefix + d.DriverName); ok && js != "" {
					var loaded thermalmodel.Model
					if err := json.Unmarshal([]byte(js), &loaded); err == nil && loaded.Forgetting > 0 {
						m = &loaded
						slog.Info("flexload thermal model restored",
							"driver", d.DriverName, "samples", m.Samples,
							"tau_h", m.TauSeconds()/3600, "quality", m.Quality())
					}
				}
			}
			s.thermal[d.DriverName] = m
			// For floor heating with a slab/floor probe, also keep a two-mass
			// model and prefer it for coast/forecast once trained.
			if d.SlabMetric != "" {
				tmm := thermalmodel.NewTwoMass()
				if st != nil {
					if js, ok := st.LoadConfig(twoMassStateKeyPrefix + d.DriverName); ok && js != "" {
						var loaded thermalmodel.TwoMass
						if err := json.Unmarshal([]byte(js), &loaded); err == nil && loaded.Forgetting > 0 {
							tmm = &loaded
							slog.Info("flexload two-mass model restored",
								"driver", d.DriverName, "samples", tmm.Samples,
								"tau_room_h", tmm.TauRoomSeconds()/3600,
								"tau_slab_h", tmm.TauSlabSeconds()/3600, "quality", tmm.Quality())
						}
					}
				}
				s.twomass[d.DriverName] = tmm
			}
			// Restore the external-heat (wood-stove) detector's learned stats.
			det := &ExternalHeatDetector{}
			if st != nil {
				if js, ok := st.LoadConfig(stoveStateKeyPrefix + d.DriverName); ok && js != "" {
					var loaded ExternalHeatDetector
					if err := json.Unmarshal([]byte(js), &loaded); err == nil {
						det = &loaded
					}
				}
			}
			s.stove[d.DriverName] = det
		case "deferrable":
			// Restore or initialize a plug load profile when metering is wired.
			if d.PowerMetric == "" {
				continue
			}
			p := NewPlugProfile(0)
			if st != nil {
				if js, ok := st.LoadConfig(plugStateKeyPrefix + d.DriverName); ok && js != "" {
					var loaded PlugProfile
					if err := json.Unmarshal([]byte(js), &loaded); err == nil && loaded.OnThresholdW > 0 {
						p = &loaded
						slog.Info("flexload plug profile restored",
							"driver", d.DriverName, "running_w", p.RunningW,
							"daily_wh", p.DailyEnergyWh, "class", p.Classify())
					}
				}
			}
			s.plug[d.DriverName] = p
		}
	}
	return s
}

// PlugProfileFor returns a copy of the learned plug profile for a driver.
func (s *Service) PlugProfileFor(driver string) (PlugProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.plug[driver]
	if !ok {
		return PlugProfile{}, false
	}
	return *p, true
}

// ThermalModel returns a copy of the learned model for a driver (for the
// API / diagnostics), and whether one exists.
func (s *Service) ThermalModel(driver string) (thermalmodel.Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.thermal[driver]
	if !ok {
		return thermalmodel.Model{}, false
	}
	return *m, true
}

// TwoMassModel returns a copy of the learned floor-heating model for a
// driver (for the API / diagnostics), and whether one exists.
func (s *Service) TwoMassModel(driver string) (thermalmodel.TwoMass, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.twomass[driver]
	if !ok {
		return thermalmodel.TwoMass{}, false
	}
	return *m, true
}

// Start spawns the service loop. Safe to call once.
func (s *Service) Start(ctx context.Context) {
	go s.loop(ctx)
}

// Stop signals the loop and persists final model state.
func (s *Service) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	sampleT := time.NewTicker(s.SampleInterval)
	replanT := time.NewTicker(s.ReplanInterval)
	defer sampleT.Stop()
	defer replanT.Stop()

	// Prime an initial dispatch so devices get a directive at startup
	// rather than waiting a full ReplanInterval.
	s.replan(ctx)
	for {
		select {
		case <-s.stop:
			s.persistAll()
			return
		case <-ctx.Done():
			s.persistAll()
			return
		case <-sampleT.C:
			s.sample()
		case <-replanT.C:
			s.replan(ctx)
		}
	}
}

// sample trains each thermostat's RC model from telemetry. Training only
// runs when a metered heating signal (HeatMetric) is configured — without a
// real power measurement we can't attribute warming to heat vs. heat-loss
// without corrupting the model, so we leave it on its physics prior (the
// scheduler still pre-heats in cheap hours and honors the comfort floor).
func (s *Service) sample() {
	now := time.Now()
	dtMax := 4 * float64(s.SampleInterval) / float64(time.Second)
	for _, d := range s.Devices {
		// Learn deferrable plug loads from their metered power.
		if d.Type == "deferrable" && d.PowerMetric != "" {
			if w, _, ok := s.Tele.LatestMetric(d.DriverName, d.PowerMetric); ok {
				s.mu.Lock()
				if p := s.plug[d.DriverName]; p != nil {
					p.Update(w, now.UnixMilli(), dtMax)
					if p.Samples%20 == 0 && p.Samples > 0 {
						s.persistPlug(d.DriverName, p)
					}
				}
				s.mu.Unlock()
			}
			continue
		}
		// Thermal training + external-heat detection both need metered heat
		// to be honest (we must know our own electrical contribution).
		if d.Type != "thermostat" || d.IndoorMetric == "" || d.HeatMetric == "" {
			continue
		}
		indoorC, ok := s.readIndoor(d)
		if !ok {
			continue
		}
		heatElecW, _, ok := s.Tele.LatestMetric(d.DriverName, d.HeatMetric)
		if !ok {
			heatElecW = 0
		}
		// HeatMetric is metered ELECTRICAL power; the RC model needs the
		// THERMAL power delivered to the zone. For direct electric COP=1;
		// for a hydronic zone metered at the heat pump, thermal = elec×COP.
		heatThermalW := heatElecW
		if d.COP > 0 {
			heatThermalW *= d.COP
		}
		outdoor := 0.0
		if s.Outdoor != nil {
			outdoor = s.Outdoor(now.UnixMilli())
		}

		s.mu.Lock()
		prev, hasPrev := s.lastIndoor[d.DriverName]
		s.lastIndoor[d.DriverName] = tempSample{c: indoorC, tsMs: now.UnixMilli()}
		m := s.thermal[d.DriverName]
		det := s.stove[d.DriverName]
		if hasPrev && m != nil {
			dt := float64(now.UnixMilli()-prev.tsMs) / 1000.0
			// Guard against long gaps (driver outage) producing a bogus delta.
			if dt > 0 && dt <= 4*float64(s.SampleInterval)/float64(time.Second) {
				// External-heat (wood-stove) detection: compare the observed
				// warming to what the model expects from our known heating.
				if det != nil {
					expDelta := m.ExpectedDeltaC(prev.c, outdoor, heatThermalW, dt)
					obsDelta := indoorC - prev.c
					wasActive := det.Active(now.UnixMilli())
					det.Update(obsDelta, expDelta, heatElecW, dt, now.UnixMilli(), m.ThermalWForRate)
					if det.Active(now.UnixMilli()) != wasActive {
						s.persistStove(d.DriverName, det)
					}
				}
				// Train the RC model only when nothing is corrupting the
				// signal: an external heat source (a wood stove's extra
				// heat-gain) would teach the model a false, too-warm building.
				externalActive := det != nil && det.Active(now.UnixMilli())
				if !externalActive {
					if m.Update(prev.c, indoorC, outdoor, heatThermalW, dt, now.UnixMilli()) {
						if m.Samples%10 == 0 {
							s.persist(d.DriverName, m)
						}
					}
					// Two-mass (floor heating) training needs the slab reading
					// at both ends of the step.
					if tmm := s.twomass[d.DriverName]; tmm != nil && d.SlabMetric != "" {
						if slabC, slabOk := s.readSlab(d); slabOk {
							prevSlab, hasPrevSlab := s.lastSlab[d.DriverName]
							s.lastSlab[d.DriverName] = tempSample{c: slabC, tsMs: now.UnixMilli()}
							if hasPrevSlab {
								if tmm.Update(prev.c, indoorC, prevSlab.c, slabC, outdoor, heatThermalW, dt, now.UnixMilli()) {
									if tmm.Samples%10 == 0 {
										s.persistTwoMass(d.DriverName, tmm)
									}
								}
							}
						}
					}
				}
			}
		}
		s.mu.Unlock()
	}
}

// stovePauseMinConfidence is the model quality required before we'll reduce
// heat below the comfort target on the strength of the learned dynamics. No
// reduction happens on an unvalidated model.
const stovePauseMinConfidence = 0.4

// stoveDecision computes the setpoint for a zone while a free external heat
// source (wood stove, strong solar gain) is firing. It is strictly per-zone:
// the detector reads only this zone's own indoor temperature, so a stove in
// the living room never lowers the bathroom.
//
// The action is graded by how much we actually know, so we never make a
// "dumb" reduction:
//
//   - Always safe (no learning needed): suppress pre-heat and hold the zone's
//     comfort target. The stove keeps the room warm so electric stays off
//     naturally; we never let the room overcool.
//   - Deeper saving (let the room ride down toward MinC) ONLY when (a) the RC
//     model is trained enough to trust its inertia, (b) we've learned the
//     stove's typical firing energy/duration, and (c) an economic check shows
//     pausing is a net win — i.e. the cost to reheat once the fire ends (at
//     the forecast price then) is less than what we save now. If reheating
//     later is pricier than heating now, we hold comfort instead.
//
// Returns (setpoint, active, reason). active=false means no stove → caller
// proceeds with the normal planner/simple logic.
func (s *Service) stoveDecision(d Device, indoorC, comfortTargetC float64, outdoorC float64, nowMs int64) (float64, bool, string) {
	s.mu.RLock()
	det := s.stove[d.DriverName]
	tm := s.thermal[d.DriverName]
	s.mu.RUnlock()
	if det == nil || !det.Active(nowMs) {
		return 0, false, ""
	}
	// Safe default: hold comfort, suppress pre-heat. Never a reduction below
	// what comfort requires.
	setpoint := comfortTargetC
	reason := "stove active — pre-heat suppressed, comfort held"

	// Nothing deeper possible if we're already at the floor.
	if comfortTargetC <= d.MinC+1e-9 || tm == nil {
		return setpoint, true, reason
	}
	m := *tm

	// Gate the deeper reduction on LEARNING: trustworthy model + observed
	// firing cycles.
	if m.Quality() < stovePauseMinConfidence || det.Cycles < 1 ||
		det.EstThermalW <= 0 || det.AvgCycleWh <= 0 {
		return setpoint, true, reason + " (insufficient learning for deeper pause)"
	}

	// Estimate remaining firing time from learned per-cycle energy and power.
	sessionH := det.AvgCycleWh / det.EstThermalW
	elapsedH := float64(nowMs-det.FiringSinceMs()) / 3_600_000.0
	remainingH := sessionH - elapsedH
	if remainingH < 0.25 {
		return setpoint, true, reason + " (fire ending soon — hold comfort)"
	}

	// Gate the deeper reduction on CALCULATION: reheat cost vs saving,
	// discounted by any heat the loop is already storing (flow temp).
	cop := d.COP
	if cop <= 0 {
		cop = 1
	}
	flowC, hasFlow := s.readFlowTemp(d)
	worth, reasonCalc := s.pauseIsNetWin(m, comfortTargetC, outdoorC, cop, remainingH, nowMs,
		flowC, hasFlow, d.NominalFlowDeltaC)
	if !worth {
		return setpoint, true, reason + " (" + reasonCalc + ")"
	}
	return d.MinC, true, "stove active + trained + " + reasonCalc + " → deep pause to MinC"
}

// readFlowTemp returns the heat pump's supply (flow) temperature for a zone.
func (s *Service) readFlowTemp(d Device) (float64, bool) {
	if d.FlowMetric == "" {
		return 0, false
	}
	src := d.FlowDriver
	if src == "" {
		src = d.DriverName
	}
	v, _, ok := s.Tele.LatestMetric(src, d.FlowMetric)
	return v, ok
}

// reheatFactor scales reheat cost by how much usable heat the hydronic loop
// is already storing. A flow temperature well above the room (a full charge)
// means the loop can deliver heat without running the compressor, so reheat
// is nearly free (factor → 0). A flow temperature at/below the room means no
// stored heat is available and reheat costs the full compressor energy
// (factor → 1). With no flow signal we assume the worst (1) so the credit is
// never applied speculatively.
func reheatFactor(flowTempC, targetRoomC, nominalDeltaC float64, hasFlow bool) float64 {
	if !hasFlow {
		return 1.0
	}
	if nominalDeltaC <= 0 {
		nominalDeltaC = 15.0 // floor-heating default
	}
	headroom := flowTempC - targetRoomC
	if headroom <= 0 {
		return 1.0
	}
	f := 1.0 - headroom/nominalDeltaC
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return f
}

// pauseIsNetWin runs the economic cost-benefit for letting a zone ride down
// instead of holding its target, over a pause of pauseH hours. It compares
// the electricity saved now against the cost to reheat later, using the
// forecast price at the reheat moment. Captures the operator's floor-heating
// checklist: demand (HeatToHoldW vs outdoor), price now, inertia (the energy
// is integrated over the learned coast), COP, and the reheat price.
func (s *Service) pauseIsNetWin(m thermalmodel.Model, targetC, outdoorC, cop, pauseH float64, nowMs int64,
	flowTempC float64, hasFlow bool, nominalFlowDeltaC float64) (bool, string) {
	if s.PriceAt == nil || pauseH <= 0 {
		return false, "no price model"
	}
	now := time.UnixMilli(nowMs)
	priceNow, ok1 := s.PriceAt(now)
	priceReheat, ok2 := s.PriceAt(now.Add(time.Duration(pauseH * float64(time.Hour))))
	if !ok1 || !ok2 {
		return false, "no price signal"
	}
	// Thermal power we'd otherwise spend to hold the target against the
	// outdoor demand → electrical via COP → energy over the pause.
	holdThermalW := m.HeatToHoldW(targetC, outdoorC)
	avoidedKWh := (holdThermalW / cop) * pauseH / 1000.0
	if avoidedKWh <= 0 {
		return false, "no avoidable load"
	}
	// First-order energy balance: the heat allowed to escape during the pause
	// must be put back to return to target → roughly the same energy, billed
	// at the reheat-time price. The reheat is discounted by heat the loop is
	// already storing (high flow temp from the heat pump): a hot loop reheats
	// the room for nearly free, a cold loop pays the full compressor cost.
	rf := reheatFactor(flowTempC, targetC, nominalFlowDeltaC, hasFlow)
	savingOre := avoidedKWh * priceNow
	reheatOre := avoidedKWh * priceReheat * rf
	if savingOre <= reheatOre {
		return false, "reheat cost (flow-adjusted) not below saving"
	}
	if hasFlow && rf < 0.5 {
		return true, "loop holds heat (high flow temp) — reheat cheap"
	}
	return true, "reheat cheaper than now (save vs reheat positive)"
}

// replan rebuilds schedules from the current forecast and dispatches the
// directive for the slot covering "now" to each device.
func (s *Service) replan(ctx context.Context) {
	if s.Dispatch == nil {
		return
	}
	// Slots come from the MPC plan and may be empty (planner disabled or not
	// yet warmed up). Simple-mode thermostats run off PriceNow + the learned
	// model, so we must NOT bail here — only planner-mode thermostats and
	// deferrables actually need a slot curve, and they no-op gracefully
	// without one.
	var slots []PriceSlot
	if s.Slots != nil {
		slots = s.Slots()
	}
	now := time.Now()
	nowMs := now.UnixMilli()

	// Simple-mode thermostats are evaluated together so they can arbitrate
	// under the shared fuse budget.
	var simpleDevs []Device
	var simpleSpecs []SimpleSpec

	for _, d := range s.Devices {
		switch d.Type {
		case "thermostat":
			// External-heat (wood-stove) override takes precedence in every
			// mode — strictly per-zone and never a reduction below comfort
			// unless the economic check says it's a net win.
			ct := d.MinC
			if d.Mode == "simple" {
				ct = d.TargetC
				if ct == 0 {
					ct = (d.MinC + d.MaxC) / 2
				}
			}
			indoorForZone, ok := s.readIndoor(d)
			if !ok {
				indoorForZone = ct
			}
			outdoorForZone := 0.0
			if s.Outdoor != nil {
				outdoorForZone = s.Outdoor(nowMs)
			}
			if sp, active, reason := s.stoveDecision(d, indoorForZone, ct, outdoorForZone, nowMs); active {
				s.dispatchSetpoint(ctx, d, sp)
				slog.Debug("flexload stove override", "driver", d.DriverName, "setpoint_c", sp, "reason", reason)
				continue
			}
			if d.Mode == "simple" {
				spec, ok := s.buildSimpleSpec(d, slots, now)
				if ok {
					simpleDevs = append(simpleDevs, d)
					simpleSpecs = append(simpleSpecs, spec)
				}
				continue
			}
			s.replanThermostat(ctx, d, slots, nowMs)
		case "deferrable":
			s.replanDeferrable(ctx, d, slots, now, nowMs)
		}
	}

	if len(simpleSpecs) > 0 {
		decisions := make([]SimpleDecision, len(simpleSpecs))
		for i := range simpleSpecs {
			decisions[i] = EvaluateSimple(simpleSpecs[i])
		}
		ArbitrateSimple(decisions, simpleSpecs, s.FuseBudgetW)
		for i, d := range simpleDevs {
			action := d.SetpointAction
			if action == "" {
				action = "setpoint"
			}
			payload, _ := json.Marshal(map[string]any{"action": action, "value": decisions[i].SetpointC})
			if err := s.Dispatch(ctx, d.DriverName, payload); err != nil {
				slog.Warn("flexload simple dispatch failed", "driver", d.DriverName, "err", err)
			}
		}
	}
}

// buildSimpleSpec assembles the inputs for a simple-mode evaluation,
// resolving the current price (PriceNow hook → Slots fallback) and live
// indoor/outdoor temperatures.
func (s *Service) buildSimpleSpec(d Device, slots []PriceSlot, now time.Time) (SimpleSpec, bool) {
	indoorC, ok := s.readIndoor(d)
	if !ok {
		indoorC = (d.MinC + d.MaxC) / 2
		slog.Warn("flexload indoor sensor unavailable, using mid-band", "driver", d.DriverName, "assumed_c", indoorC)
	}
	outdoor := 0.0
	if s.Outdoor != nil {
		outdoor = s.Outdoor(now.UnixMilli())
	}
	// Current price: prefer the independent hook so simple mode works with
	// no MPC; fall back to the slot covering now.
	priceNow := 0.0
	if s.PriceAt != nil {
		if p, ok := s.PriceAt(now); ok {
			priceNow = p
		}
	}
	if priceNow == 0 && len(slots) > 0 {
		if p, ok := priceForNow(slots, now.UnixMilli()); ok {
			priceNow = p
		}
	}
	// Threshold: explicit fixed cutoff, else the 60th-percentile of the
	// horizon prices when a curve is available. With no price signal at all
	// the threshold stays 0, which makes "expensive" never fire, so simple
	// mode degrades safely to comfort-only.
	threshold := d.PriceThresholdOre
	if threshold <= 0 && len(slots) > 0 {
		threshold = priceQuantile(slots, 0.6)
	}

	s.mu.RLock()
	tm := s.thermal[d.DriverName]
	tmm := s.twomass[d.DriverName]
	s.mu.RUnlock()
	if tm == nil {
		return SimpleSpec{}, false
	}
	model := *tm

	bh := d.BlockHorizonH
	if bh <= 0 {
		bh = 1.0
	}
	target := d.TargetC
	if target == 0 {
		target = (d.MinC + d.MaxC) / 2
	}

	// Floor heating: when a slab reading is available, use the two-mass model
	// for both the coast estimate and the confidence gate — it knows the slab
	// keeps the room warm long after the element cuts out.
	coastOverride, hasOverride := 0.0, false
	confidence := model.Quality()
	if tmm != nil {
		if slabC, okSlab := s.readSlab(d); okSlab {
			coastOverride = tmm.CoastHoursToRoomTarget(indoorC, slabC, target, outdoor, 24*time.Hour)
			hasOverride = true
			confidence = tmm.Quality()
		}
	}

	return SimpleSpec{
		Model:            model,
		CurrentC:         indoorC,
		TargetC:          target,
		MinC:             d.MinC,
		Outdoor:          outdoor,
		PriceNow:         priceNow,
		PriceThreshold:   threshold,
		BlockHorizon:     time.Duration(bh * float64(time.Hour)),
		MaxHeatW:         d.MaxHeatW,
		COP:              d.COP,
		Confidence:       confidence, // gate blocking on learned confidence
		CoastOverrideH:   coastOverride,
		HasCoastOverride: hasOverride,
	}, true
}

// readIndoor returns the zone's measured indoor temperature, reading from a
// dedicated sensor driver (IndoorDriver) when configured, else the
// thermostat itself.
func (s *Service) readIndoor(d Device) (float64, bool) {
	src := d.IndoorDriver
	if src == "" {
		src = d.DriverName
	}
	if d.IndoorMetric == "" {
		return 0, false
	}
	v, _, ok := s.Tele.LatestMetric(src, d.IndoorMetric)
	return v, ok
}

// readSlab returns the zone's slab/floor temperature (floor probe or flow
// temp), used to drive the two-mass floor-heating model.
func (s *Service) readSlab(d Device) (float64, bool) {
	if d.SlabMetric == "" {
		return 0, false
	}
	src := d.SlabDriver
	if src == "" {
		src = d.DriverName
	}
	v, _, ok := s.Tele.LatestMetric(src, d.SlabMetric)
	return v, ok
}

// priceForNow returns the price of the slot covering nowMs.
func priceForNow(slots []PriceSlot, nowMs int64) (float64, bool) {
	for i, sl := range slots {
		var end int64
		if i+1 < len(slots) {
			end = slots[i+1].StartMs
		} else {
			end = sl.StartMs + int64(sl.LenMin)*60_000
		}
		if nowMs >= sl.StartMs && nowMs < end {
			return sl.PriceOre, true
		}
	}
	// now is past all slots — return the most recent slot price as the best
	// available stale estimate (slots[0] would be the oldest, which is worse).
	if len(slots) > 0 {
		return slots[len(slots)-1].PriceOre, true
	}
	return 0, false
}

func (s *Service) replanThermostat(ctx context.Context, d Device, slots []PriceSlot, nowMs int64) {
	// Stove override is handled by the caller (replan loop) before this is
	// reached.
	indoorC, ok := s.readIndoor(d)
	if !ok {
		// No live temperature → assume mid-band so the scheduler still
		// produces a sane setpoint rather than skipping the zone.
		indoorC = (d.MinC + d.MaxC) / 2
	}
	s.mu.RLock()
	tm := s.thermal[d.DriverName]
	s.mu.RUnlock()
	if tm == nil {
		return
	}
	model := *tm

	spec := ThermalSpec{
		DriverName:      d.DriverName,
		Model:           model,
		CurrentC:        indoorC,
		MinC:            d.MinC,
		MaxC:            d.MaxC,
		MaxHeatW:        d.MaxHeatW,
		COP:             d.COP,
		Outdoor:         s.Outdoor,
		PreHeatFraction: d.PreHeatFraction,
	}
	if spec.Outdoor == nil {
		spec.Outdoor = func(int64) float64 { return 0 }
	}
	sched := PlanThermal(slots, spec)
	sp, ok := currentThermalSetpoint(sched, nowMs)
	if !ok {
		return
	}
	s.dispatchSetpoint(ctx, d, sp.TargetC)
}

// dispatchSetpoint writes a setpoint to a thermostat via its configured
// command action (default "setpoint").
func (s *Service) dispatchSetpoint(ctx context.Context, d Device, targetC float64) {
	action := d.SetpointAction
	if action == "" {
		action = "setpoint"
	}
	payload, _ := json.Marshal(map[string]any{"action": action, "value": targetC})
	if err := s.Dispatch(ctx, d.DriverName, payload); err != nil {
		slog.Warn("flexload thermostat dispatch failed", "driver", d.DriverName, "err", err)
	}
}

func (s *Service) replanDeferrable(ctx context.Context, d Device, slots []PriceSlot, now time.Time, nowMs int64) {
	earliest, deadline := dailyWindow(now, d.EarliestHour, d.DeadlineHour)
	// Fall back to the learned plug profile for power / energy the operator
	// didn't pin, so a spa vs. a water heater self-calibrate.
	powerW, energyWh := d.PowerW, d.EnergyWh
	if p, ok := s.PlugProfileFor(d.DriverName); ok {
		powerW = p.EffectivePowerW(d.PowerW)
		energyWh = p.EffectiveEnergyWh(d.EnergyWh)
	}
	// Cold-start guard: a metered plug with nothing configured can't be
	// scheduled until it has learned its load — but it can only learn by
	// running. Sending "off" now would lock it off forever (never runs →
	// never learns → never runs). So while the profile is still blank, leave
	// the appliance on its own native control (dispatch nothing) and just
	// keep observing until RunningW / DailyEnergyWh are known.
	if powerW <= 0 || energyWh <= 0 {
		if d.PowerMetric != "" {
			return // observe-only until learned
		}
		// No metering and nothing configured → nothing to schedule.
		return
	}
	spec := DeferrableSpec{
		DriverName: d.DriverName,
		EnergyWh:   energyWh,
		PowerW:     powerW,
		EarliestMs: earliest,
		DeadlineMs: deadline,
		PreferPV:   d.PreferPV,
	}
	sched := PlanDeferrable(slots, spec)
	on, ok := currentDeferrableState(sched, nowMs)
	if !ok {
		return
	}
	action := d.OffAction
	if on {
		action = d.OnAction
	}
	if action == "" {
		return // nothing to send (e.g. no off_action configured)
	}
	payload, _ := json.Marshal(map[string]any{"action": action})
	if err := s.Dispatch(ctx, d.DriverName, payload); err != nil {
		slog.Warn("flexload deferrable dispatch failed", "driver", d.DriverName, "err", err)
	}
}

// currentThermalSetpoint returns the setpoint for the slot covering nowMs.
func currentThermalSetpoint(sched ThermalSchedule, nowMs int64) (ThermalSetpoint, bool) {
	for i, sp := range sched.Setpoints {
		var end int64
		if i+1 < len(sched.Setpoints) {
			end = sched.Setpoints[i+1].StartMs
		} else {
			end = sp.StartMs + 3600_000 // assume ≤1h tail
		}
		if nowMs >= sp.StartMs && nowMs < end {
			return sp, true
		}
	}
	// Before the first slot starts, use the first.
	if len(sched.Setpoints) > 0 && nowMs < sched.Setpoints[0].StartMs {
		return sched.Setpoints[0], true
	}
	return ThermalSetpoint{}, false
}

func currentDeferrableState(sched DeferrableSchedule, nowMs int64) (on bool, ok bool) {
	for i, sl := range sched.Slots {
		var end int64
		if i+1 < len(sched.Slots) {
			end = sched.Slots[i+1].StartMs
		} else {
			end = sl.StartMs + 3600_000
		}
		if nowMs >= sl.StartMs && nowMs < end {
			return sl.On, true
		}
	}
	if len(sched.Slots) > 0 && nowMs < sched.Slots[0].StartMs {
		return sched.Slots[0].On, true
	}
	return false, false
}

// dailyWindow resolves earliest/deadline hour-of-day into absolute ms
// around now. Returns (0,0) when both hours are 0 (no window). If the
// deadline hour has already passed today, it rolls to tomorrow.
func dailyWindow(now time.Time, earliestHour, deadlineHour int) (earliestMs, deadlineMs int64) {
	if earliestHour == 0 && deadlineHour == 0 {
		return 0, 0
	}
	loc := now.Location()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if earliestHour > 0 {
		earliestMs = startOfDay.Add(time.Duration(earliestHour) * time.Hour).UnixMilli()
	}
	if deadlineHour > 0 {
		dl := startOfDay.Add(time.Duration(deadlineHour) * time.Hour)
		if !dl.After(now) {
			dl = dl.Add(24 * time.Hour) // already past today → tomorrow
		}
		deadlineMs = dl.UnixMilli()
	}
	return earliestMs, deadlineMs
}

func (s *Service) persist(driver string, m *thermalmodel.Model) {
	if s.Store == nil {
		return
	}
	js, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := s.Store.SaveConfig(thermalStateKeyPrefix+driver, string(js)); err != nil {
		slog.Warn("flexload thermal persist failed", "driver", driver, "err", err)
	}
}

func (s *Service) persistPlug(driver string, p *PlugProfile) {
	if s.Store == nil {
		return
	}
	js, err := json.Marshal(p)
	if err != nil {
		return
	}
	if err := s.Store.SaveConfig(plugStateKeyPrefix+driver, string(js)); err != nil {
		slog.Warn("flexload plug persist failed", "driver", driver, "err", err)
	}
}

func (s *Service) persistTwoMass(driver string, m *thermalmodel.TwoMass) {
	if s.Store == nil {
		return
	}
	js, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := s.Store.SaveConfig(twoMassStateKeyPrefix+driver, string(js)); err != nil {
		slog.Warn("flexload two-mass persist failed", "driver", driver, "err", err)
	}
}

func (s *Service) persistStove(driver string, det *ExternalHeatDetector) {
	if s.Store == nil {
		return
	}
	js, err := json.Marshal(det)
	if err != nil {
		return
	}
	if err := s.Store.SaveConfig(stoveStateKeyPrefix+driver, string(js)); err != nil {
		slog.Warn("flexload stove persist failed", "driver", driver, "err", err)
	}
}

func (s *Service) persistAll() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for driver, m := range s.thermal {
		s.persist(driver, m)
	}
	for driver, m := range s.twomass {
		s.persistTwoMass(driver, m)
	}
	for driver, p := range s.plug {
		s.persistPlug(driver, p)
	}
	for driver, det := range s.stove {
		s.persistStove(driver, det)
	}
}

// DevicesFromConfig is a small adapter so main.go doesn't import this
// package's internals just to translate config. It is implemented in
// main.go's wiring; this comment documents the expected mapping:
//
//	config.FlexLoad{Type, DriverName, MinC, MaxC, ...} → flexload.Device{...}
var _ = fmt.Sprintf
