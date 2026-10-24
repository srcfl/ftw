package control

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type evTimed struct {
	at time.Time
	w  float64
}

// evReadings is how the charger's own power readings behave. The hard
// limit must not care.
type evReadings int

const (
	evReadingsNormal  evReadings = iota // a poll every 5 s, telemetryDelay old
	evReadingsMissing                   // no reading at all
	evReadingsHigh                      // 2 kW above the true draw
)

// evMeterSite runs one Easee-like 3p charger with Charge now against a
// simulated 25 A site. A new current takes chargerDelay to reach the car.
// Like Easee Cloud, the driver polls every 5 s and receives the draw as it
// was telemetryDelay ago. The meter reads the physical import every second
// unless silenced; dispatch stops when it is a minute old, as in main.go.
type evMeterSite struct {
	t                            *testing.T
	now                          time.Time
	store                        *telemetry.Store
	state                        *State
	mgr                          *loadpoint.Manager
	ctrl                         *loadpoint.Controller
	chargerDelay, telemetryDelay time.Duration
	chargerMaxAgeS               float64 // the driver's power_max_age_s, as Easee Cloud declares 180
	readings                     evReadings
	houseW                       float64
	housePhaseA                  [3]float64
	carMaxW                      float64 // the car takes at most this
	carPaused, chargerIgnores    bool
	meterSilent                  bool
	loneZeroAt                   time.Time // one 0 W reading at this time
	appliedW, evW                float64
	reportedW                    float64
	reportedAt, nextPoll         time.Time
	meterAt                      time.Time
	commands, draws              []evTimed
	offers                       []evTimed // every ev_set_current
	maxGridW                     float64
	config                       memConfigStore // survives a restart
	sessionWh                    float64
	ceilingW                     float64
	unplugged                    bool
	onBoot, beforeTick           func() // after each boot; before each controller tick
}

// memConfigStore stands in for state.db across a simulated Core restart.
type memConfigStore map[string]string

func (m memConfigStore) LoadConfig(key string) (string, bool) { v, ok := m[key]; return v, ok }
func (m memConfigStore) SaveConfig(key, value string) error   { m[key] = value; return nil }

func newEVMeterSite(t *testing.T, ceilingW, houseW float64) *evMeterSite {
	t.Helper()
	return newEVMeterSiteAt(t, ceilingW, houseW, time.Now().Truncate(time.Second))
}

func newEVMeterSiteAt(t *testing.T, ceilingW, houseW float64, start time.Time) *evMeterSite {
	t.Helper()
	s := &evMeterSite{t: t, now: start, store: telemetry.NewStore(),
		chargerDelay: 5 * time.Second, telemetryDelay: 10 * time.Second, carMaxW: math.Inf(1),
		config: memConfigStore{}, ceilingW: ceilingW}
	s.setHouse(houseW)
	s.boot()
	s.draws = []evTimed{{s.now.Add(-time.Hour), 0}}
	s.report(0)
	return s
}

