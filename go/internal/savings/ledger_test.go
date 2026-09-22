package savings

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/gridcost"
)

func TestLedgerSolarCoversLoad(t *testing.T) {
	slots := []PriceSlot{{StartMs: 0, EndMs: 3_600_000, ImportOreKwh: 100, SpotOreKwh: 80}}
	buckets := []FlowBucket{{StartMs: 0, LenMs: 3_600_000, PVWh: 1000, LoadWh: 1000}}
	days := EvaluateLedger(buckets, slots, []DayWindow{{StartMs: 0, EndMs: 3_600_000}}, Battery{}, gridcost.ExportPricing{})
	d := days[0]
	if d.NoPVCostOre < 99 || d.NoPVCostOre > 101 {
		t.Fatalf("no-pv cost %v", d.NoPVCostOre)
	}
	if d.NoPVSavedOre() < 99 || d.NoPVSavedOre() > 101 {
		t.Fatalf("saved vs no pv %v", d.NoPVSavedOre())
	}
	if d.SelfSavedOre() > 0.01 || d.SelfSavedOre() < -0.01 {
		t.Fatalf("self-consumption saved %v, want 0", d.SelfSavedOre())
	}
}

func TestCheapChargeBeatsEmptySelfConsumption(t *testing.T) {
	// Actual bought 1 kWh at 10 öre and served a later 1 kWh load with it.
	// Self-consumption has no solar and starts empty, so it buys that load at 200.
	slots := []PriceSlot{
		{StartMs: 0, EndMs: 3_600_000, ImportOreKwh: 10, SpotOreKwh: 10},
		{StartMs: 3_600_000, EndMs: 7_200_000, ImportOreKwh: 200, SpotOreKwh: 150},
	}
	buckets := []FlowBucket{
		{StartMs: 0, LenMs: 3_600_000, ImportWh: 1000},
		{StartMs: 3_600_000, LenMs: 3_600_000, LoadWh: 1000},
	}
	bat := Battery{CapacityWh: 1000, MaxChargeW: 5000, MaxDischargeW: 5000}
	days := EvaluateLedger(buckets, slots, []DayWindow{{StartMs: 0, EndMs: 7_200_000}}, bat, gridcost.ExportPricing{})
	d := days[0]
	actual := d.ImportCostOre - d.ExportRevenueOre
	if actual < 9 || actual > 11 {
		t.Fatalf("actual %v", actual)
	}
	if d.SelfCostOre < 199 || d.SelfCostOre > 201 {
		t.Fatalf("self cost %v", d.SelfCostOre)
	}
	if d.SelfSavedOre() < 189 || d.SelfSavedOre() > 191 {
		t.Fatalf("saved vs self %v", d.SelfSavedOre())
	}
	if d.NoPVSavedOre() < 189 || d.NoPVSavedOre() > 191 {
		t.Fatalf("saved vs no pv %v", d.NoPVSavedOre())
	}
}
