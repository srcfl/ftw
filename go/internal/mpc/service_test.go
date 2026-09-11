package mpc

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestBuildSlotsFallsBackToForecastWhenTwinCollapses(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 48.1
	forecastPV := 1488.5770353837524
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  15,
			SpotOreKwh:  120,
			TotalOreKwh: 180,
		}},
		[]state.ForecastPoint{{
			SlotTsMs:      ts,
			SlotLenMin:    60,
			CloudCoverPct: &cloud,
			PVWEstimated:  &forecastPV,
		}},
		2500,
		ts,
		func(time.Time, float64) float64 { return 0 },
		nil,
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+forecastPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -forecastPV)
	}
}

func TestBuildSlotsKeepsTwinWhenPredictionIsSane(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 48.1
	forecastPV := 1488.5770353837524
	twinPV := 1180.0
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  15,
			SpotOreKwh:  120,
			TotalOreKwh: 180,
		}},
		[]state.ForecastPoint{{
			SlotTsMs:      ts,
			SlotLenMin:    60,
			CloudCoverPct: &cloud,
			PVWEstimated:  &forecastPV,
		}},
		2500,
		ts,
		func(time.Time, float64) float64 { return twinPV },
		nil,
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+twinPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -twinPV)
	}
}

func TestBuildSlotsCarriesInputProvenance(t *testing.T) {
	start := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	firstPV := 1200.0
	secondPV := 2400.0
	prices := []state.PricePoint{
		{
			SlotTsMs: start, SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "entsoe", FetchedAtMs: 111,
		},
		{
			SlotTsMs: start + time.Hour.Milliseconds(), SlotLenMin: 15,
			SpotOreKwh: 70, TotalOreKwh: 130, Source: "forecast", FetchedAtMs: 222,
		},
	}
	forecasts := []state.ForecastPoint{
		{
			SlotTsMs: start, SlotLenMin: 60, PVWEstimated: &firstPV,
			Source: "met.no", FetchedAtMs: 333,
		},
		{
			SlotTsMs: start + time.Hour.Milliseconds(), SlotLenMin: 60,
			PVWEstimated: &secondPV, Source: "open-meteo", FetchedAtMs: 444,
		},
	}

	slots := buildSlots(prices, forecasts, 500, start, nil, nil, nil)
	if len(slots) != 2 {
		t.Fatalf("buildSlots returned %d slots, want 2", len(slots))
	}
	if got := slots[0]; got.InputProvenanceSchema != inputProvenanceSchemaVersion ||
		got.PriceInputSource != "entsoe" || got.PriceInputAvailableAtMs != 111 ||
		got.WeatherRowSource != "met.no" || got.WeatherRowAvailableAtMs != 333 ||
		got.Confidence != 1 {
		t.Fatalf("first slot provenance = %+v", got)
	}
	if got := slots[1]; got.InputProvenanceSchema != inputProvenanceSchemaVersion ||
		got.PriceInputSource != "forecast" || got.PriceInputAvailableAtMs != 222 ||
		got.WeatherRowSource != "open-meteo" || got.WeatherRowAvailableAtMs != 444 ||
		got.Confidence != 0.6 {
		t.Fatalf("second slot provenance = %+v", got)
	}

	withoutWeather := buildSlots(prices[:1], nil, 500, start, nil, nil, nil)
	if len(withoutWeather) != 1 {
		t.Fatalf("buildSlots without weather returned %d slots, want 1", len(withoutWeather))
	}
	if got := withoutWeather[0]; got.InputProvenanceSchema != inputProvenanceSchemaVersion ||
		got.PriceInputSource != "entsoe" || got.PriceInputAvailableAtMs != 111 ||
		got.WeatherRowSource != "" || got.WeatherRowAvailableAtMs != 0 {
		t.Fatalf("slot without weather provenance = %+v", got)
	}
}

func TestSynthesizedPriceCarriesCreationProvenance(t *testing.T) {
	now := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	prices := extendPricesWithForecast(nil, "SE3",
		func(string, time.Time) float64 { return 42 },
		now.UnixMilli(), now.Add(time.Hour).UnixMilli(), 0, 0)
	if len(prices) != 1 {
		t.Fatalf("extendPricesWithForecast returned %d rows, want 1", len(prices))
	}
	slots := buildSlots(prices, nil, 500, now.UnixMilli(), nil, nil, nil)
	if len(slots) != 1 {
		t.Fatalf("buildSlots returned %d slots, want 1", len(slots))
	}
	if got := slots[0]; got.InputProvenanceSchema != inputProvenanceSchemaVersion ||
		got.PriceInputSource != "forecast" ||
		got.PriceInputAvailableAtMs != now.UnixMilli() || got.Confidence != 0.6 {
		t.Fatalf("synthesized price provenance = %+v", got)
	}
}

func TestForecastPricePersistsLastKnownInsteadOfClimatologyCliff(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	last := state.PricePoint{
		Zone: "SE3", SlotTsMs: now.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 200, TotalOreKwh: 280, Source: "entsoe",
	}
	prices := extendPricesWithForecast(
		[]state.PricePoint{last},
		"SE3",
		func(string, time.Time) float64 { return 70 },
		now.UnixMilli(),
		now.Add(2*time.Hour).UnixMilli(),
		0, 0,
	)
	if len(prices) < 2 {
		t.Fatalf("got %d prices, want published + forecast", len(prices))
	}
	var forecast []state.PricePoint
	for _, p := range prices {
		if p.Source == "forecast" {
			forecast = append(forecast, p)
		}
	}
	if len(forecast) == 0 {
		t.Fatal("no forecast rows")
	}
	first := forecast[0]
	if first.SpotOreKwh < 150 {
		t.Errorf("first unpublished hour jumped to climatology: got %.1f, want near last-known 200 (not 70)", first.SpotOreKwh)
	}
	if first.SpotOreKwh > 201 {
		t.Errorf("first unpublished hour overshot last-known: got %.1f", first.SpotOreKwh)
	}
}

func TestForecastPriceFadesTowardClimatologyOverHours(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	last := state.PricePoint{
		Zone: "SE3", SlotTsMs: now.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 200, TotalOreKwh: 280, Source: "entsoe",
	}
	prices := extendPricesWithForecast(
		[]state.PricePoint{last},
		"SE3",
		func(string, time.Time) float64 { return 70 },
		now.UnixMilli(),
		now.Add(13*time.Hour).UnixMilli(),
		0, 0,
	)
	var forecast []state.PricePoint
	for _, p := range prices {
		if p.Source == "forecast" {
			forecast = append(forecast, p)
		}
	}
	if len(forecast) < 12 {
		t.Fatalf("got %d forecast rows, want >= 12", len(forecast))
	}
	late := forecast[len(forecast)-1]
	if late.SpotOreKwh > 120 {
		t.Errorf("12 h out should have faded toward climatology 70, got %.1f", late.SpotOreKwh)
	}
	if late.SpotOreKwh >= forecast[0].SpotOreKwh {
		t.Errorf("later forecast %.1f should be below first-hour persist %.1f", late.SpotOreKwh, forecast[0].SpotOreKwh)
	}
}