// boot starts Core: new control state, loadpoint manager and controller.
// The charger, car, meter and state.db keep running across it.
func (s *evMeterSite) boot() {
	s.state = NewState(0, 0, "meter")
	s.state.SiteFuseAmps, s.state.SiteFuseVoltage, s.state.SiteFusePhases, s.state.SiteFuseSafetyA = 25, 230, 3, 0.5
	s.state.PeakImportCeilingW = s.ceilingW
	s.state.clock = func() time.Time { return s.now }
	cfg := loadpoint.Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 10350, PhaseMode: "3p",
		AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350}}
	s.mgr = loadpoint.NewManager()
	s.mgr.Load([]loadpoint.Config{cfg})
	s.mgr.SetSessionStore(s.config)
	s.ctrl = loadpoint.NewController(s.mgr,
		func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false },
		func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: !s.unplugged, RequestActive: true, PowerW: s.reportedW, PowerAt: s.reportedAt,
				DeviceID: "easee:EH000001", SessionWh: s.sessionWh, EnergyAt: s.reportedAt}, true
		},
		func(_ context.Context, _ string, payload []byte) error {
			var cmd struct {
				Action string  `json:"action"`
				PowerW float64 `json:"power_w"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_set_current" {
				s.offers = append(s.offers, evTimed{s.now, cmd.PowerW})
				s.commands = append(s.commands, evTimed{s.now.Add(s.chargerDelay), cmd.PowerW})
			}
			return nil
		})
	s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	s.ctrl.SetPerPhaseMeterAmps(func() (float64, float64, float64, bool) {
		a := s.phaseA()
		return a[0], a[1], a[2], true
	})
	s.ctrl.SetFuseEVMax(func() (float64, bool) {
		return EVFuseBudget(s.store, s.state, s.mgr.States(), s.mgr.Configs(), 17250)
	})
	s.ctrl.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(s.state) })
	s.ctrl.SetFuseLimitReason(func() string { return EVLimitReason(s.state) })
	s.ctrl.SetManualHold(cfg.ID, loadpoint.ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	if s.onBoot != nil {
		s.onBoot()
	}
}

func (s *evMeterSite) setHouse(w float64) {
	s.houseW = w
	a := w / 690
	s.housePhaseA = [3]float64{a, a, a}
}

func (s *evMeterSite) phaseA() [3]float64 {
	a := s.housePhaseA
	for i := range a {
		a[i] += s.evW / 690
	}
	return a
}

func (s *evMeterSite) emit(driver string, kind telemetry.DerType, w float64, extra map[string]any) {
	d := map[string]any{"control_power_available": true, "control_power_observed_at": s.now.Format(time.RFC3339Nano)}
	for k, v := range extra {
		d[k] = v
	}
	data, _ := json.Marshal(d)
	s.store.Update(driver, kind, w, nil, data)
	s.store.Get(driver, kind).UpdatedAt = s.now // Receipt on the simulated clock.
	s.store.RecordDriverSuccess(driver)
}

// report delivers one poll. The receipt time is all the driver knows.
func (s *evMeterSite) report(w float64) {
	s.reportedW, s.reportedAt = w, s.now
	var extra map[string]any
	if s.chargerMaxAgeS > 0 {
		extra = map[string]any{"power_max_age_s": s.chargerMaxAgeS}
	}
	s.emit("easee", telemetry.DerEV, w, extra)
}

func (s *evMeterSite) cloudW() float64 {
	w, seen := 0.0, s.now.Add(-s.telemetryDelay)
	for _, d := range s.draws {
		if !d.at.After(seen) {
			w = d.w
		}
	}
	return w
}

func (s *evMeterSite) step() {
	s.now = s.now.Add(time.Second)
	for len(s.commands) > 0 && !s.commands[0].at.After(s.now) {
		if !s.chargerIgnores {
			s.appliedW = s.commands[0].w
		}
		s.commands = s.commands[1:]
	}
	draw := math.Min(s.appliedW, s.carMaxW)
	if s.carPaused {
		draw = 0
	}
	if draw != s.evW {
		s.evW = draw
		s.draws = append(s.draws, evTimed{s.now, draw})
	}
	s.sessionWh += s.evW / 3600
	if !s.now.Before(s.nextPoll) && s.readings != evReadingsMissing {
		w := s.cloudW()
		if s.readings == evReadingsHigh {
			w += 2000
		}
		s.report(w)
		s.nextPoll = s.now.Add(5 * time.Second)
	}
	if s.now.Equal(s.loneZeroAt) {
		s.report(0)
	}
	grid := s.houseW + s.evW
	s.maxGridW = math.Max(s.maxGridW, grid)
	if !s.meterSilent {
		a := s.phaseA()
		s.emit("meter", telemetry.DerMeter, grid, map[string]any{"l1_a": a[0], "l2_a": a[1], "l3_a": a[2]})
		s.meterAt = s.now
	}
	if s.beforeTick != nil {
		s.beforeTick()
	}
	s.ctrl.TickWithDispatch(context.Background(), s.now, s.now.Sub(s.meterAt) <= time.Minute)
	s.state.SetEVFuseDemands(s.ctrl.FuseDemands())
}

func (s *evMeterSite) run(d time.Duration) {
	for end := s.now.Add(d); s.now.Before(end); {
		s.step()
	}
}

func (s *evMeterSite) offer() float64 {
	if len(s.offers) == 0 {
		return 0
	}
	return s.offers[len(s.offers)-1].w
}

func (s *evMeterSite) offersSince(at time.Time) []evTimed {
	var out []evTimed
	for _, o := range s.offers {
		if !o.at.Before(at) {
			out = append(out, o)
		}
	}
	return out
}

func offerWatts(offers []evTimed) []float64 {
	out := make([]float64, len(offers))
	for i, o := range offers {
		out[i] = o.w
	}
	return out
}

// steady brings the car to a constant draw.
func (s *evMeterSite) steady(wantOffer float64) {
	s.t.Helper()
	s.run(3 * time.Minute)
	if s.offer() != wantOffer || s.evW != wantOffer {
		s.t.Fatalf("setup: offer %.0f draw %.0f, want %.0f W", s.offer(), s.evW, wantOffer)
	}
}

// #1674, 05:49:15: the import ceiling was off and the car drew 10,242 W at
// 15 A. One 0 W Easee sample arrived while Pixii read 20.45/17.05/15.87 A
// and 12,323 W on the 25 A fuse. Proof-based credit gave
// (25-1-20.45)x230x3 = 2,449.5 W, below the 4,140 W step, and stopped the
// charge. The meter shows room on every phase, so the offer must hold.
func TestEVHardLimitHoldsOfferThroughLoneZeroSample(t *testing.T) {
	s := newEVMeterSite(t, 0, 2081)
	s.carMaxW = 10242
	s.housePhaseA = [3]float64{20.45 - 10242.0/690, 17.05 - 10242.0/690, 15.87 - 10242.0/690}
	s.run(3 * time.Minute)
	if s.offer() != 10350 || s.evW != 10242 {
		t.Fatalf("setup: offer %.0f draw %.0f", s.offer(), s.evW)
	}
	if a := s.phaseA(); math.Abs(a[0]-20.45) > .01 || math.Abs(s.houseW+s.evW-12323) > .01 {
		t.Fatalf("setup: L1 %.2f A grid %.0f W", a[0], s.houseW+s.evW)
	}
	zeroAt := s.now
	s.report(0) // 05:49:13, the lone zero
	s.step()
	capW, _ := EVFuseBudget(s.store, s.state, s.mgr.States(), s.mgr.Configs(), 17250)
	s.report(10242) // 05:49:18
	s.run(time.Minute)
	if capW < 10350 {
		t.Fatalf("lone 0 W sample cut the hard cap to %.1f W", capW)
	}
	for _, w := range offerWatts(s.offersSince(zeroAt)) {
		if w != 10350 {
			t.Fatalf("offer left 10,350 W after a lone 0 W sample: %v", offerWatts(s.offersSince(zeroAt)))
		}
	}
}

// Every update restarts Core. At the 05:49 currents the meter alone shows
// 2,449.5 W of room, below a start. Our own last order, kept for the same
// plug-in session, holds the charge through the restart.
func TestEVHardLimitRestartKeepsChargingCarAtFieldCurrents(t *testing.T) {
	s := newEVMeterSite(t, 0, 2081)
	s.carMaxW = 10242
	s.housePhaseA = [3]float64{20.45 - 10242.0/690, 17.05 - 10242.0/690, 15.87 - 10242.0/690}
	s.run(3 * time.Minute)
	if s.offer() != 10350 {
		t.Fatalf("setup: offer %.0f W", s.offer())
	}
	restartAt := s.now.Add(time.Second)
	s.boot()
	s.run(time.Minute)
	got := offerWatts(s.offersSince(restartAt))
	if len(got) == 0 {
		t.Fatal("no command after the restart")
	}
	for _, w := range got {
		if w != 10350 {
			t.Fatalf("restart moved the offer: %v", got)
		}
	}
}

// A new plug-in session while Core was down is not ours to hold: its energy
// counter went back, so the pool starts idle on measured room.
func TestEVHardLimitRestartAfterReplugStartsIdle(t *testing.T) {
	s := newEVMeterSite(t, 0, 2081)
	s.carMaxW = 10242
	s.housePhaseA = [3]float64{20.45 - 10242.0/690, 17.05 - 10242.0/690, 15.87 - 10242.0/690}
	s.run(3 * time.Minute)
	s.sessionWh = 1 // replugged during the restart
	restartAt := s.now.Add(time.Second)
	s.boot()
	s.step()
	if got := offerWatts(s.offersSince(restartAt)); len(got) == 0 || got[0] != 0 {
		t.Fatalf("a new session inherited the old order: %v", got)
	}
}

// #1674, 23:45: a 7.5 kW import ceiling, the car at 9 A beside 0.9 kW of
// house load, and Easee telemetry 5-15 s late. The charge started and
// stopped every 30 s. It must start once, keep a step of margin and stay
// inside the ceiling.
func TestEVHardLimitStartsOnceUnderImportCeilingWithLateTelemetry(t *testing.T) {
	for _, delay := range []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			s := newEVMeterSite(t, 7500, 900)
			s.telemetryDelay = delay
			s.run(5 * time.Minute)
			started := false
			for _, o := range s.offers {
				if o.w > 0 {
					started = true
				} else if started {
					t.Fatalf("start/stop cycle: offers %v", offerWatts(s.offers))
				}
			}
			// 7,500 - 900 W leaves 6,600 W; one 690 W step stays free.
			if !started || s.offer() != 5520 {
				t.Fatalf("offer %.0f W, want a steady 5,520 W", s.offer())
			}
			if s.maxGridW > 7500 {
				t.Fatalf("grid reached %.0f W above the 7,500 W ceiling", s.maxGridW)
			}
		})
	}
}

// A real house step pushes the grid to 8.15 kW under the 7.5 kW ceiling.
// The offer steps down by the measured overage, once, and never stops.
func TestEVHardLimitHouseStepStepsDownOnce(t *testing.T) {
	s := newEVMeterSite(t, 7500, 900)
	s.steady(5520)
	stepAt := s.now
	s.setHouse(2630) // 2,630 + 5,520 = 8,150 W
	s.run(evSettle + 10*time.Second)
	got := offerWatts(s.offersSince(stepAt))
	for _, w := range got {
		if w != 5520 && w != 4830 {
			t.Fatalf("house step gave offers %v, want one step to 4,830 W", got)
		}
	}
	if s.offer() != 4830 || s.houseW+s.evW > 7500 {
		t.Fatalf("offer %.0f W grid %.0f W after the step", s.offer(), s.houseW+s.evW)
	}
}

// The car pauses by itself, the house rises into the room it left, and the
// car resumes. Nothing stopped it while paused; the next meter reading cuts.
func TestEVHardLimitCutsOnNextReadingWhenCarResumesIntoHouseLoad(t *testing.T) {
	s := newEVMeterSite(t, 7500, 900)
	s.steady(5520)
	s.carPaused = true
	s.run(30 * time.Second)
	s.setHouse(5000)
	s.run(30 * time.Second)
	offered := s.offer()
	if offered < 5520 {
		t.Fatalf("paused car lost its offer: %.0f W", offered)
	}
	s.carPaused = false
	s.step() // The resumed draw is in this reading.
	if over := s.houseW + s.evW - 7500; over <= 0 || s.offer() > math.Max(0, offered-over) {
		t.Fatalf("overage %.0f W left offer %.0f W after the first reading", over, s.offer())
	}
}

// After a cut the cap rises once evSettle has passed, then one step per
// reading. It does not wait for the meter to show each rise: every rise
// counts as flowing until the charger can have applied it, so the car
// stops a step below the room.
func TestEVHardLimitRisesOnceCutApplied(t *testing.T) {
	s := newEVMeterSite(t, 7500, 900)
	s.steady(5520)
	stepAt := s.now
	s.setHouse(2900) // 920 W over: cut to 4,140 W
	s.run(10 * time.Second)
	cuts := s.offersSince(stepAt)
	if len(cuts) == 0 || s.offer() != 4140 {
		t.Fatalf("offer %.0f W after a 920 W overage", s.offer())
	}
	var cutAt time.Time
	for _, o := range cuts {
		if o.w < 5520 {
			cutAt = o.at
			break
		}
	}
	s.setHouse(900)
	recoveredAt := s.now
	s.run(time.Minute)
	got := s.offersSince(recoveredAt)
	var firstRise time.Time
	for i := 1; i < len(got); i++ {
		if got[i].w-got[i-1].w > 690 {
			t.Fatalf("rose more than one step at once: %v", offerWatts(got))
		}
		if got[i].w > got[i-1].w && firstRise.IsZero() {
			firstRise = got[i].at
		}
	}
	if firstRise.IsZero() || firstRise.Sub(cutAt) < evSettle {
		t.Fatalf("first rise %v after the cut, want at least %v", firstRise.Sub(cutAt), evSettle)
	}
	if s.offer() != 5520 || s.houseW+s.evW > 7500-690 {
		t.Fatalf("recovered to %.0f W at %.0f W grid, want 5,520 W with a step of margin", s.offer(), s.houseW+s.evW)
	}
}

// Easee Cloud may take one to three minutes to apply a new current and
// declares power_max_age_s 180. Beside 15 A of house load on the 25 A fuse,
// rises sent before the meter showed the last one stacked: a 60 s delay
// peaked at 25.0 A, a 120 s delay at 28.0 A. Each rise now counts as flowing
// until the charger can have applied it, and a charger that declares nothing
// counts as just as slow. The car settles in the measured room and climbs
// again when the house lets go.
func TestEVHardLimitSlowChargerRisesDoNotStack(t *testing.T) {
	for _, declaredS := range []float64{180, 0} {
		for _, delay := range []time.Duration{60 * time.Second, 120 * time.Second} {
			t.Run(fmt.Sprintf("declared_%.0fs/delay_%v", declaredS, delay), func(t *testing.T) {
				s := newEVMeterSite(t, 0, 15*690)
				s.chargerDelay, s.chargerMaxAgeS = delay, declaredS
				limitA := s.state.SiteFuseAmps - evPhaseMarginA
				var maxA float64
				var overSince time.Time
				watch := func(d time.Duration) {
					t.Helper()
					for end := s.now.Add(d); s.now.Before(end); {
						s.step()
						a := s.phaseA()
						worst := math.Max(a[0], math.Max(a[1], a[2]))
						maxA = math.Max(maxA, worst)
						switch {
						case worst <= limitA:
							overSince = time.Time{}
						case overSince.IsZero():
							overSince = s.now
						case s.now.Sub(overSince) > delay+3*time.Second:
							t.Fatalf("worst phase %.1f A above %.0f A for %v", worst, limitA, s.now.Sub(overSince))
						}
					}
				}
				watch(10 * time.Minute)
				// 24 - 15 A leaves 9 A; one step stays free.
				if s.offer() != 5520 || s.evW != 5520 {
					t.Fatalf("offer %.0f W draw %.0f W, want 5,520 W", s.offer(), s.evW)
				}
				s.setHouse(10 * 690)
				watch(15 * time.Minute)
				// 24 - 10 A leaves 14 A; one step stays free.
				if s.offer() != 8970 || s.evW != 8970 {
					t.Fatalf("after the house fell: offer %.0f W draw %.0f W, want 8,970 W", s.offer(), s.evW)
				}
				t.Logf("worst phase peaked at %.1f A", maxA)
			})
		}
	}
}

// A fast local charger that declares no response time counts its offers
// as flowing for evUnknownSettle, beside what the meter already shows. It
// still takes the whole room in one move, and the pending count never cuts
// it: only a measured overage does.
func TestEVHardLimitUndeclaredFastChargerTakesRoomAtOnce(t *testing.T) {
	s := newEVMeterSite(t, 0, 15*690)
	for s.offer() == 0 {
		s.step()
	}
	startedAt := s.now
	s.run(s.chargerDelay + 2*time.Second)
	// 24 - 15 A leaves 9 A; one step stays free.
	if s.offer() != 5520 || s.evW != 5520 {
		t.Fatalf("started at %.0f W, drawing %.0f W; want the 5,520 W room in one move", s.offer(), s.evW)
	}
	// 23.5 A measured, under the 24 A working limit, while the start still
	// counts as flowing.
	s.setHouse(15.5 * 690)
	s.run(time.Minute)
	if got := offerWatts(s.offersSince(startedAt)); slices.Min(got) != 5520 {
		t.Fatalf("the pending start cut the car: %v", got)
	}

	// Under a 7.5 kW import ceiling the house falls by 1,380 W: one run of
	// rises, a step per reading, takes the car to the room less a step.
	s = newEVMeterSite(t, 7500, 2280)
	s.run(5 * time.Minute)
	if s.offer() != 4140 {
		t.Fatalf("setup: offer %.0f W", s.offer())
	}
	s.setHouse(900)
	s.run(s.chargerDelay + 5*time.Second)
	if s.offer() != 5520 || s.evW != 5520 {
		t.Fatalf("offer %.0f W draw %.0f W after the house fell, want 5,520 W", s.offer(), s.evW)
	}
}

// A charger that ignores a cut gets the next one once its old current
// should have reached the meter.
func TestEVHardLimitCutsAgainAfterSettleWhenChargerIgnores(t *testing.T) {
	s := newEVMeterSite(t, 7500, 900)
	s.steady(5520)
	s.chargerIgnores = true
	stepAt := s.now
	s.setHouse(2630)
	s.run(evSettle - 2*time.Second)
	if s.offer() != 4830 {
		t.Fatalf("offers %v, want one cut within the settle time", offerWatts(s.offersSince(stepAt)))
	}
	s.run(4 * time.Second)
	if s.offer() >= 4830 {
		t.Fatalf("ignored cut was not repeated: %v", offerWatts(s.offersSince(stepAt)))
	}
}

// A car that draws well below its offer hides a cut: the cut comes off the
// offer, not the draw. With the car 4 kW under its offer and a 2 kW
// overage, the first two cuts change nothing and the third, one settle time
// later each, takes effect after the charger's delay.
func TestEVHardLimitCarUnderItsOfferDelaysCut(t *testing.T) {
	s := newEVMeterSite(t, 14000, 1000)
	s.carMaxW = 6350
	s.run(2 * time.Minute)
	if s.offer() != 10350 || s.evW != 6350 {
		t.Fatalf("setup: offer %.0f draw %.0f", s.offer(), s.evW)
	}
	stepAt := s.now
	s.setHouse(14000 + 2000 - 6350)
	for s.houseW+s.evW > 14000 {
		if s.now.Sub(stepAt) > 2*time.Minute {
			t.Fatalf("overage never cleared: offer %.0f draw %.0f", s.offer(), s.evW)
		}
		s.step()
	}
	took := s.now.Sub(stepAt)
	t.Logf("2 kW overage with the car 4 kW under its offer cleared after %v; offers %v", took, offerWatts(s.offersSince(stepAt))[:1])
	if took > 2*evSettle+s.chargerDelay+3*time.Second {
		t.Fatalf("overage lasted %v", took)
	}
}

// The charger's readings never move the offer: missing for the whole run,
// late by 15 s during a start, above what we offered, or a lone 0 W.
func TestEVHardLimitOffersIgnoreChargerReadings(t *testing.T) {
	scenario := func(configure func(*evMeterSite)) []float64 {
		s := newEVMeterSite(t, 7500, 900)
		configure(s)
		s.run(90 * time.Second) // start
		s.setHouse(2630)        // cut
		s.run(time.Minute)
		s.setHouse(900) // recover
		s.run(2 * time.Minute)
		return offerWatts(s.offers)
	}
	want := scenario(func(*evMeterSite) {})
	for name, configure := range map[string]func(*evMeterSite){
		"missing":     func(s *evMeterSite) { s.readings = evReadingsMissing },
		"late_15s":    func(s *evMeterSite) { s.telemetryDelay = 15 * time.Second },
		"above_offer": func(s *evMeterSite) { s.readings = evReadingsHigh },
		"lone_zero":   func(s *evMeterSite) { s.loneZeroAt = s.now.Add(70 * time.Second) },
	} {
		got := scenario(configure)
		if len(got) != len(want) {
			t.Fatalf("%s: offers %v, want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: offers %v, want %v", name, got, want)
			}
		}
	}
}

// Easee power missing for 60 s mid-charge leaves the offer alone.
func TestEVHardLimitHoldsThroughMissingChargerPower(t *testing.T) {
	s := newEVMeterSite(t, 7500, 900)
	s.steady(5520)
	from := s.now
	s.readings = evReadingsMissing
	s.run(time.Minute)
	s.readings = evReadingsNormal
	s.run(30 * time.Second)
	for _, w := range offerWatts(s.offersSince(from)) {
		if w != 5520 {
			t.Fatalf("missing charger power moved the offer: %v", offerWatts(s.offersSince(from)))
		}
	}
}

// A silent meter repeats its last reading. Nothing may rise on it, even
// when that reading showed room and the settle time has long passed.
func TestEVHardLimitNeverRisesOnSilentMeter(t *testing.T) {
	s := newEVMeterSite(t, 7500, 900)
	s.steady(5520)
	s.meterSilent = true
	s.setHouse(0) // 1,980 W of room the meter never shows
	from := s.now
	s.run(50 * time.Second) // under the one-minute dispatch gate
	for _, w := range offerWatts(s.offersSince(from)) {
		if w != 5520 {
			t.Fatalf("offer moved on a silent meter: %v", offerWatts(s.offersSince(from)))
		}
	}
}

// A meter gap past the dispatch gate stops the charge. When the meter
// returns, the car restarts only after the meter has kept showing room for
// the restart dwell.
func TestEVHardLimitRestartsAfterMeterGapOnlyOnSustainedRoom(t *testing.T) {
	s := newEVMeterSite(t, 7500, 900)
	s.steady(5520)
	s.meterSilent = true
	s.run(70 * time.Second)
	if s.offer() != 0 {
		t.Fatalf("stale meter left offer %.0f W", s.offer())
	}
	s.meterSilent = false
	backAt := s.now
	s.run(2 * time.Minute)
	var restartAt time.Time
	for _, o := range s.offersSince(backAt) {
		if o.w > 0 {
			restartAt = o.at
			break
		}
	}
	if restartAt.IsZero() || restartAt.Sub(backAt) < evRestartDwell {
		t.Fatalf("restart %v after the meter returned, want at least %v", restartAt.Sub(backAt), evRestartDwell)
	}
	if s.maxGridW > 7500 {
		t.Fatalf("grid reached %.0f W", s.maxGridW)
	}
}

// Our own battery order the meter may not show yet blocks a rise, but
// never cuts the car.
func TestEVHardLimitBatteryOrderBlocksRiseNeverCuts(t *testing.T) {
	s := newEVMeterSite(t, 7500, 900)
	s.steady(5520)
	s.setHouse(0) // room for a rise
	from := s.now
	for range 60 {
		s.state.batteryCommands = map[string]commandLog{"pixii": commandLog{}.add(s.now.Add(-time.Second), 0).add(s.now, 3000)}
		s.step()
	}
	for _, w := range offerWatts(s.offersSince(from)) {
		if w != 5520 {
			t.Fatalf("pending battery order moved the offer: %v", offerWatts(s.offersSince(from)))
		}
	}
}

func evMeterBudgetFixture(t *testing.T, gridW float64, phaseA []float64, offerW float64, phase string) (*telemetry.Store, *State, []loadpoint.State, []loadpoint.Config) {
	t.Helper()
	store := telemetry.NewStore()
	data := map[string]any{}
	if phaseA != nil {
		data["l1_a"], data["l2_a"], data["l3_a"] = phaseA[0], phaseA[1], phaseA[2]
	}
	raw, _ := json.Marshal(data)
	store.Update("meter", telemetry.DerMeter, gridW, nil, raw)
	store.RecordDriverSuccess("meter")
	state := NewState(0, 0, "meter")
	state.SiteFuseAmps, state.SiteFuseVoltage, state.SiteFusePhases = 25, 230, 3
	points := []loadpoint.State{{ID: "car", DriverName: "charger", PluggedIn: true, CommandedKnown: true, CommandedW: offerW}}
	configs := []loadpoint.Config{{ID: "car", DriverName: "charger", PhaseMode: phase, MinChargeW: 1380, MaxChargeW: 13110}}
	return store, state, points, configs
}

// A 3p car offered 15 A beside 11 A of house load on L1: 26 A on a 24 A
// working limit. With no earlier reading the fuse heat starts at 26 A, so
// two amps on three phases come off the offer at once.
func TestEVHardLimitTrimsPhaseOverageFromOffer(t *testing.T) {
	store, state, points, configs := evMeterBudgetFixture(t, 8280, []float64{26, 20, 20}, 10350, "3p")
	got, known := EVFuseBudget(store, state, points, configs, 17250)
	if !known || math.Abs(got-8970) > .01 {
		t.Fatalf("cap %.1f W, want 10,350 - 2 A x 690 = 8,970 W", got)
	}
}

// An overage larger than the offer less one step stops the car; a smaller
// one never does.
func TestEVHardLimitStopsOnlyWhenOverageExceedsOfferLessMinimum(t *testing.T) {
	store, state, points, configs := evMeterBudgetFixture(t, 30000, nil, 10350, "3p")
	if got, _ := EVFuseBudget(store, state, points, configs, 17250); got != 0 {
		t.Fatalf("13 kW overage left %.1f W", got)
	}
	store, state, points, configs = evMeterBudgetFixture(t, 17250+500, nil, 10350, "3p")
	if got, _ := EVFuseBudget(store, state, points, configs, 17250); math.Abs(got-9850) > .01 {
		t.Fatalf("500 W overage gave %.1f W, want 9,850 W", got)
	}
}

// An idle car starts with a step of margin under the measured room, and a
// single-phase or auto car may land its whole offer on the worst phase.
func TestEVHardLimitIdleStartNeedsRoomOnDeclaredPhases(t *testing.T) {
	for _, tc := range []struct {
		name, phase string
		worstA      float64
		want        float64
	}{
		{"3p_room", "3p", 5, (24 - 5 - 1) * 690},
		{"legacy_default", "", 5, (24 - 5 - 1) * 690},
		{"1p_room", "1p", 5, (24 - 5 - 1) * 230},
		{"auto_room", "auto", 5, (24 - 5 - 1) * 230},
		{"3p_below_minimum", "3p", 21.5, 0}, // 1,725 W < 1,380 + 690 W
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state, points, configs := evMeterBudgetFixture(t, tc.worstA*690, []float64{tc.worstA, tc.worstA, tc.worstA}, 0, tc.phase)
			got, known := EVFuseBudget(store, state, points, configs, 17250)
			if !known || math.Abs(got-tc.want) > .01 {
				t.Fatalf("cap %.1f W, want %.1f W", got, tc.want)
			}
		})
	}
}

// Our own battery order the meter may not show yet counts as a
// three-phase battery puts it: the same current on each phase. A one-phase
// car's room is one phase's, so the whole order there counted it three
// times: 4,703 W of battery order left 661 W below one phase's limit, under
// the 1,610 W a start needs, and the car stayed at 0 W (fuse_limit) until
// the order had shown for evSettle and the meter had shown room for
// evRestartDwell (the rig of 22 Oct 2026, 42 s). The cap counts 6.8 A on
// each phase instead; a three-phase car's cap does not change.
func TestEVHardLimitBatteryOrderCountsOnEachPhase(t *testing.T) {
	for _, tc := range []struct {
		phase string
		want  float64
	}{
		{"1p", (24 - 0.68 - 4703.0/690 - 1) * 230},
		{"auto", (24 - 0.68 - 4703.0/690 - 1) * 230},
		{"3p", (24 - 0.68 - 4703.0/690 - 1) * 690},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			store, state, points, configs := evMeterBudgetFixture(t, -466, []float64{-0.68, -0.68, -0.68}, 0, tc.phase)
			now := time.Now()
			state.batteryCommands = map[string]commandLog{"ferroamp": commandLog{}.add(now, 4703)}
			got, known := EVFuseBudget(store, state, points, configs, 17250)
			if !known || math.Abs(got-tc.want) > .01 {
				t.Fatalf("cap %.1f W, want %.1f W", got, tc.want)
			}
		})
	}
}

// While the hour's import ceiling holds an idle car back, the label names
// the ceiling. The fuse's room counts our battery order on its way, so the
// ceiling's room in the comparison must count it too, or a 5,500 W order
// with 2,750 W of fuse room read fuse_limit from the second reading on.
func TestEVHardLimitIdleCeilingLabelCountsBatteryOrder(t *testing.T) {
	store, state, points, configs := evMeterBudgetFixture(t, 9000, []float64{13, 13, 13}, 0, "3p")
	now := time.Now()
	state.clock = func() time.Time { return now }
	state.PeakImportCeilingW = 12000
	state.importHour = hourImport{start: state.importHourStart(now), ceilingW: 12000, limitW: 12000, riseW: 12000,
		changedAt: now, seen: true, seenCeiling: 12000, lastAt: now.Add(-time.Second)}
	state.batteryCommands = map[string]commandLog{"battery": commandLog{}.add(now, 5500)}
	for reading := 1; reading <= 2; reading++ {
		store.Get("meter", telemetry.DerMeter).UpdatedAt = now
		got, _ := EVFuseBudget(store, state, points, configs, 17250)
		if reason := EVLimitReason(state); got != 0 || reason != "import_limit" {
			t.Fatalf("reading %d: cap %.1f W %s, want 0 W import_limit", reading, got, reason)
		}
		now = now.Add(2 * time.Second)
	}
}

// A lone 0 W or unavailable charger reading is not a reason to cut, and
// before our first command a charging car's reading earns no credit: the
// meter alone decides.
func TestEVHardLimitIgnoresChargerReadings(t *testing.T) {
	for name, samples := range map[string][]string{
		"lone_zero":   {`{"control_power_available":true}`, `{"control_power_available":true}`},
		"unavailable": {`{"control_power_available":false}`},
	} {
		store, state, points, configs := evMeterBudgetFixture(t, 12323, []float64{20.45, 17.05, 15.87}, 10350, "3p")
		for i, data := range samples {
			w := 0.0
			if i < len(samples)-1 {
				w = 10242
			}
			store.Update("charger", telemetry.DerEV, w, nil, []byte(data))
			store.RecordDriverSuccess("charger")
		}
		if got, _ := EVFuseBudget(store, state, points, configs, 17250); got < 10350 {
			t.Fatalf("%s EV power cut the cap to %.1f W", name, got)
		}
	}
	store, state, points, configs := evMeterBudgetFixture(t, 12323, []float64{20.45, 17.05, 15.87}, 0, "3p")
	points[0].CommandedKnown = false
	store.Update("charger", telemetry.DerEV, 10242, nil, nil)
	store.RecordDriverSuccess("charger")
	if got, _ := EVFuseBudget(store, state, points, configs, 17250); got > (24-20.45-1)*690+.01 {
		t.Fatalf("a car reading earned %.1f W before our first command", got)
	}
}
