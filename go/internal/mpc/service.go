package mpc

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// PVPredictor lets the MPC plug in a learned PV predictor (the digital
// twin) without importing its package. Implemented by
// *pvmodel.Service.Predict. Leave nil to use the naive forecast stored
// in the DB.
type PVPredictor func(t time.Time, cloudPct float64) float64

// LoadPredictor plugs in a learned load predictor. Implemented by
// *loadmodel.Service.Predict. Leave nil to fall back to Service.BaseLoad.
type LoadPredictor func(t time.Time) float64

// PricePredictor fills in spot price for future slots that the day-ahead
// source hasn't published yet. Implemented by
// *priceforecast.Service.Predict. Returns ÖRE/kWh spot (no tariff/VAT).
// Leave nil to cap the plan horizon at what's been published.
type PricePredictor func(zone string, t time.Time) float64

// LoadpointProbe returns the EV loadpoint state the DP should extend
// itself with. Called once per replan with the slot length (minutes)
// the DP will actually use — so the probe can map any user wall-clock
// deadline to a correct slot index. Return nil when no loadpoint
// should be optimized (unplugged, unconfigured, or during initial
// rollout when operator wants to disable EV-in-DP).
//
// Wired in main.go against *loadpoint.Manager; kept as a plain
// closure type to avoid the mpc package importing loadpoint (which
// would risk a cycle if loadpoint ever needs mpc types).
type LoadpointProbe func(slotLenMin int) *LoadpointSpec

// Service wires the optimizer to the rest of the stack: pulls prices +
// forecast from the SQLite store, reads current SoC from the telemetry
// store, and re-plans on a ticker. The latest plan is cached.
type Service struct {
	Store    *state.Store
	Tele     *telemetry.Store
	Zone     string
	BaseLoad float64 // baseline household load (W). 0 disables load assumption.
	Horizon  time.Duration
	Interval time.Duration
	PV        PVPredictor    // optional — overrides stored pv_w_estimated
	Load      LoadPredictor  // optional — overrides flat BaseLoad
	Price     PricePredictor // optional — fills in future slots when day-ahead isn't published yet
	Loadpoint LoadpointProbe // optional — when non-nil, the DP extends its state with EV dimensions

	// SaveDiag is called synchronously after every successful replan
	// with the same Diagnostic the /api/mpc/diagnose endpoint would
	// return + the trigger reason ("scheduled" / "reactive-pv" /
	// "reactive-load" / "manual"). Nil disables persistence — the
	// in-memory diagnose still works. Wired in main.go against
	// state.Store.SaveDiagnostic so operators can time-travel past
	// decisions; see docs/mpc-planner.md.
	SaveDiag func(d *Diagnostic, reason string) error

	// Reactive replan: when the integrated energy gap between actual
	// and the plan's current-slot prediction exceeds a threshold over
	// a rolling ~15-minute window, trigger an off-schedule replan so
	// the schedule catches up with reality.
	//
	// Why energy, not power: a brief cloud shadow or a momentary load
	// spike both swing instantaneous power by kW but represent
	// pennies of shifted energy. Arbitrage decisions depend on
	// kWh-scale drift, not W-scale noise. Integrating over a window
	// filters the transients and keeps us honest.
	ReactiveInterval time.Duration // how often to check (default 10s)
	MinReplanGap     time.Duration // cooldown between reactive replans (default 60s)
	PVDivergenceWh   float64       // |integrated gap|; 0 disables (default 500 Wh)
	LoadDivergenceWh float64       // |integrated gap|; 0 disables (default 400 Wh)

	// Leaky integrals (Wh) of (actual − predicted) over the last
	// ~WindowMin minutes. Decayed each tick so old divergence fades.
	pvErrIntWh   float64
	loadErrIntWh float64
	lastTickMs   int64

	// SiteMeter is the driver name whose meter reading represents the
	// site's grid connection. Used by the reactive-replan check to
	// derive actual load = grid − pv − bat. Empty = skip load check.
	SiteMeter string

	// FuseMaxW is the site's grid fuse ceiling (W). When > 0, every slot
	// passed to Optimize gets `Limits.MaxImportW = FuseMaxW`, so the DP
	// joint-plans battery + EV in a way that respects the fuse from the
	// start — battery charge + EV charge + house net can't exceed this.
	// Without this, the DP can prescribe (battery_charge + EV_charge)
	// totals that bust the fuse, and dispatch has to scale them at
	// execution time. Wired from main.go (cfg.Fuse → fuseMaxW).
	FuseMaxW float64

	lastReplanAt time.Time
	lastReason   string // "scheduled" | "reactive-pv" | "reactive-load" | "manual"

	// ExportBonusOreKwh and ExportFeeOreKwh flow in from config.Price.
	// Used to compute default ExportOrePerKWh when Params doesn't set it.
	ExportBonusOreKwh float64
	ExportFeeOreKwh   float64

	// ExportFloorOreKwh, when non-nil, clamps the per-slot export ore
	// at the floor. Wired from config.Price.ExportFloorOreKwh; nil =
	// no clamp, real spot pass-through (default).
	ExportFloorOreKwh *float64

	// GridTariffOreKwh and VATPercent let the MPC turn forecast spot
	// prices into consumer-total prices when back-filling future slots
	// using s.Price. Mirrors prices.Applier semantics.
	GridTariffOreKwh float64
	VATPercent       float64

	Defaults Params

	mu              sync.RWMutex
	last            *Plan
	lastSlots       []Slot // inputs that went into the most recent Optimize call
	lastParams      Params // params that went into the most recent Optimize call
	lastLoadpointID string // ID of the loadpoint active in the most recent plan (empty = none)

	stop chan struct{}
	done chan struct{}
}