func TestBuildSlotsWeatherProvenanceFollowsTwinCloudInput(t *testing.T) {
	weatherStart := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	priceStart := weatherStart.Add(45 * time.Minute)
	cloud := 25.0
	prices := []state.PricePoint{{
		SlotTsMs: priceStart.UnixMilli(), SlotLenMin: 15,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "entsoe", FetchedAtMs: 111,
	}}
	forecasts := []state.ForecastPoint{{
		SlotTsMs: weatherStart.UnixMilli(), SlotLenMin: 60,
		CloudCoverPct: &cloud, Source: "met.no", FetchedAtMs: 333,
	}}

	slots := buildSlots(prices, forecasts, 500, priceStart.UnixMilli(),
		func(_ time.Time, cloudPct float64) float64 { return cloudPct * 10 }, nil, nil)
	if len(slots) != 1 {
		t.Fatalf("buildSlots returned %d slots, want 1", len(slots))
	}
	if got := slots[0]; got.InputProvenanceSchema != inputProvenanceSchemaVersion ||
		got.WeatherRowSource != "met.no" || got.WeatherRowAvailableAtMs != 333 {
		t.Fatalf("twin weather provenance = %+v", got)
	}
}

func TestBuildSlotsDoesNotUseFutureWeatherBeforeCoverage(t *testing.T) {
	firstTs := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	laterCloud := 91.0
	priceTs := firstTs - int64(15*time.Minute/time.Millisecond)
	prices := []state.PricePoint{{
		SlotTsMs: priceTs, SlotLenMin: 15,
		SpotOreKwh: 50, TotalOreKwh: 100,
	}}
	forecasts := []state.ForecastPoint{
		{
			SlotTsMs: firstTs, SlotLenMin: 60,
			Source: "nearest", FetchedAtMs: 111,
		},
		{
			SlotTsMs: firstTs + int64(time.Hour/time.Millisecond), SlotLenMin: 60,
			CloudCoverPct: &laterCloud, Source: "later", FetchedAtMs: 222,
		},
	}

	slots := buildSlots(prices, forecasts, 500, priceTs,
		func(_ time.Time, cloudPct float64) float64 { return cloudPct * 10 }, nil, nil)
	if len(slots) != 1 {
		t.Fatalf("buildSlots returned %d slots, want 1", len(slots))
	}
	if got := slots[0]; got.PVW != 0 || got.WeatherRowSource != "" || got.WeatherRowAvailableAtMs != 0 {
		t.Fatalf("future weather manufactured PV or provenance = %+v", got)
	}
}

func TestBuildSlotsDoesNotCreatePVAcrossWeatherGap(t *testing.T) {
	start := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	cloud := 10.0
	pvW := 3000.0
	forecasts := []state.ForecastPoint{
		{SlotTsMs: start.UnixMilli(), SlotLenMin: 60, CloudCoverPct: &cloud, PVWEstimated: &pvW, Source: "before"},
		{SlotTsMs: start.Add(2 * time.Hour).UnixMilli(), SlotLenMin: 60, CloudCoverPct: &cloud, PVWEstimated: &pvW, Source: "after"},
	}
	target := start.Add(time.Hour).UnixMilli()
	slots := buildSlots(
		[]state.PricePoint{{SlotTsMs: target, SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100}},
		forecasts, 500, target,
		func(time.Time, float64) float64 { return 5000 }, nil, nil,
	)
	if len(slots) != 1 {
		t.Fatalf("buildSlots returned %d slots, want 1", len(slots))
	}
	if got := slots[0]; got.PVW != 0 || got.WeatherRowSource != "" || got.WeatherRowAvailableAtMs != 0 {
		t.Fatalf("weather gap manufactured learned PV or provenance = %+v", got)
	}
}

func TestSnapshotPredictionsUsesFrozenPlanPredictors(t *testing.T) {
	start := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	cloud := 25.0
	weather := []state.ForecastPoint{{SlotTsMs: start.UnixMilli(), SlotLenMin: 60, CloudCoverPct: &cloud}}
	service := &Service{
		PV:   func(time.Time, float64) float64 { return 9000 },
		Load: func(time.Time) float64 { return 8000 },
	}
	points := service.snapshotPredictions(
		[]Slot{{StartMs: start.UnixMilli(), LenMin: 60}}, weather,
		func(time.Time, float64) float64 { return 1000 },
		func(time.Time) float64 { return 700 },
	)
	if points == nil || len(points.pv) != 1 || points.pv[0] != 1000 || len(points.load) != 1 || points.load[0] != 700 {
		t.Fatalf("drift baseline re-read live predictors: %+v", points)
	}
	loadOnly := service.snapshotPredictions(
		[]Slot{{StartMs: start.UnixMilli(), LenMin: 60}}, nil,
		nil, func(time.Time) float64 { return 700 },
	)
	if loadOnly == nil || loadOnly.pv != nil || len(loadOnly.load) != 1 {
		t.Fatalf("nil frozen PV fell through to live service predictor: %+v", loadOnly)
	}
	withoutWeather := service.snapshotPredictions(
		[]Slot{{StartMs: start.UnixMilli(), LenMin: 60}}, nil,
		func(time.Time, float64) float64 { return 1000 }, nil,
	)
	if withoutWeather == nil || len(withoutWeather.pv) != 1 || len(withoutWeather.pvCovered) != 1 || withoutWeather.pvCovered[0] {
		t.Fatalf("missing weather created a PV drift baseline: %+v", withoutWeather)
	}
}

func TestLookupCloudBeforeFirstForecastDoesNotUseLaterRow(t *testing.T) {
	firstTs := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	lastCloud := 91.0
	forecasts := []state.ForecastPoint{
		{SlotTsMs: firstTs, SlotLenMin: 60},
		{SlotTsMs: firstTs + int64(time.Hour/time.Millisecond), SlotLenMin: 60, CloudCoverPct: &lastCloud},
	}

	got := lookupCloud(forecasts, firstTs-int64(15*time.Minute/time.Millisecond))
	if got != 50 {
		t.Fatalf("lookupCloud before first row = %.1f, want neutral prior 50", got)
	}
}

func TestLookupPVBeforeFirstForecastReturnsZero(t *testing.T) {
	firstTs := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	firstPV := 1200.0
	lastPV := 2400.0
	forecasts := []state.ForecastPoint{
		{SlotTsMs: firstTs, SlotLenMin: 60, PVWEstimated: &firstPV},
		{SlotTsMs: firstTs + int64(time.Hour/time.Millisecond), SlotLenMin: 60, PVWEstimated: &lastPV},
	}

	got := lookupPV(forecasts, firstTs-int64(15*time.Minute/time.Millisecond))
	if got != 0 {
		t.Fatalf("lookupPV before first row = %.1f, want 0", got)
	}
}

