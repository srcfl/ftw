package savings

import (
	"math"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// constHistory generates one HistoryPoint per minute over [startMs, endMs]
// inclusive — dense enough that the 20-minute gap cap never trips and slot
// integration is essentially analytic. Site-sign convention throughout.
func constHistory(startMs, endMs int64, gridW, pvW, batW, loadW float64) []state.HistoryPoint {
	const stepMs int64 = 60_000
	out := make([]state.HistoryPoint, 0, (endMs-startMs)/stepMs+1)
	for ts := startMs; ts <= endMs; ts += stepMs {
		out = append(out, state.HistoryPoint{
			TsMs:  ts,
			GridW: gridW,
			PVW:   pvW,
			BatW:  batW,
			LoadW: loadW,
		})
	}
	return out
}

// concatHistory glues together two minute-resolution segments and de-dupes
// the boundary sample (otherwise the integration would double-count it).
func concatHistory(a, b []state.HistoryPoint) []state.HistoryPoint {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	if a[len(a)-1].TsMs == b[0].TsMs {
		b = b[1:]
	}
	return append(append([]state.HistoryPoint{}, a...), b...)
}

func priceSlot(startMs int64, lenMin int, totalOre, spotOre float64) state.PricePoint {
	return state.PricePoint{
		Zone:        "SE3",
		SlotTsMs:    startMs,
		SlotLenMin:  lenMin,
		TotalOreKwh: totalOre,
		SpotOreKwh:  spotOre,
		Source:      "test",
		FetchedAtMs: startMs,
	}
}

// approx checks two öre values for near-equality, tolerating sub-öre
// rounding from minute-resolution integration over hour slots.
func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %.4f, want %.4f (tol %.4f)", name, got, want, tol)
	}
}

var defaultParams = mpc.Params{
	// Export terms zero → SlotGridCostOre uses bare SpotOre (clamped ≥ 0).
}

const hourMs int64 = 60 * 60 * 1000

// Empty inputs return a zero result without panicking.
func TestComputeDay_Empty(t *testing.T) {
	out := ComputeDay(nil, nil, defaultParams, 0, hourMs)
	if out.ActualOre != 0 || out.NoBatteryOre != 0 || out.SavingsOre != 0 {
		t.Errorf("expected all zero costs, got %+v", out)
	}
	if out.CoveragePct != 0 {
		t.Errorf("expected zero coverage, got %.4f", out.CoveragePct)
	}
	if len(out.Slots) != 0 {
		t.Errorf("expected no slots, got %d", len(out.Slots))
	}
}

// Constant import, no battery, no PV: actual cost == no-battery cost == 100 öre.
func TestComputeDay_FlatLoadZeroSavings(t *testing.T) {
	const day0 = int64(1_700_000_000_000) // an arbitrary fixed origin
	hist := constHistory(day0, day0+hourMs, 1000, 0, 0, 1000)
	prices := []state.PricePoint{priceSlot(day0, 60, 100, 50)}

	out := ComputeDay(hist, prices, defaultParams, day0, day0+hourMs)
	approx(t, "ActualOre", out.ActualOre, 100, 0.5)
	approx(t, "NoBatteryOre", out.NoBatteryOre, 100, 0.5)
	approx(t, "SavingsOre", out.SavingsOre, 0, 0.5)
	approx(t, "ImportKWh", out.ImportKWh, 1.0, 0.01)
	if out.CoveragePct < 0.99 {
		t.Errorf("expected ~full coverage, got %.4f", out.CoveragePct)
	}
}

// Battery does textbook arbitrage: charges 1 kWh at 100 öre, discharges
// 1 kWh that displaces a 1 kWh load at 200 öre. Round-trip lossless to
// keep the arithmetic clean.
//
//	actual:  slot0 imported 1 kWh @ 100 = 100  (charging)
//	         slot1 imported 0           = 0    (battery covers load)
//	         total = 100 öre
//	no-bat:  slot0 imported 0           = 0    (no load that hour)
//	         slot1 imported 1 kWh @ 200 = 200
//	         total = 200 öre
//	savings = 100 öre
func TestComputeDay_PerfectArbitrage(t *testing.T) {
	const day0 = int64(1_700_000_000_000)
	// Slot 0: cheap, charging — load=0, bat=+1000W (charging from grid),
	// grid=+1000W (importing).
	seg0 := constHistory(day0, day0+hourMs, 1000, 0, 1000, 0)
	// Slot 1: expensive, discharging to cover load.
	// load=1000W, bat=−1000W (discharging), pv=0, grid=0.
	seg1 := constHistory(day0+hourMs, day0+2*hourMs, 0, 0, -1000, 1000)
	hist := concatHistory(seg0, seg1)

	prices := []state.PricePoint{
		priceSlot(day0, 60, 100, 50),
		priceSlot(day0+hourMs, 60, 200, 100),
	}
	out := ComputeDay(hist, prices, defaultParams, day0, day0+2*hourMs)

	approx(t, "ActualOre", out.ActualOre, 100, 1.0)
	approx(t, "NoBatteryOre", out.NoBatteryOre, 200, 1.0)
	approx(t, "SavingsOre", out.SavingsOre, 100, 1.0)
	if len(out.Slots) != 2 {
		t.Fatalf("expected 2 slot rows, got %d", len(out.Slots))
	}
	// Per-slot sanity: slot 0 imports 1 kWh, slot 1 discharges 1 kWh.
	approx(t, "slot0.GridImportKWh", out.Slots[0].GridImportKWh, 1.0, 0.02)
	approx(t, "slot1.BatDischargedKWh", out.Slots[1].BatDischargedKWh, 1.0, 0.02)
}

