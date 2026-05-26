package loadpoint

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeSender records every Send call so tests can assert on both the
// target driver and the payload contents.
type fakeSender struct {
	calls []sentCommand
	err   error
}

type sentCommand struct {
	driver          string
	power           float64
	action          string
	phaseMode       string
	phaseSplitW     float64
	minPhaseHoldS   int
	voltage         float64
	maxAmpsPerPhase float64
	sitePhases      int
}

func (f *fakeSender) Send(ctx context.Context, driver string, payload []byte) error {
	var d struct {
		Action          string  `json:"action"`
		PowerW          float64 `json:"power_w"`
		PhaseMode       string  `json:"phase_mode"`
		PhaseSplitW     float64 `json:"phase_split_w"`
		MinPhaseHoldS   int     `json:"min_phase_hold_s"`
		Voltage         float64 `json:"voltage"`
		MaxAmpsPerPhase float64 `json:"max_amps_per_phase"`
		SitePhases      int     `json:"site_phases"`
	}
	if err := json.Unmarshal(payload, &d); err != nil {
		return err
	}
	f.calls = append(f.calls, sentCommand{
		driver: driver, power: d.PowerW, action: d.Action,
		phaseMode: d.PhaseMode, phaseSplitW: d.PhaseSplitW,
		minPhaseHoldS: d.MinPhaseHoldS,
		voltage:       d.Voltage, maxAmpsPerPhase: d.MaxAmpsPerPhase,
		sitePhases:    d.SitePhases,
	})
	return f.err
}

// newTestController wires a controller with a manager pre-loaded with
// `cfgs`, a plan returning `directive` (or nothing when ok=false),
// and a telemetry func returning per-driver samples.
func newTestController(t *testing.T, cfgs []Config, directive *Directive,
	samples map[string]EVSample, sender *fakeSender) *Controller {
	t.Helper()
	m := NewManager()
	m.Load(cfgs)
	plan := PlanFunc(func(now time.Time) (Directive, bool) {
		if directive == nil {
			return Directive{}, false
		}
		return *directive, true
	})
	tel := TelemetryFunc(func(driver string) (EVSample, bool) {
		s, ok := samples[driver]
		return s, ok
	})
	return NewController(m, plan, tel, sender.Send)
}

// TestTickUnpluggedSendsNoCommand — the inline dispatch block skipped
// the command step entirely for unplugged chargers. We preserve that
// so an unplugged port isn't pinned to 0 W by the EMS (the driver's
// autonomous mode handles idle state).
func TestTickUnpluggedSendsNoCommand(t *testing.T) {
	sender := &fakeSender{}
	cfgs := []Config{{ID: "garage", DriverName: "easee", MinChargeW: 1400, MaxChargeW: 11000}}
	samples := map[string]EVSample{"easee": {PowerW: 0, Connected: false}}
	c := newTestController(t, cfgs, nil, samples, sender)

	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 0 {
		t.Errorf("unplugged loadpoint should send no command, got %d", len(sender.calls))
	}
}

// TestTickPluggedNoPlanCommandsZero — preserves the explicit-standdown
// semantics: without a plan allocation, command 0 W so the charger
// doesn't ride the previous slot's setpoint.
func TestTickPluggedNoPlanCommandsZero(t *testing.T) {
	sender := &fakeSender{}
	cfgs := []Config{{ID: "garage", DriverName: "easee", MinChargeW: 1400, MaxChargeW: 11000}}
	samples := map[string]EVSample{"easee": {PowerW: 3000, Connected: true, SessionWh: 4000}}
	c := newTestController(t, cfgs, nil, samples, sender)

	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(sender.calls))
	}
	if sender.calls[0].power != 0 {
		t.Errorf("plugged + no plan → expected 0 W, got %.0f", sender.calls[0].power)
	}
	if sender.calls[0].action != "ev_set_current" {
		t.Errorf("action = %q, want ev_set_current", sender.calls[0].action)
	}
}

// TestTickPluggedWithBudgetSendsSnappedPower — end-to-end: plan says
// 2.75 kWh over a 15-min slot (=11 kW), but the charger snaps to a
// discrete {0, 7.4, 11} kW ladder → 11 kW. This is the exact chain
// the inline dispatch ran; we're locking it in.
func TestTickPluggedWithBudgetSendsSnappedPower(t *testing.T) {
	sender := &fakeSender{}
	slotStart := time.Now().Add(-time.Second) // just started, negligible elapsed
	slotEnd := slotStart.Add(15 * time.Minute)
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4100, 7400, 11000},
	}}
	directive := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotEnd,
		LoadpointEnergyWh: map[string]float64{"garage": 2750},
	}
	samples := map[string]EVSample{"easee": {PowerW: 0, Connected: true}}
	c := newTestController(t, cfgs, directive, samples, sender)

	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(sender.calls))
	}
	// 2.75 kWh / 0.25 h = 11 kW, hits the 11000 step exactly.
	if sender.calls[0].power != 11000 {
		t.Errorf("want 11000 W, got %.0f", sender.calls[0].power)
	}
}

