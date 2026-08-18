package control

// Joined EV + home-battery + planner clock.
//
// Isolated suites did not catch "EV charging is broken beside the battery":
//
//   - mpc tests call Optimize / ValidatePlan and never Tick the charger
//   - loadpoint tests inject a Directive and never run ComputeDispatch
//   - control golden / forecast_scenarios call ComputeDispatch with a
//     SlotDirective and a pre-baked EVChargingW — no loadpoint controller
//   - go/test/e2e has Ferroamp / Sungrow batteries and no EV charger
//
// This harness is the missing seam. One pinned clock runs:
//
//	Optimize (optional) → map Action to SlotDirective + loadpoint.Directive
//	→ loadpoint.Controller.Tick → ComputeDispatch → site identity
//
// Tick order matches go/cmd/ftw/main.go: charger first, then battery
// dispatch, then the next meter sample sees both commands. Surplus-only
// leftover uses loadpoint.SurplusAvailableForEVW, the same helper main.go
// wires into SetSiteSurplusForEV.
//
// Run: go test -run 'TestEVSite' ./go/internal/control

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const (
	evSiteMeter   = "meter"
	evSiteBattery = "pixii"
	evSitePV      = "pv"
	evSiteCharger = "easee"
	evSiteLP      = "garage"

	evSiteTickS = 5
)

// evSiteConfig is one joined-site scenario. Live PV/load stay at the
// values given here for the whole run (happy-path: live matches the
// slot the plan was built from). Plan is either injected or produced
// by mpc.Optimize when OptimizeSlots/OptimizeParams are set.
type evSiteConfig struct {
	Start time.Time
	Plan  mpc.Plan

	OptimizeSlots  []mpc.Slot
	OptimizeParams mpc.Params

	LP loadpoint.Config

	LoadW float64
	PVW   float64 // site-signed (generation is negative)

	BatCapWh     float64
	BatEnergyWh  float64
	BatMaxCharge float64

	EVCapWh    float64
	EVEnergyWh float64

	FuseMaxW float64
}

type evSiteTick struct {
	N                 int
	At                time.Time
	LoadW, PVW        float64
	BatW, EVW, GridW  float64
	BatCmdW, EVCmdW   float64
	SurplusW          float64
	PlanBatW, PlanEVW float64
	PlanGridW         float64
}

type evCmdSender struct {
	lastW   float64
	lastSet bool
}

func (s *evCmdSender) Send(_ context.Context, _ string, payload []byte) error {
	var d struct {
		PowerW float64 `json:"power_w"`
	}
	if err := json.Unmarshal(payload, &d); err != nil {
		return err
	}
	s.lastW = d.PowerW
	s.lastSet = true
	return nil
}

type evSite struct {
	t   *testing.T
	cfg evSiteConfig
	now time.Time
	dt  time.Duration

	loadW, pvW       float64
	batW, evW, gridW float64
	batEnergyWh      float64
	evEnergyWh       float64
	sessionWh        float64
	surplusOnly      bool
	plan             mpc.Plan

	store  *telemetry.Store
	st     *State
	mgr    *loadpoint.Manager
	lp     *loadpoint.Controller
	sender *evCmdSender
	caps   map[string]float64
	fuseW  float64

	ticks []evSiteTick
}

