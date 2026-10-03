package mpc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func shadowTestService(t *testing.T) *Service {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		if err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: 50 + float64(i)*40, TotalOreKwh: 100 + float64(i)*80,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModePassiveArbitrage, SoCLevels: 11, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		// Pinned so the terminal credit is an exact number the test can
		// assert by hand instead of a price-derived default.
		TerminalSoCPrice: 200,
	})
	svc.BaseLoad = 500
	return svc
}

// waitFor polls until cond holds. The shadow lands asynchronously by design,
// so tests wait for it instead of assuming an ordering.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// shadowSkipFixture returns a service with a published plan, a clock the test
// moves, and that plan's inputs with and without a plugged-in car. The car's
// SoC levels and charger steps make each slot far more work for Core DP. The
// clock only moves between shadows, after shadowWG.Wait.
func shadowSkipFixture(t *testing.T) (svc *Service, now *time.Time, slots []Slot, battery, ev Params) {
	t.Helper()
	svc = shadowTestService(t)
	if svc.Replan(context.Background()) == nil {
		t.Fatal("no base plan")
	}
	clock := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return clock }
	slots, battery = svc.lastSlots, svc.lastParams
	ev = battery
	ev.Loadpoint = &LoadpointSpec{ID: "car", CapacityWh: 60000, Levels: 11, InitialSoC: .4, SoCMax: 1,
		PluggedIn: true, MaxChargeW: 11000, ChargeEfficiency: .9, AllowedStepsW: []float64{0, 4140, 11000}}
	ev.Loadpoints = []*LoadpointSpec{ev.Loadpoint}
	return svc, &clock, slots, battery, ev
}

// publishAndShadow makes a Core DP plan for these inputs the current plan,
// starts its shadow and waits for any background comparison to finish.
func publishAndShadow(t *testing.T, svc *Service, id string, slots []Slot, p Params) *ShadowPlan {
	t.Helper()
	champion := Optimize(slots, p)
	champion.DecisionID = id
	svc.mu.Lock()
	svc.last, svc.executionPlan, svc.lastSlots, svc.lastParams = &champion, &champion, slots, p
	svc.mu.Unlock()
	svc.startCoreDPShadow(champion, slots, p, "test", champion.GeneratedAtMs)
	svc.shadowWG.Wait()
	return svc.Latest().DPShadow
}

// On a Raspberry Pi 4 a shadow with a car in it needs far more than its
// deadline. After one runs out of time, the next replan of that size records
// a skip and its reason instead of spending another 10 s of CPU.
func TestCoreDPShadowSkipsAfterTimeout(t *testing.T) {
	svc, _, slots, _, ev := shadowSkipFixture(t)
	var persisted []*ShadowPlan
	svc.SaveDiag = func(d *Diagnostic, _ string) error {
		persisted = append(persisted, d.DPShadow)
		return nil
	}
	svc.shadowTimeout = -1 // already expired: the comparison runs out of time at once
	if got := publishAndShadow(t, svc, "slow", slots, ev); got == nil || got.Solver.Status != "rejected" {
		t.Fatalf("shadow past its deadline = %+v, want rejected", got)
	}

	svc.shadowTimeout = 0 // the real 10 s: a comparison that ran would finish
	got := publishAndShadow(t, svc, "next", slots, ev)
	if got == nil || got.Solver == nil || got.Solver.Status != "skipped" || got.ComparedSlots != 0 {
		t.Fatalf("same-size shadow after a timeout = %+v, want a recorded skip", got)
	}
	if why := got.Solver.FallbackReason; !strings.Contains(why, "ran out of time") || !strings.Contains(why, "2026-10-03T13:00:00Z") {
		t.Fatalf("skip reason %q must say why and when Core tries again", why)
	}
	if d := svc.Diagnose(); d == nil || d.DecisionID != "next" || d.DPShadow == nil || d.DPShadow.Solver.Status != "skipped" {
		t.Fatalf("diagnostic does not show the skip: %+v", d)
	}
	if n := len(persisted); n == 0 || persisted[n-1] == nil || persisted[n-1].Solver.Status != "skipped" {
		t.Fatal("persisted diagnostic does not show the skip")
	}
}

