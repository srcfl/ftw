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

// The start point is exact: on a 16 A fuse an idle 1p or auto pool starts at
// 1,380 W, its least order, once the worst phase leaves 6 A of room under the
// working line, and waits while it leaves 5.7 A. A hidden margin on the
// one-phase minimum would move the start and fail here. The cases are set
// from the working line, so they follow it if the line moves.
func TestIdleOnePhasePoolStartBoundary(t *testing.T) {
	steps := []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680,
		4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}
	const fuseA = 16.0
	lineA := fuseA - evPhaseMarginA // the EV working line on each phase
	site := func(t *testing.T, houseA float64, mode string) *phaseSite {
		cfg := loadpoint.Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040,
			PhaseMode: "auto", AllowedStepsW: steps}
		s := newPhaseSite(t, cfg, houseA)
		s.state.SiteFuseAmps = fuseA
		s.ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: fuseA, Voltage: 230, PhaseCnt: 3})
		s.ctrl.SetFuseEVMax(func() (float64, bool) {
			return EVFuseBudget(s.store, s.state, s.mgr.States(), s.ctrl.FuseConfigs(), fuseA*230*3)
		})
		s.ctrl.SetManualHold(cfg.ID, loadpoint.ManualHold{PowerW: 3680, PhaseMode: mode, Persistent: true})
		return s
	}
	for _, mode := range []string{"1p", "auto"} {
		t.Run(mode+" 6.0 A of room starts", func(t *testing.T) {
			s := site(t, lineA-6, mode)
			start := s.now
			for s.now.Sub(start) < evRestartDwell+5*time.Second && s.offerW == 0 {
				s.tick()
			}
			if s.offerW != 1380 || !s.offer1p {
				t.Fatalf("worst phase %.1f A: offer %.0f W, one phase %v; want 1380 W on one phase",
					lineA-6, s.offerW, s.offer1p)
			}
			for i := 0; i < 60; i++ {
				s.tick()
				if s.worstA() > fuseA {
					t.Fatalf("tick %d: %.2f A on a %.0f A fuse", i, s.worstA(), fuseA)
				}
			}
		})
		t.Run(mode+" 5.7 A of room waits", func(t *testing.T) {
			s := site(t, lineA-5.7, mode)
			for i := 0; i < 40; i++ {
				s.tick()
				if s.offerW != 0 {
					t.Fatalf("tick %d: offer %.0f W with 5.7 A of room; want 0", i, s.offerW)
				}
			}
		})
	}
}