// applyPVDownside is the Alt-2 safety mechanism: plan against forecast PV minus
// k·σ (recent PV-forecast error std) so the DP doesn't run the battery down
// betting on PV that may not arrive. The reserve emerges from the forecast
// uncertainty itself — no separate SoC/energy floor.
func TestApplyPVDownsideHaircutsGenerationByKSigma(t *testing.T) {
	slots := []Slot{
		{PVW: -3000}, // 3 kW generation
		{PVW: 0},     // night — no generation
		{PVW: -200},  // small PV, less than the haircut
	}
	applyPVDownside(slots, 1.0, 500) // k=1, σ=500 W → haircut 500 W

	if slots[0].PVW != -2500 {
		t.Errorf("PVW[0] = %v, want -2500 (3000 generation − 500 haircut)", slots[0].PVW)
	}
	if slots[1].PVW != 0 {
		t.Errorf("night PVW must stay 0, got %v", slots[1].PVW)
	}
	if slots[2].PVW != 0 {
		t.Errorf("PVW[2] = %v, want 0 (haircut exceeds the 200 W generation, floored)", slots[2].PVW)
	}
}

func TestApplyPVDownsideNoOpWhenDisabled(t *testing.T) {
	slots := []Slot{{PVW: -3000}}
	applyPVDownside(slots, 0, 500) // k=0 → raw forecast, no hedge
	if slots[0].PVW != -3000 {
		t.Errorf("k=0 must be a no-op, got %v", slots[0].PVW)
	}
	applyPVDownside(slots, 1.0, 0) // σ=0 (no error history) → no hedge
	if slots[0].PVW != -3000 {
		t.Errorf("σ=0 must be a no-op, got %v", slots[0].PVW)
	}
}

func TestApplyPVDownsideNegativeKIsNoOp(t *testing.T) {
	slots := []Slot{{PVW: -3000}}
	applyPVDownside(slots, -1.0, 500) // negative k must be guarded, not amplify PV
	if slots[0].PVW != -3000 {
		t.Errorf("negative k must be a no-op, got %v", slots[0].PVW)
	}
}

func TestApplyPVDownsideScalesWithK(t *testing.T) {
	slots := []Slot{{PVW: -3000}}
	applyPVDownside(slots, 2.0, 500) // k=2, σ=500 → haircut 1000 W
	if slots[0].PVW != -2000 {
		t.Errorf("PVW = %v, want -2000 (3000 − 2·500)", slots[0].PVW)
	}
}

// The Service seam reads σ from the PVUncertaintyW hook and the configured k.
func TestServiceApplyPVDownsideToSlotsUsesHookAndK(t *testing.T) {
	s := &Service{
		PVForecastSafetyK: 1.0,
		PVUncertaintyW:    func() float64 { return 800 }, // live σ = 800 W
	}
	slots := []Slot{{PVW: -3000}, {PVW: 0}}
	s.applyPVDownsideToSlots(slots)
	if slots[0].PVW != -2200 {
		t.Errorf("PVW[0] = %v, want -2200 (3000 − 1·800 from the hook)", slots[0].PVW)
	}
	if slots[1].PVW != 0 {
		t.Errorf("night slot must stay 0, got %v", slots[1].PVW)
	}
}

func TestServiceApplyPVDownsideToSlotsNoOpWithoutHook(t *testing.T) {
	s := &Service{PVForecastSafetyK: 1.0} // PVUncertaintyW unwired
	slots := []Slot{{PVW: -3000}}
	s.applyPVDownsideToSlots(slots)
	if slots[0].PVW != -3000 {
		t.Errorf("no σ hook → raw forecast, got %v", slots[0].PVW)
	}
}

func TestServiceApplyPVDownsideToSlotsNilServiceNoPanic(t *testing.T) {
	var s *Service
	slots := []Slot{{PVW: -3000}}
	s.applyPVDownsideToSlots(slots) // must not panic
	if slots[0].PVW != -3000 {
		t.Errorf("nil Service must be a no-op, got %v", slots[0].PVW)
	}
}

// applyPVDownsidePerSlot sizes the hedge against each slot's own generation.
// The flat form subtracted the same watt figure everywhere, which erased the
// morning and evening shoulders outright and hedged a possibly-clear tomorrow
// with today's cloudy-sky σ.
func TestApplyPVDownsidePerSlotShavesAShareOfEachSlot(t *testing.T) {
	// A day curve: night, shoulders, midday peak, shoulders, night.
	gen := []float64{0, 500, 3000, 6000, 3000, 500, 0}
	slots := make([]Slot, len(gen))
	for i, g := range gen {
		slots[i].PVW = -g
	}

	applyPVDownsidePerSlot(slots, 1.0, 0.3, 0) // σ_rel = 30 %, k = 1

	for i, g := range gen {
		want := -(g * 0.7)
		if math.Abs(slots[i].PVW-want) > 1e-9 {
			t.Errorf("slot %d: PVW = %v, want %v (30 %% off %v W of generation)",
				i, slots[i].PVW, want, g)
		}
		if slots[i].PVW > 0 {
			t.Errorf("slot %d: PVW = %v — the haircut must never add generation", i, slots[i].PVW)
		}
	}
	if slots[0].PVW != 0 || slots[6].PVW != 0 {
		t.Errorf("night slots must stay 0, got %v and %v", slots[0].PVW, slots[6].PVW)
	}
}

// k·σ_rel above 1 is arithmetically possible (k=2, σ_rel=0.6). Generation is
// floored at zero rather than turning into a phantom load.
func TestApplyPVDownsidePerSlotFloorsAtZero(t *testing.T) {
	slots := []Slot{{PVW: -4000}, {PVW: -100}}
	applyPVDownsidePerSlot(slots, 2.0, 0.6, 0) // k·σ_rel = 1.2
	for i, s := range slots {
		if s.PVW != 0 {
			t.Errorf("slot %d: PVW = %v, want 0", i, s.PVW)
		}
	}
}

// Until the twin has learned its relative error the site must keep exactly the
// hedge it had before — bit for bit, not merely "about the same".
func TestApplyPVDownsidePerSlotFallsBackToFlatWhenUnlearned(t *testing.T) {
	base := []Slot{{PVW: -6000}, {PVW: -3000}, {PVW: -400}, {PVW: 0}}
	for _, k := range []float64{0, 1, 2} {
		flat := append([]Slot(nil), base...)
		perSlot := append([]Slot(nil), base...)
		applyPVDownside(flat, k, 1891)
		applyPVDownsidePerSlot(perSlot, k, 0, 1891)
		for i := range flat {
			if flat[i].PVW != perSlot[i].PVW {
				t.Errorf("k=%v slot %d: per-slot with σ_rel=0 gave %v, flat gave %v",
					k, i, perSlot[i].PVW, flat[i].PVW)
			}
		}
	}
}

