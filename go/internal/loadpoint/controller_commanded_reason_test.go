package loadpoint

import (
	"context"
	"testing"
	"time"
)

// commanded_reason (#1009): every dispatch branch that decides the
// commanded watts must name itself, so the UI can say WHY the box
// paused instead of leaving operators to debug cable and charger.

func reasonLoadpoint() Config {
	return Config{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    4140,
		MaxChargeW:    11000,
		AllowedStepsW: ftwStepSet,
		PhaseMode:     "3p",
	}
}

func commandedReason(t *testing.T, c *Controller, id string) (float64, string) {
	t.Helper()
	st, ok := c.manager.State(id)
	if !ok {
		t.Fatalf("loadpoint %q missing", id)
	}
	return st.CommandedW, st.CommandedReason
}

func TestCommandedReasonNoPlanBudget(t *testing.T) {
	sender := &fakeSender{}
	cfg := reasonLoadpoint()
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)

	c.Tick(context.Background(), time.Now())

	if w, r := commandedReason(t, c, cfg.ID); w != 0 || r != "no_plan_budget" {
		t.Errorf("no plan: want (0, no_plan_budget), got (%.0f, %q)", w, r)
	}
}

func TestCommandedReasonPlan(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfg := reasonLoadpoint()
	dir := &Directive{
		SlotStart:         base.Add(-1 * time.Second),
		SlotEnd:           base.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 2000},
	}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)

	c.Tick(context.Background(), base)

	if w, r := commandedReason(t, c, cfg.ID); w <= 0 || r != "plan" {
		t.Errorf("plan budget: want (>0, plan), got (%.0f, %q)", w, r)
	}
}

func TestCommandedReasonSurplusPause(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfg := reasonLoadpoint()
	cfg.SurplusOnly = true
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })

	c.Tick(context.Background(), base)

	if w, r := commandedReason(t, c, cfg.ID); w != 0 || r != "pv_surplus_pause" {
		t.Errorf("no PV surplus: want (0, pv_surplus_pause), got (%.0f, %q)", w, r)
	}
}

func TestCommandedReasonFuseCapZero(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfg := reasonLoadpoint()
	dir := &Directive{
		SlotStart:         base.Add(-1 * time.Second),
		SlotEnd:           base.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 2000},
	}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	// Joint fuse allocator says the EV may have nothing. computeCommand
	// honours the cap before snapping, so the 0 is born there — it must
	// still be named a fuse decision, never "the plan chose 0".
	c.SetFuseEVMax(func() (float64, bool) { return 0, true })

	c.Tick(context.Background(), base)

	if w, r := commandedReason(t, c, cfg.ID); w != 0 || r != "fuse_limit" {
		t.Errorf("fuse cap 0: want (0, fuse_limit), got (%.0f, %q)", w, r)
	}
}

func TestCommandedReasonFuseCooldownOverridesManualHold(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfg := reasonLoadpoint()
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 11040, PhaseMode: "3p", Persistent: true})
	// The hold wants 11 kW, the fuse allows nothing below the minimum
	// step — the clamp pauses, arms the cooldown, and the reason must
	// say fuse, not manual_hold.
	c.SetFuseEVMax(func() (float64, bool) { return 0, true })

	c.Tick(context.Background(), base)

	if w, r := commandedReason(t, c, cfg.ID); w != 0 || r != "fuse_cooldown" {
		t.Errorf("hold vs fuse: want (0, fuse_cooldown), got (%.0f, %q)", w, r)
	}
}

func TestCommandedReasonSiteMeterStale(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfg := reasonLoadpoint()
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)

	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, Persistent: true})
	c.TickWithDispatch(context.Background(), base, false)
	h, _ := c.GetManualHold(cfg.ID, base)
	st, _ := c.manager.State(cfg.ID)
	got := ManualStatusFrom(h, true, st, ChargerReading{Known: true, UpdatedAt: time.Now()}, time.Now())
	if got.State != ManualLimited || got.LimitReason != "site_meter_stale" {
		t.Fatalf("manual standdown lost its known reason: %+v", got)
	}

	if w, r := commandedReason(t, c, cfg.ID); w != 0 || r != "site_meter_stale" {
		t.Errorf("standdown: want (0, site_meter_stale), got (%.0f, %q)", w, r)
	}
}

func TestCommandedReasonManualHold(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	cfg := reasonLoadpoint()
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 11000, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 11040, PhaseMode: "3p", Persistent: true})

	c.Tick(context.Background(), base)

	if w, r := commandedReason(t, c, cfg.ID); w != 11000 || r != "charger_limit" {
		t.Errorf("hold: want (11000, charger_limit), got (%.0f, %q)", w, r)
	}
}
