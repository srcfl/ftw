package control

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
)

// Cheap-hour leftover PV while the home battery is buying from the grid.
// 6 kW leftover is above the 3Φ minimum so the surplus clamp can start.
const (
	evComboLoadW = 500
	evComboPVW   = -8000 // leftover after house = 7500 W (holds 3Φ × 10 A)
	evComboBatW  = 10000
)

func evComboSlotStart() time.Time {
	return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
}

func evComboSiteStart() time.Time {
	return evComboSlotStart().Add(time.Second)
}

func TestEVSiteSurplusOnlyTakesLeftoverPVWhileBatteryGridCharges(t *testing.T) {
	// Injected plan: battery buys 10 kW. EV budget is 0 — the charger
	// must start from the opportunistic surplus clamp, the path Easee
	// sites without a vehicle SoC actually use. The old surplus reader
	// treated battery charge as already-claimed PV and offered the car
	// −grid+ev < 0 while Pixii imported, so the EV never moved.
	start := evComboSiteStart()
	site := newSiteClock(t, evSiteConfig{
		Start: start,
		Plan:  injectedChargePlan(evComboSlotStart(), 15, evComboBatW, 0, evComboLoadW, evComboPVW),
		LP:    surplusOnlyGarage(),
		LoadW: evComboLoadW,
		PVW:   evComboPVW,
	})
	site.run(12)
	got := site.requireCombo(4)
	if got.EVW > site.leftoverW()+loadpoint.SitePowerEpsW {
		t.Errorf("EV %.0f W exceeded leftover %.0f W", got.EVW, site.leftoverW())
	}
}

func TestEVSitePlannedSurplusEVChargesBesideBatteryGridCharge(t *testing.T) {
	start := evComboSiteStart()
	site := newSiteClock(t, evSiteConfig{
		Start: start,
		Plan:  injectedChargePlan(evComboSlotStart(), 15, evComboBatW, 4140, evComboLoadW, evComboPVW),
		LP:    surplusOnlyGarage(),
		LoadW: evComboLoadW,
		PVW:   evComboPVW,
	})
	site.run(12)
	site.requireCombo(4)
}

func TestEVSiteOptimizeThenDispatchChargesEVFromPVBesideBatteryImport(t *testing.T) {
	slot := evComboSlotStart()
	slots := []mpc.Slot{
		{
			StartMs: slot.UnixMilli(), LenMin: 60,
			PriceOre: 20, SpotOre: 10, LoadW: evComboLoadW, PVW: evComboPVW, Confidence: 1,
		},
		{
			StartMs: slot.Add(time.Hour).UnixMilli(), LenMin: 60,
			PriceOre: 300, SpotOre: 240, LoadW: 2500, PVW: 0, Confidence: 1,
		},
	}
	params := mpc.Params{
		Mode:                mpc.ModeArbitrage,
		SoCLevels:           11,
		CapacityWh:          20000,
		SoCMin:              0.10,
		SoCMax:              0.95,
		InitialSoC:          0.20,
		ActionLevels:        11,
		MaxChargeW:          10000,
		MaxDischargeW:       10000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    150,
		Loadpoint: &mpc.LoadpointSpec{
			ID:               evSiteLP,
			CapacityWh:       40000,
			Levels:           11,
			InitialSoC:       0.20,
			PluggedIn:        true,
			TargetSoC:        0.40,
			TargetSlotIdx:    1,
			MaxChargeW:       4140,
			AllowedStepsW:    []float64{0, 4140},
			ChargeEfficiency: 0.9,
			SurplusOnly:      true,
			NoBatteryToEV:    true,
		},
	}
	site := newSiteClock(t, evSiteConfig{
		Start:          evComboSiteStart(),
		OptimizeSlots:  slots,
		OptimizeParams: params,
		LP:             surplusOnlyGarage(),
		LoadW:          evComboLoadW,
		PVW:            evComboPVW,
		BatMaxCharge:   10000,
	})
	if site.plan().Actions[0].BatteryW < 500 {
		t.Fatalf("cheap slot should charge the home battery, got %+v", site.plan().Actions[0])
	}
	site.run(12)
	site.requireCombo(4)
}

