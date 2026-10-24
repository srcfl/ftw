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

// A one-phase charger left on an empty phase mode names no phases. Its own
// 6 A minimum stands; a three-phase floor of 4,140 W is above its 3,680 W
// maximum, and it would never charge.
func TestOnePhaseChargerWithEmptyModeCharges(t *testing.T) {
	now := time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "charger", MinChargeW: 1380, MaxChargeW: 3680,
		AllowedStepsW: []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680}}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	evW := 0.0 // 3 A of house load plus the car on one phase.
	c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 3 + evW/230 }))
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 3680, Persistent: true})
	for tick := 0; tick < 12; tick++ {
		c.Tick(context.Background(), now)
		command, ok := lastSetCurrent(sender.calls)
		if st, _ := c.manager.State(cfg.ID); !ok || command.power != 3680 {
			t.Fatalf("tick %d: offer %+v, reason %q; want 3680 W", tick, command, st.CommandedReason)
		}
		evW = command.power
		samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true, PowerW: evW}
		now = now.Add(5 * time.Second)
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

// onePhaseSteps are whole amps from 6 to 16 A on one phase at 230 V.
var onePhaseSteps = []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680}

// commandAmps is the current per phase a driver draws for the command, as
// the OCPP driver works it out: the watts over the phases the command
// names, never above max_amps_per_phase.
func commandAmps(cmd sentCommand) float64 {
	if cmd.power <= 0 || cmd.voltage <= 0 {
		return 0
	}
	a := cmd.power / (cmd.voltage * float64(PhaseFor(cmd.phaseMode, cmd.power, defaultPhaseSplitW)))
	if cmd.maxAmpsPerPhase > 0 {
		a = math.Min(a, cmd.maxAmpsPerPhase)
	}
	return a
}

// Simulatorprov 21 Oct 2026, case s5s: surplus only, 9.3 kW of surplus and
// a plan that shows no PV. The day's 1p lock hands back every step, so 1p
// went out at 5,750 W with the 25 A fuse as its only limit: 25 A on one
// phase of a 16 A charger. A one-phase charger left on an empty mode gets
// the same lock and must keep its full 16 A.
func TestSurplusOnePhaseOfferStaysWithinTheCharger(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"16 A three-phase charger, auto", Config{MinChargeW: 4140, MaxChargeW: 11040, PhaseMode: "auto", AllowedStepsW: threePhaseSteps}},
		{"16 A one-phase charger, empty mode", Config{MinChargeW: 1380, MaxChargeW: 3680, AllowedStepsW: onePhaseSteps}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
			cfg := tc.cfg
			cfg.ID, cfg.DriverName, cfg.SurplusOnly = "garage", "evse", true
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
			sender := &fakeSender{}
			c := newTestController(t, []Config{cfg}, nil, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			c.SetSiteSurplusForEV(func() (float64, bool) { return 9310, true })
			c.SetNearTermPeakSurplusW(func(time.Duration) (float64, bool) { return 0, true })
			c.SetPeakRemainingSurplusW(func() (float64, bool) { return 0, true })
			for tick := 0; tick < 10; tick++ {
				c.Tick(context.Background(), now)
				cmd, _ := lastSetCurrent(sender.calls)
				samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true, PowerW: cmd.power}
				now = now.Add(5 * time.Second)
			}
			onePhase := 0
			for _, cmd := range sender.calls {
				if cmd.action != "ev_set_current" {
					continue
				}
				if a := commandAmps(cmd); a > 16+1e-9 {
					t.Fatalf("offer %+v is %.1f A per phase to a 16 A charger", cmd, a)
				}
				if cmd.phaseMode == "1p" {
					onePhase++
					if cmd.maxAmpsPerPhase > 16+1e-9 {
						t.Fatalf("1p offer %+v allows %.1f A per phase", cmd, cmd.maxAmpsPerPhase)
					}
				}
			}
			last, ok := lastSetCurrent(sender.calls)
			if onePhase == 0 || !ok || last.phaseMode != "1p" || last.power != 3680 {
				t.Fatalf("%d 1p offers, last %+v; want the 1p lock to charge at 3,680 W (16 A)", onePhase, last)
			}
		})
	}
}

// Charge now on one phase gets the charger's own current on that phase,
// and the fuse decides the rest. A 16 A charger that can run three phases
// gets 16 A, not the 25 A fuse. A 32 A charger, whether it switches phases
// or is a one-phase charger, keeps its 32 A when the fuse allows it.
func TestChargeNowOnOnePhaseKeepsTheChargersCurrent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		fuseA  float64
		holdW  float64
		wantW  float64
		wantA  float64
		reason string
	}{
		{"16 A three-phase charger, auto", Config{MinChargeW: 4140, MaxChargeW: 11040, PhaseMode: "auto", AllowedStepsW: threePhaseSteps}, 25, 7360, 3680, 16, "charger_limit"},
		{"16 A three-phase charger, empty mode", Config{MinChargeW: 4140, MaxChargeW: 11040, AllowedStepsW: threePhaseSteps}, 25, 7360, 3680, 16, "charger_limit"},
		{"32 A three-phase charger, auto", Config{MinChargeW: 1380, MaxChargeW: 22080, PhaseMode: "auto"}, 40, 7360, 7360, 32, "manual_hold"},
		{"32 A one-phase charger", Config{MinChargeW: 1380, MaxChargeW: 7360, PhaseMode: "1p"}, 40, 7360, 7360, 32, "manual_hold"},
		{"16 A one-phase charger, empty mode", Config{MinChargeW: 1380, MaxChargeW: 3680, AllowedStepsW: onePhaseSteps}, 25, 3680, 3680, 16, "manual_hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC)
			cfg := tc.cfg
			cfg.ID, cfg.DriverName = "garage", "evse"
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
			sender := &fakeSender{}
			c := newTestController(t, []Config{cfg}, nil, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: tc.fuseA, Voltage: 230, PhaseCnt: 3})
			evW := 0.0 // 3 A of house load plus the car on one phase.
			c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 3 + evW/230 }))
			c.SetManualHold(cfg.ID, ManualHold{PowerW: tc.holdW, PhaseMode: "1p", Persistent: true})
			for tick := 0; tick < 12; tick++ {
				c.Tick(context.Background(), now)
				cmd, ok := lastSetCurrent(sender.calls)
				st, _ := c.manager.State(cfg.ID)
				if !ok || cmd.phaseMode != "1p" || cmd.power != tc.wantW || math.Abs(commandAmps(cmd)-tc.wantA) > 1e-9 || st.CommandedReason != tc.reason {
					t.Fatalf("tick %d: offer %+v (%.1f A), reason %q; want %.0f W on 1p, %.0f A, %q",
						tick, cmd, commandAmps(cmd), st.CommandedReason, tc.wantW, tc.wantA, tc.reason)
				}
				evW = cmd.power
				samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true, PowerW: evW}
				now = now.Add(5 * time.Second)
			}
		})
	}
}