// TestTickBudgetMissingForLoadpointCommandsZero — the plan may have
// allocated to other loadpoints but not this one; treat it like
// no-plan (0 W).
func TestTickBudgetMissingForLoadpointCommandsZero(t *testing.T) {
	sender := &fakeSender{}
	slotStart := time.Now()
	slotEnd := slotStart.Add(15 * time.Minute)
	cfgs := []Config{{ID: "garage", DriverName: "easee", MinChargeW: 1400, MaxChargeW: 11000}}
	directive := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotEnd,
		LoadpointEnergyWh: map[string]float64{"other": 2000}, // not "garage"
	}
	samples := map[string]EVSample{"easee": {PowerW: 2000, Connected: true}}
	c := newTestController(t, cfgs, directive, samples, sender)

	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 1 || sender.calls[0].power != 0 {
		t.Errorf("want 0 W when loadpoint has no allocation, got %+v", sender.calls)
	}
}

// TestTickMidSlotSubtractsAlreadyDelivered — if half the slot has
// elapsed at 4 kW, the controller should treat the remaining budget
// accordingly rather than re-issuing the full slot power. Locks in
// the `alreadyWh = powerW * elapsed / 3600` approximation.
func TestTickMidSlotSubtractsAlreadyDelivered(t *testing.T) {
	sender := &fakeSender{}
	// Place `now` at the midpoint of a 30-min slot so the elapsed
	// math is deterministic without sub-second jitter.
	now := time.Date(2026, 4, 22, 14, 15, 0, 0, time.UTC)
	slotStart := now.Add(-15 * time.Minute)
	slotEnd := now.Add(15 * time.Minute)
	cfgs := []Config{{
		ID:         "garage",
		DriverName: "easee",
		MinChargeW: 1400,
		MaxChargeW: 11000,
	}}
	// Budget 4 kWh, half slot consumed at 4 kW → alreadyWh = 1 kWh,
	// remaining = 3 kWh over 0.25 h = 12 kW → clamped to max 11 kW.
	directive := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotEnd,
		LoadpointEnergyWh: map[string]float64{"garage": 4000},
	}
	samples := map[string]EVSample{"easee": {PowerW: 4000, Connected: true}}
	// No AllowedStepsW → continuous passthrough after clamp.
	c := newTestController(t, cfgs, directive, samples, sender)

	c.Tick(context.Background(), now)

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(sender.calls))
	}
	if sender.calls[0].power != 11000 {
		t.Errorf("want 11000 W after mid-slot + max clamp, got %.0f", sender.calls[0].power)
	}
}

// TestTickFeedsObservationsToManager — the manager's SoC-inference
// pipeline depends on every tick calling Observe, including the
// plug-in transition that seeds the session anchor.
func TestTickFeedsObservationsToManager(t *testing.T) {
	sender := &fakeSender{}
	cfgs := []Config{{
		ID:                "garage",
		DriverName:        "easee",
		VehicleCapacityWh: 60000,
		PluginSoCPct:      30,
	}}
	samples := map[string]EVSample{"easee": {PowerW: 7400, Connected: true, SessionWh: 6000}}
	c := newTestController(t, cfgs, nil, samples, sender)

	c.Tick(context.Background(), time.Now())

	st, ok := c.manager.State("garage")
	if !ok {
		t.Fatal("manager state missing after Tick")
	}
	// 30 + 6000/60000*100 = 40.
	if st.CurrentSoCPct < 39 || st.CurrentSoCPct > 41 {
		t.Errorf("expected ~40 %% SoC after Observe, got %.2f", st.CurrentSoCPct)
	}
	if !st.PluggedIn || st.CurrentPowerW != 7400 {
		t.Errorf("observation not propagated: %+v", st)
	}
}

// TestTickNoLoadpoints — the inline block early-returned on
// len(cfg.Loadpoints) == 0. Preserve: controller without configs
// must not touch the sender.
func TestTickNoLoadpoints(t *testing.T) {
	sender := &fakeSender{}
	m := NewManager()
	m.Load(nil)
	c := NewController(m,
		func(time.Time) (Directive, bool) { return Directive{}, false },
		func(string) (EVSample, bool) { return EVSample{}, false },
		sender.Send,
	)

	c.Tick(context.Background(), time.Now())

	if len(sender.calls) != 0 {
		t.Errorf("no configs → no sends, got %d", len(sender.calls))
	}
}

// TestTickSendErrorDoesNotPanic — driver comms failures are a
// logged warning, not a crash. Preserves the previous behaviour.
func TestTickSendErrorDoesNotPanic(t *testing.T) {
	sender := &fakeSender{err: errors.New("driver offline")}
	cfgs := []Config{{ID: "garage", DriverName: "easee", MinChargeW: 1400, MaxChargeW: 11000}}
	samples := map[string]EVSample{"easee": {PowerW: 0, Connected: true}}
	c := newTestController(t, cfgs, nil, samples, sender)

	c.Tick(context.Background(), time.Now()) // must not panic
}

// TestConfigsReturnsSnapshotInOrder guards that Manager.Configs feeds
// the controller a deterministic view — otherwise a dispatch cycle
// could command different drivers on each run after a map iteration.
func TestConfigsReturnsSnapshotInOrder(t *testing.T) {
	m := NewManager()
	m.Load([]Config{
		{ID: "garage", DriverName: "easee"},
		{ID: "street", DriverName: "zap"},
	})
	got := m.Configs()
	if len(got) != 2 || got[0].ID != "garage" || got[1].ID != "street" {
		t.Errorf("Configs order broken: %+v", got)
	}
}
