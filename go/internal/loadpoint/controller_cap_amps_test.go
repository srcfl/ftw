package loadpoint

import (
	"context"
	"math"
	"testing"
	"time"
)

// cap_amps_per_phase tells a driver how far an order may rise on its own
// phases. easee_cloud starts a 6 A order at 7 A only when it says 7 A fits;
// max_amps_per_phase is the fuse rating and says nothing about the room.
func TestCommandCarriesCapAmpsPerPhase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		planW     float64
		totalW    float64 // control.EVFuseBudget
		roomA     float64 // control.EVFusePhaseRoomA; NaN: no phase currents
		hold      *ManualHold
		lastW     float64 // last tick's 3p order, which roomA already counts
		wantW     float64
		wantMaxA  float64
		wantCapA  float64
		wantNoCap bool
	}{
		// The room on the busiest phase takes a 16 A plan to 6 A.
		{name: "room limits the order to 6 A", mode: "3p", planW: 11040, totalW: 17250, roomA: 6.4,
			wantW: 4140, wantMaxA: 25, wantCapA: 6.4},
		{name: "room for exactly 7 A", mode: "3p", planW: 4140, totalW: 17250, roomA: 7,
			wantW: 4140, wantMaxA: 25, wantCapA: 7},
		// A 6 A order on its way is in the room already; it counts once.
		{name: "last 6 A order not shown yet", mode: "3p", planW: 4140, totalW: 4416, roomA: .4, lastW: 4140,
			wantW: 4140, wantMaxA: 25, wantCapA: 6.4},
		// The plan wants 6 A; the cap is the room, not the plan.
		{name: "room ample", mode: "3p", planW: 4140, totalW: 17250, roomA: 15,
			wantW: 4140, wantMaxA: 25, wantCapA: 15},
		{name: "room past the charger's maximum", mode: "3p", planW: 4140, totalW: 17250, roomA: 20,
			wantW: 4140, wantMaxA: 25, wantCapA: 16},
		// The hour's import limit or the fuse in total import, in watts.
		{name: "total budget limits", mode: "3p", planW: 4140, totalW: 4500, roomA: 15,
			wantW: 4140, wantMaxA: 25, wantCapA: 6.52},
		{name: "no phase currents", mode: "3p", planW: 4140, totalW: 5175, roomA: math.NaN(),
			wantW: 4140, wantMaxA: 25, wantCapA: 7.5},
		// A hold's current limit is the order's max_amps_per_phase; its
		// power is what the owner wants, as a plan's is.
		{name: "manual hold with a current limit", mode: "3p", totalW: 17250, roomA: 15,
			hold:  &ManualHold{PowerW: 4140, PhaseMode: "3p", MaxAmpsPerPhase: 6.5, Persistent: true},
			wantW: 4140, wantMaxA: 6.5, wantCapA: 6.5},
		{name: "manual hold", mode: "3p", totalW: 17250, roomA: 15,
			hold:  &ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true},
			wantW: 4140, wantMaxA: 25, wantCapA: 15},
		// One phase: watts over one phase's voltage.
		{name: "one phase, total budget", mode: "1p", planW: 3680, totalW: 1500, roomA: 20,
			wantW: 1380, wantMaxA: 25, wantCapA: 6.52},
		{name: "one phase, room", mode: "1p", planW: 1380, totalW: 17250, roomA: 7.5,
			wantW: 1380, wantMaxA: 25, wantCapA: 7.5},
		// A 0 W order names no current to raise.
		{name: "no room", mode: "3p", planW: 4140, totalW: 17250, roomA: 5,
			wantW: 0, wantMaxA: 25, wantNoCap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11040,
				AllowedStepsW: threePhaseSteps, PhaseMode: tc.mode}
			d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
				LoadpointEnergyWh: map[string]float64{cfg.ID: tc.planW / 4}, LoadpointMaxPowerW: map[string]float64{cfg.ID: tc.planW}}
			samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}
			sender := &fakeSender{}
			c := newTestController(t, []Config{cfg}, d, samples, sender)
			c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
			c.SetFuseEVMax(func() (float64, bool) { return tc.totalW, true })
			c.SetFusePhaseRoomA(func() (float64, bool) { return tc.roomA, !math.IsNaN(tc.roomA) })
			if tc.hold != nil {
				c.SetManualHold(cfg.ID, *tc.hold)
			}
			if tc.lastW > 0 {
				c.manager.SetCommanded(cfg.ID, tc.lastW, "plan")
				c.manager.setCommandedPhases(cfg.ID, 3)
			}
			c.Tick(context.Background(), now)
			got, ok := lastSetCurrent(sender.calls)
			if !ok || math.Abs(got.power-tc.wantW) > .01 || got.maxAmpsPerPhase != tc.wantMaxA {
				t.Fatalf("order %+v, want %.0f W with max_amps_per_phase %.1f", got, tc.wantW, tc.wantMaxA)
			}
			if tc.wantNoCap {
				if got.capAmpsPerPhase != nil {
					t.Fatalf("cap_amps_per_phase = %.2f, want none", *got.capAmpsPerPhase)
				}
				return
			}
			if got.capAmpsPerPhase == nil {
				t.Fatal("cap_amps_per_phase missing")
			}
			capA := *got.capAmpsPerPhase
			if math.Abs(capA-tc.wantCapA) > 1e-9 {
				t.Fatalf("cap_amps_per_phase = %v, want %v", capA, tc.wantCapA)
			}
			if capA > got.maxAmpsPerPhase {
				t.Fatalf("cap_amps_per_phase %.2f above max_amps_per_phase %.2f", capA, got.maxAmpsPerPhase)
			}
		})
	}
}

