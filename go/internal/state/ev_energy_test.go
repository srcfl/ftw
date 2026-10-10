package state

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func closeEnough(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-4 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func hourBucket(asset, label string, start int64, wh float64) EVChargeSample {
	return EVChargeSample{AssetID: asset, Label: label, StartMs: start, LenMs: time.Hour.Milliseconds(), EnergyWh: wh}
}

func TestAggregateEVChargingSplitsLoadpointsAndPricesGridShare(t *testing.T) {
	start := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC).UnixMilli()
	day := []EVDay{{
		Day: "2026-06-02", StartMs: start, EndMs: start + time.Hour.Milliseconds(),
	}}
	samples := []EVChargeSample{
		hourBucket("charger/garage", "easee", start, 6000),
		hourBucket("charger/driveway", "zaptec", start, 2000),
	}
	site := []LedgerFlowBucket{{
		StartMs: start, LenMs: time.Hour.Milliseconds(),
		ImportWh: 4000, LoadWh: 2000, PVWh: 6000,
	}}
	slots := []CostSlot{{
		StartMs: start, EndMs: start + time.Hour.Milliseconds(), ImportOreKwh: 200,
	}}

	got := AggregateEVCharging(samples, site, slots, day)
	// Demand is 2 kWh home + 8 kWh EV. Import is 4 kWh, so 40% of each
	// charger is grid. 200 öre/kWh on that share.
	closeEnough(t, got.Total.EnergyWh, 8000)
	closeEnough(t, got.Total.GridWh, 3200)
	closeEnough(t, got.Total.OnsiteWh, 4800)
	closeEnough(t, got.Total.CostOre, 640)
	if got.Total.CostPartial() {
		t.Fatal("fully priced window marked partial")
	}
	if got.Attribution != EVAttributionSiteMix {
		t.Fatalf("attribution %q", got.Attribution)
	}
	if len(got.Chargers) != 2 {
		t.Fatalf("chargers %+v", got.Chargers)
	}
	closeEnough(t, got.Chargers[0].EnergyWh, 2000) // driveway sorts first
	closeEnough(t, got.Chargers[0].CostOre, 160)
	closeEnough(t, got.Chargers[1].EnergyWh, 6000)
	closeEnough(t, got.Chargers[1].CostOre, 480)
	if len(got.Daily) != 1 || len(got.Daily[0].Chargers) != 2 {
		t.Fatalf("daily %+v", got.Daily)
	}
}

func TestAggregateEVChargingKeepsEnergyWhenPriceIsMissing(t *testing.T) {
	start := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC).UnixMilli()
	hour := time.Hour.Milliseconds()
	day := []EVDay{{Day: "2026-06-02", StartMs: start, EndMs: start + 2*hour}}
	samples := []EVChargeSample{
		hourBucket("charger/garage", "easee", start, 1000),
		hourBucket("charger/garage", "easee", start+hour, 1000),
	}
	slots := []CostSlot{{StartMs: start, EndMs: start + hour, ImportOreKwh: 100}}

	got := AggregateEVCharging(samples, nil, slots, day)
	closeEnough(t, got.Total.EnergyWh, 2000)
	closeEnough(t, got.Total.GridWh, 2000)
	closeEnough(t, got.Total.CostOre, 100)
	closeEnough(t, got.Total.UnpricedWh, 1000)
	if !got.Total.CostPartial() {
		t.Fatal("price gap was not marked partial")
	}
	if got.Attribution != EVAttributionImportPrice {
		t.Fatalf("attribution %q, want import price when the site meter is absent", got.Attribution)
	}
	closeEnough(t, got.Daily[0].Total.EnergyWh, 2000)
}

func TestAggregateEVChargingPricesOnlyTheCoveredHalfOfABucket(t *testing.T) {
	start := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC).UnixMilli()
	hour := time.Hour.Milliseconds()
	day := []EVDay{{Day: "2026-06-02", StartMs: start, EndMs: start + hour}}
	samples := []EVChargeSample{hourBucket("charger/garage", "easee", start, 1000)}
	slots := []CostSlot{{StartMs: start, EndMs: start + hour/2, ImportOreKwh: 200}}

	got := AggregateEVCharging(samples, nil, slots, day)
	closeEnough(t, got.Total.EnergyWh, 1000)
	closeEnough(t, got.Total.CostOre, 100)
	closeEnough(t, got.Total.UnpricedWh, 500)
	if !got.Total.CostPartial() {
		t.Fatal("half-covered bucket was not partial")
	}
}

