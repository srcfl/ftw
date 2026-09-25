package mpc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

type chargingTransport struct{ feature bool }

func (c *chargingTransport) RoundTrip(context.Context, []byte) ([]byte, error) {
	features := []string{"champion", "ev_duty"}
	if c.feature {
		features = append(features, "charging_periods")
	}
	return json.Marshal(map[string]any{"name": "ftw-solver", "version": "test", "protocol_version": 1, "features": features})
}
func (*chargingTransport) Health(context.Context) (OptimizerRuntimeInfo, error) {
	return OptimizerRuntimeInfo{}, fmt.Errorf("native handshake required")
}
func (*chargingTransport) Close() error { return nil }

func TestChargingPeriodsNegotiatesEachWorkerAndKeepsLegacyWire(t *testing.T) {
	slots, p := externalTestFixture()
	p.Loadpoint = &LoadpointSpec{ID: "car", PluggedIn: true, CapacityWh: 60000, Levels: 11, SoCMax: 1, MaxChargeW: 11000, Charging: DefaultChargingPeriods(true, 120)}
	transport := &chargingTransport{feature: true}
	external, err := NewExternalOptimizer(ExternalOptimizerConfig{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	o := &EnergyplanOptimizer{ExternalOptimizer: external}
	for _, supported := range []bool{true, false, true} {
		transport.feature = supported
		r := external.buildRequest(slots, p)
		if r.FlexLoads[0].Charging != nil {
			t.Fatal("generic sidecar received an unnegotiated field")
		}
		if err := o.addChargingPeriods(context.Background(), &r, p); err != nil {
			t.Fatal(err)
		}
		c := r.FlexLoads[0].Charging
		if (c != nil) != supported {
			t.Fatalf("supported %v, charging %+v", supported, c)
		}
		if supported && (c.MinChargeSeconds != 300 || c.StartCostOre != 5 || !c.InitialCharging || c.InitialChargeSeconds != 120) {
			t.Fatalf("state lost: %+v", c)
		}
	}
}

func TestChargingPeriodsRejectsInvalidPreference(t *testing.T) {
	for _, bad := range []float64{-1, math.Inf(1), math.NaN(), 86401} {
		lp := &LoadpointSpec{ID: "car", PluggedIn: true, CapacityWh: 60000, Levels: 11, SoCMax: 1, MaxChargeW: 11000, Charging: ChargingPeriods{MinChargeSeconds: bad}}
		if err := validateLoadpointSpecs([]*LoadpointSpec{lp}, map[string]string{}); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestNativeChargingPeriodsContinueMeasuredRun(t *testing.T) {
	template := nativeWorker(t, 500*time.Millisecond)
	defer template.Close()
	o, err := NewEnergyplanOptimizer(template.cfg.Command[0])
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	start := time.Now().Add(time.Minute).Truncate(time.Minute)
	slots := make([]Slot, 12)
	for i := range slots {
		slots[i] = Slot{StartMs: start.Add(time.Duration(i) * time.Minute).UnixMilli(), LenMin: 1, PriceOre: 100, Limits: PowerLimits{MaxImportW: 1000}}
	}
	p := Params{Mode: ModeArbitrage, CapacityWh: 10000, SoCMin: .1, SoCMax: .9, InitialSoC: .5, ChargeEfficiency: 1, DischargeEfficiency: 1,
		Loadpoint: &LoadpointSpec{ID: "car", CapacityWh: 1000, Levels: 11, SoCMax: .1, TargetSoC: .1, TargetSlotIdx: 11, PluggedIn: true,
			ChargeEfficiency: 1, MaxChargeW: 1000, AllowedStepsW: []float64{0, 1000}, Charging: DefaultChargingPeriods(true, 180)}}
	plan, err := o.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	var req externalRequest
	if err := json.Unmarshal(plan.OptimizerInput, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.FlexLoads) != 1 || req.FlexLoads[0].Charging == nil || !req.FlexLoads[0].Charging.InitialCharging {
		t.Fatal("measured run missing from request")
	}
	if plan.Actions[0].LoadpointPowerW["car"] <= 0 {
		t.Fatal("current charging was postponed")
	}
	if math.Abs(plan.TotalCostOre-10) > 1e-5 {
		t.Fatalf("preference changed the bill: %g", plan.TotalCostOre)
	}
}
