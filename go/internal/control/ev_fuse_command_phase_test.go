package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// phaseSite runs one loadpoint against EVFuseBudget on a 25 A, 3 × 230 V
// site, wired as main.go wires it. Each tick the meter shows the house plus
// the last offer, spread over the phases the offer used.
type phaseSite struct {
	now     time.Time
	store   *telemetry.Store
	state   *State
	mgr     *loadpoint.Manager
	ctrl    *loadpoint.Controller
	houseA  float64
	offerW  float64
	offer1p bool
}

func newPhaseSite(t *testing.T, cfg loadpoint.Config, houseA float64) *phaseSite {
	t.Helper()
	s := &phaseSite{now: time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC), store: telemetry.NewStore(), houseA: houseA}
	s.state = NewState(0, 0, "meter")
	s.state.SiteFuseAmps, s.state.SiteFuseVoltage, s.state.SiteFusePhases = 25, 230, 3
	s.state.clock = func() time.Time { return s.now }
	s.mgr = loadpoint.NewManager()
	s.mgr.Load([]loadpoint.Config{cfg})
	s.ctrl = loadpoint.NewController(s.mgr,
		func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false },
		func(string) (loadpoint.EVSample, bool) {
			return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: s.offerW, PowerAt: s.now}, true
		},
		func(_ context.Context, _ string, payload []byte) error {
			var cmd struct {
				Action    string  `json:"action"`
				PowerW    float64 `json:"power_w"`
				PhaseMode string  `json:"phase_mode"`
			}
			if err := json.Unmarshal(payload, &cmd); err != nil {
				return err
			}
			if cmd.Action == "ev_set_current" {
				s.offerW = cmd.PowerW
				s.offer1p = loadpoint.PhaseFor(cmd.PhaseMode, cmd.PowerW, 3680) == 1
			}
			return nil
		})
	s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	s.ctrl.SetFuseEVMax(func() (float64, bool) {
		return EVFuseBudget(s.store, s.state, s.mgr.States(), s.ctrl.FuseConfigs(), 25*230*3)
	})
	s.ctrl.SetFusePhaseRoomA(func() (float64, bool) { return EVFusePhaseRoomA(s.state) })
	return s
}

// worstA is the busiest phase: a one-phase offer may land on any of them.
func (s *phaseSite) worstA() float64 {
	if s.offer1p {
		return s.houseA + s.offerW/230
	}
	return s.houseA + s.offerW/690
}

func (s *phaseSite) tick() {
	s.now = s.now.Add(5 * time.Second)
	a := s.worstA()
	data, _ := json.Marshal(map[string]any{"control_power_available": true,
		"control_power_observed_at": s.now.Format(time.RFC3339Nano), "l1_a": a, "l2_a": a, "l3_a": a})
	s.store.Update("meter", telemetry.DerMeter, a*690, nil, data)
	s.store.Get("meter", telemetry.DerMeter).UpdatedAt = s.now
	s.store.RecordDriverSuccess("meter")
	s.ctrl.Tick(context.Background(), s.now)
}

// R2 against the real cap: a saved but unarmed battery-SoC unlock must not
// change the phases the cap is measured on.
func TestChargeNowWithUnarmedUnlockHoldsUnderEVFuseBudget(t *testing.T) {
	cfg := loadpoint.Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040,
		AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}}
	s := newPhaseSite(t, cfg, 3)
	s.mgr.SetSchedule(cfg.ID, loadpoint.Schedule{SurplusUnlockBatSoC: 0.9})
	s.ctrl.SetBatSoCProvider(func() (float64, bool) { return 0.5, true })
	s.ctrl.SetSiteSurplusForEV(func() (float64, bool) { return -2000, true })
	s.ctrl.SetManualHold(cfg.ID, loadpoint.ManualHold{PowerW: 11040, Persistent: true})
	for tick := 0; tick < 20; tick++ {
		s.tick()
		st, _ := s.mgr.State(cfg.ID)
		if s.offerW != 11040 || st.CommandedReason == "fuse_limit" {
			t.Fatalf("tick %d: offer %.0f W, reason %q; want 11040 W", tick, s.offerW, st.CommandedReason)
		}
	}
}

// An auto offer below the phase split lands on one phase. At 20 A of house
// load the cap must be measured on that phase, not on the three the config
// leaves unnamed: 2,070 W on one phase would put 29 A on a 25 A fuse.
func TestAutoOfferGetsOnePhaseOfRoomFromEVFuseBudget(t *testing.T) {
	cfg := loadpoint.Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11040,
		AllowedStepsW: []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680,
			4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}}
	s := newPhaseSite(t, cfg, 20)
	s.ctrl.SetManualHold(cfg.ID, loadpoint.ManualHold{PowerW: 2760, PhaseMode: "auto", Persistent: true})
	for tick := 0; tick < 12; tick++ {
		s.tick()
		if s.worstA() > 24 {
			t.Fatalf("tick %d: offer %.0f W puts %.1f A on one phase", tick, s.offerW, s.worstA())
		}
	}
}
