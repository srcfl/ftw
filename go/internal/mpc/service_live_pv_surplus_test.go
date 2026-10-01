package mpc

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/state"
)

const liveSurplusCapKey = "live_pv_surplus_soc_cap"

// scheduledPlanOptimizer turns a fixed battery schedule into a plan that
// passes ValidatePlan, so a test decides which later slots buy grid energy.
type scheduledPlanOptimizer struct{ batteryW []float64 }

func (o scheduledPlanOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	plan := Plan{
		GeneratedAtMs: time.Now().UnixMilli(), Mode: p.Mode, HorizonSlots: len(slots),
		CapacityWh: p.CapacityWh, InitialSoC: p.InitialSoC,
		Solver: &SolverInfo{Engine: "test", Status: "optimal"},
	}
	soc := p.InitialSoC
	for i, slot := range slots {
		batteryW := 0.0
		if i < len(o.batteryW) {
			batteryW = o.batteryW[i]
		}
		soc += loadpoint.BatteryEnergyDeltaWh(batteryW, slot.DurationHours(),
			p.ChargeEfficiency, p.DischargeEfficiency) / p.CapacityWh
		a := Action{
			SlotStartMs: slot.StartMs, SlotLenMin: slot.LenMin, ExecutionStartMs: slot.ExecutionStartMs,
			PriceOre: slot.PriceOre, SpotOre: slot.SpotOre, PVW: slot.PVW, LoadW: slot.LoadW,
			BatteryW: batteryW, GridW: loadpoint.GridW(slot.LoadW, slot.PVW, batteryW, 0), SoC: soc,
		}
		a.CostOre = reserveSlotCost(slot, p, a)
		plan.TotalCostOre += a.CostOre
		plan.Actions = append(plan.Actions, a)
	}
	return plan, nil
}

func (scheduledPlanOptimizer) Close() error { return nil }

// newLiveSurplusService plans four 15-minute slots that import at 100 öre
// and export at 50 öre, with a 500 W house load and no sun. Later grid
// charge at 100 öre clears the 50 öre export plus the 20 öre minimum spread.
func newLiveSurplusService(t *testing.T, optimizer PlanOptimizer) *Service {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	start := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute)
	for i := 0; i < 4; i++ {
		if err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: start.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModeArbitrage, SoCLevels: 101, ActionLevels: 41,
		CapacityWh: 10000, SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		// Stored energy is worth more than it costs to buy, so the Core
		// planner charges from the grid in every slot.
		TerminalSoCPrice: 300,
	})
	svc.BaseLoad = 500
	svc.MinArbitrageSpreadOreKwh = 20
	svc.Optimizer = optimizer
	return svc
}

// actionJSON returns each published action as raw JSON fields, the shape the
// plan API serves.
func actionJSON(t *testing.T, plan *Plan) []map[string]json.RawMessage {
	t.Helper()
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Actions []map[string]json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Actions) != len(plan.Actions) {
		t.Fatalf("JSON has %d actions, plan has %d", len(wire.Actions), len(plan.Actions))
	}
	return wire.Actions
}

// assertDispatchReadsStoredCaps checks that dispatch gets the stored cap for
// every slot, and that the stored cap is what the published params give.
func assertDispatchReadsStoredCaps(t *testing.T, svc *Service, plan *Plan) {
	t.Helper()
	svc.mu.RLock()
	params := svc.directiveParamsLocked(svc.lastParams)
	svc.mu.RUnlock()
	for i, a := range plan.Actions {
		if want := livePVSurplusSoCCap(plan.Actions, i, params); a.LivePVSurplusSoCCap != want {
			t.Errorf("slot %d stored cap = %v, want %v from the published params", i, a.LivePVSurplusSoCCap, want)
		}
		d, ok := svc.SlotDirectiveAt(time.UnixMilli(a.ExecutionStart()).Add(time.Second))
		if !ok {
			t.Fatalf("slot %d: no directive", i)
		}
		if d.LivePVSurplusSoCCap != a.LivePVSurplusSoCCap {
			t.Errorf("slot %d directive cap = %v, plan API cap = %v", i, d.LivePVSurplusSoCCap, a.LivePVSurplusSoCCap)
		}
	}
}

