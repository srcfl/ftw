package mpc

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWeekdayPeakHoursSkipWeekendAndHonorTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	// Friday 2026-09-04 20:00 through Monday 2026-09-07 08:00 local.
	from := time.Date(2026, 9, 4, 20, 0, 0, 0, loc)
	to := time.Date(2026, 9, 7, 8, 0, 0, 0, loc)
	hours := weekdayPeakClockHours(loc, from, to)
	if len(hours) != 2 {
		t.Fatalf("hours=%d, want Friday none + Monday 06–08 = 2, got %+v", len(hours), hours)
	}
	if hours[0].start != time.Date(2026, 9, 7, 6, 0, 0, 0, loc) || hours[1].start != time.Date(2026, 9, 7, 7, 0, 0, 0, loc) {
		t.Fatalf("unexpected hours: %+v", hours)
	}
}

func TestBindDemandChargesExpandsWeekdayHoursAndElapsed(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 10, 7, 0, 0, loc) // Wednesday
	start := time.Date(2026, 9, 9, 10, 0, 0, 0, loc)
	slots := []Slot{
		{StartMs: start.UnixMilli(), ExecutionStartMs: now.UnixMilli(), LenMin: 15, PriceOre: 100},
		{StartMs: start.Add(15 * time.Minute).UnixMilli(), LenMin: 15, PriceOre: 100},
		{StartMs: start.Add(30 * time.Minute).UnixMilli(), LenMin: 15, PriceOre: 100},
		{StartMs: start.Add(45 * time.Minute).UnixMilli(), LenMin: 15, PriceOre: 100},
		{StartMs: start.Add(time.Hour).UnixMilli(), LenMin: 60, PriceOre: 80},
	}
	importWh := func(intervals [][2]int64) ([]float64, []int64) {
		wh := make([]float64, len(intervals))
		cov := make([]int64, len(intervals))
		for i, window := range intervals {
			cov[i] = window[1] - window[0]
			if window[1]-window[0] == 7*time.Minute.Milliseconds() {
				wh[i] = 500 // 0.5 kWh in the elapsed 10:00–10:07
			} else {
				wh[i] = 3000 // 3 kWh → 3 kW over a full hour
			}
		}
		return wh, cov
	}
	all := bindDemandCharges(slots, 7000, 3, 25, 0, loc, now, importWh, HourImport{})
	// Today's completed peak, 06:00 (the first of equal hours), joins
	// today's group for a worker that announces demand_elapsed_hours.
	if len(all) != 1 || len(all[0].Hours) < 2 || !all[0].Hours[0].Elapsed ||
		all[0].Hours[0].StartMs != start.Add(-4*time.Hour).UnixMilli() || all[0].Hours[0].ElapsedImportKWh != 3 ||
		all[0].Hours[1].Elapsed {
		t.Fatalf("charges=%+v", all)
	}
	// The rest of this test is the shape every worker takes.
	got := legacyDemandCharges(all)
	if len(got) != 1 || got[0].ID != "weekday-high-2026-09" || got[0].TopN != 3 {
		t.Fatalf("charges=%+v", got)
	}
	if abs := got[0].PricePerKW - 8750; abs > 1e-9 {
		t.Fatalf("price_per_kw=%g, want 8750 after 25%% VAT", got[0].PricePerKW)
	}
	if len(got[0].Hours) < 2 {
		t.Fatalf("hours=%+v", got[0].Hours)
	}
	if got[0].Hours[0].StartMs != start.UnixMilli() || got[0].Hours[0].EndMs != start.Add(time.Hour).UnixMilli() {
		t.Fatalf("first hour=%+v", got[0].Hours[0])
	}
	if got[0].Hours[0].ElapsedImportKWh != 0.5 {
		t.Fatalf("elapsed=%g, want 0.5 kWh", got[0].Hours[0].ElapsedImportKWh)
	}
	day := start.Format("2006-01-02")
	for i, hour := range got[0].Hours {
		if hour.Group != day {
			t.Fatalf("hour %d group=%q, want %q", i, hour.Group, day)
		}
	}
	if len(got[0].AlreadyKW) == 0 {
		t.Fatal("expected already_kw from earlier weekday hours this month")
	}
	for _, kw := range got[0].AlreadyKW {
		if kw < 2.9 || kw > 3.1 {
			t.Fatalf("already_kw=%v", got[0].AlreadyKW)
		}
	}
}

