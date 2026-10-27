package control

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// C6 (rig seeds 207534, 207536, 207537, 207531): an idle pool whose order
// lands on one phase needs room for 6 A on that phase, not for the
// configured three-phase minimum of 4,140 W. Without that, Charge now on 1p
// or auto never starts on a 16 A fuse ("below minimum step … fuse_cap_w=0
// min_step_w=1380").
func TestIdleOnePhasePoolStartsOnSixAmpsOfRoom(t *testing.T) {
	steps := []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680,
		4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}
	for _, tc := range []struct {
		name   string
		fuseA  float64
		houseA float64
		mode   string
		holdW  float64
	}{
		{"16A 1p", 16, 4.5, "1p", 3680},
		{"25A 1p", 25, 13.7, "1p", 3680},
		{"16A auto", 16, 4.5, "auto", 3680},
		{"25A auto", 25, 13.7, "auto", 3680},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadpoint.Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040,
				PhaseMode: "auto", AllowedStepsW: steps}
			s := newPhaseSite(t, cfg, tc.houseA)
			fuseW := tc.fuseA * 230 * 3
			s.state.SiteFuseAmps = tc.fuseA
			s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: tc.fuseA, Voltage: 230, PhaseCnt: 3})
			s.ctrl.SetFuseEVMax(func() (float64, bool) {
				return EVFuseBudget(s.store, s.state, s.mgr.States(), s.ctrl.FuseConfigs(), fuseW)
			})
			s.ctrl.SetManualHold(cfg.ID, loadpoint.ManualHold{PowerW: tc.holdW, PhaseMode: tc.mode, Persistent: true})
			start := s.now
			deadline := start.Add(evRestartDwell + 5*time.Second) // the dwell plus one 5 s tick
			for s.now.Before(deadline) && s.offerW < 1380 {
				s.tick()
			}
			st, _ := s.mgr.State(cfg.ID)
			if s.offerW < 1380 {
				t.Fatalf("after %s: offer %.0f W, EV cap %.0f W, reason %q; want at least 1380 W",
					s.now.Sub(start), s.offerW, s.state.evLimit.capW, st.CommandedReason)
			}
			t.Logf("started after %s at %.0f W on one phase", s.now.Sub(start), s.offerW)
			if !s.offer1p {
				t.Fatalf("offer %.0f W runs on three phases; want one", s.offerW)
			}
			for i := 0; i < 12; i++ {
				s.tick()
				if s.worstA() > tc.fuseA {
					t.Fatalf("offer %.0f W puts %.1f A on a %.0f A fuse", s.offerW, s.worstA(), tc.fuseA)
				}
			}
		})
	}
}