func TestApplyPVDownsidePerSlotNoOpWhenDisabled(t *testing.T) {
	slots := []Slot{{PVW: -3000}}
	applyPVDownsidePerSlot(slots, 0, 0.3, 0) // k=0 → raw forecast
	if slots[0].PVW != -3000 {
		t.Errorf("k=0 must be a no-op, got %v", slots[0].PVW)
	}
	applyPVDownsidePerSlot(slots, -1, 0.3, 0) // negative k must not amplify PV
	if slots[0].PVW != -3000 {
		t.Errorf("negative k must be a no-op, got %v", slots[0].PVW)
	}
}

// The Service seam prefers the relative hook and keeps the absolute one as the
// fallback, so one replan cannot mix the two forms.
func TestServiceApplyPVDownsideToSlotsPrefersRelative(t *testing.T) {
	s := &Service{
		PVForecastSafetyK:     1.0,
		PVUncertaintyW:        func() float64 { return 1891 },
		PVRelativeUncertainty: func() float64 { return 0.25 },
	}
	slots := []Slot{{PVW: -6000}, {PVW: -400}, {PVW: 0}}
	s.applyPVDownsideToSlots(slots)
	if slots[0].PVW != -4500 {
		t.Errorf("PVW[0] = %v, want -4500 (6000 − 25 %%)", slots[0].PVW)
	}
	if slots[1].PVW != -300 {
		t.Errorf("PVW[1] = %v, want -300 — a flat 1891 W cut would have zeroed this shoulder",
			slots[1].PVW)
	}
	if slots[2].PVW != 0 {
		t.Errorf("night slot must stay 0, got %v", slots[2].PVW)
	}
}

// TestBuildSlots_AppliesPVResidualCorrection: a non-nil
// PVResidualCorrector adds an additive bias to the twin's per-slot
// prediction BEFORE selectPlannerPVW blends with the forecast. We mock
// the corrector to apply -200 W to the first slot only and verify the
// final slot 0 PVW reflects the correction while slot 1 does not.
func TestBuildSlots_AppliesPVResidualCorrection(t *testing.T) {
	ts0 := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	ts1 := ts0 + 15*60*1000
	cloud := 30.0
	forecastPV := 0.0 // disable forecast blending so we see the pure twin path
	twinPV := 1500.0
	// Correction of -200 W means PV generation is being under-predicted
	// by 200 W on the rolling residual; final base should be 1300 W on
	// slot 0 only.
	correctedSlot := time.UnixMilli(ts0 + 15*30*1000).UTC() // slot 0 midpoint
	pvCorrect := func(now, tTarget time.Time, base float64) float64 {
		if tTarget.Equal(correctedSlot) {
			return -200
		}
		return 0
	}
	slots := buildSlots(
		[]state.PricePoint{
			{SlotTsMs: ts0, SlotLenMin: 15, SpotOreKwh: 120, TotalOreKwh: 180},
			{SlotTsMs: ts1, SlotLenMin: 15, SpotOreKwh: 120, TotalOreKwh: 180},
		},
		[]state.ForecastPoint{
			{SlotTsMs: ts0, SlotLenMin: 60, CloudCoverPct: &cloud, PVWEstimated: &forecastPV},
		},
		2500,
		ts0,
		func(time.Time, float64) float64 { return twinPV },
		pvCorrect,
		nil,
	)
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2", len(slots))
	}
	// Slot 0: base 1500 + (-200) = 1300 → PVW = -1300 (site-sign).
	if got, want := slots[0].PVW, -1300.0; math.Abs(got-want) > 1e-6 {
		t.Fatalf("slot 0 PVW = %f, want %f (correction applied)", got, want)
	}
	// Slot 1: no correction → PVW = -1500.
	if got, want := slots[1].PVW, -1500.0; math.Abs(got-want) > 1e-6 {
		t.Fatalf("slot 1 PVW = %f, want %f (no correction)", got, want)
	}
}

// TestBuildSlots_NoDoubleCorrection: regression for the PR #381
// follow-up. The MPC must consume the UNANCHORED structural PV
// predictor plus the residual corrector — wiring the anchored
// predictor (which already folds in the same structural-vs-live bias)
// produces a double-correction so the planner sees ~0 W PV on a sunny
// day with a heavy downward residual.
//
// Worked example (Codex's reproduction):
//
//	structural prediction  = 1000 W
//	live PV measurement    =  500 W  (heavy downward bias of −500 W)
//	→ anchored Predict     ≈  500 W  (the now-anchor already pulled it)
//	→ ResidualCorrect      = −500 W  (rolling mean of the same bias)
//
// Wiring `PV = pvSvc.Predict` (anchored) + `PVResidualCorrect` gives the
// planner ≈ 0 W. Wiring `PV = pvSvc.PredictStructural` + `PVResidualCorrect`
// (the fix) gives the planner ≈ 500 W — the bias is corrected exactly
// once, by the residual layer that is designed for it.
//
// We simulate both wirings here and assert the structural one matches
// the single-correction outcome.
func TestBuildSlots_NoDoubleCorrection(t *testing.T) {
	ts0 := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 30.0
	forecastPV := 0.0 // disable forecast blending to expose the pure twin path

	const structuralW = 1000.0
	const anchoredW = 500.0 // what Predict would return after the now-anchor
	const residualW = -500.0

	// Slot 0 midpoint — buildSlots passes this to both PV and PVResidualCorrect.
	correctedSlot := time.UnixMilli(ts0 + 15*30*1000).UTC()
	pvCorrect := func(now, tTarget time.Time, base float64) float64 {
		if tTarget.Equal(correctedSlot) {
			return residualW
		}
		return 0
	}

	// --- Buggy wiring (anchored predictor + residual): double correction ---
	slotsBuggy := buildSlots(
		[]state.PricePoint{{SlotTsMs: ts0, SlotLenMin: 15, SpotOreKwh: 120, TotalOreKwh: 180}},
		[]state.ForecastPoint{{SlotTsMs: ts0, SlotLenMin: 60, CloudCoverPct: &cloud, PVWEstimated: &forecastPV}},
		2500,
		ts0,
		func(time.Time, float64) float64 { return anchoredW },
		pvCorrect,
		nil,
	)
	// Anchored 500 + residual -500 = 0 → site-sign PVW = 0.
	if got, want := slotsBuggy[0].PVW, 0.0; math.Abs(got-want) > 1e-6 {
		t.Fatalf("buggy-wiring slot 0 PVW = %f, want %f (anchored + residual double-corrects)", got, want)
	}

	// --- Correct wiring (structural predictor + residual): single correction ---
	slotsFixed := buildSlots(
		[]state.PricePoint{{SlotTsMs: ts0, SlotLenMin: 15, SpotOreKwh: 120, TotalOreKwh: 180}},
		[]state.ForecastPoint{{SlotTsMs: ts0, SlotLenMin: 60, CloudCoverPct: &cloud, PVWEstimated: &forecastPV}},
		2500,
		ts0,
		func(time.Time, float64) float64 { return structuralW },
		pvCorrect,
		nil,
	)
	// Structural 1000 + residual -500 = 500 → site-sign PVW = -500.
	if got, want := slotsFixed[0].PVW, -500.0; math.Abs(got-want) > 1e-6 {
		t.Fatalf("fixed-wiring slot 0 PVW = %f, want %f (single correction reflects live bias)", got, want)
	}
}