func TestBindDemandChargesGroupsHoursByLocalDay(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 6, 0, 0, 0, loc)
	slots := []Slot{
		{StartMs: now.UnixMilli(), LenMin: 60, PriceOre: 100},
		{StartMs: now.Add(time.Hour).UnixMilli(), LenMin: 60, PriceOre: 100},
		{StartMs: time.Date(2026, 9, 10, 6, 0, 0, 0, loc).UnixMilli(), LenMin: 60, PriceOre: 100},
	}
	got := bindDemandCharges(slots, 7000, 3, 0, 0, loc, now, nil, HourImport{})
	if len(got) != 1 || len(got[0].Hours) < 3 {
		t.Fatalf("charges=%+v", got)
	}
	groups := map[string]int{}
	for _, hour := range got[0].Hours {
		groups[hour.Group]++
	}
	if groups["2026-09-09"] < 2 {
		t.Fatalf("want at least two hours on 2026-09-09, got %v", groups)
	}
	if groups["2026-09-10"] < 1 {
		t.Fatalf("want Thursday hours grouped separately, got %v", groups)
	}
}

func TestEllevioNightWeightIncludesWeekendsAndHalvesNightHours(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, loc) // Saturday
	slots := []Slot{
		{StartMs: now.UnixMilli(), LenMin: 60, PriceOre: 100},
		{StartMs: time.Date(2026, 9, 5, 22, 0, 0, 0, loc).UnixMilli(), LenMin: 60, PriceOre: 80},
		{StartMs: time.Date(2026, 9, 5, 23, 0, 0, 0, loc).UnixMilli(), LenMin: 60, PriceOre: 80},
	}
	without := bindDemandCharges(slots, 7000, 3, 0, 0, loc, now, nil, HourImport{})
	if len(without) != 0 {
		t.Fatalf("weekday-only window should skip Saturday, got %+v", without)
	}
	got := bindDemandCharges(slots, 7000, 3, 0, 0.5, loc, now, nil, HourImport{})
	if len(got) != 1 || len(got[0].Hours) < 3 {
		t.Fatalf("ellevio hours=%+v", got)
	}
	var day, night []DemandHour
	for _, hour := range got[0].Hours {
		start := time.UnixMilli(hour.StartMs).In(loc)
		if start.Hour() == 10 {
			day = append(day, hour)
		}
		if start.Hour() == 22 || start.Hour() == 23 {
			night = append(night, hour)
		}
	}
	if len(day) != 1 || day[0].Weight != 0 {
		t.Fatalf("day hour should omit default weight 1, got %+v", day)
	}
	if len(night) != 2 {
		t.Fatalf("night hours=%+v", night)
	}
	for _, hour := range night {
		if hour.Weight != 0.5 {
			t.Fatalf("night weight=%g, want 0.5, hour=%+v", hour.Weight, hour)
		}
		if hour.Group != "2026-09-05" {
			t.Fatalf("night group=%q", hour.Group)
		}
	}
}

