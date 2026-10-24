package control

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/sitesim"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// simCoreEvery is control_interval_s's default. The plant steps every second.
const simCoreEvery = 2 * time.Second

// simSite runs Core's control tick against a sitesim.Plant in main.go's
// order: site import, charger tick, then battery dispatch. Core's settings
// are the config defaults. Every plant second it checks the invariants and
// keeps the first one broken.
type simSite struct {
	p       *sitesim.Plant
	lp      loadpoint.Config
	surplus bool               // the car charges from surplus; otherwise Charge now while plugged
	plan    loadpoint.PlanFunc // nil: no plan, and the car charges on Charge now
	setup   func(*simSite)     // after each boot

	db    memConfigStore // state.db: survives a restart
	store *telemetry.Store
	state *State
	mgr   *loadpoint.Manager
	ctrl  *loadpoint.Controller

	every time.Duration // control_interval_s; zero: simCoreEvery
	field *fieldRun     // the field matrix's own checks; nil outside it

	downUntil, nextTick time.Time
	meter               sitesim.MeterReading
	meterAt, evAt       time.Time // last reading
	ev                  sitesim.EVReading
	seen                map[string]time.Time // each driver's last reading
	offerW, batteryCmdW float64              // our last commands
	raisedAt            time.Time            // our last command that raised import
	outsideAt           time.Time            // the last outside step a scenario marked
	defaulted           bool
	commands            []simCommand

	hourStart time.Time // the plant's clock hour
	hourWh    float64   // its import, as the plant measured it

	heat                 [3]float64 // the phases' I², filtered as Core's heat model does
	heatSet              bool
	overSince, needSince time.Time
	limitSince           time.Time // a phase above the fuse less its margin since
	ownRaise             bool      // that run followed a raise we sent
	peakA                float64
	overLimit            time.Duration // time with a phase over the fuse less its margin
	idleWithRoom         time.Duration // time the car was idle with room for it
	overAt               time.Time     // last second a phase was over the fuse less its margin
	cutsWithRoom         int           // car orders lowered with no phase over that limit within the response time
	failure              string
}

type simCommand struct {
	at     time.Time
	device string
	w      float64
}

func (c simCommand) String() string {
	return fmt.Sprintf("%s %.0f W at %s", c.device, c.w, c.at.Format("Jan 2 15:04:05"))
}

// boot starts Core with no memory. The devices keep their last command.
func (s *simSite) boot() {
	s.store, s.seen, s.evAt, s.defaulted = telemetry.NewStore(), map[string]time.Time{}, time.Time{}, false
	s.state = NewState(0, 42, "meter")
	s.state.clock = func() time.Time { return s.p.Now }
	s.state.SlewRateW, s.state.MinDispatchIntervalS = 3000, 2
	s.state.SiteFuseAmps, s.state.SiteFuseVoltage, s.state.SiteFusePhases, s.state.SiteFuseSafetyA = s.p.Fuse.RatingA, s.p.Voltage, 3, .5
	if b := s.p.Battery; b != nil {
		s.state.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: b.MaxChargeW, MaxDischargeW: b.MaxDischargeW}}
	}
	s.mgr = loadpoint.NewManager()
	s.mgr.SetNowFn(s.state.clock)
	s.mgr.Load([]loadpoint.Config{s.lp})
	if s.db == nil {
		s.db = memConfigStore{}
	}
	s.mgr.SetSessionStore(s.db)
	plan := s.plan
	if plan == nil {
		plan = func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false }
	}
	s.ctrl = loadpoint.NewController(s.mgr, plan, s.evSample, s.send)
	s.ctrl.SetDriverOnline(func(d string) bool { h := s.store.DriverHealth(d); return h != nil && h.IsOnline() })
	s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: s.p.Fuse.RatingA, Voltage: s.p.Voltage, PhaseCnt: 3})
	s.ctrl.SetFuseEVMax(func() (float64, bool) {
		return EVFuseBudget(s.store, s.state, s.mgr.States(), s.ctrl.FuseConfigs(), s.fuseMaxW())
	})
	s.ctrl.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(s.state) })
	s.ctrl.SetFuseLimitReason(func() string { return EVLimitReason(s.state) })
	s.ctrl.SetSitePhases(func() (loadpoint.SitePhases, bool) { return SitePhases(s.store, s.state) })
	s.ctrl.SetSiteSurplusForEV(func() (float64, bool) {
		if !s.meterFresh() {
			return 0, false
		}
		var batW float64
		for _, r := range s.store.ReadingsByType(telemetry.DerBattery) {
			if h := s.store.DriverHealth(r.Driver); h != nil && h.Status != telemetry.StatusOffline {
				batW += r.SmoothedW
			}
		}
		meterW := s.store.Get("meter", telemetry.DerMeter).SmoothedW
		return loadpoint.SurplusAvailableForEVW(meterW, batW, s.store.SumOnlineEVW(), s.ctrl.AnyLoadpointSurplusActive()), true
	})
	s.ctrl.SetBatSoCProvider(func() (float64, bool) {
		r := s.store.Get("battery", telemetry.DerBattery)
		if h := s.store.DriverHealth("battery"); r == nil || r.SoC == nil || h == nil || !h.IsOnline() {
			return 0, false
		}
		return *r.SoC, true
	})
	if s.setup != nil {
		s.setup(s)
	}
}