// ---- upperHalfMeanPrice (arbitrage terminal valuation) ----

func TestUpperHalfMeanLiftsTerminalCreditAboveOverallMean(t *testing.T) {
	// Live-shaped horizon: midday cheap valley + evening peak. Mean is
	// pulled down by the cheap hours; upper-half mean reflects the
	// hours when stored SoC would actually be sold.
	prices := []state.PricePoint{
		{TotalOreKwh: 170}, // cheap midday
		{TotalOreKwh: 175},
		{TotalOreKwh: 180},
		{TotalOreKwh: 200},
		{TotalOreKwh: 250},
		{TotalOreKwh: 300},
		{TotalOreKwh: 320}, // evening peak
		{TotalOreKwh: 345},
	}
	overall := 0.0
	for _, p := range prices {
		overall += p.TotalOreKwh
	}
	overall /= float64(len(prices))
	got := upperHalfMeanPrice(prices)
	// Upper half = {250, 300, 320, 345} mean = 303.75.
	if math.Abs(got-303.75) > 0.01 {
		t.Errorf("upperHalfMeanPrice = %.2f, want 303.75", got)
	}
	if got <= overall {
		t.Errorf("upper-half mean (%.2f) must exceed overall mean (%.2f) on a non-flat horizon", got, overall)
	}
}

func TestUpperHalfMeanFallsBackForTinyHorizon(t *testing.T) {
	// With 4 or fewer slots, taking the "upper half" loses meaning —
	// fall back to plain mean.
	prices := []state.PricePoint{
		{TotalOreKwh: 100},
		{TotalOreKwh: 300},
	}
	got := upperHalfMeanPrice(prices)
	if math.Abs(got-200) > 0.01 {
		t.Errorf("upperHalfMeanPrice (tiny horizon) = %.2f, want 200 (plain mean)", got)
	}
}

func TestUpperHalfMeanEmptyReturnsZero(t *testing.T) {
	if got := upperHalfMeanPrice(nil); got != 0 {
		t.Errorf("upperHalfMeanPrice(nil) = %f, want 0", got)
	}
}

// ---- Terminal SoC valuation ----

func TestSelfConsumptionTerminalPriceIsMeanImport(t *testing.T) {
	// Retail 300 öre/kWh average across the horizon. Spot/bonus/fee are
	// irrelevant in self-consumption mode (operator never sells stored
	// energy, so the export side doesn't enter the value of a kept kWh).
	prices := []state.PricePoint{
		{SpotOreKwh: 80, TotalOreKwh: 300},
		{SpotOreKwh: 80, TotalOreKwh: 300},
	}
	got := selfConsumptionTerminalPrice(prices, 60, 6)
	if math.Abs(got-300) > 1e-9 {
		t.Fatalf("terminal price = %f, want 300 (mean import)", got)
	}
}

func TestSelfConsumptionTerminalPriceIgnoresExportRate(t *testing.T) {
	// Even when export rate (spot+bonus−fee) exceeds retail, the
	// terminal value of stored SoC is still mean import — self-consumption
	// mode never sells stored energy, so the export side is moot.
	prices := []state.PricePoint{{SpotOreKwh: 500, TotalOreKwh: 100}}
	got := selfConsumptionTerminalPrice(prices, 0, 0)
	if math.Abs(got-100) > 1e-9 {
		t.Fatalf("terminal price = %f, want 100 (mean import) regardless of export rate", got)
	}
}

func TestSelfConsumptionTerminalPriceEmpty(t *testing.T) {
	got := selfConsumptionTerminalPrice(nil, 0, 0)
	if got != 0 {
		t.Fatalf("terminal price = %f, want 0", got)
	}
}

// End-to-end proof: with the new self-consumption terminal valuation, a
// battery that's ≥50% full WILL discharge to cover load instead of
// choosing "idle — import to cover load". Regression test for the exact
// bug we saw on homelab-rpi (bat_w=0 on every slot with SoC=84%).
func TestOptimizeSelfConsumptionDischargesWithSpreadTerminalPrice(t *testing.T) {
	// 4-slot horizon, PV < load in every slot so battery has work to do.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 7200 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 10800 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
	}

	// Build PricePoints identical to the slots and compute the
	// mode-appropriate terminal price. Mirrors what service.replan does.
	prices := []state.PricePoint{
		{SpotOreKwh: 80, TotalOreKwh: 300}, {SpotOreKwh: 80, TotalOreKwh: 300},
		{SpotOreKwh: 80, TotalOreKwh: 300}, {SpotOreKwh: 80, TotalOreKwh: 300},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoC = 0.8
	p.ExportBonusOreKwh = 60
	p.ExportFeeOreKwh = 6
	p.TerminalSoCPrice = selfConsumptionTerminalPrice(prices, 60, 6)

	plan := Optimize(slots, p)
	var discharging int
	for _, a := range plan.Actions {
		if a.BatteryW < -1e-6 {
			discharging++
		}
		if a.BatteryW > 1e-6 {
			t.Errorf("slot at %d charging %.0fW with no PV surplus", a.SlotStartMs, a.BatteryW)
		}
	}
	if discharging == 0 {
		t.Fatalf("expected at least one discharging slot with SoC=80%% and load>PV, got %+v", plan.Actions)
	}
}

// ---- online battery fleet snapshot ----