// Past hours this month follow the same tariff rule as planned hours. An EV
// that drew 8 kW at 02:00 set a 4 kW Ellevio peak, not 8 kW, and several hot
// hours on one day count as that day's one peak.
func TestEllevioAlreadyKWWeighsNightsAndKeepsOnePeakPerDay(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 10, 7, 0, 0, loc)
	start := time.Date(2026, 9, 9, 10, 0, 0, 0, loc)
	slots := []Slot{
		{StartMs: start.UnixMilli(), ExecutionStartMs: now.UnixMilli(), LenMin: 60, PriceOre: 100},
		{StartMs: start.Add(time.Hour).UnixMilli(), LenMin: 60, PriceOre: 100},
	}
	evNight := time.Date(2026, 9, 8, 2, 0, 0, 0, loc).UnixMilli()
	importWh := func(intervals [][2]int64) ([]float64, []int64) {
		wh := make([]float64, len(intervals))
		cov := make([]int64, len(intervals))
		for i, window := range intervals {
			cov[i] = window[1] - window[0]
			switch {
			case window[0] == evNight:
				wh[i] = 8000
			case window[0] == evNight+time.Hour.Milliseconds():
				wh[i] = 7000
			default:
				wh[i] = 1000
			}
		}
		return wh, cov
	}
	got := bindDemandCharges(slots, 7000, 3, 0, 0.5, loc, now, importWh, HourImport{})
	if len(got) != 1 {
		t.Fatalf("charges=%+v", got)
	}
	// Today is planned: its completed peak is an hour of its group, and
	// already_kw holds 1-8 September. The earlier shape holds all nine.
	if len(got[0].AlreadyKW) != 8 || !got[0].Hours[0].Elapsed {
		t.Fatalf("already_kw=%v hours=%+v", got[0].AlreadyKW, got[0].Hours)
	}
	already := legacyDemandCharges(got)[0].AlreadyKW
	if len(already) != 9 {
		t.Fatalf("already_kw=%v, want one peak for each of 1–9 September", already)
	}
	for i, kw := range already {
		want := 1.0
		if i == 7 { // 8 September
			want = 4
		}
		if kw != want {
			t.Fatalf("already_kw=%v, want 4 kW on 8 September and 1 kW on other days", already)
		}
	}
}

// At 10:17 with slot 0 at 10:15 the clock hour 10-11 is in progress: it
// goes out with its import since 10:00, for a worker that announces
// demand_elapsed_hours only.
func TestBindDemandChargesSendsTheRunningHourBeforeTheFirstSlot(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 9, 10, 17, 0, 0, loc)
	start := time.Date(2026, 9, 9, 10, 15, 0, 0, loc)
	hour := time.Date(2026, 9, 9, 10, 0, 0, 0, loc)
	slots := []Slot{
		{StartMs: start.UnixMilli(), ExecutionStartMs: now.UnixMilli(), LenMin: 15, LoadW: 1200},
		{StartMs: start.Add(15 * time.Minute).UnixMilli(), LenMin: 45},
		{StartMs: start.Add(time.Hour).UnixMilli(), LenMin: 60},
	}
	running := [2]int64{hour.UnixMilli(), now.UnixMilli()}
	importWh := func(intervals [][2]int64) ([]float64, []int64) {
		wh, cov := make([]float64, len(intervals)), make([]int64, len(intervals))
		for i, window := range intervals {
			if window == running {
				wh[i], cov[i] = 850, window[1]-window[0]
			}
		}
		return wh, cov
	}
	got := bindDemandCharges(slots, 7000, 1, 0, 0, loc, now, importWh, HourImport{})
	if len(got) != 1 || len(got[0].Hours) != 2 {
		t.Fatalf("charges=%+v", got)
	}
	first := got[0].Hours[0]
	if first.StartMs != hour.UnixMilli() || first.EndMs != hour.Add(time.Hour).UnixMilli() || !first.Elapsed ||
		first.ElapsedImportKWh != 0.85 || first.Group != "2026-09-09" {
		t.Fatalf("running hour=%+v; want 10:00-11:00 with 0.85 kWh, for demand_elapsed_hours", first)
	}
	if got[0].Hours[1].Elapsed {
		t.Fatalf("11:00 is a planned hour for every worker: %+v", got[0].Hours[1])
	}
	legacy := legacyDemandCharges(got)
	if len(legacy) != 1 || len(legacy[0].Hours) != 1 || legacy[0].Hours[0].StartMs != start.Add(45*time.Minute).UnixMilli() {
		t.Fatalf("a worker without the feature gets the hours from 11:00, got %+v", legacy)
	}
}

