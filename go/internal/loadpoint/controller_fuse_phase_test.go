package loadpoint

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestSharedFuseGuardUsesCommandPhase(t *testing.T) {
	for _, tc := range []struct {
		name, configMode, holdMode, commandMode, demandMode string
		manual, schedule, surplus                           bool
		wantW, worstA, commandW                             float64
	}{
		// An unset mode is auto: three phases, named in the order.
		{name: "legacy_plan", wantW: 13110, worstA: 5, commandW: 13110, commandMode: "3p", demandMode: "3p"},
		{name: "legacy_manual", manual: true, wantW: 13110, worstA: 5, commandW: 13110, commandMode: "3p", demandMode: "3p"},
		{name: "legacy_manual_over_schedule", manual: true, schedule: true, wantW: 13110, worstA: 5, commandW: 13110, commandMode: "3p", demandMode: "3p"},
		{name: "scheduled_1p", configMode: "1p", schedule: true, wantW: 2760, worstA: 20, commandMode: "1p", demandMode: "1p"},
		// Planned charging on auto runs on three phases: the 3Φ minimum does
		// not fit 4 A of room, and no one-phase order takes its place.
		{name: "scheduled_auto_runs_3p", configMode: "auto", schedule: true, wantW: 4140, worstA: 20, commandMode: "3p", demandMode: "3p"},
		{name: "scheduled_auto_room", configMode: "auto", schedule: true, wantW: 13110, worstA: 5, commandW: 13110, commandMode: "3p", demandMode: "3p"},
		{name: "surplus_manual_auto", manual: true, surplus: true, wantW: 2760, worstA: 20, commandMode: "auto", demandMode: "auto"},
		{name: "manual_1p_over_3p", configMode: "3p", manual: true, holdMode: "1p", wantW: 2760, worstA: 20, commandMode: "1p", demandMode: "1p"},
		{name: "manual_3p_over_auto", configMode: "auto", manual: true, holdMode: "3p", wantW: 13110, worstA: 5, commandW: 13110, commandMode: "3p", demandMode: "3p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			cfg := Config{ID: "garage", DriverName: "charger", MinChargeW: 1380, MaxChargeW: 13110,
				AllowedStepsW: []float64{0, 1380, 2760, 4140, 13110}, PhaseMode: tc.configMode, SurplusOnly: tc.surplus}
			d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
				LoadpointEnergyWh: map[string]float64{cfg.ID: tc.wantW / 4}, LoadpointMaxPowerW: map[string]float64{cfg.ID: tc.wantW}}
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
			sender := &fakeSender{}
			c := newTestController(t, []Config{cfg}, d, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			c.SetFuseEVMax(meteredEVCap(c, func() float64 { return tc.worstA }))
			if tc.schedule {
				c.manager.SetSchedule(cfg.ID, Schedule{SoC: .8, TimeOfDayMinUTC: 13 * 60, Recurring: true})
			}
			if tc.manual {
				c.SetManualHold(cfg.ID, ManualHold{PowerW: tc.wantW, PhaseMode: tc.holdMode, Persistent: true})
			}
			c.Tick(context.Background(), now)
			command, known := lastSetCurrent(sender.calls)
			if !known || math.Abs(command.power-tc.commandW) > .01 || command.phaseMode != tc.commandMode {
				t.Fatalf("command=%+v, want %.0f W with phase %q", command, tc.commandW, tc.commandMode)
			}
			if phases := PhaseFor(command.phaseMode, command.power, 3680); command.power > 0 && tc.worstA+command.power/(230*float64(phases)) > 24.001 {
				t.Fatalf("resolved %dp command crosses the phase working limit: %+v", phases, command)
			}
			demand := c.FuseDemands()
			if len(demand) != 1 || demand[0].PhaseMode != tc.demandMode {
				t.Fatalf("command phase and battery reservation disagree: %+v, want %q", demand, tc.demandMode)
			}
		})
	}
}