// Without a fuse budget Core knows no cap, and an older Core sends none:
// a driver must read a missing field as "no room above the order".
func TestCommandOmitsCapAmpsWithoutFuseBudget(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1380, MaxChargeW: 11040,
		AllowedStepsW: threePhaseSteps, PhaseMode: "3p"}
	d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 4140 / 4}, LoadpointMaxPowerW: map[string]float64{cfg.ID: 4140}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, d, map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: true}}, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.Tick(context.Background(), now)
	got, ok := lastSetCurrent(sender.calls)
	if !ok || got.power != 4140 {
		t.Fatalf("order %+v, want 4140 W", got)
	}
	if got.capAmpsPerPhase != nil {
		t.Fatalf("cap_amps_per_phase = %.2f without a fuse budget", *got.capAmpsPerPhase)
	}
}

// Two chargers never get the same room. Each keeps its own share, and the
// room neither took goes to the first in the split's order only.
func TestCapAmpsPerPhaseSplitsRoomBetweenChargers(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	a := Config{ID: "a", DriverName: "easee-a", MinChargeW: 1380, MaxChargeW: 11040,
		AllowedStepsW: threePhaseSteps, PhaseMode: "3p"}
	b := a
	b.ID, b.DriverName = "b", "easee-b"
	d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
		LoadpointEnergyWh:  map[string]float64{a.ID: 4140 / 4, b.ID: 4140 / 4},
		LoadpointMaxPowerW: map[string]float64{a.ID: 4140, b.ID: 4140}}
	samples := map[string]EVSample{a.DriverName: {Connected: true, RequestActive: true}, b.DriverName: {Connected: true, RequestActive: true}}
	sender := &fakeSender{}
	c := newTestController(t, []Config{a, b}, d, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetFuseEVMax(func() (float64, bool) { return 17250, true })
	c.SetFusePhaseRoomA(func() (float64, bool) { return 13, true })
	c.Tick(context.Background(), now)
	caps := map[string]float64{}
	for _, sent := range sender.calls {
		if sent.action != "ev_set_current" || sent.power != 4140 || sent.capAmpsPerPhase == nil {
			t.Fatalf("order %+v, want 4140 W with a cap", sent)
		}
		caps[sent.driver] = *sent.capAmpsPerPhase
	}
	// 13 A of room: 6 A each, and the 1 A left goes to a.
	if caps[a.DriverName] != 7 || caps[b.DriverName] != 6 {
		t.Fatalf("caps %v, want a 7 A and b 6 A", caps)
	}
}
