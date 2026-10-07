package mpc

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

type chargingTransport struct{ feature, publishedPrices bool }

func (c *chargingTransport) RoundTrip(context.Context, []byte) ([]byte, error) {
	features := []string{"champion", "ev_duty"}
	if c.publishedPrices {
		features = append(features, "published_prices")
	}
	if c.feature {
		features = append(features, "charging_periods")
	}
	return json.Marshal(map[string]any{"name": "ftw-solver", "version": "test", "protocol_version": 1, "features": features})
}
func (*chargingTransport) Close() error { return nil }

func TestChargingPeriodsNegotiatesEachWorkerAndKeepsLegacyWire(t *testing.T) {
	slots, p := externalTestFixture()
	p.Loadpoint = &LoadpointSpec{ID: "car", PluggedIn: true, CapacityWh: 60000, Levels: 11, SoCMax: 1, MaxChargeW: 11000, Charging: DefaultChargingPeriods(true, 120)}
	transport := &chargingTransport{feature: true}
	external := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}, transport: transport}
	o := &EnergyplanOptimizer{ExternalOptimizer: external}
	for _, supported := range []bool{true, false, true} {
		transport.feature = supported
		r := external.buildRequest(slots, p)
		if r.FlexLoads[0].Charging != nil {
			t.Fatal("generic sidecar received an unnegotiated field")
		}
		if err := o.prepareEnergyplanRequest(context.Background(), &r, p); err != nil {
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

func TestDepartureMissFollowsEnergyplanNeedRounding(t *testing.T) {
	// The home box: one 300 s run at 4140 W stores 310.5 Wh at 0.9.
	lp := &LoadpointSpec{MaxChargeW: 11000, AllowedStepsW: []float64{0, 4140, 6900, 11000}, ChargeEfficiency: .9,
		Charging: DefaultChargingPeriods(false, 0)}
	if got := lp.departureMissWh(); math.Abs(got-155.25) > 1e-9 {
		t.Fatalf("half a run %.6f, want 155.25", got)
	}
	// Core sends 0.9 for an unset efficiency, and MinChargeW as the step.
	lp.ChargeEfficiency, lp.AllowedStepsW, lp.MinChargeW = 0, nil, 4140
	if got := lp.departureMissWh(); math.Abs(got-155.25) > 1e-9 {
		t.Fatalf("defaults give %.6f, want 155.25", got)
	}
	for name, lp := range map[string]*LoadpointSpec{
		"no minimum run":         {MaxChargeW: 11000, Charging: ChargingPeriods{StartCostOre: 5}},
		"charging at plan start": {MaxChargeW: 11000, Charging: DefaultChargingPeriods(true, 120)},
		"no charging step":       {Charging: DefaultChargingPeriods(false, 0)},
		"run below 2 Wh":         {MaxChargeW: 10, ChargeEfficiency: 1, Charging: DefaultChargingPeriods(false, 0)},
	} {
		if got := lp.departureMissWh(); got != 1 {
			t.Fatalf("%s: %.6f, want the 1 Wh rule", name, got)
		}
	}
}

func TestValidatePlanCountsADepartureMissOnlyAboveHalfARun(t *testing.T) {
	// One run stores 3600 W * 300 s at efficiency 1 = 300 Wh. The car cannot
	// charge, so it misses its whole need; 150 Wh is 1/512 of 76800 Wh.
	for _, tc := range []struct {
		name     string
		need     float64
		charging ChargingPeriods
		counted  bool
	}{
		{"below half a run", 149, DefaultChargingPeriods(false, 0), false},
		{"at half a run", 150, DefaultChargingPeriods(false, 0), false},
		{"above half a run", 151, DefaultChargingPeriods(false, 0), true},
		{"charging at plan start", 2, DefaultChargingPeriods(true, 120), true},
		{"no minimum run", 2, ChargingPeriods{}, true},
		{"no minimum run, 1 Wh", 1, ChargingPeriods{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots, p := nearTargetReserveFixture()
			p.Loadpoint = &LoadpointSpec{ID: "easee", CapacityWh: 76800, InitialSoC: .75 - tc.need/76800, SoCMax: .75,
				Levels: 11, PluggedIn: true, TargetSoC: .75, TargetSlotIdx: 3, MaxChargeW: 11000,
				AllowedStepsW: []float64{0, 3600, 11000}, ChargeEfficiency: 1, SurplusOnly: true, Charging: tc.charging}
			plan := coreReservePlan(context.Background(), slots, p)
			if err := ValidatePlan(slots, p, &plan); err != nil {
				t.Fatal(err)
			}
			missing, counted := plan.LoadpointShortfallWh["easee"]
			if counted != tc.counted || (counted && math.Abs(missing-tc.need) > 1e-6) {
				t.Fatalf("need %.3f Wh: shortfall %v", tc.need, plan.LoadpointShortfallWh)
			}
		})
	}
}