func (s *simSite) fuseMaxW() float64 { return s.p.Fuse.RatingA * s.p.Voltage * 3 }

// meterFresh is Core's view: a site reading inside the 60 s watchdog.
func (s *simSite) meterFresh() bool {
	m := s.store.Get("meter", telemetry.DerMeter)
	return m != nil && s.p.Now.Sub(m.UpdatedAt) <= time.Minute
}

func (s *simSite) evSample(string) (loadpoint.EVSample, bool) {
	if s.evAt.IsZero() || s.p.Now.Sub(s.evAt) > time.Minute {
		return loadpoint.EVSample{}, false
	}
	return loadpoint.EVSample{Connected: s.ev.Plugged, RequestActive: true, PowerW: s.ev.W, PowerAt: s.evAt, PowerMaxAge: s.declaredMaxAge(),
		DeviceID: "easee:SIM1", SessionWh: s.ev.SessionWh, EnergyAt: s.evAt}, true
}

func (s *simSite) send(_ context.Context, driver string, payload []byte) error {
	var cmd struct {
		Action    string  `json:"action"`
		PowerW    float64 `json:"power_w"`
		PhaseMode string  `json:"phase_mode"`
		SplitW    float64 `json:"phase_split_w"`
		MaxA      float64 `json:"max_amps_per_phase"`
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return err
	}
	if driver != s.lp.DriverName || s.p.Offline("evse") {
		return errors.New("sitesim: no such charger online")
	}
	if cmd.Action != "ev_set_current" {
		return nil
	}
	// (c) A site meter older than the watchdog raises nothing.
	if age := s.p.Now.Sub(s.meterAt); age > time.Minute && cmd.PowerW > s.offerW {
		s.fail("raised the car %.0f -> %.0f W on a meter %v old", s.offerW, cmd.PowerW, age)
	}
	if cmd.PowerW > s.offerW {
		s.raisedAt = s.p.Now
	}
	if cmd.PowerW < s.offerW && (s.overAt.IsZero() || s.p.Now.Sub(s.overAt) > s.responseTime(true)) {
		s.cutsWithRoom++
	}
	s.offerW = cmd.PowerW
	s.commands = append(s.commands, simCommand{s.p.Now, "evse", cmd.PowerW})
	// Without a split the Easee driver splits auto at the fuse's current
	// on one phase (easee_cloud.lua pick_phases).
	if cmd.SplitW <= 0 && cmd.MaxA > 0 {
		cmd.SplitW = s.p.Voltage * cmd.MaxA
	}
	s.p.EVSE.Command(s.p.Now, cmd.PowerW, cmd.PhaseMode, cmd.SplitW, s.p.Voltage)
	return nil
}

func (s *simSite) emit(driver string, kind telemetry.DerType, w float64, soc *float64, extra map[string]any) {
	d := map[string]any{"control_power_available": true, "control_power_observed_at": s.p.Now.Format(time.RFC3339Nano)}
	maps.Copy(d, extra)
	data, _ := json.Marshal(d)
	s.store.Update(driver, kind, w, soc, data)
	r := s.store.Get(driver, kind)
	r.UpdatedAt = s.p.Now // Receipt on the plant's clock, as are SoC times:
	if soc != nil {       // Update stamps both with the wall clock.
		r.SoCUpdatedAt = s.p.Now
	}
	s.store.RecordDriverSuccess(driver)
	s.seen[driver] = s.p.Now
}

func (s *simSite) step() {
	tick := s.p.Step()
	now := s.p.Now
	if tick.Meter != nil {
		s.meter, s.meterAt = *tick.Meter, now
	}
	if tick.Restart > 0 {
		s.downUntil = now.Add(tick.Restart)
	}
	if !s.downUntil.IsZero() && !now.Before(s.downUntil) {
		s.downUntil = time.Time{}
		s.boot()
	}
	if s.downUntil.IsZero() {
		if m := tick.Meter; m != nil {
			s.emit("meter", telemetry.DerMeter, m.W, nil, map[string]any{"l1_a": m.PhaseA[0], "l2_a": m.PhaseA[1], "l3_a": m.PhaseA[2]})
		}
		if b := tick.Battery; b != nil {
			s.emit("battery", telemetry.DerBattery, b.W, &b.SoC, nil)
		}
		if e := tick.EV; e != nil {
			s.ev, s.evAt = *e, now
			s.emit(s.lp.DriverName, telemetry.DerEV, e.W, nil, s.evData())
		}
		if !now.Before(s.nextTick) {
			s.nextTick = now.Add(s.coreEvery())
			s.control()
		}
	}
	s.check()
}