// A skip covers shadows at least as large as the one that ran out of time.
// A battery-only shadow is far smaller than one with a car and still runs.
func TestCoreDPShadowSkipKeepsSmallerShadows(t *testing.T) {
	svc, _, slots, battery, ev := shadowSkipFixture(t)
	svc.shadowTimeout = -1
	publishAndShadow(t, svc, "slow", slots, ev)
	svc.shadowTimeout = 0
	got := publishAndShadow(t, svc, "battery", slots, battery)
	if got == nil || got.Solver.Status != "optimal" || got.ComparedSlots != len(slots) {
		t.Fatalf("battery-only shadow while EV shadows are skipped = %+v, want a comparison", got)
	}
}

// The skip lasts an hour, then a shadow of that size runs again. The horizon
// shrinks through the day, so a later try may fit the deadline.
func TestCoreDPShadowRetriesAfterAnHour(t *testing.T) {
	svc, now, slots, _, ev := shadowSkipFixture(t)
	svc.shadowTimeout = -1
	publishAndShadow(t, svc, "slow", slots, ev)
	svc.shadowTimeout = 0
	*now = now.Add(59 * time.Minute)
	if got := publishAndShadow(t, svc, "early", slots, ev); got == nil || got.Solver.Status != "skipped" {
		t.Fatalf("shadow 59 min after a timeout = %+v, want skipped", got)
	}
	*now = now.Add(time.Minute)
	got := publishAndShadow(t, svc, "retry", slots, ev)
	if got == nil || got.Solver.Status != "optimal" || got.ComparedSlots != len(slots) {
		t.Fatalf("shadow an hour after a timeout = %+v, want a comparison", got)
	}
}

// The same rule through real replans: Energyplan plans, a car is plugged in,
// and the shadow runs out of time. The next replan with the car records a
// skip; a replan after the car leaves gets its comparison.
func TestNativeEnergyplanSkipsEVShadowAfterTimeout(t *testing.T) {
	template := nativeWorker(t, 500*time.Millisecond)
	defer template.Close()
	o, err := NewEnergyplanOptimizer(template.cfg.Command[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	svc := shadowTestService(t)
	svc.Optimizer = o
	_, fixture := nativeFixture()
	car, plugged := *fixture.Loadpoint, true
	svc.Loadpoint = func(int) *LoadpointSpec {
		if !plugged {
			return nil
		}
		copy := car
		return &copy
	}
	replan := func(what string) *ShadowPlan {
		t.Helper()
		plan := svc.Replan(context.Background())
		if plan == nil || plan.Solver == nil || plan.Solver.Fallback || plan.Solver.Engine == "core" {
			t.Fatalf("%s: Energyplan did not plan: %+v", what, plan)
		}
		if (svc.lastParams.Loadpoint != nil) != plugged {
			t.Fatalf("%s: car in plan = %v, want %v", what, svc.lastParams.Loadpoint != nil, plugged)
		}
		svc.shadowWG.Wait()
		return svc.Diagnose().DPShadow
	}

	svc.shadowTimeout = -1
	if got := replan("first"); got == nil || got.Solver.Status != "rejected" {
		t.Fatalf("EV shadow past its deadline = %+v, want rejected", got)
	}
	svc.shadowTimeout = 0
	if got := replan("second"); got == nil || got.Solver.Status != "skipped" || got.ComparedSlots != 0 {
		t.Fatalf("next EV shadow = %+v, want a recorded skip", got)
	}
	plugged = false
	if got := replan("battery"); got == nil || got.Solver.Status != "optimal" || got.ComparedSlots == 0 {
		t.Fatalf("battery-only shadow = %+v, want a comparison", got)
	}
}

// Only the deadline marks a size as too slow. A newer replan or Stop cancels
// a shadow for reasons that say nothing about the box's speed.
func TestCoreDPShadowCancellationDoesNotSkip(t *testing.T) {
	svc := shadowTestService(t)
	slots, p := nativeBenchmarkFixture(true)
	p.SoCLevels, p.ActionLevels = 101, 201
	running := Plan{DecisionID: "cancelled"}
	svc.last = &running
	svc.startCoreDPShadow(running, slots, p, "test", 0)
	svc.mu.Lock()
	svc.shadowCancel()
	svc.mu.Unlock()
	svc.shadowWG.Wait()
	got := publishAndShadow(t, svc, "next", slots[:4], p)
	if got == nil || got.Solver.Status != "optimal" || got.ComparedSlots != 4 {
		t.Fatalf("same-size shadow after a cancellation = %+v, want a comparison", got)
	}
}