// The hour in progress counts Core's live import counter when it measures
// that hour, else the history. Time neither covers counts at the larger of
// the covered mean and slot 0's forecast import, never at zero.
func TestRunningHourImportCountsUncoveredTime(t *testing.T) {
	hour := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	window := [2]int64{hour.UnixMilli(), hour.Add(40 * time.Minute).UnixMilli()}
	twenty := 20 * time.Minute
	for _, c := range []struct {
		name           string
		historyWh      float64
		historyCovered time.Duration
		live           HourImport
		slot0W, wantWh float64
	}{
		{"live counter, fully covered", 100, twenty, HourImport{Start: hour, ImportedWh: 2000, Covered: 40 * time.Minute}, 500, 2000},
		{"live counter, half covered at 3 kW", 100, twenty, HourImport{Start: hour, ImportedWh: 1000, Covered: twenty}, 500, 2000},
		{"live counter of another hour", 600, twenty, HourImport{Start: hour.Add(-time.Hour), ImportedWh: 9000, Covered: time.Hour}, 500, 1200},
		{"history half covered, forecast above its mean", 600, twenty, HourImport{}, 2400, 1400},
		{"no record at all", 0, 0, HourImport{}, 1500, 1000},
		{"no record and no house import", 0, 0, HourImport{}, 0, 0},
	} {
		got := runningHourImportWh(window, c.historyWh, c.historyCovered.Milliseconds(), c.live, c.slot0W)
		if math.Abs(got-c.wantWh) > 1e-9 {
			t.Errorf("%s: %g Wh, want %g", c.name, got, c.wantWh)
		}
	}
}

func weekdayEvening(t *testing.T, history bool) ([]Slot, func([][2]int64) ([]float64, []int64), time.Time) {
	t.Helper()
	loc := time.UTC
	now := time.Date(2026, 9, 9, 19, 20, 0, 0, loc) // Wednesday
	start := time.Date(2026, 9, 9, 19, 15, 0, 0, loc)
	slots := []Slot{
		{StartMs: start.UnixMilli(), ExecutionStartMs: now.UnixMilli(), LenMin: 15, LoadW: 900, PriceOre: 100},
		{StartMs: start.Add(15 * time.Minute).UnixMilli(), LenMin: 30, LoadW: 900, PriceOre: 100},
	}
	if !history {
		return slots, nil, now
	}
	// Every completed hour imported 1 kWh plus 0.1 kWh per hour of the day,
	// so other days peak at 19:00 (2.9 kWh) and today at 18:00 (2.8 kWh);
	// 19:00-19:20 imported 0.7 kWh.
	importWh := func(intervals [][2]int64) ([]float64, []int64) {
		wh, cov := make([]float64, len(intervals)), make([]int64, len(intervals))
		for i, window := range intervals {
			cov[i] = window[1] - window[0]
			if cov[i] == time.Hour.Milliseconds() {
				wh[i] = 1000 + 100*float64(time.UnixMilli(window[0]).UTC().Hour())
			} else {
				wh[i] = 700
			}
		}
		return wh, cov
	}
	return slots, importWh, now
}

