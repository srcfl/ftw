package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A 25 A three-phase site. The car is planned at 11 kW but draws 8.3 kW
// because the charger's load balancer cut it on L1, while the battery
// charges 4.75 kW. The total still fits under the fuse, so the old
// aggregate check let the battery keep the headroom the car needed.
const (
	phaseSiteFuseA   = 25.0
	phaseSiteMarginA = 0.5
	phaseSiteVoltage = 230.0
	phaseSiteFuseW   = phaseSiteFuseA * phaseSiteVoltage * 3
	phaseSiteEVPlanW = 11000.0
	phaseSiteEVNowW  = 8300.0
	phaseSiteBatW    = 4750.0
)

func phaseSiteStore(t *testing.T, houseA [3]float64) *telemetry.Store {
	t.Helper()
	symmetricA := (phaseSiteEVNowW + phaseSiteBatW) / (3 * phaseSiteVoltage)
	houseW := (houseA[0] + houseA[1] + houseA[2]) * phaseSiteVoltage
	data, err := json.Marshal(map[string]float64{
		"l1_a": houseA[0] + symmetricA,
		"l2_a": houseA[1] + symmetricA,
		"l3_a": houseA[2] + symmetricA,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, houseW+phaseSiteEVNowW+phaseSiteBatW, nil, data)
	store.DriverHealthMut("meter").RecordSuccess()
	soc := 0.5
	store.Update("pixii", telemetry.DerBattery, phaseSiteBatW, &soc, nil)
	store.DriverHealthMut("pixii").RecordSuccess()
	return store
}

func phaseSiteState() *State {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:          now,
		SlotEnd:            now.Add(15 * time.Minute),
		BatteryEnergyWh:    phaseSiteBatW / 4,
		LoadpointEnergyWh:  map[string]float64{"easee": phaseSiteEVPlanW / 4},
		LoadpointMaxPowerW: map[string]float64{"easee": phaseSiteEVPlanW},
		Strategy:           "arbitrage",
	}
	st := newStateWithEnergyDispatch(dir, "meter")
	st.SiteFuseAmps = phaseSiteFuseA
	st.SiteFuseVoltage = phaseSiteVoltage
	st.SiteFusePhases = 3
	st.SiteFuseSafetyA = phaseSiteMarginA
	st.EVChargingW = phaseSiteEVNowW
	st.DriverLimits = map[string]PowerLimits{"pixii": {MaxChargeW: 6000, MaxDischargeW: 6000}}
	return st
}

func TestScheduledEVGetsWorstPhaseBeforeBatteryCharge(t *testing.T) {
	houseA := [3]float64{4, 0.5, 0.5} // 1150 W, mostly on L1
	store := phaseSiteStore(t, houseA)
	st := phaseSiteState()

	targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 15000}), phaseSiteFuseW)
	if len(targets) != 1 {
		t.Fatalf("targets %v", targets)
	}
	if !st.FuseSaturated || math.Abs(st.FuseEVMaxW-phaseSiteEVPlanW) > 1 {
		t.Fatalf("planned car lost its share: cap %.0f saturated %v", st.FuseEVMaxW, st.FuseSaturated)
	}
	batW := targets[0].TargetW
	if batW < 0 {
		t.Fatalf("battery forced to discharge: %v", targets)
	}
	worstA := houseA[0] + (batW+phaseSiteEVPlanW)/(3*phaseSiteVoltage)
	if worstA > phaseSiteFuseA-phaseSiteMarginA+0.01 {
		t.Fatalf("L1 at %.2f A with battery %.0f W and car %.0f W; limit %.1f A",
			worstA, batW, phaseSiteEVPlanW, phaseSiteFuseA-phaseSiteMarginA)
	}
	if batW < 3000 {
		t.Fatalf("battery gave up more than L1 needs: %.0f W", batW)
	}
}

func TestBalancedHouseKeepsBatteryCharge(t *testing.T) {
	store := phaseSiteStore(t, [3]float64{5.0 / 3, 5.0 / 3, 5.0 / 3}) // same 1150 W, balanced
	st := phaseSiteState()

	targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 15000}), phaseSiteFuseW)
	if len(targets) != 1 {
		t.Fatalf("targets %v", targets)
	}
	if st.FuseSaturated {
		t.Fatalf("balanced phases fit under the fuse; cap %.0f", st.FuseEVMaxW)
	}
	if math.Abs(targets[0].TargetW-phaseSiteBatW) > 1 {
		t.Fatalf("battery target %.0f W, want planned %.0f W", targets[0].TargetW, phaseSiteBatW)
	}
}

func TestPhaseImportCeiling(t *testing.T) {
	st := phaseSiteState()
	store := phaseSiteStore(t, [3]float64{4, 0.5, 0.5})
	meter := store.Get("meter", telemetry.DerMeter)
	aggregate := phaseSiteFuseW - phaseSiteMarginA*3*phaseSiteVoltage
	imbalanceW := (4 - 5.0/3) * 3 * phaseSiteVoltage

	if got := st.phaseImportCeilingW(phaseSiteFuseW, meter); math.Abs(got-(aggregate-imbalanceW)) > 1 {
		t.Fatalf("ceiling %.0f W, want %.0f W", got, aggregate-imbalanceW)
	}
	if got := PhaseImbalanceW(store, st, time.Minute, time.Now()); math.Abs(got-imbalanceW) > 1 {
		t.Fatalf("imbalance %.0f W, want %.0f W", got, imbalanceW)
	}
	if got := PhaseImbalanceW(store, st, time.Minute, time.Now().Add(2*time.Minute)); got != 0 {
		t.Fatalf("stale meter gave imbalance %.0f W", got)
	}

	// A tariff peak is a billing total, not a breaker: the imbalance is
	// not taken off it.
	st.PeakImportCeilingW = 9000
	if got := st.phaseImportCeilingW(phaseSiteFuseW, meter); got != 9000 {
		t.Fatalf("peak ceiling %.0f W, want 9000 W", got)
	}
	st.PeakImportCeilingW = 0

	// A meter without every configured phase gives the aggregate ceiling.
	partial := telemetry.NewStore()
	data, _ := json.Marshal(map[string]float64{"l1_a": 20, "l2_a": 10})
	partial.Update("meter", telemetry.DerMeter, 8000, nil, data)
	if got := st.phaseImportCeilingW(phaseSiteFuseW, partial.Get("meter", telemetry.DerMeter)); got != aggregate {
		t.Fatalf("partial phases: ceiling %.0f W, want %.0f W", got, aggregate)
	}

	// Exporting on every phase is not an import imbalance.
	export := telemetry.NewStore()
	data, _ = json.Marshal(map[string]float64{"l1_a": -10, "l2_a": -10, "l3_a": -10})
	export.Update("meter", telemetry.DerMeter, -30*phaseSiteVoltage, nil, data)
	if got := st.phaseImportCeilingW(phaseSiteFuseW, export.Get("meter", telemetry.DerMeter)); got != aggregate {
		t.Fatalf("balanced export: ceiling %.0f W, want %.0f W", got, aggregate)
	}
}
