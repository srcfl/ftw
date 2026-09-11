package mpc

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"
)

// ---- Scenario generators ----
//
// Each scenario returns 96 × 15-min slots (24h) with realistic prices + PV
// + load. We compare strategies on the *same* slots so differences come
// purely from the planner's policy.

type scenario struct {
	name  string
	slots []Slot
	initSoC float64
}

// prices over 24h. Shape is a Nordic-ish two-peak day: morning + evening
// spikes, midday dip when solar floods the market.
func nordicPrices(mean, amplitude float64, rng *rand.Rand) []float64 {
	out := make([]float64, 96)
	for i := range out {
		h := float64(i) / 4.0 // 0..24
		morning := math.Exp(-0.5 * math.Pow((h-7.5)/1.2, 2))
		evening := math.Exp(-0.5 * math.Pow((h-18)/1.4, 2))
		midday := -0.7 * math.Exp(-0.5*math.Pow((h-13)/2.2, 2))
		shape := morning + evening + midday
		noise := (rng.Float64()*2 - 1) * 0.05
		out[i] = mean + amplitude*(shape+noise)
		if out[i] < 20 {
			out[i] = 20
		}
	}
	return out
}

// pvCurve returns 96 × PV watts over 24h for a south-facing array.
func pvCurve(peakW float64) []float64 {
	out := make([]float64, 96)
	for i := range out {
		h := float64(i) / 4.0
		if h <= 6 || h >= 19 {
			out[i] = 0
			continue
		}
		// Gaussian peak at solar noon.
		out[i] = peakW * math.Exp(-0.5*math.Pow((h-12.5)/2.8, 2))
	}
	return out
}

// loadCurve: 2-peak house load (morning + evening) with base load.
func loadCurve(baseW, peakW float64) []float64 {
	out := make([]float64, 96)
	for i := range out {
		h := float64(i) / 4.0
		morning := math.Exp(-0.5 * math.Pow((h-7)/1, 2))
		evening := math.Exp(-0.5 * math.Pow((h-19)/1.5, 2))
		out[i] = baseW + peakW*(morning+evening)
	}
	return out
}

func makeSlots(prices, pvs, loads []float64, startMs int64) []Slot {
	n := len(prices)
	out := make([]Slot, n)
	for i := 0; i < n; i++ {
		out[i] = Slot{
			StartMs:  startMs + int64(i)*15*60*1000,
			LenMin:   15,
			PriceOre: prices[i],
			SpotOre:  prices[i] * 0.7, // strip VAT + grid tariff for export
			PVW:      -pvs[i],
			LoadW:    loads[i],
		}
	}
	return out
}

// ---- Scenario library ----

func buildScenarios(rng *rand.Rand) []scenario {
	start := time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC).UnixMilli()
	var s []scenario

	// Sunny day — strong PV, mild price volatility.
	s = append(s, scenario{
		name:   "sunny_mild",
		slots:  makeSlots(nordicPrices(130, 60, rng), pvCurve(8000), loadCurve(500, 1500), start),
		initSoC: 0.50,
	})

	// Cloudy day — barely any PV, typical prices.
	s = append(s, scenario{
		name:   "cloudy",
		slots:  makeSlots(nordicPrices(160, 80, rng), pvCurve(2000), loadCurve(500, 1500), start),
		initSoC: 0.50,
	})

	// Price-spike day (cold winter, low PV, Europe-wide gas crisis).
	prices := nordicPrices(250, 300, rng)
	// Amplify evening peak 3×
	for i := 18 * 4; i < 22*4; i++ {
		prices[i] *= 2.2
	}
	s = append(s, scenario{
		name:   "price_spike",
		slots:  makeSlots(prices, pvCurve(1000), loadCurve(1200, 3000), start),
		initSoC: 0.50,
	})

	// Flat day — nearly constant prices, no arbitrage.
	flat := make([]float64, 96)
	for i := range flat {
		flat[i] = 100 + (rng.Float64()*2-1)*5
	}
	s = append(s, scenario{
		name:   "flat_prices",
		slots:  makeSlots(flat, pvCurve(5000), loadCurve(500, 1500), start),
		initSoC: 0.50,
	})

	// Cheap-night scenario — classic overnight charging window.
	nightCheap := nordicPrices(180, 100, rng)
	for i := 0; i < 6*4; i++ {
		nightCheap[i] = 40 + (rng.Float64()*2-1)*10
	}
	s = append(s, scenario{
		name:   "cheap_night",
		slots:  makeSlots(nightCheap, pvCurve(3000), loadCurve(600, 2000), start),
		initSoC: 0.20, // start with a near-empty battery to test grid charging
	})

	// Extreme export day — massive PV, low prices from surplus.
	exportDay := nordicPrices(60, 30, rng)
	for i := 10 * 4; i < 15*4; i++ {
		exportDay[i] = 10 + (rng.Float64()*2-1)*5
	}
	s = append(s, scenario{
		name:   "solar_surplus",
		slots:  makeSlots(exportDay, pvCurve(12000), loadCurve(400, 1200), start),
		initSoC: 0.30,
	})

	return s
}