// At 19:20 on a weekday the hour in progress is the day's last tariff hour.
// Today's group holds it and today's completed peak, so today is one
// sample and already_kw keeps the other days only. A worker without
// demand_elapsed_hours gets exactly the request it got before.
func TestBindDemandChargesPutsTodaysPeakInTodaysGroup(t *testing.T) {
	slots, importWh, now := weekdayEvening(t, true)
	got := bindDemandCharges(slots, 7000, 3, 0, 0, time.UTC, now, importWh, HourImport{})
	if len(got) != 1 {
		t.Fatalf("charges=%+v", got)
	}
	peak := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	want := []DemandHour{
		{StartMs: peak.UnixMilli(), EndMs: peak.Add(time.Hour).UnixMilli(), ElapsedImportKWh: 2.8, Group: "2026-09-09", Elapsed: true},
		{StartMs: peak.Add(time.Hour).UnixMilli(), EndMs: peak.Add(2 * time.Hour).UnixMilli(), ElapsedImportKWh: 0.7, Group: "2026-09-09", Elapsed: true},
	}
	if !reflect.DeepEqual(got[0].Hours, want) {
		t.Fatalf("hours=%+v\nwant %+v", got[0].Hours, want)
	}
	// 1-8 September hold six weekdays at 2.9 kW; today is in its group instead.
	if len(got[0].AlreadyKW) != 6 || len(got[0].LegacyAlreadyKW) != 7 {
		t.Fatalf("already_kw=%v legacy=%v", got[0].AlreadyKW, got[0].LegacyAlreadyKW)
	}
	legacy, err := json.Marshal(externalDemandCharges(legacyDemandCharges(got)))
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"id":"weekday-high-2026-09","price_per_kw":7000,"top_n":3,"already_kw":[2.9,2.9,2.9,2.9,2.9,2.9,2.8]}]`; string(legacy) != want {
		t.Fatalf("legacy demand_charges %s\nwant %s", legacy, want)
	}
}

// At 23:20 on the last day of a month under a tariff with night hours, the
// hour in progress is the month's last, so the new shape starts with that
// month. A worker without demand_elapsed_hours still gets the charges in
// their earlier order: months with planned hours first, then a month with
// completed days only. The expected JSON is what Core sent before.
func TestLegacyDemandChargesKeepTheirOrderAtTheEndOfAMonth(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 20, 0, 0, time.UTC)
	start := time.Date(2026, 10, 31, 23, 15, 0, 0, time.UTC)
	slots := []Slot{{StartMs: start.UnixMilli(), ExecutionStartMs: now.UnixMilli(), LenMin: 45, LoadW: 900, PriceOre: 100}}
	for h := range 3 {
		slots = append(slots, Slot{StartMs: start.Add(45*time.Minute + time.Duration(h)*time.Hour).UnixMilli(), LenMin: 60, LoadW: 900, PriceOre: 100})
	}
	importWh := func(intervals [][2]int64) ([]float64, []int64) {
		wh, cov := make([]float64, len(intervals)), make([]int64, len(intervals))
		for i, window := range intervals {
			cov[i] = window[1] - window[0]
			if cov[i] == time.Hour.Milliseconds() {
				wh[i] = 1000 + 100*float64(time.UnixMilli(window[0]).UTC().Hour())
			} else {
				wh[i] = 700
			}
		}
		return wh, cov
	}
	got := bindDemandCharges(slots, 5200, 3, 25, 0.5, time.UTC, now, importWh, HourImport{})
	if len(got) != 2 || got[0].ID != "weekday-high-2026-10" || len(got[0].Hours) != 2 || len(got[0].AlreadyKW) != 30 {
		t.Fatalf("charges=%+v; want October first, with today's peak and the hour in progress", got)
	}
	legacy, err := json.Marshal(externalDemandCharges(legacyDemandCharges(got)))
	if err != nil {
		t.Fatal(err)
	}
	november := `{"id":"weekday-high-2026-11","price_per_kw":6500,"top_n":3,"hours":[` +
		`{"start_ms":1793491200000,"end_ms":1793494800000,"weight":0.5,"group":"2026-11-01"},` +
		`{"start_ms":1793494800000,"end_ms":1793498400000,"weight":0.5,"group":"2026-11-01"},` +
		`{"start_ms":1793498400000,"end_ms":1793502000000,"weight":0.5,"group":"2026-11-01"}]}`
	october := `{"id":"weekday-high-2026-10","price_per_kw":6500,"top_n":3,"already_kw":[` +
		strings.TrimSuffix(strings.Repeat("3.1,", 31), ",") + `]}`
	if want := "[" + november + "," + october + "]"; string(legacy) != want {
		t.Fatalf("legacy demand_charges %s\nwant %s", legacy, want)
	}
}

type demandShapeTransport struct{ elapsed bool }

func (d *demandShapeTransport) RoundTrip(context.Context, []byte) ([]byte, error) {
	features := []string{"champion", "ev_duty", "published_prices", "demand_charges"}
	if d.elapsed {
		features = append(features, "demand_elapsed_hours")
	}
	return json.Marshal(map[string]any{"name": "ftw-solver", "version": "test", "protocol_version": 1, "features": features})
}
func (*demandShapeTransport) Close() error { return nil }

// Core sends the hour in progress and today's completed peak only to a
// worker that announces demand_elapsed_hours. Any other worker gets the
// earlier request, byte for byte, with the budget of the charges it gets.
func TestEnergyplanRequestNegotiatesElapsedDemandHours(t *testing.T) {
	transport := &demandShapeTransport{}
	external := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: 7 * time.Second}, transport: transport, timeBudget: energyplanTimeBudget}
	o := &EnergyplanOptimizer{ExternalOptimizer: external}
	_, fixture := externalTestFixture()
	request := func(slots []Slot, p Params) externalRequest {
		t.Helper()
		r := external.buildRequest(slots, p)
		r.RequestID = "fixed"
		if err := o.prepareEnergyplanRequest(context.Background(), &r, slots, p); err != nil {
			t.Fatal(err)
		}
		return r
	}
	wire := func(r externalRequest) string {
		raw, err := json.Marshal(r.DemandCharges)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	slots, importWh, now := weekdayEvening(t, true)
	p := fixture
	p.DemandCharges = bindDemandCharges(slots, 7000, 3, 0, 0, time.UTC, now, importWh, HourImport{})
	transport.elapsed = false
	if got, want := wire(request(slots, p)), `[{"id":"weekday-high-2026-09","price_per_kw":7000,"top_n":3,"already_kw":[2.9,2.9,2.9,2.9,2.9,2.9,2.8]}]`; got != want {
		t.Fatalf("old worker demand_charges %s\nwant %s", got, want)
	}
	transport.elapsed = true
	if got, want := wire(request(slots, p)), `[{"id":"weekday-high-2026-09","price_per_kw":7000,"top_n":3,"already_kw":[2.9,2.9,2.9,2.9,2.9,2.9],"hours":[{"start_ms":1788976800000,"end_ms":1788980400000,"elapsed_import_kwh":2.8,"group":"2026-09-09"},{"start_ms":1788980400000,"end_ms":1788984000000,"elapsed_import_kwh":0.7,"group":"2026-09-09"}]}]`; got != want {
		t.Fatalf("new worker demand_charges %s\nwant %s", got, want)
	}
	// A new site has no history: its only demand hour is the one in
	// progress, counted at slot 0's forecast import. The old worker gets no
	// demand charge and the battery's own budget.
	slots, _, now = weekdayEvening(t, false)
	p.DemandCharges = bindDemandCharges(slots, 7000, 3, 0, 0, time.UTC, now, nil, HourImport{})
	if len(p.DemandCharges) != 1 || len(p.DemandCharges[0].Hours) != 1 || p.DemandCharges[0].Hours[0].ElapsedImportKWh != 0.3 {
		t.Fatalf("charges=%+v; want the hour in progress at 900 W for 20 minutes", p.DemandCharges)
	}
	transport.elapsed = false
	if r := request(slots, p); r.DemandCharges != nil || r.Settings.TimeLimitS != energyplanSmallBudget.Seconds() {
		t.Fatalf("old worker demand_charges %s, time_limit_s %g; want none and %g", wire(r), r.Settings.TimeLimitS, energyplanSmallBudget.Seconds())
	}
	transport.elapsed = true
	if r := request(slots, p); len(r.DemandCharges) != 1 || r.Settings.TimeLimitS != energyplanFleetBudget.Seconds() {
		t.Fatalf("new worker demand_charges %s, time_limit_s %g", wire(r), r.Settings.TimeLimitS)
	}
}

// The tariff exists for either worker, so the reserve's price-only pass
// keeps away from it even when the worker took no demand charge.
func TestReservePricingKeepsAwayFromADemandTariffWithOnlyTheRunningHour(t *testing.T) {
	slots, p := externalTestFixture()
	p.Loadpoint = &LoadpointSpec{ID: "car", PluggedIn: true, CapacityWh: 60000, Levels: 11, SoCMax: 1, MaxChargeW: 11000,
		TargetSoC: 0.5, TargetSlotIdx: 1}
	reserve := Plan{Actions: []Action{{SlotStartMs: slots[0].StartMs, SlotLenMin: 60, LoadpointW: 4000},
		{SlotStartMs: slots[1].StartMs, SlotLenMin: 60}}}
	p.DemandCharges = []DemandCharge{{ID: "weekday-high-2026-09", PricePerKW: 7000, TopN: 3,
		Hours: []DemandHour{{StartMs: slots[0].StartMs - 900_000, EndMs: slots[0].StartMs + 2_700_000, Elapsed: true}}}}
	got, err := priceReserveCharging(context.Background(), slots, p, reserve)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, reserve) {
		t.Fatalf("reserve changed: %+v", got)
	}
}

func TestOptimizerRequestDropsOreKeys(t *testing.T) {
	o := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}}
	slots, p := externalTestFixture()
	p.DemandCharges = []DemandCharge{{
		ID: "weekday-high-2026-09", PricePerKW: 8750, TopN: 3,
		Hours: []DemandHour{{StartMs: slots[0].StartMs, EndMs: slots[0].StartMs + 3600_000}},
	}}
	raw, err := json.Marshal(o.buildRequest(slots, p))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, key := range []string{"price_ore", "spot_ore", "export_ore_per_kwh", "terminal_price_ore_kwh"} {
		if strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("planner wire still has %s: %s", key, body)
		}
	}
	for _, key := range []string{"price_per_kwh", "spot_per_kwh", "demand_charges", "price_per_kw"} {
		if !strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("planner wire missing %s", key)
		}
	}
}

func TestNativeDemandChargesOnWeekdayHour(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	start := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	slots := []Slot{
		{StartMs: start.UnixMilli(), LenMin: 60, PriceOre: 100, SpotOre: 50, LoadW: 500,
			Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000}},
		{StartMs: start.Add(time.Hour).UnixMilli(), LenMin: 60, PriceOre: 300, SpotOre: 240, LoadW: 500,
			Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000}},
	}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		TerminalSoCPrice: 100,
		DemandCharges: []DemandCharge{{
			ID: "weekday-high-2026-09", PricePerKW: 7000, TopN: 1,
			Hours: []DemandHour{{StartMs: start.UnixMilli(), EndMs: start.Add(time.Hour).UnixMilli()}},
		}},
	}
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
	if len(req.DemandCharges) != 1 || req.DemandCharges[0].PricePerKW != 7000 || len(req.DemandCharges[0].Hours) != 1 {
		t.Fatalf("demand_charges=%+v", req.DemandCharges)
	}
	if plan.Solver.Backend != "fleet_milp_rust" {
		t.Fatalf("backend=%s, want fleet_milp_rust", plan.Solver.Backend)
	}
}

// A worker that announces demand_elapsed_hours takes the hour in progress
// that began before slot 0 and today's completed peak in today's group, and
// Core accepts its plan. The bundled worker skips until its pin takes them.
func TestNativeEnergyplanTakesElapsedDemandHours(t *testing.T) {
	binary := os.Getenv("FTW_NATIVE_SOLVER")
	if binary == "" {
		t.Skip("run make native-solver-test to include the Rust worker")
	}
	o, err := NewEnergyplanOptimizer(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := o.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !optimizerHasFeature(info, "demand_elapsed_hours") {
		t.Skip("this worker does not take demand_elapsed_hours")
	}
	slots, importWh, now := weekdayEvening(t, true)
	evening := time.Date(2026, 9, 9, 20, 0, 0, 0, time.UTC)
	for h := range 3 {
		slots = append(slots, Slot{StartMs: evening.Add(time.Duration(h) * time.Hour).UnixMilli(), LenMin: 60, LoadW: 900, PriceOre: 60})
	}
	for i := range slots {
		slots[i].SpotOre = 30
		slots[i].Limits = PowerLimits{MaxImportW: 11000, MaxExportW: 11000}
	}
	_, p := externalTestFixture()
	p.DemandCharges = bindDemandCharges(slots, 7000, 3, 0, 0, time.UTC, now, importWh, HourImport{})
	plan, err := o.Optimize(ctx, slots, p)
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
	if len(req.DemandCharges) != 1 || len(req.DemandCharges[0].Hours) != 2 || req.DemandCharges[0].Hours[1].StartMs >= slots[0].StartMs {
		t.Fatalf("demand_charges=%+v; want today's peak and the hour in progress", req.DemandCharges)
	}
	if plan.Solver == nil || plan.Solver.Backend != "fleet_milp_rust" {
		t.Fatalf("solver %+v", plan.Solver)
	}
}

func TestEnergyplanTimeBudgetUsesFleetWhenDemandCharges(t *testing.T) {
	slots := []Slot{{StartMs: time.Now().UnixMilli(), LenMin: 15}}
	p := Params{CapacityWh: 10000, DemandCharges: []DemandCharge{{ID: "weekday-high-2026-09"}}}
	if got := energyplanTimeBudget(slots, p); got != energyplanFleetBudget {
		t.Fatalf("budget=%s, want %s", got, energyplanFleetBudget)
	}
}