// New constructs a service. Caller wires it in main.go after store + telemetry.
func New(st *state.Store, tl *telemetry.Store, zone string, p Params) *Service {
	return &Service{
		Store:            st,
		Tele:             tl,
		Zone:             zone,
		Defaults:         p,
		Horizon:          48 * time.Hour, // always plan 48h — forecaster fills beyond day-ahead
		Interval:         15 * time.Minute,
		ReactiveInterval: 10 * time.Second,
		MinReplanGap:     60 * time.Second,
		PVDivergenceWh:   500, // 500 Wh sustained gap over ~15 min
		LoadDivergenceWh: 400,
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
	}
}

// UpdateCapacity swaps the aggregate battery capacity + charge/discharge
// bounds on the active planner. Called from the config-reload path when
// the operator adds or removes a driver (or promotes/demotes an EV
// loadpoint) and the MPC battery pool changes. Without this, the
// planner would keep optimising against its startup-time capacity
// snapshot while the dispatch layer already saw the new numbers — the
// plan's SoC% and terminal credit would drift from reality until the
// next process restart. Codex P1 on PR #121.
//
// Caller is expected to pass the same totals buildMPC would have
// computed from the new config: totalCap across battery drivers,
// aggregate max charge/discharge clamped to fuse capacity.
func (s *Service) UpdateCapacity(totalCapWh, maxChargeW, maxDischargeW float64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Defaults.CapacityWh = totalCapWh
	s.Defaults.MaxChargeW = maxChargeW
	s.Defaults.MaxDischargeW = maxDischargeW
	s.mu.Unlock()
}

