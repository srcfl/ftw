package loadpoint

import (
	"context"
	"math"
	"testing"
	"time"
)

// threePhaseSteps are Easee's whole-amp offers at 230 V: 6–16 A on one
// phase, then 6–16 A on three.
var threePhaseSteps = []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680,
	4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}

// meteredEVCap stands in for control.EVFuseBudget: the plugged cars' last
// offers plus the room the meter shows below the fuse less 1 A, on the
// phases FuseConfigs names for this tick.
func meteredEVCap(c *Controller, worstA func() float64) func() (float64, bool) {
	return func() (float64, bool) {
		site := c.siteFuse()
		phases, offersW := float64(site.Phases()), 0.0
		for _, cfg := range c.FuseConfigs() {
			if st, ok := c.manager.State(cfg.ID); ok && st.PluggedIn {
				if cfg.PhaseMode != "" && cfg.PhaseMode != "3p" {
					phases = 1
				}
				offersW += math.Max(0, st.CommandedW)
			}
		}
		return math.Max(0, offersW+(site.MaxAmps-1-worstA())*site.Voltage*phases), true
	}
}

// R2 of the 21 Oct 2026 review: a saved battery-SoC unlock that is not
// armed made the loadpoint's own fuse layer expect "auto" while Charge now
// sent the driver's 3p. The last offer counted as 0 and new room as one
// phase, so the charge ran 4,830 / 0 / 0 W with fuse_limit on a 25 A site.
func TestChargeNowHoldsWithUnarmedBatteryUnlock(t *testing.T) {
	now := time.Date(2026, 1, 10, 19, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040,
		AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.manager.SetSchedule(cfg.ID, Schedule{SurplusUnlockBatSoC: 0.9})
	c.SetBatSoCProvider(func() (float64, bool) { return 0.5, true })
	c.SetSiteSurplusForEV(func() (float64, bool) { return -2000, true })
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	evW := 0.0 // The meter shows 3 A of house load plus the car's last offer.
	c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 3 + evW/690 }))
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 11040, Persistent: true, StartedAt: now, UpdatedAt: now})
	for tick := 0; tick < 20; tick++ {
		c.Tick(context.Background(), now)
		command, ok := lastSetCurrent(sender.calls)
		st, _ := c.manager.State(cfg.ID)
		if !ok || command.power < cfg.MaxChargeW-690 || st.CommandedReason == "fuse_limit" {
			t.Fatalf("tick %d: offer %+v, reason %q; want about %.0f W", tick, command, st.CommandedReason, cfg.MaxChargeW)
		}
		if c.IsBatSoCArmed(cfg.ID) {
			t.Fatal("the unlock armed at 50% SoC")
		}
		evW = command.power
		samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true, PowerW: evW}
		now = now.Add(5 * time.Second)
	}
}

// On a one-phase site the command goes out as 1p. The loadpoint's own fuse
// layer expected the empty mode's 3p, so the last offer counted as 0 and
// Charge now ran 4,830 / 0 / 0 W.
func TestChargeNowHoldsOnOnePhaseSite(t *testing.T) {
	now := time.Date(2026, 1, 10, 19, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 7360,
		AllowedStepsW: []float64{0, 1380, 2300, 3220, 4140, 4830, 5520, 6440, 7360}}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 1})
	evW := 0.0 // 3 A of house load plus the car's last offer.
	c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 3 + evW/230 }))
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4830, Persistent: true, StartedAt: now, UpdatedAt: now})
	for tick := 0; tick < 20; tick++ {
		c.Tick(context.Background(), now)
		command, ok := lastSetCurrent(sender.calls)
		if st, _ := c.manager.State(cfg.ID); !ok || command.power != 4830 || command.phaseMode != "1p" {
			t.Fatalf("tick %d: offer %+v, reason %q; want 4830 W on 1p", tick, command, st.CommandedReason)
		}
		evW = command.power
		samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true, PowerW: evW}
		now = now.Add(5 * time.Second)
	}
}

// R1 of the 21 Oct 2026 review: with an unset or auto phase mode the
// surplus dwell sent 3p while the fuse room was measured on one phase.
// 9 kW of surplus gave 2,300–3,450 W labelled 3p, under 6 A, so the car
// did not charge. The cap must be measured for the dwell's 3p.
func TestSurplusOnlyAutoPhaseOffersLegalThreePhaseSteps(t *testing.T) {
	for _, phaseMode := range []string{"", "auto"} {
		t.Run("mode_"+phaseMode, func(t *testing.T) {
			now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
			cfg := surplusTestConfig()
			cfg.DriverName = "easee"
			cfg.PhaseMode = phaseMode
			cfg.AllowedStepsW = threePhaseSteps
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
			sender := &fakeSender{}
			c := newTestController(t, []Config{cfg}, nil, samples, sender)
			const surplusW = 9000
			c.SetSiteSurplusForEV(func() (float64, bool) { return surplusW, true })
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			evW := 0.0 // PV export on every phase, less the car's draw.
			c.SetFuseEVMax(meteredEVCap(c, func() float64 { return math.Abs(evW-surplusW) / 690 }))
			for tick := 0; tick < 20; tick++ {
				c.Tick(context.Background(), now)
				command, ok := lastSetCurrent(sender.calls)
				st, _ := c.manager.State(cfg.ID)
				if !ok || command.phaseMode != "3p" || command.power/(3*230) < 6 || command.power > surplusW {
					t.Fatalf("tick %d: offer %+v, reason %q; want a 3p step of at least 6 A within the surplus", tick, command, st.CommandedReason)
				}
				// The first offer may stop short while the meter still shows
				// the full export; from then on it follows the surplus.
				if tick > 0 && command.power < surplusW-690 {
					t.Fatalf("tick %d: offer %+v, reason %q; want within 1 A of %d W", tick, command, st.CommandedReason, surplusW)
				}
				evW = command.power
				samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true, PowerW: evW}
				now = now.Add(5 * time.Second)
			}
		})
	}
}

// A fuse cap snaps to steps the commanded phases can carry. Four amps of
// room per phase is 2,760 W, but on three phases that is under 6 A.
func TestThreePhaseFuseCapNeverSnapsBelowSixAmps(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11040,
		AllowedStepsW: threePhaseSteps, PhaseMode: "3p"}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 20 }))
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 11040, Persistent: true})
	c.Tick(context.Background(), now)
	command, ok := lastSetCurrent(sender.calls)
	if st, _ := c.manager.State(cfg.ID); !ok || command.power != 0 || st.CommandedReason != "fuse_limit" {
		t.Fatalf("offer %+v, reason %q; want 0 W fuse_limit", command, st.CommandedReason)
	}
}