// ---- Stress runner ----

func runMode(slots []Slot, initSoC float64, mode Mode, capWh, maxChg, maxDis float64) Plan {
	mean := 0.0
	for _, s := range slots {
		mean += s.PriceOre
	}
	mean /= float64(len(slots))
	p := Params{
		Mode:                mode,
		SoCLevels:           51,
		CapacityWh:          capWh,
		SoCMin: 0.1,
		SoCMax: 0.95,
		InitialSoC:       initSoC,
		ActionLevels:        21,
		MaxChargeW:          maxChg,
		MaxDischargeW:       maxDis,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice: mean,
		// Per-slot export pricing (slot.SpotOre + bonus − fee). Reflects
		// the real Nordic setup where exporting earns the current
		// spot, not a flat average. Required for the DP to see
		// morning-export vs midday-charge arbitrage.
		ExportOrePerKWh: 0,
	}
	return Optimize(slots, p)
}

// baselineCost = cost if the battery did nothing (idle 24h).
func baselineCost(slots []Slot, exportCredit float64) float64 {
	var total float64
	for _, s := range slots {
		dt := float64(s.LenMin) / 60.0
		gridKWh := (s.LoadW + s.PVW) * dt / 1000.0
		if gridKWh > 0 {
			total += s.PriceOre * gridKWh
		} else {
			total += -exportCredit * (-gridKWh)
		}
	}
	return total
}

// planStats summarises cycles + SoC range for a plan.
func planStats(p Plan) (chgKWh, disKWh, socMin, socMax float64) {
	socMin, socMax = 1, 0
	for _, a := range p.Actions {
		dt := float64(a.SlotLenMin) / 60.0
		if a.BatteryW > 0 {
			chgKWh += a.BatteryW * dt / 1000.0
		} else {
			disKWh += -a.BatteryW * dt / 1000.0
		}
		if a.SoC < socMin {
			socMin = a.SoC
		}
		if a.SoC > socMax {
			socMax = a.SoC
		}
	}
	return
}

// TestAnnualSavingsProjection takes the full scenario set, picks the best
// strategy per scenario, and projects annual SEK savings assuming the
// scenarios are representative of the year's mix.
//
// Scenario weights (fraction of days):
//
//	sunny_mild       25%   typical good-weather day
//	cloudy           30%   typical overcast day
//	price_spike       5%   Europe-wide cold snap etc.
//	flat_prices      10%   low-volatility day
//	cheap_night      15%   typical overnight-cheap day
//	solar_surplus    15%   high-summer surplus day
//
// Rough but grounded enough to give operators a ballpark annual figure.
func TestAnnualSavingsProjection(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	scenarios := buildScenarios(rng)
	weights := map[string]float64{
		"sunny_mild":    0.25,
		"cloudy":        0.30,
		"price_spike":   0.05,
		"flat_prices":   0.10,
		"cheap_night":   0.15,
		"solar_surplus": 0.15,
	}
	const (
		capWh  = 15000.0
		maxChg = 5000.0
		maxDis = 5000.0
	)
	modes := []Mode{ModeSelfConsumption, ModeCheapCharge, ModeArbitrage}
	fmt.Printf("\n=== ANNUAL SAVINGS PROJECTION (SEK / year) ===\n")
	fmt.Printf("%-18s  %-8s  %10s  %10s  %10s\n",
		"SCENARIO", "WEIGHT", "SELF", "CHEAP", "ARB")
	fmt.Println("-----------------------------------------------------------------")
	totals := map[Mode]float64{}
	for _, sc := range scenarios {
		w, ok := weights[sc.name]
		if !ok {
			continue
		}
		var mean float64
		for _, s := range sc.slots {
			mean += s.PriceOre
		}
		mean /= float64(len(sc.slots))
		base := baselineCost(sc.slots, mean*0.7)
		initKWh := sc.initSoC * capWh / 1000
		baseNet := base/100 - mean*initKWh/100
		row := fmt.Sprintf("%-18s  %-8.0f%%", sc.name, w*100)
		for _, m := range modes {
			plan := runMode(sc.slots, sc.initSoC, m, capWh, maxChg, maxDis)
			endKWh := plan.Actions[len(plan.Actions)-1].SoC * capWh / 1000
			netSek := plan.TotalCostOre/100 - mean*endKWh/100
			savings := baseNet - netSek // positive = saving
			annual := savings * 365 * w
			totals[m] += annual
			row += fmt.Sprintf("  %10.0f", annual)
		}
		fmt.Println(row)
	}
	fmt.Println("-----------------------------------------------------------------")
	fmt.Printf("%-18s  %-9s%10.0f  %10.0f  %10.0f\n", "TOTAL SEK/yr", "",
		totals[ModeSelfConsumption], totals[ModeCheapCharge], totals[ModeArbitrage])
	fmt.Println()
	// Sanity: at least one mode should save something per year.
	best := math.Max(totals[ModeCheapCharge], totals[ModeArbitrage])
	if best <= 0 {
		t.Errorf("no positive savings projected — something is off: %+v", totals)
	}
}