func (s *simSite) run(d time.Duration) {
	for end := s.p.Now.Add(d); s.p.Now.Before(end) && s.failure == ""; {
		s.step()
	}
}

// control is one tick of main.go's control loop.
func (s *simSite) control() {
	now := s.p.Now
	for d, at := range s.seen {
		if h := s.store.DriverHealthMut(d); now.Sub(at) > time.Minute && h.IsOnline() {
			h.SetOffline() // The watchdog.
		}
	}
	if _, held := s.ctrl.GetManualHold(s.lp.ID, now); !s.surplus && s.plan == nil && !held && s.ev.Plugged {
		s.ctrl.SetManualHold(s.lp.ID, loadpoint.ManualHold{PowerW: s.lp.MaxChargeW, PhaseMode: s.lp.PhaseMode, Persistent: true, StartedAt: now, UpdatedAt: now})
	}
	allowed := s.meterFresh()
	s.state.ObserveSiteImport(s.store)
	s.ctrl.TickWithDispatch(context.Background(), now, allowed)
	if !allowed {
		if !s.defaulted {
			s.batteryCommand(0) // The driver's default mode, sent once.
			s.defaulted = true
		}
		return
	}
	s.defaulted = false
	points := s.mgr.States()
	for i := range points {
		_, points[i].ManualActive = s.ctrl.GetManualHold(points[i].ID, now)
	}
	s.state.EVSurplusOnlyReserveW = loadpoint.SurplusReserveW(points, nil)
	s.state.EVSurplusOnlyChargingW = loadpoint.SurplusChargingW(points)
	s.state.EVCurtailHeadroomW = loadpoint.SurplusPotentialW(points)
	s.state.SetEVCommanded(points)
	s.state.SetEVFuseDemands(s.ctrl.FuseDemands())
	var capacityWh float64
	if b := s.p.Battery; b != nil {
		capacityWh = b.CapacityWh
	}
	for _, target := range ComputeDispatch(s.store, s.state, map[string]float64{"battery": capacityWh}, s.fuseMaxW()) {
		if target.Driver == "battery" {
			s.batteryCommand(target.TargetW)
		}
	}
}

// batteryCommand sends w. More than an amp of extra import on one phase
// counts as our raise; smaller moves are the PI's trim.
func (s *simSite) batteryCommand(w float64) {
	b := s.p.Battery
	if b == nil || s.p.Offline("battery") {
		return
	}
	if w-s.batteryCmdW > s.p.Voltage {
		s.raisedAt = s.p.Now
	}
	s.batteryCmdW = w
	s.commands = append(s.commands, simCommand{s.p.Now, "battery", w})
	b.Command(s.p.Now, w)
}

// responseTime is how long a command takes to show on the meter: a control
// tick, the device's delay and a meter reading. slowest picks the slower of
// the charger and the battery.
func (s *simSite) responseTime(slowest bool) time.Duration {
	d := s.p.EVSE.Delay
	if b := s.p.Battery; b != nil {
		d = min(d, b.Latency)
		if slowest {
			d = max(s.p.EVSE.Delay, b.Latency)
		}
	}
	return d + max(s.p.Meter.Every, sitesim.Step) + s.coreEvery()
}