// Every solver path publishes through one gate. A plan that buys grid energy
// later, at an import price above this slot's export price plus the minimum
// spread, must tell the plan API and dispatch the same SoC ceiling.
func TestReplanStoresLivePVSurplusSoCCap(t *testing.T) {
	cases := []struct {
		name      string
		optimizer PlanOptimizer
		fallback  bool
		want      float64 // 0: any value above zero
	}{
		// Idle now, then 2 kW of grid charge in each of the next two slots:
		// 0.5 + 2 × 2000 W × 0.25 h × 0.95 / 10 kWh.
		{name: "external optimizer", optimizer: scheduledPlanOptimizer{batteryW: []float64{0, 2000, 2000, 0}}, want: 0.595},
		{name: "core planner"},
		{name: "core fallback after optimizer failure", optimizer: failingPrimaryOptimizer{}, fallback: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newLiveSurplusService(t, tc.optimizer)
			plan := svc.Replan(context.Background())
			if plan == nil || len(plan.Actions) != 4 {
				t.Fatalf("plan = %+v", plan)
			}
			if got := plan.Solver != nil && plan.Solver.Fallback; got != tc.fallback {
				t.Fatalf("fallback = %v, want %v", got, tc.fallback)
			}
			if svc.Latest() != plan {
				t.Fatal("Replan returned a plan it did not publish")
			}
			got := plan.Actions[0].LivePVSurplusSoCCap
			if got <= 0 || got > 1 {
				t.Fatalf("current slot cap = %v, want a SoC above zero", got)
			}
			if tc.want > 0 && math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("current slot cap = %v, want %v", got, tc.want)
			}
			assertDispatchReadsStoredCaps(t, svc, plan)
			raw, ok := actionJSON(t, plan)[0][liveSurplusCapKey]
			if !ok {
				t.Fatalf("plan JSON omits %s", liveSurplusCapKey)
			}
			var wire float64
			if err := json.Unmarshal(raw, &wire); err != nil || wire != got {
				t.Fatalf("plan JSON %s = %s, want %v", liveSurplusCapKey, raw, got)
			}
		})
	}
}

// Without later grid-funded charge the planner gives no permission. The plan
// JSON still sends the zero, so a client can tell it from a box without the
// field.
func TestReplanWithoutLaterGridChargeSendsZeroLivePVSurplusSoCCap(t *testing.T) {
	svc := newLiveSurplusService(t, scheduledPlanOptimizer{batteryW: []float64{0, 0, -1000, -1000}})
	plan := svc.Replan(context.Background())
	if plan == nil || len(plan.Actions) != 4 {
		t.Fatalf("plan = %+v", plan)
	}
	for i, a := range plan.Actions {
		if a.LivePVSurplusSoCCap != 0 {
			t.Errorf("slot %d cap = %v, want 0", i, a.LivePVSurplusSoCCap)
		}
	}
	assertDispatchReadsStoredCaps(t, svc, plan)
	for i, fields := range actionJSON(t, plan) {
		if raw, ok := fields[liveSurplusCapKey]; !ok || string(raw) != "0" {
			t.Errorf("slot %d JSON has %s = %s (present %v), want 0", i, liveSurplusCapKey, raw, ok)
		}
	}
}