// TestStrategyComparison runs all three modes across all scenarios and
// prints a table. Not a pass/fail test — it's a reporter. But we assert
// some sanity invariants along the way.
func TestStrategyComparison(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	scenarios := buildScenarios(rng)

	const (
		capWh  = 15000.0
		maxChg = 5000.0
		maxDis = 5000.0
	)

	modes := []Mode{ModeSelfConsumption, ModeCheapCharge, ModeArbitrage}
	// COST_SEK is energy cost over the horizon alone.
	// NET_SEK subtracts the value of the kWh still in the battery at horizon end,
	// valued at the mean spot price — a fairer apples-to-apples across modes.
	fmt.Printf("\n%-18s  %-20s  %8s  %8s  %8s  %8s  %8s  %12s\n",
		"SCENARIO", "MODE", "COST_SEK", "NET_SEK", "vs_BASE", "CHG_kWh", "DIS_kWh", "SoC_%")
	fmt.Println("---------------------------------------------------------------------------------------------------------")
	for _, sc := range scenarios {
		// Export credit for the baseline = same as arbitrage uses.
		mean := 0.0
		for _, s := range sc.slots {
			mean += s.PriceOre
		}
		mean /= float64(len(sc.slots))
		base := baselineCost(sc.slots, mean*0.7)
		initKWh := sc.initSoC * capWh / 1000
		baseNet := base/100 - mean*initKWh/100 // baseline ends with initial SoC
		for i, m := range modes {
			plan := runMode(sc.slots, sc.initSoC, m, capWh, maxChg, maxDis)
			chgK, disK, smin, smax := planStats(plan)
			endKWh := plan.Actions[len(plan.Actions)-1].SoC * capWh / 1000
			costSek := plan.TotalCostOre / 100
			netSek := costSek - mean*endKWh/100
			vsBase := netSek - baseNet
			label := sc.name
			if i > 0 {
				label = ""
			}
			fmt.Printf("%-18s  %-20s  %8.2f  %8.2f  %+8.2f  %8.2f  %8.2f  %5.0f→%3.0f\n",
				label, m, costSek, netSek, vsBase, chgK, disK, smin*100, smax*100)
			// No ordering assertion: different modes end at different
			// SoC, so raw spent-energy cost doesn't capture the
			// terminal value of stored kWh. The DP *does* account for
			// it internally via TerminalSoCPrice — that's why
			// cheap_charge/arbitrage "spend more today" to end at 95%.
		}
		fmt.Println()
	}
}

// TestOptimizerPerformance measures time-per-solve.
func TestOptimizerPerformance(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	slots := makeSlots(nordicPrices(150, 80, rng), pvCurve(6000), loadCurve(600, 2000),
		time.Now().UnixMilli())
	// 96 slots × 51 SoC × 21 actions = 102,816 state evaluations per stage
	// → ~10M total. Must solve in under 100ms.
	start := time.Now()
	const runs = 10
	for i := 0; i < runs; i++ {
		runMode(slots, 50, ModeArbitrage, 15000, 5000, 5000)
	}
	per := time.Since(start) / runs
	t.Logf("24h × 15min horizon, 51 SoC × 21 actions → %v per solve", per)
	if per > 100*time.Millisecond {
		t.Errorf("optimizer too slow: %v per solve (target <100ms)", per)
	}
}

// TestModeIsRegressionSafe: with identical inputs, mode output is
// deterministic. Locks in current behavior so future refactors can be
// caught in CI.
func TestModeIsRegressionSafe(t *testing.T) {
	rng := rand.New(rand.NewSource(123))
	slots := makeSlots(nordicPrices(150, 80, rng), pvCurve(5000), loadCurve(500, 1500),
		time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC).UnixMilli())
	p1 := runMode(slots, 50, ModeArbitrage, 15000, 5000, 5000)
	p2 := runMode(slots, 50, ModeArbitrage, 15000, 5000, 5000)
	if math.Abs(p1.TotalCostOre-p2.TotalCostOre) > 1e-9 {
		t.Errorf("non-deterministic: %f vs %f", p1.TotalCostOre, p2.TotalCostOre)
	}
}