func newEVSite(t *testing.T, cfg evSiteConfig) *evSite {
	t.Helper()
	if cfg.Start.IsZero() {
		cfg.Start = time.Date(2026, 8, 18, 12, 0, 1, 0, time.UTC)
	}
	if cfg.BatCapWh <= 0 {
		cfg.BatCapWh = 20000
	}
	if cfg.BatEnergyWh <= 0 {
		cfg.BatEnergyWh = 4000
	}
	if cfg.BatMaxCharge <= 0 {
		cfg.BatMaxCharge = 10000
	}
	if cfg.EVCapWh <= 0 {
		cfg.EVCapWh = 60000
	}
	if cfg.FuseMaxW <= 0 {
		cfg.FuseMaxW = 25 * 230 * 3 // 17.25 kW — combo charge must fit
	}
	if cfg.LP.ID == "" {
		cfg.LP.ID = evSiteLP
	}
	if cfg.LP.DriverName == "" {
		cfg.LP.DriverName = evSiteCharger
	}
	if cfg.LP.MinChargeW <= 0 {
		cfg.LP.MinChargeW = 1380
	}
	if cfg.LP.MaxChargeW <= 0 {
		cfg.LP.MaxChargeW = 11040
	}
	if len(cfg.LP.AllowedStepsW) == 0 {
		cfg.LP.AllowedStepsW = []float64{0, 1380, 4140, 6900, 11040}
	}
	if cfg.LP.PhaseSplitW <= 0 {
		cfg.LP.PhaseSplitW = 3680
	}

	plan := cfg.Plan
	if len(cfg.OptimizeSlots) > 0 {
		plan = mpc.Optimize(cfg.OptimizeSlots, cfg.OptimizeParams)
		if len(plan.Actions) == 0 {
			t.Fatalf("Optimize returned no actions")
		}
	}
	if len(plan.Actions) == 0 {
		t.Fatal("ev site needs an injected plan or OptimizeSlots")
	}

	s := &evSite{
		t:           t,
		cfg:         cfg,
		now:         cfg.Start,
		dt:          evSiteTickS * time.Second,
		loadW:       cfg.LoadW,
		pvW:         cfg.PVW,
		batEnergyWh: cfg.BatEnergyWh,
		evEnergyWh:  cfg.EVEnergyWh,
		surplusOnly: cfg.LP.SurplusOnly,
		plan:        plan,
		store:       telemetry.NewStore(),
		sender:      &evCmdSender{},
		caps:        map[string]float64{evSiteBattery: cfg.BatCapWh},
		fuseW:       cfg.FuseMaxW,
	}
	s.gridW = s.loadW + s.pvW + s.batW + s.evW

	st := NewState(0, 0, evSiteMeter)
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewEnabled = false
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.BatteryCoversEV = false
	st.DriverLimits = map[string]PowerLimits{
		evSiteBattery: {MaxChargeW: cfg.BatMaxCharge, MaxDischargeW: cfg.BatMaxCharge},
	}
	st.clock = func() time.Time { return s.now }
	st.SlotDirective = func(now time.Time) (SlotDirective, bool) {
		d, _, ok := s.directives(now)
		return d, ok
	}
	s.st = st

	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{cfg.LP})
	s.mgr = mgr

	lp := loadpoint.NewController(mgr,
		func(now time.Time) (loadpoint.Directive, bool) {
			_, d, ok := s.directives(now)
			return d, ok
		},
		func(driver string) (loadpoint.EVSample, bool) {
			if driver != cfg.LP.DriverName {
				return loadpoint.EVSample{}, false
			}
			return loadpoint.EVSample{
				PowerW:        s.evW,
				SessionWh:     s.sessionWh,
				Connected:     true,
				RequestActive: true,
			}, true
		},
		s.sender.Send,
	)
	lp.SetSiteSurplusForEV(func() (float64, bool) {
		return loadpoint.SurplusAvailableForEVW(s.gridW, s.batW, s.evW, lp.AnyLoadpointSurplusActive()), true
	})
	lp.SetNearTermPeakSurplusW(func(time.Duration) (float64, bool) {
		leftover := -(s.loadW + s.pvW)
		if leftover < 0 {
			leftover = 0
		}
		return leftover, true
	})
	s.lp = lp
	s.publish()
	return s
}

func (s *evSite) directives(now time.Time) (SlotDirective, loadpoint.Directive, bool) {
	s.t.Helper()
	nowMs := now.UnixMilli()
	for _, a := range s.plan.Actions {
		endMs := a.SlotStartMs + int64(a.SlotLenMin)*60*1000
		if nowMs < a.SlotStartMs || nowMs >= endMs {
			continue
		}
		hours := float64(a.SlotLenMin) / 60.0
		lpWh := map[string]float64{}
		if a.LoadpointW != 0 {
			lpWh[s.cfg.LP.ID] = a.LoadpointW * hours
		}
		start := time.UnixMilli(a.SlotStartMs)
		end := time.UnixMilli(endMs)
		return SlotDirective{
				SlotStart:         start,
				SlotEnd:           end,
				BatteryEnergyWh:   a.BatteryW * hours,
				Strategy:          "arbitrage",
				PlannedGridW:      a.GridW,
				HasPlannedGridW:   true,
				LoadpointEnergyWh: lpWh,
			}, loadpoint.Directive{
				SlotStart:         start,
				SlotEnd:           end,
				LoadpointEnergyWh: lpWh,
			}, true
	}
	return SlotDirective{}, loadpoint.Directive{}, false
}