// PV exactly equals load → no flow either way, regardless of battery.
// Both costs are zero, savings are zero.
func TestComputeDay_PerfectSelfConsumption(t *testing.T) {
	const day0 = int64(1_700_000_000_000)
	// load=1000, pv=−1000, grid=0, bat=0
	hist := constHistory(day0, day0+hourMs, 0, -1000, 0, 1000)
	prices := []state.PricePoint{priceSlot(day0, 60, 150, 80)}

	out := ComputeDay(hist, prices, defaultParams, day0, day0+hourMs)
	approx(t, "ActualOre", out.ActualOre, 0, 0.5)
	approx(t, "NoBatteryOre", out.NoBatteryOre, 0, 0.5)
	approx(t, "SavingsOre", out.SavingsOre, 0, 0.5)
	approx(t, "PVKWh", out.PVKWh, 1.0, 0.02)
	approx(t, "LoadKWh", out.LoadKWh, 1.0, 0.02)
}

// A slot where the meter both imports and exports must be priced
// per-direction. Half the slot import 1000W (= 0.5 kWh @ 100 öre = 50 öre);
// half the slot export 1000W (= 0.5 kWh @ 50 öre = 25 öre revenue). Net
// cost = 25 öre. Naive netting would yield 0 — that's the bug we're
// guarding against.
func TestComputeDay_MixedDirectionPricedSeparately(t *testing.T) {
	const day0 = int64(1_700_000_000_000)
	// First 30 min: import 1000W (load=1000, pv=0, grid=+1000, bat=0).
	imp := constHistory(day0, day0+hourMs/2, 1000, 0, 0, 1000)
	// Last 30 min: export 1000W (load=0, pv=−1000, grid=−1000, bat=0).
	exp := constHistory(day0+hourMs/2, day0+hourMs, -1000, -1000, 0, 0)
	hist := concatHistory(imp, exp)

	prices := []state.PricePoint{priceSlot(day0, 60, 100, 50)}
	out := ComputeDay(hist, prices, defaultParams, day0, day0+hourMs)

	// 0.5 kWh import × 100 öre/kWh − 0.5 kWh export × 50 öre/kWh = 50 − 25 = 25 öre
	approx(t, "ActualOre", out.ActualOre, 25, 1.0)
	// No-battery counterfactual is identical (no battery activity).
	approx(t, "NoBatteryOre", out.NoBatteryOre, 25, 1.0)
	approx(t, "SavingsOre", out.SavingsOre, 0, 1.0)
	approx(t, "ImportKWh", out.ImportKWh, 0.5, 0.02)
	approx(t, "ExportKWh", out.ExportKWh, 0.5, 0.02)
}

// A 30-minute gap in the middle of a slot exceeds MaxGapMs (20 min) and
// must NOT be attributed — the row after the gap should contribute zero
// energy and zero coverage. Without the cap, a stale W reading across an
// outage would silently inflate the cost in either direction.
func TestComputeDay_LargeGapNotAttributed(t *testing.T) {
	const day0 = int64(1_700_000_000_000)
	// Two segments of 5 minutes each, separated by a 30-minute gap.
	first := constHistory(day0, day0+5*60_000, 1000, 0, 0, 1000)
	second := constHistory(day0+35*60_000, day0+40*60_000, 1000, 0, 0, 1000)
	hist := append(first, second...)

	prices := []state.PricePoint{priceSlot(day0, 60, 100, 50)}
	out := ComputeDay(hist, prices, defaultParams, day0, day0+hourMs)

	// Coverage should be ~10 min out of 60 → ~0.167.
	if out.CoveragePct > 0.25 || out.CoveragePct < 0.10 {
		t.Errorf("unexpected coverage %.4f, want ~0.167", out.CoveragePct)
	}
	// Energy: only the 10 min that were covered. ~1000W × 10/60 h = 0.167 kWh.
	approx(t, "ImportKWh", out.ImportKWh, 0.167, 0.02)
}

// Confirm coverage tracking finds zero history at all → CoveragePct = 0
// (not NaN, not 1).
func TestComputeDay_NoHistoryButPrices(t *testing.T) {
	const day0 = int64(1_700_000_000_000)
	prices := []state.PricePoint{priceSlot(day0, 60, 100, 50)}
	out := ComputeDay(nil, prices, defaultParams, day0, day0+hourMs)
	if out.CoveragePct != 0 {
		t.Errorf("expected zero coverage with no history, got %.4f", out.CoveragePct)
	}
	approx(t, "ActualOre", out.ActualOre, 0, 0.5)
	approx(t, "NoBatteryOre", out.NoBatteryOre, 0, 0.5)
}