// check holds the invariants for this second.
func (s *simSite) check() {
	p, now := s.p, s.p.Now
	if hour := ImportHourStart(now.Add(-sitesim.Step)); !hour.Equal(s.hourStart) {
		s.hourStart, s.hourWh = hour, 0
	}
	s.hourWh += math.Max(0, (p.A[0]+p.A[1]+p.A[2])*p.Voltage) * sitesim.Step.Hours()
	if p.Fuse.AnyBlown() {
		s.fail("the main fuse blew: damage %.2f", p.Fuse.Damage)
	}
	// (a) A phase above the fuse less its margin after a raise we sent within
	// the slowest response time is our own doing and may last 3 s. After an
	// outside change, Core's own limit, read from the physical currents as
	// fuseHeat reads the meter, may break for the fastest response time plus
	// 3 s: a phase may sit above the fuse less its margin only while its
	// I²-filtered current is under it, and never above the instant limit.
	fuseA := p.Fuse.RatingA
	limitA, k := fuseA-evPhaseMarginA, -math.Expm1(-sitesim.Step.Seconds()/fuseHeatTau.Seconds())
	over, overLimit := false, false
	for i, a := range p.A {
		if s.heatSet {
			s.heat[i] += (a*a - s.heat[i]) * k
		} else {
			s.heat[i] = a * a
		}
		over = over || math.Abs(a) > fuseInstantFactor*fuseA || (math.Abs(a) > limitA && s.heat[i] > limitA*limitA)
		overLimit = overLimit || math.Abs(a) > limitA
		s.peakA = math.Max(s.peakA, math.Abs(a))
	}
	s.heatSet = true
	if overLimit {
		s.overAt = now
		s.overLimit += sitesim.Step
		if s.limitSince.IsZero() {
			s.limitSince, s.ownRaise = now, !s.raisedAt.IsZero() && now.Sub(s.raisedAt) <= s.responseTime(true) && !s.outsideAt.After(s.raisedAt)
		}
	} else {
		s.limitSince = time.Time{}
	}
	if s.ownRaise && !s.limitSince.IsZero() && now.Sub(s.limitSince) > 3*time.Second {
		s.fail("a raise we sent kept a phase over %.0f A for more than 3 s", limitA)
	}
	if !over {
		s.overSince = time.Time{}
	} else if s.overSince.IsZero() {
		s.overSince = now
	}
	if allow := s.responseTime(false) + 3*time.Second; !s.overSince.IsZero() && now.Sub(s.overSince) > allow {
		s.fail("over Core's limit for more than %v (heat %.1f A, limit %.0f A)", allow, math.Sqrt(max(s.heat[0], s.heat[1], s.heat[2])), limitA)
	}
	// (b) With the meter on schedule, room for the car's minimum and a car
	// that asks, the car draws within a minute plus the charger's delay.
	if !s.wantsCharge() {
		s.needSince = time.Time{}
	} else if s.needSince.IsZero() {
		s.needSince = now
	}
	if !s.needSince.IsZero() && p.EVSE.DrawW <= 0 {
		s.idleWithRoom += sitesim.Step
		if wait := time.Minute + p.EVSE.Delay; now.Sub(s.needSince) > wait {
			s.fail("the car is idle after %v with room for it", wait)
		}
	}
	if s.field != nil {
		s.field.check(s)
	}
}

// wantsCharge reports whether the car should charge now: it is plugged
// and asks, Core and the meter are running, and the uncontrolled load
// leaves room for the car's minimum on its phases and under the hour's
// import ceiling (and, for surplus charging, exports at least that much).
// Under the ceiling Core starts nothing in an hour's last quarter.
func (s *simSite) wantsCharge() bool {
	p, e := s.p, s.p.EVSE
	if p.Car == nil || !p.Car.Plugged || p.Offline("evse") || !s.downUntil.IsZero() || p.Now.Sub(s.meterAt) > max(p.Meter.Every, sitesim.Step)+sitesim.Step {
		return false
	}
	phases := min(e.Phases, p.Car.Phases)
	var baseW float64
	for i, houseW := range p.HouseW {
		w := houseW + p.PVW/3
		baseW += w
		if i < phases && math.Abs(w/p.Voltage+e.MinA) > p.Fuse.RatingA-evPhaseMarginA {
			return false
		}
	}
	minW := e.MinA * p.Voltage * float64(phases)
	if ceilingW := s.state.PeakImportCeilingW; ceilingW > 0 {
		left := s.hourStart.Add(time.Hour).Sub(p.Now)
		if left < 15*time.Minute || baseW+minW > (ceilingW-s.hourWh)/left.Hours() {
			return false
		}
	}
	if s.field != nil {
		return s.field.wantsCharge(s, baseW)
	}
	return !s.surplus || -baseW >= minW
}

func (s *simSite) fail(format string, args ...any) {
	if s.failure != "" {
		return
	}
	st, _ := s.mgr.State(s.lp.ID)
	s.failure = fmt.Sprintf("%v: "+format+"\n  phases %.1f A, offer %.0f W (%s), car draws %.0f W, battery %.0f W",
		append(append([]any{s.p.Now.Format("15:04:05")}, args...), s.p.A, s.offerW, st.CommandedReason, s.p.EVSE.DrawW, s.batteryW())...)
}

func (s *simSite) batteryW() float64 {
	if s.p.Battery == nil {
		return 0
	}
	return s.p.Battery.W
}

// simScenario is one house. known names the finding that makes it fail on
// master: the row must keep failing, and fails once the fix lands until the
// fix deletes that line. expect checks the plant at times along the run.
type simScenario struct {
	name    string
	known   string
	start   time.Time // zero: simStart
	run     time.Duration
	surplus bool
	plan    loadpoint.PlanFunc
	plant   func(*sitesim.Plant)
	lp      func(*loadpoint.Config)
	setup   func(*simSite)
	expect  []simExpect
}

type simExpect struct {
	at    time.Duration
	check func(*simSite) error
}

var simStart = time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)