func (s *evSite) actionAt(now time.Time) (mpc.Action, bool) {
	nowMs := now.UnixMilli()
	for _, a := range s.plan.Actions {
		endMs := a.SlotStartMs + int64(a.SlotLenMin)*60*1000
		if nowMs >= a.SlotStartMs && nowMs < endMs {
			return a, true
		}
	}
	return mpc.Action{}, false
}

func (s *evSite) publish() {
	s.t.Helper()
	soc := s.batEnergyWh / s.cfg.BatCapWh
	if soc < 0 {
		soc = 0
	}
	if soc > 1 {
		soc = 1
	}
	// Kalman first-sample is exact; repeats settle after a step so
	// ComputeDispatch sees approximately the physics, not a lag that
	// would hide the combo under test.
	for i := 0; i < 8; i++ {
		s.store.Update(evSiteMeter, telemetry.DerMeter, s.gridW, nil, nil)
		s.store.Update(evSiteBattery, telemetry.DerBattery, s.batW, &soc, nil)
		s.store.Update(evSitePV, telemetry.DerPV, s.pvW, nil, nil)
		s.store.Update(evSiteCharger, telemetry.DerEV, s.evW, nil, nil)
	}
	s.store.DriverHealthMut(evSiteMeter).RecordSuccess()
	s.store.DriverHealthMut(evSiteBattery).RecordSuccess()
	s.store.DriverHealthMut(evSitePV).RecordSuccess()
	s.store.DriverHealthMut(evSiteCharger).RecordSuccess()
}

func (s *evSite) tick() evSiteTick {
	s.t.Helper()
	s.publish()
	surplus := loadpoint.SurplusAvailableForEVW(s.gridW, s.batW, s.evW, s.lp.AnyLoadpointSurplusActive())
	planA, _ := s.actionAt(s.now)

	s.sender.lastSet = false
	s.lp.Tick(context.Background(), s.now)
	evCmd := 0.0
	if s.sender.lastSet {
		evCmd = s.sender.lastW
	}

	lpStates := s.mgr.States()
	s.st.EVSurplusOnlyReserveW = loadpoint.SurplusReserveW(lpStates, nil)
	s.st.EVSurplusOnlyChargingW = loadpoint.SurplusChargingW(lpStates)
	s.st.EVCurtailHeadroomW = loadpoint.SurplusPotentialW(lpStates)

	targets := ComputeDispatch(s.store, s.st, s.caps, s.fuseW)
	var batCmd float64
	for _, tg := range targets {
		if tg.Driver == evSiteBattery {
			batCmd = tg.TargetW
		}
	}

	hours := s.dt.Hours()
	s.evW = evCmd
	if s.evW < 0 {
		s.evW = 0
	}
	s.batW = batCmd
	headroomWh := s.cfg.BatCapWh*0.95 - s.batEnergyWh
	if s.batW > 0 && s.batW*hours > headroomWh && hours > 0 {
		s.batW = headroomWh / hours
		if s.batW < 0 {
			s.batW = 0
		}
	}
	floorWh := s.cfg.BatCapWh * 0.10
	if s.batW < 0 && s.batEnergyWh+s.batW*hours/0.95 < floorWh && hours > 0 {
		s.batW = -(s.batEnergyWh - floorWh) * 0.95 / hours
		if s.batW > 0 {
			s.batW = 0
		}
	}
	s.gridW = s.loadW + s.pvW + s.batW + s.evW
	if s.batW >= 0 {
		s.batEnergyWh += s.batW * hours * 0.95
	} else {
		s.batEnergyWh += s.batW * hours / 0.95
	}
	s.evEnergyWh += s.evW * hours * 0.90
	s.sessionWh += s.evW * hours

	rec := evSiteTick{
		N:         len(s.ticks),
		At:        s.now,
		LoadW:     s.loadW,
		PVW:       s.pvW,
		BatW:      s.batW,
		EVW:       s.evW,
		GridW:     s.gridW,
		BatCmdW:   batCmd,
		EVCmdW:    evCmd,
		SurplusW:  surplus,
		PlanBatW:  planA.BatteryW,
		PlanEVW:   planA.LoadpointW,
		PlanGridW: planA.GridW,
	}
	s.checkInvariants(rec)
	s.ticks = append(s.ticks, rec)
	s.now = s.now.Add(s.dt)
	return rec
}