func TestAutomaticManualReleasePrecedesSharedPhaseBudget(t *testing.T) {
	for _, release := range []string{"soc", "idle"} {
		t.Run(release, func(t *testing.T) {
			now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			cfg := Config{ID: "garage", DriverName: "charger", MinChargeW: 1380, MaxChargeW: 13110,
				AllowedStepsW: []float64{0, 1380, 2760, 4140, 13110}, VehicleCapacityWh: 60000, PluginSoC: .8, PhaseMode: "1p"}
			d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
				LoadpointEnergyWh: map[string]float64{cfg.ID: 690}, LoadpointMaxPowerW: map[string]float64{cfg.ID: 2760}}
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: release != "idle"}}
			sender := &fakeSender{}
			c := newTestController(t, []Config{cfg}, d, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 20 }))
			c.manager.SetSchedule(cfg.ID, Schedule{SoC: .9, TimeOfDayMinUTC: 13 * 60, Recurring: true})
			hold := ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true}
			if release == "soc" {
				hold.ReleaseAtSoC = .8
			}
			c.SetManualHold(cfg.ID, hold)
			if release == "idle" {
				c.Tick(context.Background(), now.Add(-SessionCompletionTimeout))
				if _, active := c.GetManualHold(cfg.ID, now); !active {
					t.Fatal("idle hold released before its debounce elapsed")
				}
			}
			c.Tick(context.Background(), now)
			if _, active := c.GetManualHold(cfg.ID, now); active {
				t.Fatal("completed manual hold was not released")
			}
			command, known := lastSetCurrent(sender.calls)
			if !known || command.phaseMode != "1p" || command.power != 0 {
				t.Fatalf("automatic order reused the earlier manual 3p budget: %+v", command)
			}
		})
	}
}

func TestManualPhaseChangeAfterSharedBudgetWaitsForNextTick(t *testing.T) {
	for _, tc := range []struct {
		name, beforeMode, afterMode       string
		beforeW, afterW, worstA, commandW float64
	}{
		{"3p_to_1p", "3p", "1p", 4140, 1380, 17, 0},
		{"3p_to_auto", "3p", "auto", 4140, 1380, 17, 0},
		{"new_stop", "3p", "3p", 4140, 0, 17, 0},
		// 1,380 W on three phases is 2 A each: no legal 3p step.
		{"1p_to_3p", "1p", "3p", 1380, 4140, 17, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			a := Config{ID: "first", DriverName: "first", MinChargeW: 1380, MaxChargeW: 13110,
				AllowedStepsW: []float64{0, 1380, 2760, 4140, 13110}}
			b := a
			b.ID, b.DriverName = "second", "second"
			d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute)}
			samples := map[string]EVSample{a.DriverName: {Connected: true, RequestActive: true}, b.DriverName: {Connected: true, RequestActive: true}}
			sender := &fakeSender{}
			c := newTestController(t, []Config{a, b}, d, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			c.SetFuseEVMax(meteredEVCap(c, func() float64 { return tc.worstA }))
			c.SetManualHold(a.ID, ManualHold{PhaseMode: "3p", Persistent: true})
			c.SetManualHold(b.ID, ManualHold{PowerW: tc.beforeW, PhaseMode: tc.beforeMode, Persistent: true})
			c.send = func(ctx context.Context, driver string, payload []byte) error {
				err := sender.Send(ctx, driver, payload)
				if driver == a.DriverName {
					c.SetManualHold(b.ID, ManualHold{PowerW: tc.afterW, PhaseMode: tc.afterMode, Persistent: true})
				}
				return err
			}
			c.Tick(context.Background(), now)
			command, known := lastSetCurrent(sender.calls)
			if !known || command.driver != b.DriverName || command.power != tc.commandW || command.phaseMode != tc.afterMode {
				t.Fatalf("new choice used another phase's frozen budget: %+v", command)
			}
			for _, sent := range sender.calls {
				if sent.driver == a.DriverName && sent.power != 0 {
					t.Fatal("first car's Stop was overridden")
				}
			}
			if tc.beforeMode == "3p" && tc.afterW > 0 {
				c.Tick(context.Background(), now.Add(5*time.Second))
				command, known = lastSetCurrent(sender.calls)
				if !known || command.power != 1380 || command.phaseMode != tc.afterMode {
					t.Fatalf("new phase did not recover with its next safe budget: %+v", command)
				}
			}
		})
	}
}