// simPlant is a 25 A site with an Easee Cloud-like charger, which answers
// in 5 s and reports the draw 10 s late, and a 3p car that takes 16 A.
func simPlant(start time.Time) *sitesim.Plant {
	p := sitesim.New(start, 25)
	p.EVSE = &sitesim.EVSE{Phases: 3, MinA: 6, MaxA: 16, Delay: 5 * time.Second, ReadbackLag: 10 * time.Second}
	p.Car = &sitesim.Car{Phases: 3, MaxW: 11040, Plugged: true}
	return p
}

func simLoadpoint() loadpoint.Config {
	cfg := loadpoint.Config{ID: "garage", DriverName: "evse", MinChargeW: 4140, MaxChargeW: 11040, PhaseMode: "3p"}
	for a := 0.0; a <= 16; a++ {
		if a == 0 || a >= 6 {
			cfg.AllowedStepsW = append(cfg.AllowedStepsW, a*690)
		}
	}
	return cfg
}

func houseA(l1, l2, l3 float64) [3]float64 { return [3]float64{l1 * 230, l2 * 230, l3 * 230} }

// fieldNightLoadpoint is the 22 Oct 2026 field site's Easee: auto, with
// one-phase levels up to 3.8 kW and three-phase levels from 4.8 kW.
func fieldNightLoadpoint(c *loadpoint.Config) {
	c.MinChargeW, c.MaxChargeW, c.PhaseMode = 1600, 11100, "auto"
	c.AllowedStepsW = []float64{0, 1600, 1900, 2100, 2400, 2600, 2800, 3100, 3300, 3500, 3800,
		4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11100}
}

// carPhasesUnknown checks that the session's car was never marked one-phase.
func carPhasesUnknown(s *simSite) error {
	if st, _ := s.mgr.State(s.lp.ID); st.CarPhases != 0 {
		return fmt.Errorf("car marked one-phase on L%d; car draws %.0f W on %d phases", st.CarPhase, s.p.EVSE.DrawW, s.p.EVSE.DrawPhases)
	}
	return nil
}

// planEV is a saved goal and a plan that gives the car w in every quarter
// hour: the path Core takes overnight.
func planEV(w float64) loadpoint.PlanFunc {
	return func(now time.Time) (loadpoint.Directive, bool) {
		slot := now.Truncate(15 * time.Minute)
		return loadpoint.Directive{SlotStart: slot, SlotEnd: slot.Add(15 * time.Minute),
			LoadpointEnergyWh: map[string]float64{"garage": w / 4}}, true
	}
}

// planBatteryCharge has the plan charge the battery at w for the hour.
func planBatteryCharge(w float64) func(*simSite) {
	return func(s *simSite) {
		s.state.Mode, s.state.UseEnergyDispatch = ModePlannerArbitrage, true
		slot := SlotDirective{SlotStart: s.p.Now, SlotEnd: s.p.Now.Add(time.Hour), BatteryEnergyWh: w, Strategy: "arbitrage"}
		s.state.SlotDirective = func(time.Time) (SlotDirective, bool) { return slot, true }
	}
}

// simDay is a weekday from midnight in Self mode: the car is plugged
// overnight with Charge now, a 9 kW PV peak at 13:00 fills the 20 kWh
// battery by 12:45 and a heavy evening puts L1 at 13.7 A before the car.
// The battery lasts the heavy evening; "battery reaches its floor" is the
// case where it does not. Core restarts at 18:31, the battery driver drops
// out at 20:01 and the meter goes silent at 22:31.
func simDay(p *sitesim.Plant) {
	p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 5000, MaxDischargeW: 5000, CapacityWh: 20000, SoC: .5, Latency: 2 * time.Second}
	for q := range 96 {
		h, houseW, pvW := float64(q)/4, 250.0, 0.0
		switch {
		case h >= 6.5 && h < 8.5:
			houseW += 600
		case h >= 17 && h < 20:
			houseW += 2500
		case h >= 20 && h < 22:
			houseW += 1000
		}
		if h > 7 && h < 19 {
			pvW = -9000 * math.Sin(math.Pi*(h-7)/12)
		}
		plugged := h < 7 || h >= 17
		p.Events = append(p.Events, sitesim.Event{At: time.Duration(q) * 15 * time.Minute, Do: func(p *sitesim.Plant) {
			p.HouseW, p.PVW, p.Car.Plugged = [3]float64{houseW + 400, houseW, houseW}, pvW, plugged
		}})
	}
	p.Events = append(p.Events,
		sitesim.Event{At: 18*time.Hour + 31*time.Minute, Kind: sitesim.CoreRestart, For: 20 * time.Second},
		sitesim.Event{At: 20*time.Hour + time.Minute, Kind: sitesim.DriverOffline, Device: "battery", For: 2 * time.Minute},
		sitesim.Event{At: 22*time.Hour + 31*time.Minute, Kind: sitesim.MeterSilent, For: 90 * time.Second})
	slices.SortStableFunc(p.Events, func(a, b sitesim.Event) int { return cmp.Compare(a.At, b.At) })
}