func (s *evSite) run(n int) []evSiteTick {
	s.t.Helper()
	out := make([]evSiteTick, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s.tick())
	}
	return out
}

func (s *evSite) leftoverW() float64 {
	v := -(s.loadW + s.pvW)
	if v < 0 {
		return 0
	}
	return v
}

func (s *evSite) checkInvariants(rec evSiteTick) {
	s.t.Helper()
	ident := rec.LoadW + rec.PVW + rec.BatW + rec.EVW
	if math.Abs(rec.GridW-ident) > 1 {
		s.t.Fatalf("tick %d: grid identity %.1f != load+pv+bat+ev %.1f", rec.N, rec.GridW, ident)
	}
	if rec.GridW > s.fuseW+50 {
		s.t.Fatalf("tick %d: grid %.0f W over fuse %.0f W", rec.N, rec.GridW, s.fuseW)
	}
	if s.surplusOnly && rec.EVW > 50 {
		if rec.EVW > s.leftoverW()+50 {
			s.t.Fatalf("tick %d: surplus-only EV %.0f W exceeds leftover PV after house %.0f W (grid=%.0f bat=%.0f)",
				rec.N, rec.EVW, s.leftoverW(), rec.GridW, rec.BatW)
		}
		if rec.BatW < -50 {
			house := rec.LoadW + rec.PVW
			if house < 0 {
				house = 0
			}
			if -rec.BatW > house+50 {
				s.t.Fatalf("tick %d: battery discharge %.0f W feeds surplus-only EV %.0f W (house residual %.0f W)",
					rec.N, rec.BatW, rec.EVW, house)
			}
		}
	}
}

func (s *evSite) requireCombo(afterTicks int) evSiteTick {
	s.t.Helper()
	for _, rec := range s.ticks {
		if rec.N < afterTicks {
			continue
		}
		if rec.EVW > 1000 && rec.BatW > 500 && rec.GridW > 100 {
			return rec
		}
	}
	s.t.Fatalf("no tick after %d had EV charging from leftover PV while the home battery grid-charged; ticks=%s",
		afterTicks, s.dumpTicks())
	return evSiteTick{}
}

func (s *evSite) requireIdleEV(afterTicks int) {
	s.t.Helper()
	for _, rec := range s.ticks {
		if rec.N < afterTicks {
			continue
		}
		if rec.EVW > 50 {
			s.t.Fatalf("tick %d: surplus-only EV imported without leftover PV: ev=%.0f grid=%.0f bat=%.0f pv=%.0f; ticks=%s",
				rec.N, rec.EVW, rec.GridW, rec.BatW, rec.PVW, s.dumpTicks())
		}
	}
}

func (s *evSite) dumpTicks() string {
	b := make([]byte, 0, 256)
	for _, rec := range s.ticks {
		b = append(b, []byte(
			rec.At.Format("15:04:05")+" ev="+itoa(rec.EVW)+" bat="+itoa(rec.BatW)+" grid="+itoa(rec.GridW)+" surplus="+itoa(rec.SurplusW)+"\n",
		)...)
	}
	return string(b)
}

func itoa(w float64) string {
	return jsonNumber(w)
}

func jsonNumber(w float64) string {
	b, _ := json.Marshal(math.Round(w))
	return string(b)
}

func injectedChargePlan(start time.Time, slotMin int, batW, evW, loadW, pvW float64) mpc.Plan {
	gridW := loadW + pvW + batW + evW
	return mpc.Plan{
		GeneratedAtMs: start.UnixMilli(),
		Mode:          mpc.ModeArbitrage,
		HorizonSlots:  1,
		Actions: []mpc.Action{{
			SlotStartMs: start.UnixMilli(),
			SlotLenMin:  slotMin,
			BatteryW:    batW,
			LoadpointW:  evW,
			GridW:       gridW,
			LoadW:       loadW,
			PVW:         pvW,
		}},
	}
}

func surplusOnlyGarage() loadpoint.Config {
	return loadpoint.Config{
		ID:            evSiteLP,
		DriverName:    evSiteCharger,
		MinChargeW:    1380,
		MaxChargeW:    11040,
		AllowedStepsW: []float64{0, 1380, 4140, 6900, 11040},
		PhaseSplitW:   3680,
		SurplusOnly:   true,
	}
}