// A diagnostic snapshot does not store the caps. After a restart, the
// restored plan must still give dispatch and the plan API the same ceiling
// the live plan had.
func TestRestoredDiagnosticKeepsLivePVSurplusSoCCap(t *testing.T) {
	svc := newLiveSurplusService(t, scheduledPlanOptimizer{batteryW: []float64{0, 2000, 2000, 0}})
	var saved []byte
	svc.SaveDiag = func(d *Diagnostic, _ string) error {
		var err error
		saved, err = json.Marshal(d)
		return err
	}
	plan := svc.Replan(context.Background())
	if plan == nil || saved == nil {
		t.Fatalf("plan = %+v, diagnostic saved = %t", plan, saved != nil)
	}
	var d Diagnostic
	if err := json.Unmarshal(saved, &d); err != nil {
		t.Fatal(err)
	}

	restored := &Service{Defaults: Params{Mode: ModeArbitrage}}
	if !restored.RestoreDiagnostic(&d, time.Now(), "") {
		t.Fatal("RestoreDiagnostic returned false")
	}
	latest := restored.Latest()
	if latest == nil || len(latest.Actions) != len(plan.Actions) {
		t.Fatalf("restored plan = %+v", latest)
	}
	for i := range plan.Actions {
		if got, want := latest.Actions[i].LivePVSurplusSoCCap, plan.Actions[i].LivePVSurplusSoCCap; math.Abs(got-want) > 1e-9 {
			t.Errorf("slot %d restored cap = %v, live plan had %v", i, got, want)
		}
	}
	if latest.Actions[0].LivePVSurplusSoCCap <= 0 {
		t.Fatalf("restored current slot cap = %v, want above zero", latest.Actions[0].LivePVSurplusSoCCap)
	}
	assertDispatchReadsStoredCaps(t, restored, latest)
}

// InstallPlan stores the caps on its own copy and leaves the caller's
// actions as they were.
func TestInstallPlanDoesNotWriteCallerActions(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Minute)
	actions := []Action{
		{SlotStartMs: start.UnixMilli(), SlotLenMin: 15, SpotOre: 40, SoC: 0.5},
		{SlotStartMs: start.Add(15 * time.Minute).UnixMilli(), SlotLenMin: 15, PriceOre: 120, BatteryW: 2000, GridW: 2500, SoC: 0.55},
	}
	svc := &Service{}
	svc.InstallPlan(Plan{GeneratedAtMs: now.UnixMilli(), Actions: actions},
		Params{Mode: ModeArbitrage, CapacityWh: 10000, ChargeEfficiency: 1}, "")
	if actions[0].LivePVSurplusSoCCap != 0 {
		t.Fatalf("caller action cap = %v, want it untouched", actions[0].LivePVSurplusSoCCap)
	}
	if got := svc.Latest().Actions[0].LivePVSurplusSoCCap; math.Abs(got-0.55) > 1e-9 {
		t.Fatalf("published cap = %v, want 0.55", got)
	}
}

// The Core DP shadow swaps in a copy of the active plan with its comparison
// data. The copy must keep the stored caps.
func TestCoreDPShadowKeepsLivePVSurplusSoCCap(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Minute)
	svc := &Service{}
	svc.InstallPlan(Plan{DecisionID: testDecisionID1, GeneratedAtMs: now.UnixMilli(), Actions: []Action{
		{SlotStartMs: start.UnixMilli(), SlotLenMin: 15, SpotOre: 40, SoC: 0.5},
		{SlotStartMs: start.Add(15 * time.Minute).UnixMilli(), SlotLenMin: 15, PriceOre: 120, BatteryW: 2000, GridW: 2500, SoC: 0.55},
	}}, Params{Mode: ModeArbitrage, CapacityWh: 10000, ChargeEfficiency: 1}, "")
	before := svc.Latest()
	svc.recordCoreDPShadow(*before, nil, Params{}, "test", now.UnixMilli(), &ShadowPlan{})
	after := svc.Latest()
	if after == before || after.DPShadow == nil {
		t.Fatal("shadow result did not replace the active plan")
	}
	if got := after.Actions[0].LivePVSurplusSoCCap; got != before.Actions[0].LivePVSurplusSoCCap || got <= 0 {
		t.Fatalf("cap after shadow = %v, before = %v", got, before.Actions[0].LivePVSurplusSoCCap)
	}
	assertDispatchReadsStoredCaps(t, svc, after)
}