// Latest returns the most recently computed plan (nil before first run).
func (s *Service) Latest() *Plan {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

// MaxPlanAge is the staleness cutoff. Once a plan's `generated_at_ms`
// is older than this, we consider it stale and the control loop falls
// back to self_consumption. Picked to be ~2× the replan interval so a
// single missed replan doesn't flip us into fallback.
const MaxPlanAge = 30 * time.Minute

// SlotDirective is the plan's per-slot instruction to the EMS under the
// energy-allocation contract (see docs/plan-ems-contract.md). The plan
// allocates total battery energy (Wh, site-signed) for the slot; the
// EMS converts to instantaneous power each tick from remaining energy
// and remaining time. Grid flow is the residual — no PI target on it.
//
// Signed convention matches Action.BatteryW: positive = charge, negative
// = discharge. Magnitude is the total energy expected to move into (or
// out of) the battery fleet across the slot.
type SlotDirective struct {
	SlotStart       time.Time
	SlotEnd         time.Time
	BatteryEnergyWh float64 // total energy for the slot (site-signed)
	SoCTargetPct    float64 // plan's SoC at SlotEnd — used by divergence detector
	Strategy        Mode    // echoed for logging + API

	// PVLimitW is the recommended cap on aggregate PV inverter output
	// for this slot (W, positive). 0 means "no curtailment". Set by
	// annotateCurtailment when exporting at zero / negative revenue
	// would lose money — the dispatch layer divides this across the
	// site's PV-supporting drivers and sends `curtail` commands.
	PVLimitW float64

	// LoadpointEnergyWh carries per-loadpoint EV energy budgets for
	// this slot. Keyed by Loadpoint.ID. Positive = charging energy
	// the plan allocated. Empty map when no loadpoints are
	// configured / active. The dispatch layer converts energy to
	// instantaneous power via the same `remaining_wh × 3600 /
	// remaining_s` formula it uses for the battery.
	LoadpointEnergyWh map[string]float64

	// LoadpointSoCTargetPct is the plan's EV SoC at SlotEnd per
	// loadpoint. Used by per-loadpoint divergence check in Phase 4.1.
	LoadpointSoCTargetPct map[string]float64
}

// SlotDirectiveAt returns the energy-allocation directive for the slot
// containing `now`. Non-breaking companion to SlotAt — the control loop
// switches to this when the new dispatch path is enabled; SlotAt stays
// for the legacy grid-target path until that's retired.
func (s *Service) SlotDirectiveAt(now time.Time) (SlotDirective, bool) {
	if s == nil {
		return SlotDirective{}, false
	}
	// Snapshot plan + loadpoint ID together under the same RLock so
	// a concurrent replan() can't swap one without the other — a
	// classic read-race that Codex flagged.
	s.mu.RLock()
	p := s.last
	lpID := s.lastLoadpointID
	s.mu.RUnlock()
	if p == nil {
		return SlotDirective{}, false
	}
	if time.Since(time.UnixMilli(p.GeneratedAtMs)) > MaxPlanAge {
		return SlotDirective{}, false
	}
	nowMs := now.UnixMilli()
	for _, a := range p.Actions {
		slotLenMs := int64(a.SlotLenMin) * 60 * 1000
		endMs := a.SlotStartMs + slotLenMs
		if nowMs < a.SlotStartMs || nowMs >= endMs {
			continue
		}
		// energy_wh = power_w * hours. a.SlotLenMin/60 gives hours.
		energyWh := a.BatteryW * float64(a.SlotLenMin) / 60.0
		d := SlotDirective{
			SlotStart:       time.UnixMilli(a.SlotStartMs),
			SlotEnd:         time.UnixMilli(endMs),
			BatteryEnergyWh: energyWh,
			SoCTargetPct:    a.SoCPct,
			Strategy:        s.Defaults.Mode,
			PVLimitW:        a.PVLimitW,
		}
		// EV energy budget for the slot (single-loadpoint for now —
		// keyed under lpID snapshot so the dispatch layer routes
		// to the right driver).
		if a.LoadpointW > 0 && lpID != "" {
			lpEnergyWh := a.LoadpointW * float64(a.SlotLenMin) / 60.0
			d.LoadpointEnergyWh = map[string]float64{
				lpID: lpEnergyWh,
			}
			d.LoadpointSoCTargetPct = map[string]float64{
				lpID: a.LoadpointSoCPct,
			}
		}
		return d, true
	}
	return SlotDirective{}, false
}

// SlotAt returns the plan's directive for the slot containing `now`.
// Returns (mode, grid_target_w, ok). Dispatch uses `mode` to select
// the EMS strategy and `grid_target_w` as the PI setpoint. The plan is
// a scheduler (decides WHEN); the EMS is the regulator (decides HOW).
//
// Legacy — the new path uses SlotDirectiveAt. See docs/plan-ems-contract.md.
func (s *Service) SlotAt(now time.Time) (string, float64, bool) {
	if s == nil {
		return "", 0, false
	}
	s.mu.RLock()
	p := s.last
	s.mu.RUnlock()
	if p == nil {
		return "", 0, false
	}
	if time.Since(time.UnixMilli(p.GeneratedAtMs)) > MaxPlanAge {
		return "", 0, false
	}
	nowMs := now.UnixMilli()
	for _, a := range p.Actions {
		end := a.SlotStartMs + int64(a.SlotLenMin)*60*1000
		if nowMs >= a.SlotStartMs && nowMs < end {
			return actionToSlot(a, s.Defaults.Mode)
		}
	}
	return "", 0, false
}

// actionToSlot translates an MPC action into (mode_string, grid_target_w, true).
// The mapping from planner-mode + action to EMS mode:
//   - self_consumption → always self_consumption with grid_target=0
//   - cheap_charge → "charge" when the plan says charge, otherwise self_consumption
//   - arbitrage → "charge" / "self_consumption" (with negative grid target for export) / self_consumption
func actionToSlot(a Action, plannerMode Mode) (string, float64, bool) {
	switch plannerMode {
	case ModeSelfConsumption:
		return "self_consumption", 0, true
	case ModeCheapCharge:
		if a.BatteryW > 100 {
			return "charge", 0, true
		}
		return "self_consumption", 0, true
	case ModeArbitrage:
		if a.BatteryW > 100 {
			return "charge", 0, true
		}
		if a.BatteryW < -100 {
			// Planned discharge-to-export: use self_consumption with a
			// negative grid target so the PI actively drives grid negative
			// (i.e. discharges batteries to export). peak_shaving doesn't
			// work here because it only reacts to over-peak import and
			// won't push the grid into export territory.
			return "self_consumption", a.GridW, true
		}
		return "self_consumption", 0, true
	default:
		return "self_consumption", 0, true
	}
}

// SetMode changes the planner's operating mode and forces an immediate
// replan so the new mode takes effect within one control cycle.
func (s *Service) SetMode(ctx context.Context, mode Mode) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Defaults.Mode = mode
	s.mu.Unlock()
	s.replan(ctx)
}

// Start runs the planner in a goroutine. Does an initial plan immediately.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go s.loop(ctx)
}

// Stop terminates the planner.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	s.lastReason = "scheduled"
	// Block on real battery SoC before the initial replan. The
	// fallback (Params.InitialSoCPct, default 50%) produces a plan
	// that doesn't match reality, and nothing replans on SoC drift —
	// once the bogus plan is live it stays live until the next
	// Interval tick (15 min default), or until PV/load divergence
	// integrators cross threshold (which can take much longer). The
	// service is only constructed when at least one battery is
	// configured (see buildMPC), so waiting is correct rather than
	// risky. Warn periodically if the wait runs long so operators
	// notice a stuck driver.
	if !s.waitForRealSoC(ctx) {
		return
	}
	s.replan(ctx)
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	var reactiveTick <-chan time.Time
	if s.ReactiveInterval > 0 && (s.PVDivergenceWh > 0 || s.LoadDivergenceWh > 0) {
		rt := time.NewTicker(s.ReactiveInterval)
		defer rt.Stop()
		reactiveTick = rt.C
	}
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s.lastReason = "scheduled"
			s.replan(ctx)
		case <-reactiveTick:
			s.checkDivergence(ctx)
		}
	}
}