// Slot straddling the day window should be clipped at the boundary so it
// contributes only its in-window portion (here 30 of 60 min).
func TestComputeDay_SlotClippedAtDayBoundary(t *testing.T) {
	const day0 = int64(1_700_000_000_000)
	// Slot starts 30 min before day0 and runs 60 min — only the back
	// half should be used.
	slot := priceSlot(day0-hourMs/2, 60, 100, 50)
	hist := constHistory(day0-hourMs, day0+hourMs, 1000, 0, 0, 1000)

	out := ComputeDay(hist, []state.PricePoint{slot}, defaultParams, day0, day0+hourMs)
	if len(out.Slots) != 1 {
		t.Fatalf("expected 1 slot row, got %d", len(out.Slots))
	}
	// Only 30 min of the 60-min priced slot lies inside the day window.
	approx(t, "ImportKWh", out.ImportKWh, 0.5, 0.02)
	approx(t, "ActualOre", out.ActualOre, 50, 1.0)
}

// Out-of-order inputs should still produce the same answer — ComputeDay
// is contracted to sort defensively.
func TestComputeDay_UnsortedInputsTolerated(t *testing.T) {
	const day0 = int64(1_700_000_000_000)
	hist := constHistory(day0, day0+hourMs, 1000, 0, 0, 1000)
	// Reverse to test sort.
	rev := make([]state.HistoryPoint, len(hist))
	for i := range hist {
		rev[i] = hist[len(hist)-1-i]
	}
	prices := []state.PricePoint{priceSlot(day0, 60, 100, 50)}
	out := ComputeDay(rev, prices, defaultParams, day0, day0+hourMs)
	approx(t, "ActualOre", out.ActualOre, 100, 0.5)
}

// Decomposition: PV exceeds load, surplus splits between battery and grid.
func TestDecompose_PVSurplusSplitsBatteryGrid(t *testing.T) {
	in := slotIntegrals{
		LoadKWh:       1.0,
		PVKWh:         2.0,
		BatChargedKWh: 0.7,
		// no discharge, no import, export = surplus − to_bat = 0.3
	}
	f := decomposeFlows(in)
	approx(t, "self", f.SelfConsumptionKWh, 1.0, 0.001)
	approx(t, "pv→bat", f.PVToBatKWh, 0.7, 0.001)
	approx(t, "direct_export", f.DirectExportKWh, 0.3, 0.001)
	approx(t, "grid→bat", f.GridToBatKWh, 0, 0.001)
	// Identity: pv = self + pv→bat + direct_export
	approx(t, "pv identity", f.SelfConsumptionKWh+f.PVToBatKWh+f.DirectExportKWh, 2.0, 0.001)
}

// Decomposition: battery covers load, no PV, no grid.
func TestDecompose_BatteryCoversLoad(t *testing.T) {
	in := slotIntegrals{
		LoadKWh:          1.0,
		BatDischargedKWh: 1.0,
	}
	f := decomposeFlows(in)
	approx(t, "bat→home", f.BatToHomeKWh, 1.0, 0.001)
	approx(t, "self", f.SelfConsumptionKWh, 0, 0.001)
	approx(t, "grid→home", f.GridToHomeKWh, 0, 0.001)
	approx(t, "bat→grid", f.BatToGridKWh, 0, 0.001)
}

// Decomposition: arbitrage charging (battery charged from grid, no PV).
func TestDecompose_ArbitrageCharging(t *testing.T) {
	in := slotIntegrals{
		LoadKWh:       0.0,
		PVKWh:         0.0,
		BatChargedKWh: 1.0,
	}
	f := decomposeFlows(in)
	approx(t, "grid→bat", f.GridToBatKWh, 1.0, 0.001)
	approx(t, "pv→bat", f.PVToBatKWh, 0, 0.001)
	approx(t, "self", f.SelfConsumptionKWh, 0, 0.001)
}

// Decomposition: battery flips direction within the slot — both PV→bat
// (during charge phase) and bat→grid (during discharge phase) can coexist.
func TestDecompose_BatteryFlipsDirection(t *testing.T) {
	in := slotIntegrals{
		LoadKWh:          0.5,
		PVKWh:            1.5,
		BatChargedKWh:    0.6, // earlier in slot when PV > load
		BatDischargedKWh: 0.3, // later in slot, sold to grid
	}
	f := decomposeFlows(in)
	approx(t, "self", f.SelfConsumptionKWh, 0.5, 0.001)
	// pvSurplus = 1.0; pv→bat = min(1.0, 0.6) = 0.6; direct_export = 0.4
	approx(t, "pv→bat", f.PVToBatKWh, 0.6, 0.001)
	approx(t, "direct_export", f.DirectExportKWh, 0.4, 0.001)
	// loadDeficit = 0; bat→home = 0; bat→grid = 0.3
	approx(t, "bat→home", f.BatToHomeKWh, 0, 0.001)
	approx(t, "bat→grid", f.BatToGridKWh, 0.3, 0.001)
}
