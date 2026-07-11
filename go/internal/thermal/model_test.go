package thermal

import "testing"

func ptr(v float64) *float64 { return &v }

func TestSyntheticPriceUsesExportValueWhenPVSurplus(t *testing.T) {
	price := MarginalPrice{
		ImportOreKWh:               180,
		ExportOreKWh:               40,
		PVSurplusW:                 2500,
		ImportPressureOreKWh:       10,
		BatteryOpportunityOreKWh:   5,
		ThermalUrgencyCreditOreKWh: 20,
	}

	got := price.SyntheticOreKWh()
	want := 35.0
	if got != want {
		t.Fatalf("SyntheticOreKWh() = %.1f, want %.1f", got, want)
	}
}

func TestDecideIntentProtectsComfortBeforePrice(t *testing.T) {
	space := 19.4
	intent := DecideIntent(DecisionInput{
		Kind:       AssetSpaceHeat,
		SpaceTempC: &space,
		SpaceBand:  TemperatureBand{MinC: 20, NormalC: 21, MaxC: 22.5},
		Price: MarginalPrice{
			ImportOreKWh: 300,
		},
		ExpensiveAboveOreKWh: ptr(200),
	})

	if intent.Kind != IntentProtectComfort {
		t.Fatalf("intent = %q, want %q", intent.Kind, IntentProtectComfort)
	}
}

func TestDecideIntentBoostsDHWBeforeSpacePreheat(t *testing.T) {
	space := 21.0
	dhw := 48.0
	intent := DecideIntent(DecisionInput{
		Kind:          AssetSpaceHeat,
		SpaceTempC:    &space,
		SpaceBand:     TemperatureBand{MinC: 20, NormalC: 21, MaxC: 22.5},
		DHWTempC:      &dhw,
		DHWBand:       TemperatureBand{MinC: 45, NormalC: 50, MaxC: 58},
		AllowDHWBoost: true,
		Price: MarginalPrice{
			ImportOreKWh: 40,
		},
		CheapBelowOreKWh: ptr(60),
	})

	if intent.Kind != IntentBoostDHW {
		t.Fatalf("intent = %q, want %q", intent.Kind, IntentBoostDHW)
	}
}

func TestDecideIntentShedsWhenExpensiveAndComfortAllows(t *testing.T) {
	space := 21.5
	intent := DecideIntent(DecisionInput{
		Kind:       AssetSpaceHeat,
		SpaceTempC: &space,
		SpaceBand:  TemperatureBand{MinC: 20, NormalC: 21, MaxC: 22.5},
		Price: MarginalPrice{
			ImportOreKWh: 260,
		},
		ExpensiveAboveOreKWh: ptr(200),
	})

	if intent.Kind != IntentShed {
		t.Fatalf("intent = %q, want %q", intent.Kind, IntentShed)
	}
}

func TestDecideIntentPreconditionsOnPVSurplus(t *testing.T) {
	space := 21.0
	intent := DecideIntent(DecisionInput{
		Kind:            AssetSpaceHeat,
		SpaceTempC:      &space,
		SpaceBand:       TemperatureBand{MinC: 20, NormalC: 21, MaxC: 22.5},
		PVPreconditionW: 1000,
		Price: MarginalPrice{
			ImportOreKWh: 180,
			ExportOreKWh: 35,
			PVSurplusW:   1600,
		},
	})

	if intent.Kind != IntentPrecondition {
		t.Fatalf("intent = %q, want %q", intent.Kind, IntentPrecondition)
	}
}

// A CheapBelowOreKWh threshold of exactly 0 means "precondition only when
// energy is free or negative-priced". Because Swedish spot prices go negative
// regularly, 0 is a legitimate threshold and must not be treated as "unset".
func TestDecideIntentPreconditionsAtZeroThresholdOnNegativePrice(t *testing.T) {
	space := 21.0
	intent := DecideIntent(DecisionInput{
		Kind:       AssetSpaceHeat,
		SpaceTempC: &space,
		SpaceBand:  TemperatureBand{MinC: 20, NormalC: 21, MaxC: 22.5},
		Price: MarginalPrice{
			ImportOreKWh: -50,
		},
		CheapBelowOreKWh: ptr(0),
	})

	if intent.Kind != IntentPrecondition {
		t.Fatalf("intent = %q, want %q", intent.Kind, IntentPrecondition)
	}
}

func TestDecideIntentCoolingUsesInverseTemperatureHeadroom(t *testing.T) {
	space := 23.0
	intent := DecideIntent(DecisionInput{
		Kind:                 AssetCooling,
		SpaceTempC:           &space,
		SpaceBand:            TemperatureBand{MinC: 21, NormalC: 23, MaxC: 25},
		PVPreconditionW:      1000,
		ExpensiveAboveOreKWh: ptr(200),
		Price: MarginalPrice{
			ImportOreKWh: 260,
			ExportOreKWh: 30,
			PVSurplusW:   1200,
		},
	})

	if intent.Kind != IntentPrecondition {
		t.Fatalf("intent = %q, want %q", intent.Kind, IntentPrecondition)
	}
	if intent.PreconditionHeadroomC != 2 {
		t.Fatalf("precondition headroom = %.1f, want 2.0", intent.PreconditionHeadroomC)
	}
	if intent.ShedHeadroomC != 2 {
		t.Fatalf("shed headroom = %.1f, want 2.0", intent.ShedHeadroomC)
	}
}