// checkDivergence compares live PV + load to what the current slot of
// the cached plan expected. If the gap exceeds thresholds AND the
// cooldown has elapsed, trigger an off-schedule replan so the plan
// catches up with reality.
func (s *Service) checkDivergence(ctx context.Context) {
	s.mu.RLock()
	plan := s.last
	last := s.lastReplanAt
	s.mu.RUnlock()
	if plan == nil || len(plan.Actions) == 0 {
		return
	}
	if time.Since(last) < s.MinReplanGap {
		return
	}
	// Find the slot covering now.
	nowMs := time.Now().UnixMilli()
	var slot *Action
	for i := range plan.Actions {
		a := &plan.Actions[i]
		end := a.SlotStartMs + int64(a.SlotLenMin)*60*1000
		if nowMs >= a.SlotStartMs && nowMs < end {
			slot = a
			break
		}
	}
	if slot == nil {
		return
	}
	// Live PV — sum all DerPV readings (site sign: negative = generating).
	var pvW float64
	for _, r := range s.Tele.ReadingsByType(telemetry.DerPV) {
		pvW += r.SmoothedW
	}

	// Live load = grid − pv − bat when we have a site meter wired.
	var loadW float64
	haveLoad := false
	if s.SiteMeter != "" {
		if m := s.Tele.Get(s.SiteMeter, telemetry.DerMeter); m != nil {
			var batW float64
			for _, r := range s.Tele.ReadingsByType(telemetry.DerBattery) {
				batW += r.SmoothedW
			}
			evW := s.Tele.SumOnlineEVW()
			// House-only load: subtract EV so the divergence detector
			// compares actual house consumption against the plan's
			// house-load forecast, not a moving "house + EV" target.
			loadW = m.SmoothedW - pvW - batW - evW
			if loadW < 0 {
				loadW = 0
			}
			haveLoad = true
		}
	}

	// Leaky integral of energy error (Wh). Decay with a 15-minute
	// half-life so transients fade but a sustained offset accumulates.
	// decay = 0.5^(dt/halflife), halflife = 900s.
	const halflifeS = 900.0
	tickMs := time.Now().UnixMilli()
	dtS := 0.0
	if s.lastTickMs > 0 {
		dtS = float64(tickMs-s.lastTickMs) / 1000.0
	}
	s.lastTickMs = tickMs
	decay := 1.0
	if dtS > 0 {
		decay = math.Pow(0.5, dtS/halflifeS)
	}
	dtH := dtS / 3600.0
	pvErrW := pvW - slot.PVW
	s.mu.Lock()
	s.pvErrIntWh = s.pvErrIntWh*decay + pvErrW*dtH
	if haveLoad {
		loadErrW := loadW - slot.LoadW
		s.loadErrIntWh = s.loadErrIntWh*decay + loadErrW*dtH
	} else {
		s.loadErrIntWh *= decay
	}
	pvInt := s.pvErrIntWh
	loadInt := s.loadErrIntWh
	s.mu.Unlock()

	reason := ""
	switch {
	case s.PVDivergenceWh > 0 && math.Abs(pvInt) > s.PVDivergenceWh:
		reason = "reactive-pv"
	case s.LoadDivergenceWh > 0 && math.Abs(loadInt) > s.LoadDivergenceWh:
		reason = "reactive-load"
	}
	if reason == "" {
		return
	}
	slog.Info("mpc: reactive replan",
		"reason", reason,
		"pv_err_wh", pvInt, "loadint_wh", loadInt,
		"pv_w_now", pvW, "plan_pv_w", slot.PVW,
		"load_w_now", loadW, "plan_load_w", slot.LoadW)
	s.lastReason = reason
	// Reset integrals after triggering so we don't immediately re-fire.
	s.mu.Lock()
	s.pvErrIntWh = 0
	s.loadErrIntWh = 0
	s.mu.Unlock()
	s.replan(ctx)
}

// Replan recomputes the plan once using current prices + forecast + SoC.
// Exposed for tests and API triggers.
func (s *Service) Replan(ctx context.Context) *Plan { return s.replan(ctx) }

// ReplanWithReason is Replan with an explicit reason string that lands
// in slog + the diagnose snapshot. Use it when an external event (API
// mutation, settings change, mode flip) forces a replan — the default
// "scheduled" reason loses that provenance, which makes time-travel
// debugging harder when the operator asks "why did the plan change at
// 12:34?". Reasons should be short kebab-style, e.g.
// "surplus_only_disabled", "target_soc_changed", "mode_changed".
func (s *Service) ReplanWithReason(ctx context.Context, reason string) *Plan {
	if reason != "" {
		s.mu.Lock()
		s.lastReason = reason
		s.mu.Unlock()
	}
	return s.replan(ctx)
}

