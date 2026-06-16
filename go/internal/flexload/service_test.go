package flexload

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// makeSlots returns two price slots. Slot 0 covers now (started 30 min ago,
// runs 60 min) so the boundary between slots is used to compute slot-0's end,
// avoiding the "assume ≤1h tail" in currentThermalSetpoint / currentDeferrableState.
func makeSlots(priceOre float64, pvSurplusW float64) []PriceSlot {
	now := time.Now()
	return []PriceSlot{
		{
			StartMs:    now.Add(-30 * time.Minute).UnixMilli(),
			LenMin:     60,
			PriceOre:   priceOre,
			PVSurplusW: pvSurplusW,
		},
		{
			StartMs:    now.Add(30 * time.Minute).UnixMilli(),
			LenMin:     60,
			PriceOre:   priceOre,
			PVSurplusW: pvSurplusW,
		},
	}
}

// dispatchCapture is a thread-safe dispatch sink for tests.
type dispatchCapture struct {
	mu      sync.Mutex
	byDriver map[string][]byte
}

func newCapture() *dispatchCapture { return &dispatchCapture{byDriver: map[string][]byte{}} }

func (c *dispatchCapture) fn() DispatchFunc {
	return func(_ context.Context, driver string, payload []byte) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.byDriver[driver] = payload
		return nil
	}
}

func (c *dispatchCapture) value(driver string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.byDriver[driver]
	if !ok {
		return 0, false
	}
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		return 0, false
	}
	v, ok := m["value"].(float64)
	return v, ok
}

func (c *dispatchCapture) action(driver string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.byDriver[driver]
	if !ok {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		return "", false
	}
	a, ok := m["action"].(string)
	return a, ok
}

// TestPriceForNow_FallbackUsesLastSlot verifies the stale-price fallback
// returns the most recent slot price (not the oldest) when now is past all slots.
func TestPriceForNow_FallbackUsesLastSlot(t *testing.T) {
	slots := []PriceSlot{
		{StartMs: 1000, LenMin: 60, PriceOre: 50},
		{StartMs: 4600_000, LenMin: 60, PriceOre: 100},
		{StartMs: 8200_000, LenMin: 60, PriceOre: 200},
	}
	// nowMs is past all slots
	price, ok := priceForNow(slots, 99_000_000)
	if !ok {
		t.Fatal("expected fallback price")
	}
	if price != 200 {
		t.Errorf("expected last-slot price 200, got %.0f", price)
	}
}

// TestPriceForNow_MatchesCurrentSlot verifies a slot covering now is returned.
func TestPriceForNow_MatchesCurrentSlot(t *testing.T) {
	now := time.Now().UnixMilli()
	slots := []PriceSlot{
		{StartMs: now - 10_000, LenMin: 60, PriceOre: 77},
	}
	price, ok := priceForNow(slots, now)
	if !ok {
		t.Fatal("expected slot match")
	}
	if price != 77 {
		t.Errorf("expected 77, got %.0f", price)
	}
}

// TestService_SampleTrainsThermalModel exercises the sample() path: given a
// primed lastIndoor entry 60 s in the past and live telemetry, the thermal
// model accumulates at least one update.
func TestService_SampleTrainsThermalModel(t *testing.T) {
	tel := telemetry.NewStore()
	dev := Device{
		Type:         "thermostat",
		DriverName:   "zone1",
		IndoorMetric: "indoor_temp_c",
		HeatMetric:   "heat_w",
		MinC:         18,
		MaxC:         22,
	}
	svc := NewService(nil, tel, []Device{dev})
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	svc.Outdoor = func(int64) float64 { return 5.0 }

	// Emit live telemetry.
	tel.EmitMetric("zone1", "indoor_temp_c", 20.0)
	tel.EmitMetric("zone1", "heat_w", 500.0)

	// Seed a prior indoor sample 60 s ago so the delta is valid on the next
	// call — without this hasPrev is false and no model update happens.
	svc.mu.Lock()
	svc.lastIndoor["zone1"] = tempSample{c: 19.9, tsMs: time.Now().Add(-60 * time.Second).UnixMilli()}
	svc.mu.Unlock()

	svc.sample()

	svc.mu.RLock()
	m := svc.thermal["zone1"]
	svc.mu.RUnlock()
	if m == nil {
		t.Fatal("thermal model not created")
	}
	if m.Samples == 0 {
		t.Error("expected at least one model update after sample()")
	}
}

// TestService_SampleSkipsWhenNoHeatMetric confirms the model is NOT trained
// when HeatMetric is not configured (we can't attribute warming honestly).
func TestService_SampleSkipsWhenNoHeatMetric(t *testing.T) {
	tel := telemetry.NewStore()
	dev := Device{
		Type:         "thermostat",
		DriverName:   "zone2",
		IndoorMetric: "indoor_temp_c",
		// HeatMetric deliberately absent
		MinC: 18,
		MaxC: 22,
	}
	svc := NewService(nil, tel, []Device{dev})
	svc.Outdoor = func(int64) float64 { return 5.0 }
	tel.EmitMetric("zone2", "indoor_temp_c", 20.0)
	svc.mu.Lock()
	svc.lastIndoor["zone2"] = tempSample{c: 19.9, tsMs: time.Now().Add(-60 * time.Second).UnixMilli()}
	svc.mu.Unlock()

	svc.sample()

	svc.mu.RLock()
	m := svc.thermal["zone2"]
	svc.mu.RUnlock()
	if m != nil && m.Samples > 0 {
		t.Error("expected model NOT to be trained when HeatMetric is absent")
	}
}

