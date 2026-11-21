package heatrun

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memSink struct{ recs []ShadowRecord }

func (m *memSink) Append(r []ShadowRecord) error {
	m.recs = append(m.recs, r...)
	return nil
}

// shadowHouse is the bathroom floor in shadow, heated at a steady 600 W
// as its heat input reports.
func shadowHouse() (*house, *Service, *memSink, *float64) {
	h := newHouse()
	h.cfg.Write = false
	h.cfg.Zones[0].HeatInput = "wiser_7/hp_power_w"
	s := h.service()
	watts := 600.0
	inner := s.in.Latest
	s.in.Latest = func(driver, metric string) (float64, time.Time, bool) {
		if driver == "wiser_7" && metric == "hp_power_w" {
			return watts, h.now, true
		}
		return inner(driver, metric)
	}
	sink := &memSink{}
	s.in.Shadow = sink
	return h, s, sink, &watts
}

func (h *house) runSlot(s *Service) {
	for i := 0; i < int(slot/refreshEvery); i++ {
		h.now = h.now.Add(refreshEvery)
		s.Refresh(t0ctx)
	}
}

var t0ctx = context.Background()

func TestShadowLogComparesTheEndedSlotWithWhatWasSaidForIt(t *testing.T) {
	h, s, sink, _ := shadowHouse()
	s.Tick(t0ctx)
	if len(sink.recs) != 1 {
		t.Fatalf("first tick logged %d records, want 1", len(sink.recs))
	}
	first := sink.recs[0]
	if first.Last != nil {
		t.Fatalf("first record has a last slot: %+v", first.Last)
	}
	if !first.Planned || first.Next == nil || first.Horizon == nil {
		t.Fatalf("first record has no plan: %+v", first)
	}
	if first.Next.BaseTempC == 0 {
		t.Fatalf("baseline prediction missing: %+v", first.Next)
	}
	if first.WriteOn || first.Write != "shadow: writing is off" {
		t.Fatalf("write state %v %q", first.WriteOn, first.Write)
	}
	if first.HeatInput != "wiser_7/hp_power_w" {
		t.Fatalf("heat input %q", first.HeatInput)
	}

	h.runSlot(s)
	h.floor = 23.4
	s.Tick(t0ctx)
	if len(sink.recs) != 2 {
		t.Fatalf("second tick: %d records, want 2", len(sink.recs))
	}
	last := sink.recs[1].Last
	if last == nil {
		t.Fatal("second record says nothing about the slot that ended")
	}
	if !last.Start.Equal(first.At) {
		t.Fatalf("last slot starts %v, want %v", last.Start, first.At)
	}
	if last.Forecast == nil || *last.Forecast != *first.Next {
		t.Fatalf("forecast %+v, want what was said: %+v", last.Forecast, first.Next)
	}
	if last.StartC == nil || *last.StartC != 23 || last.EndC == nil || *last.EndC != 23.4 {
		t.Fatalf("temperatures %v -> %v, want 23 -> 23.4", deref(last.StartC), deref(last.EndC))
	}
	// 600 W through a quarter is 0.15 kWh.
	if last.InputKWh == nil || math.Abs(*last.InputKWh-0.15) > 1e-9 {
		t.Fatalf("input %v kWh, want 0.15", deref(last.InputKWh))
	}
	if math.Abs(last.InputCoverage-1) > 1e-9 {
		t.Fatalf("coverage %v, want 1", last.InputCoverage)
	}
	if last.Price == nil || *last.Price != 100 {
		t.Fatalf("price %v, want 100", deref(last.Price))
	}
}

func TestShadowLogLeavesOutAForecastAcrossAGapAndCountsMissingInput(t *testing.T) {
	h, s, sink, watts := shadowHouse()
	s.Tick(t0ctx)
	// Core is away for a slot: no refreshes, no tick.
	h.now = h.now.Add(slot)
	h.runSlot(s)
	s.Tick(t0ctx)
	last := sink.recs[len(sink.recs)-1].Last
	if last == nil || last.Forecast != nil {
		t.Fatalf("a forecast compared across a gap: %+v", last)
	}

	// Half the next slot the heat input is stale: only half is covered.
	*watts = 400
	s.in.Latest = staleAfter(s.in.Latest, h, h.now.Add(slot/2))
	h.runSlot(s)
	s.Tick(t0ctx)
	last = sink.recs[len(sink.recs)-1].Last
	if last == nil || last.Forecast == nil {
		t.Fatalf("contiguous slot lost its forecast: %+v", last)
	}
	if math.Abs(last.InputCoverage-0.5) > 0.05 {
		t.Fatalf("coverage %v, want about 0.5", last.InputCoverage)
	}
	if last.InputKWh == nil || math.Abs(*last.InputKWh-0.05) > 0.005 {
		t.Fatalf("input %v kWh, want about 0.05 (400 W for half a quarter)", deref(last.InputKWh))
	}
}

// staleAfter freezes the heat input's timestamp from cut on, as a driver
// that stopped answering.
func staleAfter(inner func(string, string) (float64, time.Time, bool), h *house, cut time.Time) func(string, string) (float64, time.Time, bool) {
	return func(driver, metric string) (float64, time.Time, bool) {
		v, at, ok := inner(driver, metric)
		if metric == "hp_power_w" && h.now.After(cut) {
			return v, cut.Add(-inputFreshFor - time.Minute), ok
		}
		return v, at, ok
	}
}

func TestJSONLLogAppendsLinesAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heat-shadow.jsonl")
	l := &JSONLLog{Path: path, MaxBytes: 400}
	rec := ShadowRecord{At: time.Unix(0, 0).UTC(), Zone: "badrum", Sensor: "wiser_7/floor_temp_c", Source: "direct_electric", Write: "shadow: writing is off"}
	for i := 0; i < 6; i++ {
		if err := l.Append([]ShadowRecord{rec}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	if fi.Size() > 400 {
		t.Fatalf("current file %d bytes, over the limit", fi.Size())
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var got ShadowRecord
		if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
			t.Fatalf("line is not a record: %v", err)
		}
		if got.Zone != "badrum" {
			t.Fatalf("zone %q", got.Zone)
		}
	}
}

// When the heat that went in is the heat the plan asked for, the model
// temperature is the plan's own: the log tests the very model FTW plans
// with, not another one.
func TestShadowLogModelTemperatureIsThePlannersModel(t *testing.T) {
	h, s, sink, watts := shadowHouse()
	s.Tick(t0ctx)
	next := sink.recs[0].Next
	if next == nil {
		t.Fatal("no plan")
	}
	if next.PlanHeatKWh <= 0 {
		t.Fatalf("the plan heats nothing in its first slot, so the test would prove nothing: %+v", next)
	}
	*watts = next.PlanHeatKWh / SlotH * 1000
	h.runSlot(s)
	s.Tick(t0ctx)
	last := sink.recs[len(sink.recs)-1].Last
	if last == nil || last.ModelEndC == nil {
		t.Fatalf("no model temperature: %+v", last)
	}
	if math.Abs(*last.ModelEndC-next.PlanTempC) > 1e-6 {
		t.Fatalf("model end %.6f, plan said %.6f for the same heat", *last.ModelEndC, next.PlanTempC)
	}
	// Direct electric: the electricity is the heat, and costs the price.
	if last.ElecKWh == nil || *last.ElecKWh != *last.InputKWh {
		t.Fatalf("elec %v, want the heat input %v", deref(last.ElecKWh), deref(last.InputKWh))
	}
	if last.ActualCost == nil || math.Abs(*last.ActualCost-*last.ElecKWh**last.Price) > 1e-9 {
		t.Fatalf("cost %v, want elec x price", deref(last.ActualCost))
	}
}

// A heat pump zone learns against its space heat, but what it costs is the
// pump's draw: the log integrates the two apart.
func TestShadowLogHeatPumpCostsTheDrawNotTheHeat(t *testing.T) {
	h, s, sink, _ := shadowHouse()
	h.cfg.Zones[0].Source = "heat_pump"
	h.cfg.Zones[0].HeatInput = "CTC/space_heat_w"
	inner := s.in.Latest
	s.in.Latest = func(driver, metric string) (float64, time.Time, bool) {
		if driver == "CTC" {
			switch metric {
			case "space_heat_w":
				return 4000, h.now, true
			case "hp_power_w":
				return 1000, h.now, true
			}
		}
		return inner(driver, metric)
	}
	s.Tick(t0ctx)
	h.runSlot(s)
	s.Tick(t0ctx)
	last := sink.recs[len(sink.recs)-1].Last
	if last == nil || last.InputKWh == nil || math.Abs(*last.InputKWh-1.0) > 1e-9 {
		t.Fatalf("heat %v kWh, want 1.0 (4 kW for a quarter)", deref(last.InputKWh))
	}
	if last.ElecKWh == nil || math.Abs(*last.ElecKWh-0.25) > 1e-9 {
		t.Fatalf("elec %v kWh, want 0.25 (1 kW for a quarter)", deref(last.ElecKWh))
	}
	if last.ActualCost == nil || math.Abs(*last.ActualCost-0.25**last.Price) > 1e-9 {
		t.Fatalf("cost %v, want the draw at the price", deref(last.ActualCost))
	}
}

// Half a slot of samples is too little to turn into a temperature or a cost.
func TestShadowLogJudgesNothingFromAThinSlot(t *testing.T) {
	h, s, sink, _ := shadowHouse()
	s.Tick(t0ctx)
	s.in.Latest = staleAfter(s.in.Latest, h, h.now.Add(slot/2))
	h.runSlot(s)
	s.Tick(t0ctx)
	last := sink.recs[len(sink.recs)-1].Last
	if last == nil || last.InputKWh == nil {
		t.Fatalf("no input at all: %+v", last)
	}
	if last.ModelEndC != nil || last.ActualCost != nil {
		t.Fatalf("judged a half-covered slot: model %v, cost %v", deref(last.ModelEndC), deref(last.ActualCost))
	}
}

// A slot that opened late, as the first after Core starts, is modelled
// from when its start temperature was read, not from the slot's start.
func TestShadowLogModelsALateSlotFromItsFirstReading(t *testing.T) {
	h, s, sink, _ := shadowHouse()
	h.now = h.now.Add(time.Minute)
	s.Tick(t0ctx)
	first := sink.recs[0]
	tr := s.tracks[first.Zone]
	for i := 0; i < int((slot-time.Minute)/refreshEvery); i++ {
		h.now = h.now.Add(refreshEvery)
		s.Refresh(t0ctx)
	}
	s.Tick(t0ctx)
	last := sink.recs[len(sink.recs)-1].Last
	if last == nil || last.ModelEndC == nil {
		t.Fatalf("no model temperature: %+v", last)
	}
	want := tr.model.endC(*tr.startC, tr.heat.wh/1000/tr.heat.covered.Hours(), (slot - time.Minute).Hours())
	if math.Abs(*last.ModelEndC-want) > 1e-9 {
		t.Fatalf("model end %.6f, want %.6f over the 14 minutes after the reading", *last.ModelEndC, want)
	}
}
