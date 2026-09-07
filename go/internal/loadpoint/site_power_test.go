package loadpoint

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestGridWIncludesEV(t *testing.T) {
	// 500 house, 8 kW PV, 10 kW battery charge, 4.14 kW EV → import.
	if got := GridW(500, -8000, 10000, 4140); got != 6640 {
		t.Errorf("GridW = %.0f, want 6640", got)
	}
}

func TestPVLeftoverAndHouseResidualAreComplements(t *testing.T) {
	if got := PVLeftoverAfterHouseW(500, -8000); got != 7500 {
		t.Errorf("leftover = %.0f, want 7500", got)
	}
	if got := HouseResidualW(500, -8000); got != 0 {
		t.Errorf("residual with surplus = %.0f, want 0", got)
	}
	if got := PVLeftoverAfterHouseW(2000, -500); got != 0 {
		t.Errorf("leftover when house wins = %.0f, want 0", got)
	}
	if got := HouseResidualW(2000, -500); got != 1500 {
		t.Errorf("residual = %.0f, want 1500", got)
	}
}

func TestSurplusOnlyExceedsHousePV(t *testing.T) {
	if SurplusOnlyExceedsHousePV(4140, 500, -8000) {
		t.Fatal("4140 W fits in 7500 W leftover")
	}
	if !SurplusOnlyExceedsHousePV(4140, 500, 0) {
		t.Fatal("4140 W with no PV must exceed leftover")
	}
	if SurplusOnlyExceedsHousePV(SitePowerEpsW, 500, 0) {
		t.Fatal("idle/noise EV must not trip leftover")
	}
}

func TestBatteryDischargeFeedsEV(t *testing.T) {
	if !BatteryDischargeFeedsEV(-4000, 4000, 500, 0) {
		t.Fatal("4000 W discharge with 500 W house residual must count as feeding EV")
	}
	if BatteryDischargeFeedsEV(-400, 4000, 500, 0) {
		t.Fatal("discharge within house residual is house cover, not EV feed")
	}
	if BatteryDischargeFeedsEV(4000, 4140, 500, -8000) {
		t.Fatal("battery charge cannot feed the EV")
	}
	if BatteryDischargeFeedsEV(-4000, 0, 500, 0) {
		t.Fatal("idle EV cannot be fed")
	}
}

func TestEffectivePVWActiveZeroIsTrueCap(t *testing.T) {
	if got := EffectivePVW(-5000, 0, false); got != -5000 {
		t.Errorf("inactive = %.0f, want -5000", got)
	}
	if got := EffectivePVW(-5000, 0, true); got != 0 {
		t.Errorf("active zero = %.0f, want 0", got)
	}
	if got := EffectivePVW(-5000, 2000, true); got != -2000 {
		t.Errorf("partial cap = %.0f, want -2000", got)
	}
}

func TestPlannedSurplusForEVWSkipsGridFundedCharge(t *testing.T) {
	// leftover 7500, battery soaking 2000 of it.
	if got := PlannedSurplusForEVW(500, -8000, 2000, 0); got != 5500 {
		t.Errorf("PV-soak: got %.0f, want 5500", got)
	}
	if got := PlannedSurplusForEVW(500, -8000, 10000, 2500); got != 7500 {
		t.Errorf("grid-funded: got %.0f, want 7500 (soak does not apply)", got)
	}
	// Soak + EV that together import: pass grid minus EV so soak is
	// still detected (meter import 640 is the leak, not battery buying).
	if got := PlannedSurplusForEVW(500, -8000, 4000, 640-6900); got != 3500 {
		t.Errorf("soak+EV: got %.0f, want 3500", got)
	}
}

type sitePhysicsFixture struct {
	Flows []struct {
		Name           string  `json:"name"`
		LoadW          float64 `json:"load_w"`
		PVW            float64 `json:"pv_w"`
		BatteryW       float64 `json:"battery_w"`
		EVW            float64 `json:"ev_w"`
		GridW          float64 `json:"grid_w"`
		LeftoverW      float64 `json:"leftover_w"`
		HouseResidualW float64 `json:"house_residual_w"`
		FeedsEV        bool    `json:"feeds_ev"`
	} `json:"flows"`
	EnergySteps []struct {
		Name         string  `json:"name"`
		PowerW       float64 `json:"power_w"`
		DtH          float64 `json:"dt_h"`
		ChargeEff    float64 `json:"charge_eff"`
		DischargeEff float64 `json:"discharge_eff"`
		DeltaWh      float64 `json:"delta_wh"`
	} `json:"energy_steps"`
}

func loadSitePhysicsFixture(t *testing.T) sitePhysicsFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/site_physics.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture sitePhysicsFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestSitePhysicsTable(t *testing.T) {
	fixture := loadSitePhysicsFixture(t)
	for _, row := range fixture.Flows {
		if got := GridW(row.LoadW, row.PVW, row.BatteryW, row.EVW); got != row.GridW {
			t.Errorf("%s: GridW = %.6g, want %.6g", row.Name, got, row.GridW)
		}
		if got := PVLeftoverAfterHouseW(row.LoadW, row.PVW); got != row.LeftoverW {
			t.Errorf("%s: leftover = %.6g, want %.6g", row.Name, got, row.LeftoverW)
		}
		if got := HouseResidualW(row.LoadW, row.PVW); got != row.HouseResidualW {
			t.Errorf("%s: residual = %.6g, want %.6g", row.Name, got, row.HouseResidualW)
		}
		if got := BatteryDischargeFeedsEV(row.BatteryW, row.EVW, row.LoadW, row.PVW); got != row.FeedsEV {
			t.Errorf("%s: feeds EV = %v, want %v", row.Name, got, row.FeedsEV)
		}
	}
	for _, row := range fixture.EnergySteps {
		got := BatteryEnergyDeltaWh(row.PowerW, row.DtH, row.ChargeEff, row.DischargeEff)
		if math.Abs(got-row.DeltaWh) > 1e-9 {
			t.Errorf("%s: delta = %.12g, want %.12g", row.Name, got, row.DeltaWh)
		}
	}
}