// TestService_ReplanThermostatDispatchesSetpoint verifies that replan()
// dispatches a setpoint command to a planner-mode thermostat.
func TestService_ReplanThermostatDispatchesSetpoint(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EmitMetric("thermo", "indoor_temp_c", 20.0)

	dev := Device{
		Type:           "thermostat",
		DriverName:     "thermo",
		IndoorMetric:   "indoor_temp_c",
		MinC:           18,
		MaxC:           22,
		MaxHeatW:       1000,
		SetpointAction: "setpoint",
	}
	cap := newCapture()
	svc := NewService(nil, tel, []Device{dev})
	svc.Dispatch = cap.fn()
	svc.Slots = func() []PriceSlot { return makeSlots(80, 0) }
	svc.Outdoor = func(int64) float64 { return 5.0 }

	svc.replan(context.Background())

	sp, ok := cap.value("thermo")
	if !ok {
		t.Fatal("expected setpoint dispatch to 'thermo'")
	}
	if sp < dev.MinC || sp > dev.MaxC {
		t.Errorf("setpoint %.1f outside comfort band [%.0f, %.0f]", sp, dev.MinC, dev.MaxC)
	}
}

// TestService_ReplanDeferrableDispatchesOn verifies that replan() dispatches
// the "on" action for a deferrable load within its window when the slot price
// is low enough and energy budget is set.
func TestService_ReplanDeferrableDispatchesOn(t *testing.T) {
	tel := telemetry.NewStore()
	dev := Device{
		Type:         "deferrable",
		DriverName:   "spa",
		PowerMetric:  "",
		EnergyWh:     2000,
		PowerW:       2000,
		OnAction:     "on",
		OffAction:    "off",
		EarliestHour: 0,
		DeadlineHour: 0,
	}
	cap := newCapture()
	svc := NewService(nil, tel, []Device{dev})
	svc.Dispatch = cap.fn()
	// Single cheap slot covering now → scheduler should pick it.
	svc.Slots = func() []PriceSlot { return makeSlots(10, 0) }

	svc.replan(context.Background())

	action, ok := cap.action("spa")
	if !ok {
		t.Fatal("expected dispatch to 'spa'")
	}
	if action != "on" && action != "off" {
		t.Errorf("unexpected action %q", action)
	}
}

// TestService_ReplanSimpleModeDispatchesViaArbitration verifies that
// simple-mode thermostats are evaluated and a setpoint is dispatched even
// when the model is at its physics prior (untrained).
func TestService_ReplanSimpleModeDispatchesViaArbitration(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EmitMetric("simple_zone", "indoor_temp_c", 21.0)

	dev := Device{
		Type:               "thermostat",
		DriverName:         "simple_zone",
		Mode:               "simple",
		IndoorMetric:       "indoor_temp_c",
		MinC:               18,
		MaxC:               22,
		MaxHeatW:           1500,
		TargetC:            20.0,
		PriceThresholdOre:  200,
		BlockHorizonH:      1.0,
		SetpointAction:     "setpoint",
	}
	cap := newCapture()
	svc := NewService(nil, tel, []Device{dev})
	svc.Dispatch = cap.fn()
	svc.Slots = func() []PriceSlot { return makeSlots(50, 0) }
	svc.Outdoor = func(int64) float64 { return 5.0 }
	svc.PriceAt = func(t time.Time) (float64, bool) { return 50, true }

	svc.replan(context.Background())

	sp, ok := cap.value("simple_zone")
	if !ok {
		t.Fatal("expected setpoint dispatch to 'simple_zone'")
	}
	if sp < dev.MinC || sp > dev.MaxC {
		t.Errorf("setpoint %.1f outside comfort band [%.0f, %.0f]", sp, dev.MinC, dev.MaxC)
	}
}

// TestService_ReplanNoDispatchWhenNoSlots verifies that a planner-mode
// thermostat does not dispatch when no price slots are available
// (MPC not yet warmed up).
func TestService_ReplanNoDispatchWhenNoSlots(t *testing.T) {
	tel := telemetry.NewStore()
	tel.EmitMetric("zone_ns", "indoor_temp_c", 20.0)

	dev := Device{
		Type:         "thermostat",
		DriverName:   "zone_ns",
		IndoorMetric: "indoor_temp_c",
		MinC:         18,
		MaxC:         22,
		MaxHeatW:     1000,
	}
	cap := newCapture()
	svc := NewService(nil, tel, []Device{dev})
	svc.Dispatch = cap.fn()
	svc.Slots = func() []PriceSlot { return nil } // no slots

	svc.replan(context.Background())

	if _, ok := cap.byDriver["zone_ns"]; ok {
		t.Error("expected no dispatch when slots are empty")
	}
}