func TestOnlineFleetParamsUsesCapacityWeightedOnlineSoC(t *testing.T) {
	tel := telemetry.NewStore()
	socA := 0.20
	socB := 0.80
	socOffline := 0.95
	tel.Update("a", telemetry.DerBattery, 0, &socA, nil)
	tel.DriverHealthMut("a").RecordSuccess()
	tel.Update("b", telemetry.DerBattery, 0, &socB, nil)
	tel.DriverHealthMut("b").RecordSuccess()
	tel.Update("offline", telemetry.DerBattery, 0, &socOffline, nil)
	tel.DriverHealthMut("offline").SetOffline()

	s := &Service{Tele: tel, FuseMaxW: 6000}
	p, ok := s.onlineFleetParams(Params{InitialSoC: 0.5}, []BatteryFleetMember{
		{Driver: "a", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 4000},
		{Driver: "b", CapacityWh: 30000, MaxChargeW: 5000, MaxDischargeW: 5000},
		{Driver: "offline", CapacityWh: 50000, MaxChargeW: 9000, MaxDischargeW: 9000},
	})
	if !ok {
		t.Fatal("onlineFleetParams returned ok=false")
	}
	if p.CapacityWh != 40000 {
		t.Fatalf("CapacityWh = %.0f, want 40000", p.CapacityWh)
	}
	// (10 kWh * 20% + 30 kWh * 80%) / 40 kWh = 65%.
	if math.Abs(p.InitialSoC-0.65) > 1e-9 {
		t.Fatalf("InitialSoC = %.3f, want 0.650", p.InitialSoC)
	}
	if p.MaxChargeW != 6000 {
		t.Fatalf("MaxChargeW = %.0f, want fuse-clamped 6000", p.MaxChargeW)
	}
	if p.MaxDischargeW != 6000 {
		t.Fatalf("MaxDischargeW = %.0f, want fuse-clamped 6000", p.MaxDischargeW)
	}
	if len(p.Storages) != 2 {
		t.Fatalf("len(Storages) = %d, want 2", len(p.Storages))
	}
	if p.Storages[0].ID != "a" || p.Storages[0].InitialEnergyWh != 2000 {
		t.Fatalf("Storages[0] = %+v, want battery a at 2000 Wh", p.Storages[0])
	}
	if p.Storages[1].ID != "b" || p.Storages[1].InitialEnergyWh != 24000 {
		t.Fatalf("Storages[1] = %+v, want battery b at 24000 Wh", p.Storages[1])
	}
	if p.Storages[0].MaxChargeW != 2250 || p.Storages[1].MaxChargeW != 3750 {
		t.Fatalf("storage charge limits = %.0f + %.0f, want fuse-scaled 2250 + 3750",
			p.Storages[0].MaxChargeW, p.Storages[1].MaxChargeW)
	}
	if math.Abs(p.Storages[0].MaxDischargeW-8000.0/3.0) > 1e-9 || math.Abs(p.Storages[1].MaxDischargeW-10000.0/3.0) > 1e-9 {
		t.Fatalf("storage discharge limits = %.3f + %.3f, want proportional 6000 W total",
			p.Storages[0].MaxDischargeW, p.Storages[1].MaxDischargeW)
	}
}

// A battery that answers polls but rejects every command is not a battery
// the plan can spend. Counting its capacity and its charge/discharge limits
// makes the optimizer promise energy that never arrives.
func TestOnlineFleetParamsDropsCommandFaultedBattery(t *testing.T) {
	tel := telemetry.NewStore()
	socA := 0.20
	socRefusing := 0.95
	tel.Update("a", telemetry.DerBattery, 0, &socA, nil)
	tel.DriverHealthMut("a").RecordSuccess()
	tel.Update("refusing", telemetry.DerBattery, 0, &socRefusing, nil)
	tel.DriverHealthMut("refusing").RecordSuccess()
	tel.SetDriverCommandFault("refusing", true, "modbus write refused")

	s := &Service{Tele: tel, FuseMaxW: 20000}
	p, ok := s.onlineFleetParams(Params{InitialSoC: 0.5}, []BatteryFleetMember{
		{Driver: "a", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 4000},
		{Driver: "refusing", CapacityWh: 50000, MaxChargeW: 9000, MaxDischargeW: 9000},
	})
	if !ok {
		t.Fatal("onlineFleetParams returned ok=false")
	}
	if p.CapacityWh != 10000 {
		t.Fatalf("CapacityWh = %.0f, want 10000 — the refusing battery must not be counted", p.CapacityWh)
	}
	if len(p.Storages) != 1 || p.Storages[0].ID != "a" {
		t.Fatalf("Storages = %+v, want only battery a", p.Storages)
	}
	if math.Abs(p.InitialSoC-0.20) > 1e-9 {
		t.Fatalf("InitialSoC = %.3f, want 0.200 — the refusing battery's 95%% must not count", p.InitialSoC)
	}
}

func TestOnlineFleetParamsRequiresOnlineSoCTelemetry(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("no-soc", telemetry.DerBattery, 0, nil, nil)
	tel.DriverHealthMut("no-soc").RecordSuccess()
	s := &Service{Tele: tel}

	_, ok := s.onlineFleetParams(Params{InitialSoC: 0.5}, []BatteryFleetMember{
		{Driver: "no-soc", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 3000},
		{Driver: "missing", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 3000},
	})
	if ok {
		t.Fatal("onlineFleetParams ok=true without any online battery SoC")
	}
}

// ---- Edge cases / hardening ----

func TestBuildSlotsEmptyForecast(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  60,
			SpotOreKwh:  100,
			TotalOreKwh: 200,
		}},
		nil, // empty forecasts
		1500,
		ts,
		nil,
		nil,
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(slots))
	}
	// With no forecast, PVW should be 0 (no panic).
	if slots[0].PVW != 0 {
		t.Errorf("expected PVW=0 with empty forecast, got %f", slots[0].PVW)
	}
	if slots[0].LoadW != 1500 {
		t.Errorf("expected LoadW=1500, got %f", slots[0].LoadW)
	}
}

func TestSelectPlannerPVWBothNaN(t *testing.T) {
	got := selectPlannerPVW(math.NaN(), math.NaN(), false)
	if got != 0 {
		t.Errorf("both NaN should return 0, got %f", got)
	}
}

// Radiation-backed forecast: predicted twin gets a minority vote so an
// under-trained RLS can't dominate. 4000W forecast + 2000W twin with
// PlannerRadiationWeight=0.3 → 0.7*4000 + 0.3*2000 = 3400.
func TestSelectPlannerPVWRadiationBlend(t *testing.T) {
	got := selectPlannerPVW(4000, 2000, true)
	want := 0.7*4000 + 0.3*2000
	if math.Abs(got-want) > 0.01 {
		t.Errorf("radiation blend: got %f, want %f", got, want)
	}
}

// Even a wild twin overshoot gets capped by the 30% weight — 4000W
// forecast + 10000W twin → 0.7*4000 + 0.3*10000 = 5800. Still sane,
// not the full 10000 the cloud-only path would have let through.
func TestSelectPlannerPVWRadiationBlendClampsWildTwin(t *testing.T) {
	got := selectPlannerPVW(4000, 10000, true)
	want := 0.7*4000 + 0.3*10000
	if math.Abs(got-want) > 0.01 {
		t.Errorf("radiation blend clamping: got %f, want %f", got, want)
	}
	// Without radiation backing, the same inputs would let the twin
	// take over completely (cloud-only path, not collapsed).
	if got := selectPlannerPVW(4000, 10000, false); got != 10000 {
		t.Errorf("cloud-only path should pass through twin prediction, got %f", got)
	}
}