func simScenarios() []simScenario {
	slowCharger := func(delay time.Duration) func(*sitesim.Plant) {
		return func(p *sitesim.Plant) { p.HouseW, p.EVSE.Delay = houseA(15, 15, 15), delay }
	}
	// 9 kW of export to a charger that reads back at once.
	surplus := func(p *sitesim.Plant) { p.HouseW, p.PVW, p.EVSE.ReadbackLag = houseA(2, 2, 2), -9000-3*460, 0 }
	surplusPhase := func(mode string) func(*loadpoint.Config) {
		return func(c *loadpoint.Config) {
			c.PhaseMode, c.SurplusOnly, c.MinChargeW, c.PhaseSplitW = mode, true, 1380, 3680
			steps := []float64{0}
			for a := 6.0; a <= 16; a++ {
				steps = append(steps, a*230)
			}
			c.AllowedStepsW = append(steps, c.AllowedStepsW[1:]...)
		}
	}
	return []simScenario{
		{name: "export start", known: "export counted as import, fuse_heat.go:120", run: 5 * time.Minute,
			plant: func(p *sitesim.Plant) { p.HouseW, p.PVW = houseA(2, 2, 2), -22*690 }},
		{name: "export start, unsigned phase currents", known: "export counted as import, fuse_heat.go:120", run: 5 * time.Minute,
			plant: func(p *sitesim.Plant) { p.HouseW, p.PVW, p.Meter.Unsigned = houseA(2, 2, 2), -22*690, true }},
		{name: "charger's own load balancer", known: "a cut comes off an offer the car does not draw, ev_fuse_budget.go:172", run: 20 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.HouseW, p.EVSE.LoadBalancerA = houseA(12, 12, 12), 25
				p.Events = []sitesim.Event{
					{At: 5 * time.Minute, Do: func(p *sitesim.Plant) { p.HouseW = houseA(18, 12, 12) }},
					{At: 10 * time.Minute, Do: func(p *sitesim.Plant) { p.HouseW = houseA(4, 4, 4) }},
					{At: 15 * time.Minute, Do: func(p *sitesim.Plant) { p.HouseW = houseA(20, 20, 20) }},
				}
			}},
		{name: "slow cloud charger 60 s", run: 15 * time.Minute,
			plant: slowCharger(time.Minute)},
		{name: "slow cloud charger 120 s", run: 15 * time.Minute,
			plant: slowCharger(2 * time.Minute)},
		{name: "#1761 replay", run: 20 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.HouseW, p.Car.MaxW = houseA(1, 1, 1), 4200
				p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 9400, MaxDischargeW: 9400, CapacityWh: 20000, SoC: .3, Latency: 3 * time.Second}
				p.Events = []sitesim.Event{{At: 10 * time.Minute, Do: func(p *sitesim.Plant) { p.Car.MaxW = 11040 }}}
			},
			lp: func(c *loadpoint.Config) {
				c.MaxChargeW, c.AllowedStepsW = 10350, c.AllowedStepsW[:len(c.AllowedStepsW)-1]
			},
			setup: planBatteryCharge(9400),
			expect: []simExpect{{9*time.Minute + 50*time.Second, func(s *simSite) error {
				p := s.p
				offerW := p.EVSE.OfferA * p.Voltage * float64(p.EVSE.OfferPhases)
				if offerW != 10350 || math.Abs(p.EVSE.DrawW-4200) > 50 || math.Abs(p.Battery.W-9400) > 200 {
					return fmt.Errorf("not the #1761 state before the car takes its offer: offer %.0f W, car %.0f W, battery %.0f W", offerW, p.EVSE.DrawW, p.Battery.W)
				}
				return nil
			}}}},
		{name: "meter every 30 s after a cut", known: "a meter slower than evSettle makes every reading a gap, ev_fuse_budget.go:141", run: 10 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.HouseW = houseA(3, 3, 3)
				p.Events = []sitesim.Event{
					{At: 3 * time.Minute, Do: func(p *sitesim.Plant) { p.HouseW = houseA(23, 23, 23) }},
					{At: 3*time.Minute + 3*time.Second, Do: func(p *sitesim.Plant) { p.HouseW, p.Meter.Every = houseA(3, 3, 3), 30*time.Second }},
				}
			}},
		{name: "phase mode 3p, 9 kW surplus", run: 5 * time.Minute, surplus: true, plant: surplus, lp: surplusPhase("3p")},
		{name: "phase mode empty, 9 kW surplus", run: 5 * time.Minute, surplus: true,
			plant: surplus, lp: surplusPhase("")},
		{name: "phase mode auto, 9 kW surplus", run: 5 * time.Minute, surplus: true,
			plant: surplus, lp: surplusPhase("auto")},
		{name: "9 kW surplus, readback 10 s late", known: "surplus adds back the charger's late readback, surplus_reserve.go:128", run: 5 * time.Minute, surplus: true,
			plant: func(p *sitesim.Plant) { surplus(p); p.EVSE.ReadbackLag = 10 * time.Second }, lp: surplusPhase("3p")},
		{name: "surplus_unlock_bat_soc saved, Charge now", run: 5 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.HouseW = houseA(3, 3, 3)
				p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 5000, MaxDischargeW: 5000, CapacityWh: 10000, SoC: .5}
			},
			lp: func(c *loadpoint.Config) { c.PhaseMode = "" },
			setup: func(s *simSite) {
				planBatteryCharge(0)(s)
				s.mgr.SetSchedule(s.lp.ID, loadpoint.Schedule{SurplusUnlockBatSoC: .9})
			}},
		{name: "battery reaches its floor under Charge now", run: 5 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.HouseW = houseA(13.7, 12, 12)
				p.Battery = &sitesim.Battery{Phases: 3, MaxChargeW: 5000, MaxDischargeW: 5000, CapacityWh: 10000, SoC: .07, Latency: 2 * time.Second}
			}},
		{name: "end of a heavy hour, low phases",
			start: time.Date(2026, 10, 21, 2, 0, 0, 0, time.Local), run: 65 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.HouseW = houseA(10, 10, 10)
				p.Events = []sitesim.Event{
					{At: 9 * time.Minute, Kind: sitesim.CoreRestart, For: 20 * time.Second},
					{At: 50 * time.Minute, Do: func(p *sitesim.Plant) { p.HouseW = houseA(1.5, 1.5, 1.5) }},
				}
			},
			setup: func(s *simSite) { s.state.PeakImportCeilingW = 6000 },
			expect: []simExpect{{57 * time.Minute, func(s *simSite) error {
				st, _ := s.mgr.State(s.lp.ID)
				if s.p.EVSE.DrawW > 0 || st.CommandedReason != "import_limit" {
					return fmt.Errorf("near the end of the hour: car draws %.0f W, reason %q, want idle on import_limit", s.p.EVSE.DrawW, st.CommandedReason)
				}
				return nil
			}}}},
		// Field report 22 Oct 2026, v0.155.0-beta.1: a 3x20 A site, phase_mode
		// auto, an Easee step table with one-phase levels, a plan for 11.1 kW.
		// The car ran on one phase at 3.3 kW and FTW cut it with room on every
		// phase. Planned charging now runs on three phases and only measured
		// overage cuts.
		{name: "night plan, auto, one-phase steps", start: time.Date(2026, 10, 22, 3, 0, 0, 0, time.Local), run: 40 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.Fuse.RatingA, p.HouseW, p.EVSE.Delay = 20, houseA(1.6, 1.7, 2.3), 40*time.Second
			},
			lp:   fieldNightLoadpoint,
			plan: planEV(11100),
			expect: []simExpect{
				{2 * time.Minute, func(s *simSite) error {
					if s.p.EVSE.DrawPhases != 3 || s.p.EVSE.DrawW < 4140 {
						return fmt.Errorf("car draws %.0f W on %d phases, want three", s.p.EVSE.DrawW, s.p.EVSE.DrawPhases)
					}
					return nil
				}},
				{40*time.Minute - time.Second, func(s *simSite) error {
					if s.cutsWithRoom > 0 || s.p.EVSE.SessionWh < 6000 {
						return fmt.Errorf("%d cuts with room on every phase, %.2f kWh in 40 min", s.cutsWithRoom, s.p.EVSE.SessionWh/1000)
					}
					return nil
				}},
			}},
		// A one-phase car (a plug-in hybrid) on the field night's table: the
		// plan's 4.8 kW three-phase order is 7 A a phase, which the car takes
		// on L1 alone, 1.6 kW. Once the meter shows it, the order is the
		// current per phase the car should take, and it gets 3.7 kW as fast
		// as the fuse limit lets the order rise.
		{name: "one-phase car, field table", start: time.Date(2026, 10, 22, 3, 0, 0, 0, time.Local), run: 15 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.Fuse.RatingA, p.HouseW, p.EVSE.Delay = 20, houseA(1.6, 1.7, 2.3), 40*time.Second
				p.Car.Phases, p.Car.MaxW = 1, 3700
			},
			lp:   fieldNightLoadpoint,
			plan: planEV(4800),
			expect: []simExpect{{8 * time.Minute, func(s *simSite) error {
				st, _ := s.mgr.State(s.lp.ID)
				if st.CarPhases != 1 || st.CarPhase != 1 || s.p.EVSE.DrawW < 3600 {
					return fmt.Errorf("car phases %d (L%d), car draws %.0f W", st.CarPhases, st.CarPhase, s.p.EVSE.DrawW)
				}
				return nil
			}}}},
		// A house load on one phase as a three-phase car starts, and a
		// two-phase car: neither is one-phase.
		{name: "three-phase car, kettle on L1 at the start", start: time.Date(2026, 10, 22, 3, 0, 0, 0, time.Local), run: 10 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.Fuse.RatingA, p.HouseW, p.EVSE.Delay, p.Car.MaxW = 25, houseA(1, 1, 1), 40*time.Second, 7360
				p.Events = []sitesim.Event{
					{At: 5 * time.Second, Do: func(p *sitesim.Plant) { p.HouseW = houseA(10, 1, 1) }},
					{At: 4 * time.Minute, Do: func(p *sitesim.Plant) { p.HouseW = houseA(1, 1, 1) }},
				}
			},
			lp: fieldNightLoadpoint, plan: planEV(11100),
			expect: []simExpect{{10*time.Minute - time.Second, carPhasesUnknown}}},
		{name: "two-phase car", start: time.Date(2026, 10, 22, 3, 0, 0, 0, time.Local), run: 10 * time.Minute,
			plant: func(p *sitesim.Plant) {
				p.Fuse.RatingA, p.HouseW, p.EVSE.Delay = 25, houseA(1, 1, 1), 40*time.Second
				p.Car.Phases, p.Car.MaxW = 2, 7360
			},
			lp: fieldNightLoadpoint, plan: planEV(11100),
			expect: []simExpect{{10*time.Minute - time.Second, carPhasesUnknown}}},
		{name: "a day", start: time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local), run: 24 * time.Hour, plant: simDay},
	}
}