func (s *Service) replan(_ context.Context) *Plan {
	now := time.Now()
	untilMs := now.Add(s.Horizon).UnixMilli()
	sinceMs := now.UnixMilli() - 15*60*1000 // small margin — slot starting ≤15min ago still in-flight

	prices, err := s.Store.LoadPrices(s.Zone, sinceMs, untilMs)
	if err != nil {
		slog.Warn("mpc: load prices", "err", err)
		return nil
	}
	// Extend prices into the horizon using the learned forecast when
	// the day-ahead source hasn't published that far yet. Otherwise
	// the plan silently truncates the moment we pass the published
	// cutoff — operators lose overnight planning exactly when they'd
	// most want it.
	if s.Price != nil {
		prices = extendPricesWithForecast(prices, s.Zone, s.Price,
			now.UnixMilli(), untilMs, s.GridTariffOreKwh, s.VATPercent)
	}
	if len(prices) == 0 {
		slog.Info("mpc: no prices available yet")
		return nil
	}

	forecasts, err := s.Store.LoadForecasts(sinceMs, untilMs)
	if err != nil {
		slog.Warn("mpc: load forecasts", "err", err)
		// continue without PV forecast
	}

	slots := buildSlots(prices, forecasts, s.BaseLoad, now.UnixMilli(), s.PV, s.Load)
	if len(slots) == 0 {
		return nil
	}

	// Plumb the site fuse into per-slot limits so the DP joint-plans
	// battery + EV under the fuse constraint instead of producing plans
	// that dispatch then has to scale at execution time. The DP already
	// honours Slot.Limits.MaxImportW (mpc.go:450); we just feed it.
	if s.FuseMaxW > 0 {
		for i := range slots {
			if slots[i].Limits.MaxImportW <= 0 || slots[i].Limits.MaxImportW > s.FuseMaxW {
				slots[i].Limits.MaxImportW = s.FuseMaxW
			}
		}
	}

	// Current SoC: average of battery readings (weighted by capacity is
	// ideal, but for v1 we aggregate into one "mega-battery" so a mean
	// across whatever batteries are reporting is fine).
	p := s.Defaults
	p.InitialSoCPct = currentSoCPct(s.Tele, p.InitialSoCPct)

	// Export pricing is per-slot now: pass bonus/fee into Params so
	// the DP can compute `slot.SpotOre + bonus − fee` per slot. Leave
	// p.ExportOrePerKWh at 0 (operators can still set it via Params
	// to force a flat feed-in tariff).
	p.ExportBonusOreKwh = s.ExportBonusOreKwh
	p.ExportFeeOreKwh = s.ExportFeeOreKwh
	p.ExportFloorOreKwh = s.ExportFloorOreKwh

	// Default terminal valuation. Mode-dependent because self-consumption
	// is a constrained game: the battery can only offset local load, not
	// export, so stored energy is worth what it SAVES on future import
	// (retail) MINUS what you'd otherwise have earned exporting surplus
	// PV into the grid. Using the full retail import price as the terminal
	// value overvalues SoC by the export rate, so the DP always picks
	// "idle, import to cover load" over "discharge now, refill from PV
	// tomorrow" (because discharging loses η_rt while the extra retail-
	// priced terminal credit is never realised).
	terminalDefaulted := false
	if p.TerminalSoCPrice <= 0 {
		terminalDefaulted = true
		switch p.Mode {
		case ModeSelfConsumption, ModeCheapCharge:
			p.TerminalSoCPrice = selfConsumptionTerminalPrice(prices,
				s.ExportBonusOreKwh, s.ExportFeeOreKwh)
		default:
			// Arbitrage: battery can export, so full import price is the
			// right upper bound on SoC value.
			var sum float64
			for _, pr := range prices {
				sum += pr.TotalOreKwh
			}
			p.TerminalSoCPrice = sum / float64(len(prices))
		}
	}

	// Loadpoint extension: if a probe is wired AND returns an active
	// spec, the DP adds an EV SoC dimension and produces per-slot
	// LoadpointW decisions. One loadpoint at a time — multi-LP
	// support is on the roadmap.
	var loadpointID string
	if s.Loadpoint != nil {
		slotLenMin := 60 // safe fallback — most price sources are hourly
		if len(slots) > 0 && slots[0].LenMin > 0 {
			slotLenMin = slots[0].LenMin
		}
		if spec := s.Loadpoint(slotLenMin); spec != nil && spec.PluggedIn {
			p.Loadpoint = spec
			loadpointID = spec.ID
		}
	}

	// Surplus-only LP override: when an EV is connected to a surplus-
	// only loadpoint, the battery is forbidden from grid-charging
	// (mpc.go feasibility). The default arbitrage terminal credit
	// (mean retail import price across the horizon) then becomes
	// misleading — it tells the DP "stored energy is worth full
	// retail" while the only realistic discharge path is local
	// self-consumption (battery → house, battery → EV via the still-
	// allowed PV-only charge). Re-evaluate the terminal credit using
	// the self-consumption formula so the planner stops chasing a
	// reward it can no longer earn through grid arbitrage. Only
	// applies when we just defaulted above; an explicit caller-
	// supplied TerminalSoCPrice is respected.
	if terminalDefaulted && p.Loadpoint != nil && p.Loadpoint.SurplusOnly &&
		p.Mode != ModeSelfConsumption && p.Mode != ModeCheapCharge {
		p.TerminalSoCPrice = selfConsumptionTerminalPrice(prices,
			s.ExportBonusOreKwh, s.ExportFeeOreKwh)
	}

	slog.Info("mpc: optimize params",
		"mode", p.Mode,
		"terminal_ore", p.TerminalSoCPrice,
		"max_charge_w", p.MaxChargeW,
		"max_discharge_w", p.MaxDischargeW,
		"capacity_wh", p.CapacityWh,
		"soc_levels", p.SoCLevels,
		"action_levels", p.ActionLevels,
		"soc_start", p.InitialSoCPct,
		"loadpoint_active", p.Loadpoint != nil,
		"loadpoint_id", loadpointID,
	)
	plan := Optimize(slots, p)

	// Tag each action with the effective EMS mode so the UI can render
	// a mode-band showing which strategy drives each slot.
	for i := range plan.Actions {
		mode, _, _ := actionToSlot(plan.Actions[i], p.Mode)
		plan.Actions[i].EMSMode = mode
	}

	// Baselines — counter-factual dispatch costs over the same horizon
	// so the UI can show savings-vs-X numbers. Skip when already in
	// self-consumption mode: the SC baseline is the plan itself, which
	// makes the badge trivially zero and distracts from the price
	// signal. For SC runs the UI still has the plan cost on its own.
	if p.Mode != ModeSelfConsumption {
		bl := ComputeBaselines(slots, p)
		plan.Baselines = &bl
	}

	s.mu.Lock()
	s.last = &plan
	s.lastSlots = slots
	s.lastParams = p
	s.lastLoadpointID = loadpointID
	s.lastReplanAt = time.Now()
	reason := s.lastReason
	if reason == "" {
		reason = "manual"
	}
	replanAtMs := s.lastReplanAt.UnixMilli()
	s.mu.Unlock()
	// Horizon statistics — surfaced in logs so operators can
	// reconstruct "what did the DP know?" without pulling the full
	// Diagnostic JSON. Captures the three factors most likely to
	// explain a surprising decision: mean price level, mean data
	// confidence (how much of the horizon is forecast vs day-ahead),
	// and the capacity envelope.
	var sumPrice, sumConf float64
	for i := range slots {
		sumPrice += slots[i].PriceOre
		c := slots[i].Confidence
		if c <= 0 {
			c = 1.0
		}
		sumConf += c
	}
	var meanPrice, meanConf float64
	if n := len(slots); n > 0 {
		meanPrice = sumPrice / float64(n)
		meanConf = sumConf / float64(n)
	}
	slog.Info("mpc: replanned",
		"slots", len(slots),
		"soc_start", p.InitialSoCPct,
		"cost_ore", plan.TotalCostOre,
		"reason", reason,
		"mean_price_ore", meanPrice,
		"mean_confidence", meanConf,
		"terminal_soc_price_ore", p.TerminalSoCPrice,
		"capacity_wh", p.CapacityWh,
		"max_charge_w", p.MaxChargeW,
		"max_discharge_w", p.MaxDischargeW)

	// Persist a diagnostic snapshot so operators can time-travel to
	// this replan later. Best-effort: errors log and continue so a
	// flaky disk never blocks planning.
	//
	// Critically: build from the LOCAL plan/slots/p we just computed,
	// not from s.last via Diagnose(). A concurrent replan could have
	// swapped s.last between our unlock and the Diagnose() call,
	// which would pair a different plan with OUR reason — writing a
	// corrupt snapshot. Using the locals keeps (plan, reason)
	// atomically consistent even under concurrent replans.
	if s.SaveDiag != nil {
		if d := buildDiagnostic(&plan, slots, p, s.Zone, replanAtMs, reason); d != nil {
			if err := s.SaveDiag(d, reason); err != nil {
				slog.Warn("mpc: persist diagnostic failed", "err", err)
			}
		}
	}
	return &plan
}