func TestAggregateEVChargingGivesSolarSurplusAZeroCashCost(t *testing.T) {
	start := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC).UnixMilli()
	hour := time.Hour.Milliseconds()
	day := []EVDay{{Day: "2026-06-02", StartMs: start, EndMs: start + hour}}
	samples := []EVChargeSample{hourBucket("charger/garage", "easee", start, 5000)}
	site := []LedgerFlowBucket{{
		StartMs: start, LenMs: hour, ExportWh: 2000, PVWh: 7000,
	}}
	// No price at all. The car did not import, so the missing price does
	// not make the cash cost partial.
	got := AggregateEVCharging(samples, site, nil, day)
	closeEnough(t, got.Total.EnergyWh, 5000)
	closeEnough(t, got.Total.GridWh, 0)
	closeEnough(t, got.Total.OnsiteWh, 5000)
	closeEnough(t, got.Total.CostOre, 0)
	closeEnough(t, got.Total.UnpricedWh, 0)
	if got.Total.CostPartial() {
		t.Fatal("onsite charging with no import was marked partial")
	}
}

func TestAggregateEVChargingAssignsDSTDaysWithoutDoubleCounting(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, m time.Month, d, hh, mm int) int64 {
		return time.Date(y, m, d, hh, mm, 0, 0, loc).UnixMilli()
	}

	springStart := time.Date(2026, 3, 29, 0, 0, 0, 0, loc)
	springEnd := time.Date(2026, 3, 30, 0, 0, 0, 0, loc)
	if springEnd.Sub(springStart) != 23*time.Hour {
		t.Fatalf("spring-forward day length %s, want 23h", springEnd.Sub(springStart))
	}
	spring := []EVDay{{
		Day: "2026-03-29", StartMs: springStart.UnixMilli(), EndMs: springEnd.UnixMilli(),
	}}
	springSamples := []EVChargeSample{
		hourBucket("charger/garage", "easee", at(2026, 3, 28, 23, 0), 100),
		hourBucket("charger/garage", "easee", at(2026, 3, 29, 1, 0), 200),
		hourBucket("charger/garage", "easee", at(2026, 3, 29, 3, 0), 300),
		hourBucket("charger/driveway", "zaptec", at(2026, 3, 29, 3, 0), 50),
		hourBucket("charger/garage", "easee", at(2026, 3, 30, 0, 0), 400),
	}
	springSlots := []CostSlot{{
		StartMs: springStart.UnixMilli(), EndMs: springEnd.UnixMilli(), ImportOreKwh: 100,
	}}
	got := AggregateEVCharging(springSamples, nil, springSlots, spring)
	closeEnough(t, got.Total.EnergyWh, 550)
	closeEnough(t, got.Total.CostOre, 55)
	if len(got.Chargers) != 2 {
		t.Fatalf("spring chargers %+v", got.Chargers)
	}
	closeEnough(t, got.Chargers[1].EnergyWh, 500)

	fallStart := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	fallEnd := time.Date(2026, 10, 26, 0, 0, 0, 0, loc)
	if fallEnd.Sub(fallStart) != 25*time.Hour {
		t.Fatalf("fall-back day length %s, want 25h", fallEnd.Sub(fallStart))
	}
	// 02:30 happens twice. Build both from UTC so the choice does not depend
	// on which ambiguous local time time.Date returns.
	firstHalf := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC).UnixMilli()  // 02:30 CEST
	secondHalf := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC).UnixMilli() // 02:30 CET
	for _, ms := range []int64{firstHalf, secondHalf} {
		local := time.UnixMilli(ms).In(loc)
		if local.Hour() != 2 || local.Minute() != 30 || local.Day() != 25 {
			t.Fatalf("repeated hour rendered as %s", local)
		}
	}
	fall := []EVDay{{Day: "2026-10-25", StartMs: fallStart.UnixMilli(), EndMs: fallEnd.UnixMilli()}}
	fallSamples := []EVChargeSample{
		{AssetID: "charger/garage", Label: "easee", StartMs: firstHalf, LenMs: 5 * time.Minute.Milliseconds(), EnergyWh: 100},
		{AssetID: "charger/garage", Label: "easee", StartMs: secondHalf, LenMs: 5 * time.Minute.Milliseconds(), EnergyWh: 250},
		{AssetID: "charger/driveway", Label: "zaptec", StartMs: secondHalf, LenMs: 5 * time.Minute.Milliseconds(), EnergyWh: 25},
		hourBucket("charger/garage", "easee", at(2026, 10, 26, 0, 30), 900),
	}
	// Price only the first occurrence. The repeated hour and the next day stay unpriced.
	fallSlots := []CostSlot{{
		StartMs: firstHalf, EndMs: firstHalf + 5*time.Minute.Milliseconds(), ImportOreKwh: 50,
	}}
	fallGot := AggregateEVCharging(fallSamples, nil, fallSlots, fall)
	closeEnough(t, fallGot.Total.EnergyWh, 375)
	closeEnough(t, fallGot.Total.CostOre, 5)
	closeEnough(t, fallGot.Total.UnpricedWh, 275)
	if !fallGot.Total.CostPartial() {
		t.Fatal("unpriced repeated hour was not partial")
	}
	closeEnough(t, fallGot.Chargers[0].EnergyWh, 25)
	closeEnough(t, fallGot.Chargers[1].EnergyWh, 350)
}

func TestLocalDaysUseCivilMidnightsAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 30, 1, 0, 0, 0, loc)
	days := LocalDays(now, 2)
	if len(days) != 2 || days[0].Day != "2026-03-29" || days[1].Day != "2026-03-30" {
		t.Fatalf("days %+v", days)
	}
	if time.UnixMilli(days[0].EndMs).Sub(time.UnixMilli(days[0].StartMs)) != 23*time.Hour {
		t.Fatalf("completed spring day is %s", time.UnixMilli(days[0].EndMs).Sub(time.UnixMilli(days[0].StartMs)))
	}
	if days[1].EndMs != now.UnixMilli() {
		t.Fatalf("today ended at %d, want now", days[1].EndMs)
	}
}

func TestEVEnergySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC).UnixMilli()
	hour := time.Hour.Milliseconds()
	insert := func(asset, label string, at int64, wh float64) {
		t.Helper()
		if _, err := s.history.Exec(`INSERT INTO energy_assets(asset_id, device_id, kind, label, read_only, first_seen_ms, last_seen_ms)
			VALUES (?, ?, ?, ?, 0, ?, ?)`, asset, asset, AssetVehicleCharger, label, at, at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.history.Exec(`INSERT INTO energy_ledger_entries(
			schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, energy_wh,
			source, quality, provenance, sample_count, observed_at_ms)
			VALUES (1, ?, ?, ?, ?, ?, 'hardware_counter', 'measured', 'counter', 1, ?)`,
			asset, FlowVehicleCharge, at, hour, wh, at+hour); err != nil {
			t.Fatal(err)
		}
	}
	insert("charger/garage", "easee", start, 6000)
	insert("charger/driveway", "zaptec", start, 2000)
	if _, err := s.history.Exec(`INSERT INTO energy_ledger_entries(
		schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, energy_wh,
		source, quality, provenance, sample_count, observed_at_ms)
		VALUES (1, 'site/meter', ?, ?, ?, 4000, 'hardware_counter', 'measured', 'counter', 1, ?)`,
		FlowGridImport, start, hour, start+hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`INSERT INTO energy_ledger_entries(
		schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, energy_wh,
		source, quality, provenance, sample_count, observed_at_ms)
		VALUES (1, 'site/load', ?, ?, ?, 2000, 'hardware_counter', 'measured', 'counter', 1, ?)`,
		FlowConsumerUse, start, hour, start+hour); err != nil {
		t.Fatal(err)
	}
	// Second hour has energy and no price.
	insertGap := start + hour
	if _, err := s.history.Exec(`INSERT INTO energy_ledger_entries(
		schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, energy_wh,
		source, quality, provenance, sample_count, observed_at_ms)
		VALUES (1, 'charger/garage', ?, ?, ?, 1000, 'hardware_counter', 'measured', 'counter', 1, ?)`,
		FlowVehicleCharge, insertGap, hour, insertGap+hour); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePrices([]PricePoint{{
		Zone: "SE3", SlotTsMs: start, SlotLenMin: 60, SpotOreKwh: 80, TotalOreKwh: 200,
		Source: "test", FetchedAtMs: start,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	days := []EVDay{{Day: "2026-06-02", StartMs: start, EndMs: start + 2*hour}}
	samples, err := reopened.LoadEVChargeSamples(context.Background(), days[0].StartMs, days[0].EndMs)
	if err != nil {
		t.Fatal(err)
	}
	site, err := reopened.LedgerFlowBuckets(context.Background(), days[0].StartMs, days[0].EndMs)
	if err != nil {
		t.Fatal(err)
	}
	slots, err := reopened.CostSlots(context.Background(), "SE3", days[0].StartMs, days[0].EndMs)
	if err != nil {
		t.Fatal(err)
	}
	got := AggregateEVCharging(samples, site, slots, days)
	closeEnough(t, got.Total.EnergyWh, 9000)
	// First hour: 8 kWh EV, 4 kWh import, 2 kWh load → 40% grid → 3.2 kWh at 200 öre.
	// Second hour: 1 kWh with no site split and no price.
	closeEnough(t, got.Total.CostOre, 640)
	closeEnough(t, got.Total.UnpricedWh, 1000)
	if !got.Total.CostPartial() {
		t.Fatal("restarted window lost the partial-cost flag")
	}
	if got.Attribution != EVAttributionMixed {
		t.Fatalf("attribution %q", got.Attribution)
	}
	closeEnough(t, got.Chargers[0].EnergyWh, 2000)
	closeEnough(t, got.Chargers[1].EnergyWh, 7000)
}