// runSimScenario runs sc from start and returns the site, with the first
// broken invariant or expectation in failure.
func runSimScenario(sc simScenario, start time.Time) *simSite {
	p := simPlant(start)
	if sc.plant != nil {
		sc.plant(p)
	}
	lp := simLoadpoint()
	if sc.lp != nil {
		sc.lp(&lp)
	}
	s := &simSite{p: p, lp: lp, surplus: sc.surplus, plan: sc.plan, setup: sc.setup}
	if sc.plan != nil {
		setup := sc.setup
		s.setup = func(s *simSite) {
			s.mgr.SetSchedule(s.lp.ID, loadpoint.Schedule{SoC: .8})
			if setup != nil {
				setup(s)
			}
		}
	}
	s.boot()
	for _, e := range sc.expect {
		s.run(start.Add(e.at).Sub(p.Now))
		if err := e.check(s); err != nil {
			s.fail("%v", err)
		}
	}
	s.run(start.Add(sc.run).Sub(p.Now))
	return s
}

func TestSiteSimScenarios(t *testing.T) {
	for _, sc := range simScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			start := sc.start
			if start.IsZero() {
				start = simStart
			}
			s := runSimScenario(sc, start)
			t.Logf("peak phase %.1f A, %v over %.0f A; car idle with room %v", s.peakA, s.overLimit, s.p.Fuse.RatingA-evPhaseMarginA, s.idleWithRoom)
			switch {
			case sc.known == "" && s.failure != "":
				t.Fatal(s.failure)
			case sc.known != "" && s.failure == "":
				t.Fatalf("passes now: delete its known line (%s)", sc.known)
			case sc.known != "":
				t.Logf("fails as known (%s): %s", sc.known, s.failure)
			}
		})
	}
}