// LastReplanInfo returns when the most recent replan ran and why.
// Exposed for the UI so operators see "reactive-pv 12s ago" vs
// "scheduled 11m ago" and understand why the plan changed.
func (s *Service) LastReplanInfo() (time.Time, string) {
	if s == nil {
		return time.Time{}, ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastReplanAt, s.lastReason
}

// extendPricesWithForecast appends synthesized price rows for slots between
// the last published price and `untilMs`, using the learned predictor.
// Synthesized rows are tagged `source="forecast"` so the UI can distinguish
// them visually.
func extendPricesWithForecast(prices []state.PricePoint, zone string, pricer PricePredictor, nowMs, untilMs int64, gridTariff, vatPct float64) []state.PricePoint {
	// Find the latest published slot end.
	var latestEndMs int64
	slotLen := 60
	for _, p := range prices {
		sl := p.SlotLenMin
		if sl <= 0 {
			sl = 60
		}
		end := p.SlotTsMs + int64(sl)*60*1000
		if end > latestEndMs {
			latestEndMs = end
		}
		if sl > 0 {
			slotLen = sl
		}
	}
	// If published already covers the horizon, nothing to do.
	if latestEndMs >= untilMs {
		return prices
	}
	// Start synthesizing from the later of (latestEndMs, nowMs).
	start := latestEndMs
	if start < nowMs {
		start = nowMs
	}
	// Round down to the slotLen grid.
	mod := start % (int64(slotLen) * 60 * 1000)
	start -= mod
	for ts := start; ts < untilMs; ts += int64(slotLen) * 60 * 1000 {
		t := time.UnixMilli(ts).UTC()
		spot := pricer(zone, t)
		total := (spot + gridTariff) * (1 + vatPct/100.0)
		prices = append(prices, state.PricePoint{
			Zone:        zone,
			SlotTsMs:    ts,
			SlotLenMin:  slotLen,
			SpotOreKwh:  spot,
			TotalOreKwh: total,
			Source:      "forecast",
			FetchedAtMs: nowMs,
		})
	}
	return prices
}

// buildSlots joins price rows with forecast rows by start time. Prices drive
// slot count + duration; forecast PV is interpolated forward (last valid
// value carries) because forecast is usually hourly while prices are 15-min.
//
// If `pv` is non-nil, the planner uses the learned twin's prediction
// (fed with the forecast's cloud cover) instead of the naive pv_w_estimated
// that the forecast service stored at fetch time. This lets the model
// learn system-specific orientation/shading/soiling and drive planning
// off the better signal without re-fetching weather.
func buildSlots(prices []state.PricePoint, forecasts []state.ForecastPoint, baseLoad float64, nowMs int64, pv PVPredictor, load LoadPredictor) []Slot {
	out := make([]Slot, 0, len(prices))
	for _, pr := range prices {
		slotLen := pr.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		slotEnd := pr.SlotTsMs + int64(slotLen)*60*1000
		if slotEnd <= nowMs {
			continue // past slot
		}
		slotT := time.UnixMilli(pr.SlotTsMs).UTC()
		var pvW float64
		forecastPVW := lookupPV(forecasts, pr.SlotTsMs)
		if pv != nil {
			cloud := lookupCloud(forecasts, pr.SlotTsMs)
			radiationBacked := lookupHasRadiation(forecasts, pr.SlotTsMs)
			pvW = selectPlannerPVW(forecastPVW, pv(slotT, cloud), radiationBacked)
		} else {
			pvW = forecastPVW
		}
		loadW := baseLoad
		if load != nil {
			loadW = load(slotT)
		}
		// Confidence from the price source: real day-ahead → 1.0,
		// ML-forecasted → 0.6 (user-tunable hook for later). Anything
		// else (seed data, ENTSOE, elprisetjustnu) → 1.0 too.
		conf := 1.0
		if pr.Source == "forecast" {
			conf = 0.6
		}
		out = append(out, Slot{
			StartMs:    pr.SlotTsMs,
			LenMin:     slotLen,
			PriceOre:   pr.TotalOreKwh,
			SpotOre:    pr.SpotOreKwh,
			PVW:        -math.Abs(pvW),
			LoadW:      loadW,
			Confidence: conf,
		})
	}
	return out
}

const (
	// When the learned PV twin collapses to (near) zero while the weather
	// forecast still expects material daylight output, the planner
	// degenerates into "import full load, battery idle". Fall back to the
	// stored forecast in that quantifiable failure mode instead of trusting
	// a near-zero model output.
	plannerMinForecastPVFallbackW = 200.0
	plannerMaxCollapsedPVW        = 50.0
	plannerMaxCollapsedPVFrac     = 0.10
)

// selfConsumptionTerminalPrice is the per-kWh öre value of leftover SoC at
// the end of the horizon, for the modes where the battery cannot export.
// Equals the mean retail-import price minus the mean export price
// (spot + bonus − fee, floored at 0). That's what one kWh in the battery
// actually earns you: it displaces one kWh of future retail import
// instead of one kWh that would otherwise have been exported.
//
// Floored at 0 so we never credit SoC negatively; if export rates exceed
// retail (rare, subsidy edge cases) the planner in these modes should just
// stay SoC-neutral rather than actively drain.
func selfConsumptionTerminalPrice(prices []state.PricePoint, bonus, fee float64) float64 {
	if len(prices) == 0 {
		return 0
	}
	var importSum, exportSum float64
	for _, pr := range prices {
		importSum += pr.TotalOreKwh
		exp := pr.SpotOreKwh + bonus - fee
		if exp < 0 {
			exp = 0
		}
		exportSum += exp
	}
	n := float64(len(prices))
	spread := (importSum - exportSum) / n
	if spread < 0 {
		spread = 0
	}
	return spread
}

// PlannerRadiationWeight is how much the RLS twin's prediction
// contributes when the forecast is backed by a measured-radiation
// (or direct-PV) signal. The rest comes from the forecast itself.
//
// The twin's job in that regime is per-site calibration: orientation,
// soiling, inverter derate — a multiplicative correction on top of an
// already physically-grounded prediction. Letting it contribute more
// than ~30 % re-introduces the very brittleness we switched off
// cloud-only forecasts to escape (an under-trained twin can produce
// wild predictions from the time-of-day features alone when fed
// non-representative training data).
const PlannerRadiationWeight = 0.3

func selectPlannerPVW(forecastPVW, predictedPVW float64, radiationBacked bool) float64 {
	// Invalid predicted → fall back to forecast (unchanged).
	switch {
	case math.IsNaN(predictedPVW), math.IsInf(predictedPVW, 0), predictedPVW < 0:
		if math.IsNaN(forecastPVW) || math.IsInf(forecastPVW, 0) {
			return 0
		}
		return forecastPVW
	}

	// Radiation-backed forecasts (open_meteo, forecast_solar) have the
	// correct diurnal shape and cloud response already. Blend the twin's
	// prediction in as a thin per-site calibration instead of letting it
	// override the forecast. Typical picture on homelab-rpi after the
	// switch: forecast shows smooth bell curve 0–8 kW, an under-trained
	// twin still spits random spikes from overfit feature vectors — and
	// we want the smooth curve.
	if radiationBacked && forecastPVW > 0 {
		return (1-PlannerRadiationWeight)*forecastPVW + PlannerRadiationWeight*predictedPVW
	}

	// Cloud-only legacy path: prefer the twin when forecast is near zero
	// (forecast probably missing), fall back to forecast when the twin
	// collapsed to ~0 (twin probably broken).
	if forecastPVW < plannerMinForecastPVFallbackW {
		return predictedPVW
	}
	collapseCeil := math.Max(plannerMaxCollapsedPVW, forecastPVW*plannerMaxCollapsedPVFrac)
	if predictedPVW <= collapseCeil {
		return forecastPVW
	}
	return predictedPVW
}

// lookupHasRadiation reports whether the forecast row covering `ts` has
// a measured-radiation or direct-PV signal from the provider (as
// opposed to a cloud-derated naive estimate). Used by the planner to
// decide how much to trust the forecast vs the RLS twin. Anchors on
// the same slot-boundary rules as lookupPV.
func lookupHasRadiation(forecasts []state.ForecastPoint, ts int64) bool {
	for _, f := range forecasts {
		slotLen := f.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		end := f.SlotTsMs + int64(slotLen)*60*1000
		if ts >= f.SlotTsMs && ts < end {
			return f.SolarWm2 != nil
		}
	}
	return false
}

// lookupCloud returns the cloud cover (%) for the forecast row covering
// `ts`, falling back to the nearest neighbour. 50% is the neutral
// prior if no forecast is available at all.
func lookupCloud(forecasts []state.ForecastPoint, ts int64) float64 {
	if len(forecasts) == 0 {
		return 50
	}
	for i, f := range forecasts {
		slotLen := f.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		end := f.SlotTsMs + int64(slotLen)*60*1000
		if ts >= f.SlotTsMs && ts < end {
			if f.CloudCoverPct != nil {
				return *f.CloudCoverPct
			}
			return 50
		}
		if ts < f.SlotTsMs && i > 0 {
			if prev := forecasts[i-1]; prev.CloudCoverPct != nil {
				return *prev.CloudCoverPct
			}
		}
	}
	if last := forecasts[len(forecasts)-1]; last.CloudCoverPct != nil {
		return *last.CloudCoverPct
	}
	return 50
}

// lookupPV finds the forecast row whose slot covers ts and returns its PV
// estimate (W, non-negative). Returns 0 if no forecast or no estimate.
// Strictly respects slot boundaries: does NOT carry forward beyond the last
// forecast slot, because doing so would project stale PV into nighttime or
// far-future slots where the forecast didn't cover.
func lookupPV(forecasts []state.ForecastPoint, ts int64) float64 {
	if len(forecasts) == 0 {
		return 0
	}
	// Binary-search would be faster, but len is typically ≤ 49 (met.no).
	for i, f := range forecasts {
		slotLen := f.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		end := f.SlotTsMs + int64(slotLen)*60*1000
		if ts >= f.SlotTsMs && ts < end {
			if f.PVWEstimated != nil {
				return *f.PVWEstimated
			}
			return 0
		}
		// Fall back: if between rows, use the preceding row (interpolation
		// within the forecast range only).
		if ts < f.SlotTsMs && i > 0 {
			if prev := forecasts[i-1]; prev.PVWEstimated != nil {
				return *prev.PVWEstimated
			}
		}
	}
	// After last row — return 0 (no forecast coverage).
	return 0
}

// waitForRealSoC blocks until at least one online battery driver is
// reporting a non-nil SoC, or the context is cancelled. Returns true
// on success, false on cancellation. Re-warns every warnEvery so a
// stuck driver shows up in logs even after the first notice scrolls
// past.
func (s *Service) waitForRealSoC(ctx context.Context) bool {
	if s.Tele == nil {
		return true
	}
	const (
		pollEvery = 250 * time.Millisecond
		warnEvery = 30 * time.Second
	)
	if s.hasRealSoC() {
		return true
	}
	waitStart := time.Now()
	warn := time.NewTicker(warnEvery)
	defer warn.Stop()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-warn.C:
			slog.Warn("mpc: still waiting for real battery SoC before first replan",
				"waited", time.Since(waitStart))
		case <-tick.C:
			if s.hasRealSoC() {
				slog.Info("mpc: real battery SoC available, proceeding with first replan",
					"waited", time.Since(waitStart))
				return true
			}
		}
	}
}

// hasRealSoC reports whether at least one online battery driver has
// emitted a non-nil SoC value.
func (s *Service) hasRealSoC() bool {
	for _, r := range s.Tele.ReadingsByType(telemetry.DerBattery) {
		if r.SoC == nil {
			continue
		}
		if h := s.Tele.DriverHealth(r.Driver); h == nil || !h.IsOnline() {
			continue
		}
		return true
	}
	return false
}

// currentSoCPct averages SoC across battery readings in the telemetry store.
// Telemetry stores SoC as a fraction in [0, 1]; the MPC expects [0, 100].
// Falls back to `fallback` (already in percent) if no readings are present.
func currentSoCPct(t *telemetry.Store, fallback float64) float64 {
	if t == nil {
		return fallback
	}
	bats := t.ReadingsByType(telemetry.DerBattery)
	if len(bats) == 0 {
		return fallback
	}
	var sum float64
	var n int
	for _, b := range bats {
		if b.SoC != nil {
			sum += *b.SoC
			n++
		}
	}
	if n == 0 {
		return fallback
	}
	return sum / float64(n) * 100.0
}