// A provider's explicit zero is a valid signal, including night.
func TestSelectPlannerPVWRadiationZeroDoesNotInventPV(t *testing.T) {
	if got := selectPlannerPVW(0, 300, true); got != 0 {
		t.Fatalf("provider zero became %v W", got)
	}
}

func TestSelectPlannerPVWContinuousAtLegacyThreshold(t *testing.T) {
	before := selectPlannerPVW(6000, 50, true)
	after := selectPlannerPVW(6000, 51, true)
	if math.Abs((after-before)-PlannerRadiationWeight) > 1e-9 {
		t.Fatalf("one watt changed blend %v -> %v", before, after)
	}
}

func TestPlannerPVWeightRequiresBoundedTrust(t *testing.T) {
	for _, tc := range []struct{ weight, want float64 }{{0, 6000}, {1, 50}, {-1, 6000}, {2, 50}, {math.NaN(), 6000}} {
		if got := selectPlannerPVWithWeight(6000, 50, true, tc.weight); got != tc.want {
			t.Fatalf("weight %v: got%v want%v", tc.weight, got, tc.want)
		}
	}
}

// Cap must be a no-op when forecast and twin are within PlannerForecastCapRatio.
// A well-trained twin slightly under-predicting because of orientation or
// soiling should not trigger the cap.
func TestSelectPlannerPVWForecastCapInactiveWhenRatioOK(t *testing.T) {
	forecast := 4000.0
	twin := 2000.0 // 2× — well below cap threshold of 3×

	got := selectPlannerPVW(forecast, twin, true)
	want := (1-PlannerRadiationWeight)*forecast + PlannerRadiationWeight*twin
	if math.Abs(got-want) > 0.01 {
		t.Errorf("cap should be inactive when forecast/twin=2x: got %.2f, want %.2f", got, want)
	}
}

// Cap must not activate when the twin is near-zero (< 50 W):  that means the
// physics night gate fired and the twin result is meaningless — the forecast
// should dominate unchanged.
func TestSelectPlannerPVWForecastCapInactiveWhenTwinNearZero(t *testing.T) {
	// Twin near-zero (e.g. cs < 50 W/m² after physics gate) but forecast
	// still has some radiation signal at twilight.
	forecast := 300.0
	twin := 30.0 // below the 50 W threshold

	got := selectPlannerPVW(forecast, twin, true)
	want := (1-PlannerRadiationWeight)*forecast + PlannerRadiationWeight*twin // no cap
	if math.Abs(got-want) > 0.01 {
		t.Errorf("cap should be inactive when twin < 50 W: got %.2f, want %.2f", got, want)
	}
}

// Strict self_consumption: even with a high terminal price (= mean
// import), the DP must still discharge when battery has headroom
// (SoC > min + 20). This used to be a guardrail documenting the
// OPPOSITE behaviour — that a too-high terminal price blocked
// discharge and we'd just sit and import. The strict-SC bias
// introduced in the planner-logic investigation round inverts it:
// self_consumption now means "use the battery first" regardless of
// the terminal-value arithmetic. That matches the operator intent
// the mode name implies.
func TestOptimizeSelfConsumptionDischargesDespiteHighTerminal(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoC = 0.8
	p.TerminalSoCPrice = 300 // mean import price — pre-strict this would have blocked discharge.

	plan := Optimize(slots, p)
	var anyDischarge bool
	for _, a := range plan.Actions {
		if a.BatteryW < -100 {
			anyDischarge = true
			break
		}
	}
	if !anyDischarge {
		t.Fatalf("strict SC should discharge despite terminal=mean; got actions %+v", plan.Actions)
	}
}

// SlotDirectiveAt returns energy-allocation directive for the slot
// containing now. Verifies that power is converted to energy via the
// slot length, that stale plans return ok=false, and that out-of-window
// queries return ok=false.
func TestSlotDirectiveAt(t *testing.T) {
	// Anchor on real wall clock — SlotDirectiveAt rejects plans older
	// than MaxPlanAge (30 min) via time.Since(GeneratedAtMs), so a
	// hardcoded past timestamp would make this test flaky as soon as
	// the wall clock drifts past the plan's age ceiling.
	now := time.Now().UTC().Truncate(time.Second)
	slotStart := now.Add(-3 * time.Minute) // we're 3 min into a 15-min slot
	slotLenMin := 15

	s := &Service{
		Defaults: Params{Mode: ModeArbitrage},
		lastParams: Params{
			Mode: ModeArbitrage, CapacityWh: 10000, ChargeEfficiency: 1,
		},
		last: &Plan{
			DecisionID:    testDecisionID1,
			GeneratedAtMs: now.Add(-time.Minute).UnixMilli(),
			Actions: []Action{
				{
					SlotStartMs: slotStart.UnixMilli(),
					SlotLenMin:  slotLenMin,
					SpotOre:     40,
					BatteryW:    800, // 800 W × 15/60 h = 200 Wh for the slot
					SoC:         0.455,
					GridW:       -150, // plan expects 150 W export
				},
				{
					SlotStartMs: slotStart.Add(15 * time.Minute).UnixMilli(),
					SlotLenMin:  slotLenMin,
					PriceOre:    120, // later grid charge costs more than export earns now
					BatteryW:    2000,
					GridW:       1500,
					SoC:         0.8,
				},
			},
		},
	}

	d, ok := s.SlotDirectiveAt(now)
	if !ok {
		t.Fatal("SlotDirectiveAt returned ok=false, want true")
	}
	if d.DecisionID != testDecisionID1 {
		t.Errorf("DecisionID = %q, want %q", d.DecisionID, testDecisionID1)
	}
	if want := 200.0; math.Abs(d.BatteryEnergyWh-want) > 0.01 {
		t.Errorf("BatteryEnergyWh = %f, want %f", d.BatteryEnergyWh, want)
	}
	if !d.SlotStart.Equal(slotStart) {
		t.Errorf("SlotStart = %v, want %v", d.SlotStart, slotStart)
	}
	if want := slotStart.Add(15 * time.Minute); !d.SlotEnd.Equal(want) {
		t.Errorf("SlotEnd = %v, want %v", d.SlotEnd, want)
	}
	if d.SoCTarget != 0.455 {
		t.Errorf("SoCTarget = %f, want 0.455", d.SoCTarget)
	}
	if d.Strategy != ModeArbitrage {
		t.Errorf("Strategy = %v, want arbitrage", d.Strategy)
	}
	// GridW must surface unchanged from the plan action — this is the
	// wiring the control-layer PlannedGridW cap depends on. If it
	// silently breaks, the cap silently never fires.
	if d.GridW != -150 {
		t.Errorf("GridW = %f, want −150 (must propagate from Action.GridW)", d.GridW)
	}
	if math.Abs(d.LivePVSurplusSoCCap-0.4925) > 1e-9 {
		t.Errorf("LivePVSurplusSoCCap = %f, want 0.4925 from 375 Wh of later grid-funded charge", d.LivePVSurplusSoCCap)
	}
}