// Core reads only the plant's clock: a run that crosses the wall clock does
// what the same run a day earlier or later does. Battery-full latching once
// read SoC times from the wall clock.
func TestSiteSimIgnoresWallClock(t *testing.T) {
	sc := simScenario{run: 15 * time.Hour, plant: simDay}
	// The battery is full from 12:45, so it is full when the plant passes now.
	base := time.Now().Add(-13*time.Hour - 30*time.Minute).Truncate(time.Second)
	want := runSimScenario(sc, base)
	if want.failure != "" {
		t.Fatal(want.failure)
	}
	for _, shift := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		got := runSimScenario(sc, base.Add(shift))
		if i := firstCommandDiff(want, got, shift); i >= 0 {
			t.Fatalf("shifted %v: command %d is %v, want %v", shift, i, commandAt(got.commands, i), commandAt(want.commands, i))
		}
	}
}

func firstCommandDiff(a, b *simSite, shift time.Duration) int {
	for i := range max(len(a.commands), len(b.commands)) {
		x, y := commandAt(a.commands, i), commandAt(b.commands, i)
		if x.device != y.device || x.w != y.w || !x.at.Add(shift).Equal(y.at) {
			return i
		}
	}
	return -1
}

func commandAt(log []simCommand, i int) simCommand {
	if i < len(log) {
		return log[i]
	}
	return simCommand{}
}
