package control

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// roomCharger applies each command delay after it was sent, on the phases
// the command names: a single-phase command lands on L1.
type roomCharger struct {
	cfg       loadpoint.Config
	delay     time.Duration
	connected bool
	queue     []roomCommand
	drawW     float64
	onePhase  bool
}

type roomCommand struct {
	at       time.Time
	w        float64
	onePhase bool
}

// roomSite is a 25 A three-phase site with chargers that declare their
// delay as power_max_age_s. The meter reads every second; the loadpoint
// ticks every 5 s, wired as main.go wires it.
type roomSite struct {
	t        *testing.T
	now      time.Time
	start    time.Time
	store    *telemetry.Store
	state    *State
	mgr      *loadpoint.Manager
	ctrl     *loadpoint.Controller
	houseA   [3]float64
	chargers []*roomCharger
	static   bool // EVFuseBudget reads the configured phase modes
	worstA   float64
}

func newRoomSite(t *testing.T, houseA float64, chargers ...*roomCharger) *roomSite {
	t.Helper()
	s := &roomSite{t: t, now: time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC), store: telemetry.NewStore(), chargers: chargers}
	s.start = s.now
	s.houseA = [3]float64{houseA, houseA, houseA}
	s.state = NewState(0, 0, "meter")
	s.state.SiteFuseAmps, s.state.SiteFuseVoltage, s.state.SiteFusePhases = 25, 230, 3
	s.state.clock = func() time.Time { return s.now }
	byDriver := map[string]*roomCharger{}
	var configs []loadpoint.Config
	for _, c := range chargers {
		byDriver[c.cfg.DriverName] = c
		configs = append(configs, c.cfg)
	}
	s.mgr = loadpoint.NewManager()
	s.mgr.Load(configs)
	s.ctrl = loadpoint.NewController(s.mgr,
		func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false },
		func(driver string) (loadpoint.EVSample, bool) {
			c := byDriver[driver]
			return loadpoint.EVSample{Connected: c.connected, RequestActive: c.connected, PowerW: c.drawW, PowerAt: s.now}, true
		},
		func(_ context.Context, driver string, payload []byte) error {
			var cmd struct {
				Action    string  `json:"action"`
				PowerW    float64 `json:"power_w"`
				PhaseMode string  `json:"phase_mode"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_set_current" {
				c := byDriver[driver]
				c.queue = append(c.queue, roomCommand{s.now, cmd.PowerW, loadpoint.PhaseFor(cmd.PhaseMode, cmd.PowerW, 3680) == 1})
			}
			return nil
		})
	s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	s.ctrl.SetFuseEVMax(func() (float64, bool) {
		configs := s.ctrl.FuseConfigs()
		if s.static {
			configs = s.mgr.Configs()
		}
		return EVFuseBudget(s.store, s.state, s.mgr.States(), configs, 25*230*3)
	})
	s.ctrl.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(s.state) })
	return s
}

func (s *roomSite) phaseA() [3]float64 {
	a := s.houseA
	for _, c := range s.chargers {
		switch {
		case !c.connected:
		case c.onePhase:
			a[0] += c.drawW / 230
		default:
			for i := range a {
				a[i] += c.drawW / 690
			}
		}
	}
	return a
}

func (s *roomSite) run(d time.Duration) {
	s.t.Helper()
	for end := s.now.Add(d); s.now.Before(end); {
		s.now = s.now.Add(time.Second)
		for _, c := range s.chargers {
			for len(c.queue) > 0 && !s.now.Before(c.queue[0].at.Add(c.delay)) {
				c.drawW, c.onePhase, c.queue = c.queue[0].w, c.queue[0].onePhase, c.queue[1:]
			}
			if !c.connected {
				c.drawW = 0
			}
			data, _ := json.Marshal(map[string]any{"power_max_age_s": c.delay.Seconds()})
			s.store.Update(c.cfg.DriverName, telemetry.DerEV, c.drawW, nil, data)
		}
		a := s.phaseA()
		data, _ := json.Marshal(map[string]any{"control_power_available": true,
			"control_power_observed_at": s.now.Format(time.RFC3339Nano), "l1_a": a[0], "l2_a": a[1], "l3_a": a[2]})
		s.store.Update("meter", telemetry.DerMeter, (a[0]+a[1]+a[2])*230, nil, data)
		s.store.Get("meter", telemetry.DerMeter).UpdatedAt = s.now
		s.store.RecordDriverSuccess("meter")
		s.worstA = math.Max(s.worstA, max(a[0], a[1], a[2]))
		if s.now.Sub(s.start)%(5*time.Second) == 0 {
			s.ctrl.Tick(context.Background(), s.now)
		}
	}
}

// requireWithinLimit fails when a phase passed the 24 A working limit.
func (s *roomSite) requireWithinLimit(after string) {
	s.t.Helper()
	if limitA := s.state.SiteFuseAmps - evPhaseMarginA; s.worstA > limitA+1e-6 {
		s.t.Fatalf("%s: a phase reached %.1f A, over the %.0f A working limit", after, s.worstA, limitA)
	}
}

func roomTestCharger(id string, delay time.Duration) *roomCharger {
	steps := []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680,
		4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}
	return &roomCharger{cfg: loadpoint.Config{ID: id, DriverName: id, MinChargeW: 1380, MaxChargeW: 11040, AllowedStepsW: steps},
		delay: delay, connected: true}
}

// A 3p offer moved to 1p at nearly the same watts triples the current on
// one phase. The budget counts current per phase, so the switch needs room
// for the rise on that phase. Before, 7,360 W went out on L1 beside 12 A:
// 36 A for 50 s with a 5 s charger and for 90 s with a 90 s charger.
func TestEVFusePhaseSwitchNeedsRoomOnThePhase(t *testing.T) {
	for _, delay := range []time.Duration{5 * time.Second, 90 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			c := roomTestCharger("a", delay)
			s := newRoomSite(t, 5, c)
			s.houseA[0] = 12
			s.ctrl.SetManualHold("a", loadpoint.ManualHold{PowerW: 8280, PhaseMode: "3p", Persistent: true})
			s.run(5 * time.Minute)
			s.ctrl.SetManualHold("a", loadpoint.ManualHold{PowerW: 7360, PhaseMode: "1p", Persistent: true})
			s.run(6 * time.Minute)
			s.requireWithinLimit("after 3p to 1p")
			if !c.onePhase || c.drawW < 1380 {
				t.Fatalf("the car draws %.0f W (1p %v), want a 1p charge", c.drawW, c.onePhase)
			}
		})
	}
}

// Two cars at 8 A; the first is raised. Room a slow charger's cut frees is
// not there until that charger can have applied it, and a rise takes only
// the room net of every charger's reservation. Before, the raised car took
// the other's share in one tick: 27 A for 60 s.
func TestEVFuseRaisedCarTakesOnlyFreeRoom(t *testing.T) {
	for _, tc := range []struct {
		name           string
		aDelay, bDelay time.Duration
	}{
		{"a_fast_b_slow", 5 * time.Second, 90 * time.Second},
		{"both_slow", 60 * time.Second, 120 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := roomTestCharger("a", tc.aDelay), roomTestCharger("b", tc.bDelay)
			s := newRoomSite(t, 3, a, b)
			s.ctrl.SetManualHold("a", loadpoint.ManualHold{PowerW: 5520, PhaseMode: "3p", Persistent: true})
			s.ctrl.SetManualHold("b", loadpoint.ManualHold{PowerW: 5520, PhaseMode: "3p", Persistent: true})
			s.run(10 * time.Minute)
			s.ctrl.SetManualHold("a", loadpoint.ManualHold{PowerW: 11040, PhaseMode: "3p", Persistent: true})
			s.run(8 * time.Minute)
			s.requireWithinLimit("after car a was raised")
			if a.drawW <= 5520 {
				t.Fatalf("car a stayed at %.0f W beside 3 A of house and car b at %.0f W", a.drawW, b.drawW)
			}
		})
	}
}

// A 1p hold over a 3p loadpoint: each offer counts on the phases its own
// command names, never the configured ones, even when the budget is read
// with the configured phase mode. Before, 1p offers counted as 3p and L1
// reached 39 A. The hold asks 24 A; the charger is 16 A per phase, so the
// car climbs from 9 A beside 15 A to the charger's 16 A once L1 falls.
func TestEVFuseOnePhaseOffersCountOnTheirPhase(t *testing.T) {
	for _, delay := range []time.Duration{5 * time.Second, 90 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			c := roomTestCharger("a", delay)
			c.cfg.PhaseMode = "3p"
			s := newRoomSite(t, 5, c)
			s.static = true
			s.houseA[0] = 15
			s.ctrl.SetManualHold("a", loadpoint.ManualHold{PowerW: 5520, PhaseMode: "1p", Persistent: true})
			s.run(5 * time.Minute)
			s.houseA[0] = 5
			s.run(6 * time.Minute)
			s.requireWithinLimit("after L1 fell")
			if !c.onePhase || c.drawW < 3680 {
				t.Fatalf("the car draws %.0f W (1p %v), want it to climb on L1", c.drawW, c.onePhase)
			}
		})
	}
}

// A silent meter never frees room. An offer sent after the last reading
// counts against it until a newer reading could show the offer.
func TestEVFusePhaseRoomHoldsOnSilentMeter(t *testing.T) {
	store, state, points, configs := evMeterBudgetFixture(t, 15*690, []float64{15, 15, 15}, 0, "3p")
	now := time.Now().Truncate(time.Second)
	state.clock = func() time.Time { return now }
	store.Get("meter", telemetry.DerMeter).UpdatedAt = now
	points[0].CommandedPhases = 3
	room := func() float64 {
		t.Helper()
		EVFuseBudget(store, state, points, configs, 17250)
		roomA, ok := EVFusePhaseRoomA(state)
		if !ok {
			t.Fatal("no phase room from a meter with phase currents")
		}
		return roomA
	}
	if got := room(); math.Abs(got-9) > 1e-9 {
		t.Fatalf("room %.2f A beside 15 A, want 9 A", got)
	}
	points[0].CommandedW = 5520
	now = now.Add(time.Second)
	if got := room(); math.Abs(got-1) > 1e-9 {
		t.Fatalf("room %.2f A after an 8 A offer, want 1 A", got)
	}
	now = now.Add(10 * time.Minute)
	if got := room(); math.Abs(got-1) > 1e-9 {
		t.Fatalf("room %.2f A after ten silent minutes, want 1 A", got)
	}
}