func TestLivePVSurplusSoCCapEconomicGate(t *testing.T) {
	base := []Action{
		{SpotOre: 50, BatteryW: 0, SoC: 0.4},
		{SlotLenMin: 15, PriceOre: 120, BatteryW: 2000, GridW: 1500, SoC: 0.75},
	}
	tests := []struct {
		name    string
		mutate  func([]Action)
		params  Params
		wantCap float64
	}{
		{name: "cap follows later grid-funded energy", wantCap: 0.4375},
		{name: "profitable current export is preserved", mutate: func(a []Action) {
			a[0].SpotOre = 150
		}, wantCap: 0},
		{name: "minimum spread rejects marginal replacement", params: Params{MinArbitrageSpreadOreKwh: 80}, wantCap: 0},
		{name: "future PV charge is not grid funded", mutate: func(a []Action) {
			a[1].GridW = -500
		}, wantCap: 0},
		{name: "current discharge is never reversed", mutate: func(a []Action) {
			a[0].BatteryW = -1000
		}, wantCap: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actions := append([]Action(nil), base...)
			if tc.mutate != nil {
				tc.mutate(actions)
			}
			p := tc.params
			p.CapacityWh = 10000
			p.ChargeEfficiency = 1
			if got := livePVSurplusSoCCap(actions, 0, p); got != tc.wantCap {
				t.Errorf("cap = %.1f, want %.1f", got, tc.wantCap)
			}
		})
	}
}

// Discharge intent (negative BatteryW) surfaces as negative energy.
func TestSlotDirectiveAtDischarge(t *testing.T) {
	// Use real wall clock — MaxPlanAge (30 min) would reject a hardcoded
	// past timestamp. Same flake as TestSlotDirectiveAt earlier.
	now := time.Now().UTC().Truncate(time.Second)
	s := &Service{
		last: &Plan{
			GeneratedAtMs: now.UnixMilli(),
			Actions: []Action{{
				SlotStartMs: now.UnixMilli(),
				SlotLenMin:  15,
				BatteryW:    -2400, // discharge 600 Wh over 15 min
			}},
		},
	}
	d, ok := s.SlotDirectiveAt(now)
	if !ok {
		t.Fatal("ok=false")
	}
	if want := -600.0; math.Abs(d.BatteryEnergyWh-want) > 0.01 {
		t.Errorf("BatteryEnergyWh = %f, want %f", d.BatteryEnergyWh, want)
	}
}

// A plan older than MaxPlanAge should not surface any directive — the
// control loop falls back to auto_fallback.
func TestSlotDirectiveAtStalePlan(t *testing.T) {
	now := time.Now()
	s := &Service{
		last: &Plan{
			GeneratedAtMs: now.Add(-MaxPlanAge - time.Minute).UnixMilli(),
			Actions: []Action{{
				SlotStartMs: now.UnixMilli(),
				SlotLenMin:  15,
				BatteryW:    800,
			}},
		},
	}
	if _, ok := s.SlotDirectiveAt(now); ok {
		t.Error("SlotDirectiveAt returned ok=true for stale plan, want false")
	}
}

// A query outside any slot's time window should return ok=false.
func TestSlotDirectiveAtOutOfWindow(t *testing.T) {
	slotStart := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	s := &Service{
		last: &Plan{
			GeneratedAtMs: slotStart.UnixMilli(),
			Actions: []Action{{
				SlotStartMs: slotStart.UnixMilli(),
				SlotLenMin:  15,
				BatteryW:    800,
			}},
		},
	}
	future := slotStart.Add(30 * time.Minute) // 15 min past slot end
	if _, ok := s.SlotDirectiveAt(future); ok {
		t.Error("SlotDirectiveAt returned ok=true for out-of-window time")
	}
}

// Nil service must not panic.
func TestSlotDirectiveAtNilService(t *testing.T) {
	var s *Service
	if _, ok := s.SlotDirectiveAt(time.Now()); ok {
		t.Error("nil Service returned ok=true")
	}
}

func TestClampForecastPVHouseNameplate(t *testing.T) {
	t.Parallel()
	wild := 3544200.0
	ok := 3200.0
	bjorn := 2046200.0 // tooltip 2046.2 kW with rated 18960 W
	rows := clampForecastPV([]state.ForecastPoint{
		{PVWEstimated: &wild},
		{PVWEstimated: &ok},
		{PVWEstimated: &bjorn},
	}, 18960)
	if rows[0].PVWEstimated == nil || *rows[0].PVWEstimated != 18960 {
		t.Fatalf("3544 kW row must cut to 18960 W, got %+v", rows[0].PVWEstimated)
	}
	if rows[1].PVWEstimated == nil || *rows[1].PVWEstimated != 3200 {
		t.Fatalf("in-range estimate must stay, got %+v", rows[1].PVWEstimated)
	}
	if rows[2].PVWEstimated == nil || *rows[2].PVWEstimated != 18960 {
		t.Fatalf("2046.2 kW row must cut to 18960 W, got %+v", rows[2].PVWEstimated)
	}
}

func TestCapSlotsPVToNameplate(t *testing.T) {
	t.Parallel()
	slots := capSlotsPVToNameplate([]Slot{
		{PVW: -2046200},
		{PVW: -3200},
		{PVW: 0},
	}, 18960)
	if slots[0].PVW != -18960 {
		t.Fatalf("2 MW slot must cut to −18960 W, got %v", slots[0].PVW)
	}
	if slots[1].PVW != -3200 {
		t.Fatalf("in-range generation must stay, got %v", slots[1].PVW)
	}
	if slots[2].PVW != 0 {
		t.Fatalf("night slot must stay 0, got %v", slots[2].PVW)
	}
}

func TestCapPlanPVToNameplate(t *testing.T) {
	t.Parallel()
	plan := &Plan{Actions: []Action{{PVW: -2046200}, {PVW: -4000}}}
	capPlanPVToNameplate(plan, 18960)
	if plan.PVNameplateW != 18960 {
		t.Fatalf("plan must carry nameplate, got %v", plan.PVNameplateW)
	}
	if plan.Actions[0].PVW != -18960 {
		t.Fatalf("published action must cut to −18960 W, got %v", plan.Actions[0].PVW)
	}
	if plan.Actions[1].PVW != -4000 {
		t.Fatalf("in-range action must stay, got %v", plan.Actions[1].PVW)
	}
}
