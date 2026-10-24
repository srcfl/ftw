package loadpoint

import (
	"context"
	"testing"
	"time"
)

func fuseRecoveryLoadpoint() Config {
	cfg := reasonLoadpoint()
	cfg.MaxChargeW = 11040
	cfg.AllowedStepsW = []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}
	return cfg
}

func TestFirstConnectionIsObservedBeforeFuseBudget(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := fuseRecoveryLoadpoint()
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true, PowerW: 10000}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
	seenConnected := false
	c.SetFuseEVMax(func() (float64, bool) {
		st, _ := c.manager.State(cfg.ID)
		seenConnected = st.PluggedIn
		if !st.PluggedIn {
			return 3560, true
		}
		return 13560, true
	})
	c.Tick(context.Background(), now)
	set, ok := lastSetCurrent(sender.calls)
	if !seenConnected || !ok || set.power != 10350 {
		t.Fatalf("fresh connection absent from shared budget: connected=%v command=%+v", seenConnected, set)
	}
}

func TestManualSinglePhaseUsesItsLegalMinimumInSharedFuseBudget(t *testing.T) {
	for _, phaseMode := range []string{"1p", "auto"} {
		t.Run(phaseMode, func(t *testing.T) {
			now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			cfg := fuseRecoveryLoadpoint()
			cfg.AllowedStepsW = append(cfg.AllowedStepsW, 1380, 2300, 3680)
			sender := &fakeSender{}
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
			c := newTestController(t, []Config{cfg}, nil, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			c.SetManualHold(cfg.ID, ManualHold{PowerW: 3680, PhaseMode: phaseMode, Persistent: true})
			c.SetFuseEVMax(func() (float64, bool) { return 5000, true })
			c.Tick(context.Background(), now)
			set, ok := lastSetCurrent(sender.calls)
			if !ok || set.power != 3680 {
				t.Fatalf("legal single-phase offer was treated as below the three-phase minimum: %+v", set)
			}
		})
	}
}

func TestManualPhaseOverrideKeepsSixAmpMinimum(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := fuseRecoveryLoadpoint()
	cfg.MinChargeW, cfg.AllowedStepsW = 1380, nil
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 1000, PhaseMode: "1p", Persistent: true})
	c.SetFuseEVMax(func() (float64, bool) { return 5000, true })
	c.Tick(context.Background(), now)
	set, ok := lastSetCurrent(sender.calls)
	if !ok || set.power != 0 || len(c.FuseDemands()) != 0 {
		t.Fatalf("sub6A request became a charger or battery reservation: command=%+v demand=%+v", set, c.FuseDemands())
	}
}

func TestUnspecifiedSurplusPhaseKeepsSinglePhaseFuseBound(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "charger", MinChargeW: 1380, MaxChargeW: 11040, SurplusOnly: true,
		AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11040}}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	// A hold without a phase override inherits surplus mode's auto choice.
	// At2760 W that may be a12 A offer on the already20 A phase.
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 2760, Persistent: true})
	c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 20 }))
	c.Tick(context.Background(), now)
	set, ok := lastSetCurrent(sender.calls)
	if !ok || set.power != 0 {
		t.Fatalf("only4 A remains per phase, no legal6 A auto offer fits: %+v", set)
	}
}

func TestUnknownFuseVerdictIsFrozenForAllCars(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	configs := []Config{fuseRecoveryLoadpoint(), fuseRecoveryLoadpoint()}
	configs[0].ID, configs[0].DriverName = "a", "charger-a"
	configs[1].ID, configs[1].DriverName = "b", "charger-b"
	sender := &fakeSender{}
	samples := map[string]EVSample{"charger-a": {Connected: true, RequestActive: true}, "charger-b": {Connected: true, RequestActive: true}}
	c := newTestController(t, configs, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	for _, cfg := range configs {
		c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	}
	reads := 0
	c.SetFuseEVMax(func() (float64, bool) {
		reads++
		return 5000, reads > 1
	})
	c.Tick(context.Background(), now)
	if reads != 1 {
		t.Fatalf("one unknown verdict became different per-car budgets: reads=%d", reads)
	}
	// (_, false) keeps its existing no-constraint meaning for the whole
	// first tick. Only the next tick may adopt the new shared5000 W cap.
	var firstTotal float64
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			firstTotal += cmd.power
		}
	}
	if firstTotal != 8280 {
		t.Fatalf("unknown verdict changed its no-constraint meaning: %.0f W", firstTotal)
	}
	sender.calls = nil
	c.Tick(context.Background(), now.Add(5*time.Second))
	var nextTotal float64
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			nextTotal += cmd.power
		}
	}
	if reads != 2 || nextTotal <= 0 || nextTotal > 5000 {
		t.Fatalf("next known budget was not shared: reads=%d total=%.0f W", reads, nextTotal)
	}
}

func TestManualCurrentCeilingLeavesFuseRoomForSecondCar(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	configs := []Config{fuseRecoveryLoadpoint(), fuseRecoveryLoadpoint()}
	configs[0].ID, configs[0].DriverName = "a", "charger-a"
	configs[1].ID, configs[1].DriverName = "b", "charger-b"
	sender := &fakeSender{}
	samples := map[string]EVSample{"charger-a": {Connected: true, RequestActive: true}, "charger-b": {Connected: true, RequestActive: true}}
	c := newTestController(t, configs, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetFuseEVMax(func() (float64, bool) { return 9000, true })
	c.SetManualHold("a", ManualHold{PowerW: 10350, PhaseMode: "3p", MaxAmpsPerPhase: 6, Persistent: true})
	c.SetManualHold("b", ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	c.Tick(context.Background(), now)
	commands := map[string]float64{}
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			commands[cmd.driver] = cmd.power
		}
	}
	if commands["charger-a"] != 4140 || commands["charger-b"] != 4140 {
		t.Fatalf("current-limited car reserved unused fuse room: %+v", commands)
	}
}
