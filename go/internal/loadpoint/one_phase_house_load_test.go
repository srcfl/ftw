package loadpoint

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A three-phase car at 6 A (4140 W, 18 A over one phase's voltage), an oven
// of 3 kW on L1 and a little PV on every phase: the meter reads
// [17, 4.5, 4.5] A. No three-phase car should be marked one-phase.
func TestHouseLoadDoesNotMarkAThreePhaseCar(t *testing.T) {
	r := newPhaseRig()
	r.order(4140)
	r.run(0, 60*time.Second, func(time.Duration) [3]float64 { return [3]float64{17, 4.5, 4.5} },
		func(time.Duration) float64 { return 4140 })
	if got := r.carPhases(); got != 0 {
		t.Errorf("three-phase car at 6 A marked as %d-phase from meter [17 4.5 4.5] A", got)
	}
}

// End to end through Controller.Tick: a planned 4140 W three-phase slot,
// the same meter and charger readings every 5 s. The order must stay near
// 4140 W.
func TestHouseLoadKeepsAPlannedThreePhaseOrder(t *testing.T) {
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040, PhaseMode: "3p"}
	start := time.Now()
	slot := start.Add(-time.Second)
	dir := Directive{SlotStart: slot, SlotEnd: slot.Add(time.Hour), LoadpointEnergyWh: map[string]float64{cfg.ID: 4140}}
	var orders []float64
	send := func(_ context.Context, _ string, p []byte) error {
		var payload map[string]any
		_ = json.Unmarshal(p, &payload)
		w, _ := payload["power_w"].(float64)
		orders = append(orders, w)
		return nil
	}
	var now time.Time
	c := NewController(NewManager(), PlanFunc(func(time.Time) (Directive, bool) { return dir, true }),
		TelemetryFunc(func(string) (EVSample, bool) {
			return EVSample{Connected: true, RequestActive: true, PowerW: 4140, PowerAt: now, PowerMaxAge: 10 * time.Second}, true
		}), send)
	c.manager.Load([]Config{cfg})
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetSitePhases(func() (SitePhases, bool) { return SitePhases{A: [3]float64{17, 4.5, 4.5}, At: now}, true })
	c.manager.Observe(cfg.ID, true, 0, 0, true)
	c.manager.SetSchedule(cfg.ID, Schedule{SoC: .8})
	for d := time.Duration(0); d <= 300*time.Second; d += 5 * time.Second {
		now = start.Add(d)
		c.Tick(context.Background(), now)
	}
	for _, w := range orders {
		if w > 6000 {
			t.Fatalf("a planned 4140 W three-phase slot went out as %.0f W", w)
		}
	}
}

// The same site with a closed loop: a real three-phase car takes the order
// on all three phases, so the meter is the house [12, -1.5, -1.5] A plus the
// order's current on each phase. The order must not swing between 6 A and
// 16 A.
func TestHouseLoadDoesNotSwingAThreePhaseOrder(t *testing.T) {
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 4140, MaxChargeW: 11040, PhaseMode: "3p"}
	start := time.Now()
	slot := start.Add(-time.Second)
	dir := Directive{SlotStart: slot, SlotEnd: slot.Add(time.Hour), LoadpointEnergyWh: map[string]float64{cfg.ID: 4140}}
	var orders []float64
	drawW := 0.0
	send := func(_ context.Context, _ string, p []byte) error {
		var payload map[string]any
		_ = json.Unmarshal(p, &payload)
		w, _ := payload["power_w"].(float64)
		orders = append(orders, w)
		return nil
	}
	var now time.Time
	c := NewController(NewManager(), PlanFunc(func(time.Time) (Directive, bool) { return dir, true }),
		TelemetryFunc(func(string) (EVSample, bool) {
			return EVSample{Connected: true, RequestActive: true, PowerW: drawW, PowerAt: now, PowerMaxAge: 10 * time.Second}, true
		}), send)
	c.manager.Load([]Config{cfg})
	c.SetSiteFuse(SiteFuse{MaxAmps: 35, Voltage: 230, PhaseCnt: 3})
	c.SetSitePhases(func() (SitePhases, bool) {
		a := drawW / 690
		return SitePhases{A: [3]float64{12 + a, -1.5 + a, -1.5 + a}, At: now}, true
	})
	c.manager.Observe(cfg.ID, true, 0, 0, true)
	c.manager.SetSchedule(cfg.ID, Schedule{SoC: .8})
	marks, prev := 0, 0
	for d := time.Duration(0); d <= 15*time.Minute; d += 5 * time.Second {
		now = start.Add(d)
		c.Tick(context.Background(), now)
		if len(orders) > 0 {
			drawW = orders[len(orders)-1] // the car follows the order by the next tick
		}
		st, _ := c.manager.State(cfg.ID)
		if st.CarPhases != prev {
			if st.CarPhases == 1 {
				marks++
			}
			prev = st.CarPhases
		}
	}
	hi := 0
	for _, w := range orders {
		if w > 6000 {
			hi++
		}
	}
	if marks > 0 || hi > 0 {
		t.Fatalf("three-phase car marked one-phase %d times; %d of %d orders above 6 kW on a 4140 W plan", marks, hi, len(orders))
	}
}