func TestEVSiteIdleSurplusOnlyEVDoesNotBlockNightGridCharge(t *testing.T) {
	// #953: plugged idle surplus-only car, no PV, cheap night.
	start := time.Date(2026, 8, 18, 2, 0, 1, 0, time.UTC)
	slot := time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC)
	site := newSiteClock(t, evSiteConfig{
		Start: start,
		Plan:  injectedChargePlan(slot, 15, 5000, 0, 500, 0),
		LP:    surplusOnlyGarage(),
		LoadW: 500,
		PVW:   0,
	})
	site.run(8)
	site.requireIdleEV(2)
	var charged bool
	for _, rec := range site.ticks {
		if rec.N >= 2 && rec.BatW > 500 && rec.GridW > 100 {
			charged = true
			break
		}
	}
	if !charged {
		t.Fatalf("home battery should grid-charge at night beside an idle surplus-only EV; ticks=%s", site.dumpTicks())
	}
}

func TestEVSiteSurplusOnlyPausesWhenLeftoverCannotHold3Phase(t *testing.T) {
	// 1.3 kW leftover is below the 3Φ minimum. The clamp must pause
	// rather than import the gap. Battery may still buy from the grid.
	start := evComboSiteStart()
	const loadW, pvW = 500.0, -1800.0 // leftover 1300 W
	site := newSiteClock(t, evSiteConfig{
		Start: start,
		Plan:  injectedChargePlan(evComboSlotStart(), 15, 5000, 11000, loadW, pvW),
		LP:    surplusOnlyGarage(),
		LoadW: loadW,
		PVW:   pvW,
	})
	site.run(8)
	site.requireIdleEV(4)
}

func TestEVSiteScheduledEVMayImportOnCheapNight(t *testing.T) {
	start := time.Date(2026, 8, 18, 2, 0, 1, 0, time.UTC)
	slot := time.Date(2026, 8, 18, 2, 0, 0, 0, time.UTC)
	lp := surplusOnlyGarage()
	lp.SurplusOnly = false
	site := newSiteClock(t, evSiteConfig{
		Start: start,
		Plan:  injectedChargePlan(slot, 15, 4000, 4140, 500, 0),
		LP:    lp,
		LoadW: 500,
		PVW:   0,
	})
	site.run(8)
	var imported bool
	for _, rec := range site.ticks {
		if rec.N >= 2 && rec.EVW > 1000 && rec.GridW > 100 {
			imported = true
			break
		}
	}
	if !imported {
		t.Fatalf("scheduled (not surplus-only) EV should import on a cheap night; ticks=%s", site.dumpTicks())
	}
}

func TestEVSiteSurplusOnlyDoesNotClaimPVSoakWhenTogetherTheyWouldImport(t *testing.T) {
	// Battery plan 4 kW of 7.5 kW leftover — soak, not Pixii buying.
	// The old live reader treated any meter import as grid-charge and
	// offered the full leftover, so a 3Φ snap held while soak+EV imported.
	start := evComboSiteStart()
	site := newSiteClock(t, evSiteConfig{
		Start: start,
		Plan:  injectedChargePlan(evComboSlotStart(), 15, 4000, 0, evComboLoadW, evComboPVW),
		LP:    surplusOnlyGarage(),
		LoadW: evComboLoadW,
		PVW:   evComboPVW,
	})
	site.run(12)
	const soakHeadroom = 3500.0 // leftover 7500 − soak 4000
	for _, rec := range site.ticks {
		if rec.N < 4 {
			continue
		}
		if rec.EVW > soakHeadroom+loadpoint.SitePowerEpsW {
			t.Fatalf("tick %d: surplus-only EV %.0f W claimed PV-soak (headroom %.0f); ticks=%s",
				rec.N, rec.EVW, soakHeadroom, site.dumpTicks())
		}
	}
}

func TestEVSiteBatteryDoesNotDischargeIntoSurplusOnlyEV(t *testing.T) {
	start := evComboSiteStart()
	site := newSiteClock(t, evSiteConfig{
		Start:        start,
		Plan:         injectedChargePlan(evComboSlotStart(), 15, -4000, 4000, 500, 0),
		LP:           surplusOnlyGarage(),
		LoadW:        500,
		PVW:          0,
		BatEnergyWh:  16000,
		BatMaxCharge: 10000,
	})
	site.run(8)
	for _, rec := range site.ticks {
		if rec.EVW > loadpoint.SitePowerEpsW && rec.BatW < -loadpoint.SitePowerEpsW {
			t.Fatalf("tick %d: surplus-only EV %.0f W with battery discharge %.0f W", rec.N, rec.EVW, rec.BatW)
		}
		if rec.EVW > loadpoint.SitePowerEpsW {
			t.Fatalf("tick %d: surplus-only EV charged without PV: %.0f W", rec.N, rec.EVW)
		}
	}
}
