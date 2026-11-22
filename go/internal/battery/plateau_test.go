package battery

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

var day0 = time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)

type shortfall struct {
	charge         bool
	grid, a, tempC *float64
	batteryW       func(i int) float64
}

func f(v float64) *float64 { return &v }

// feed ticks n samples five seconds apart from start, then idles long enough
// to end the run. It returns the time after the idle.
func feed(p *Plateaus, start time.Time, n int, s shortfall) time.Time {
	at := start
	for i := range n {
		var grid, a *float64
		if s.grid != nil {
			grid = f(*s.grid + 20*math.Sin(float64(i)))
		}
		if s.a != nil {
			a = f(*s.a + 0.2*math.Sin(float64(i)))
		}
		p.Tick(at, &ShortfallSample{At: at, Charge: s.charge, BatteryW: s.batteryW(i), BatteryA: a, GridW: grid, TempC: s.tempC})
		at = at.Add(5 * time.Second)
	}
	at = at.Add(time.Minute)
	p.Tick(at, nil)
	return at
}

// A night of grid charging: the grid sits at 4.3 kW while the house load
// moves the battery's share between 1.3 and 4 kW.
func gridNight(i int) float64 { return 4300 - 300 - 2700*(0.5+0.5*math.Sin(float64(i)/7)) }

func TestPlateauRunNeedsAFlatMinute(t *testing.T) {
	p := &Plateaus{}
	feed(p, day0, 11, shortfall{charge: true, grid: f(4300), batteryW: gridNight})
	if len(p.Series) != 0 {
		t.Fatalf("under a minute made a run: %+v", p.Series)
	}
	feed(p, day0.Add(time.Hour), 60, shortfall{charge: true, grid: f(4300), batteryW: gridNight})
	if len(p.Series) != 1 || p.Series[0].Kind != LimitGridImport || p.Series[0].Direction != "charge" {
		t.Fatalf("want one flat grid import series, got %+v", p.Series)
	}
	run := p.Series[0].Runs[0]
	if math.Abs(run.Median-4300) > 25 || run.Samples != 60 || run.StartMs != day0.Add(time.Hour).UnixMilli() {
		t.Fatalf("run %+v", run)
	}
}

func TestPlateauRunEndsOnGapAndDirection(t *testing.T) {
	p := &Plateaus{}
	at := day0
	flat := func(int) float64 { return 3000 }
	for range 20 {
		p.Tick(at, &ShortfallSample{At: at, Charge: true, BatteryW: 3000})
		at = at.Add(5 * time.Second)
	}
	// A break under 30 s keeps the run going.
	p.Tick(at, nil)
	at = at.Add(20 * time.Second)
	for range 20 {
		p.Tick(at, &ShortfallSample{At: at, Charge: true, BatteryW: 3000})
		at = at.Add(5 * time.Second)
	}
	// A change of direction ends it.
	feed(p, at, 20, shortfall{charge: false, batteryW: flat})
	if len(p.Series) != 2 {
		t.Fatalf("want a charge and a discharge series, got %+v", p.Series)
	}
	for _, s := range p.Series {
		if len(s.Runs) != 1 {
			t.Fatalf("%s: runs %+v", s.Direction, s.Runs)
		}
		if s.Direction == "charge" && s.Runs[0].Samples != 40 {
			t.Fatalf("short break split the run: %+v", s.Runs)
		}
	}
	// A gap over 30 s ends a run.
	p = &Plateaus{}
	at = day0
	for range 20 {
		p.Tick(at, &ShortfallSample{At: at, Charge: true, BatteryW: 3000})
		at = at.Add(5 * time.Second)
	}
	at = at.Add(40 * time.Second)
	feed(p, at, 20, shortfall{charge: true, batteryW: flat})
	if len(p.Series) != 1 || len(p.Series[0].Runs) != 2 {
		t.Fatalf("want two runs after a gap, got %+v", p.Series)
	}
}

func TestOnlyFlatAmountsMakeRuns(t *testing.T) {
	p := &Plateaus{}
	// Discharge on a current limit: 22.4 A while the pack voltage drifts
	// from 187 to 192 V, so the power drifts with it.
	feed(p, day0, 120, shortfall{charge: false, a: f(22.4), batteryW: func(i int) float64 { return 22.4 * (187 + 5*float64(i)/120) }})
	// Grid charge with a varying house: battery power is not flat.
	feed(p, day0.Add(2*time.Hour), 120, shortfall{charge: true, grid: f(4300), a: nil, batteryW: gridNight})
	kinds := map[string]bool{}
	for _, s := range p.Series {
		kinds[s.Direction+"/"+s.Kind] = true
	}
	if !kinds["discharge/battery_current"] || !kinds["charge/grid_import"] || kinds["charge/battery_power"] {
		t.Fatalf("flat amounts %v", kinds)
	}
}

func TestPlateauTrustNeedsThreeRunsOnTwoDays(t *testing.T) {
	p := &Plateaus{}
	night := shortfall{charge: true, grid: f(4316), batteryW: gridNight}
	at := day0
	for range 3 {
		at = feed(p, at, 60, night)
	}
	sample := ShortfallSample{At: at, Charge: true, BatteryW: 2500, GridW: f(4320)}
	if _, ok := p.Explain(sample); ok {
		t.Fatal("one night of runs was trusted")
	}
	feed(p, day0.Add(24*time.Hour), 60, night)
	known, ok := p.Explain(sample)
	if !ok || known.Kind != LimitGridImport || known.Seen != 4 || known.SinceMs != day0.UnixMilli() || math.Abs(known.Value-4316) > 25 || known.Unit != "W" {
		t.Fatalf("trusted plateau %+v ok=%v", known, ok)
	}
	// A battery well below its plateau is not explained.
	if _, ok := p.Explain(ShortfallSample{Charge: true, BatteryW: 1000, GridW: f(1500)}); ok {
		t.Fatal("a drop to 1.5 kW grid import sat on the 4.3 kW plateau")
	}
	// Nor is the other direction.
	if _, ok := p.Explain(ShortfallSample{Charge: false, BatteryW: 2500, GridW: f(4320)}); ok {
		t.Fatal("a charge plateau explained a discharge")
	}
	if got := p.Known(); len(got) != 1 {
		t.Fatalf("known %+v", got)
	}
}

func TestLowerPlateauBecomesNormalAfterTwoDays(t *testing.T) {
	p := &Plateaus{}
	high := shortfall{charge: true, grid: f(4300), batteryW: gridNight}
	low := shortfall{charge: true, grid: f(3100), batteryW: func(i int) float64 { return gridNight(i) * 0.6 }}
	for d := range 3 {
		feed(p, day0.Add(time.Duration(d)*24*time.Hour), 60, high)
	}
	lowSample := ShortfallSample{Charge: true, BatteryW: 2000, GridW: f(3110)}
	at := day0.Add(14 * 24 * time.Hour)
	for range 3 {
		at = feed(p, at, 60, low)
	}
	if known, ok := p.Explain(lowSample); ok {
		t.Fatalf("a new lower level was normal after one day: %+v", known)
	}
	feed(p, day0.Add(15*24*time.Hour), 60, low)
	known, ok := p.Explain(lowSample)
	if !ok || math.Abs(known.Value-3100) > 25 || known.SinceMs != day0.Add(14*24*time.Hour).UnixMilli() {
		t.Fatalf("lower plateau %+v ok=%v", known, ok)
	}
	if known.Earlier == nil || math.Abs(known.Earlier.Value-4300) > 25 || known.Earlier.Seen != 3 {
		t.Fatalf("lost the earlier plateau: %+v", known.Earlier)
	}
	if _, ok := p.Explain(ShortfallSample{Charge: true, BatteryW: 3500, GridW: f(4300)}); ok {
		t.Fatal("the replaced plateau still explains")
	}
}

func TestPlateauTemperatureBands(t *testing.T) {
	p := &Plateaus{}
	cold := shortfall{charge: false, a: f(12), tempC: f(-3), batteryW: func(int) float64 { return 12 * 190 }}
	for d := range 3 {
		feed(p, day0.Add(time.Duration(d)*24*time.Hour), 60, cold)
	}
	inCold := ShortfallSample{Charge: false, BatteryW: 2280, BatteryA: f(12.1), TempC: f(-4.5)}
	if known, ok := p.Explain(inCold); !ok || known.Kind != LimitBatteryCurrent || known.TempBandC == nil || *known.TempBandC != -10 {
		t.Fatalf("cold plateau %+v ok=%v", known, ok)
	}
	for _, temp := range []*float64{f(15), nil} {
		warm := inCold
		warm.TempC = temp
		if _, ok := p.Explain(warm); ok {
			t.Fatalf("a -3 °C plateau explained a shortfall at %v", temp)
		}
	}
}

func TestPlateausStayBounded(t *testing.T) {
	p := &Plateaus{}
	at := day0
	for range 70 {
		at = feed(p, at, 12, shortfall{charge: true, batteryW: func(int) float64 { return 2000 }})
	}
	if n := len(p.Series[0].Runs); n != plateauKeepMax {
		t.Fatalf("kept %d runs", n)
	}
	p.Tick(at.Add(61*24*time.Hour), nil)
	if len(p.Series) != 0 {
		t.Fatalf("runs older than 60 days kept: %+v", p.Series)
	}
	// A run is cut at two hours.
	p = &Plateaus{}
	feed(p, day0, 2*60*12+12, shortfall{charge: true, batteryW: func(int) float64 { return 2000 }})
	if n := len(p.Series[0].Runs); n != 2 {
		t.Fatalf("a long run made %d runs", n)
	}
}

func TestPlateausSurviveTheModelStore(t *testing.T) {
	m := New("battery")
	m.Plateaus = &Plateaus{}
	for d := range 3 {
		feed(m.Plateaus, day0.Add(time.Duration(d)*24*time.Hour), 60, shortfall{charge: true, grid: f(4300), batteryW: gridNight})
	}
	// An open run is not stored.
	m.Plateaus.Tick(day0.Add(4*24*time.Hour), &ShortfallSample{At: day0.Add(4 * 24 * time.Hour), Charge: true, BatteryW: 2000})
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Model
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	before, after := m.Plateaus.Known(), back.Plateaus.Known()
	if len(before) != 1 || len(after) != 1 || before[0].Value != after[0].Value || before[0].Seen != after[0].Seen || back.Plateaus.open != nil {
		t.Fatalf("round trip: %+v -> %+v", before, after)
	}
	// A model stored before plateaus existed still loads.
	var old Model
	if err := json.Unmarshal([]byte(`{"name":"battery","a":0.7,"b":0.3}`), &old); err != nil || old.Plateaus != nil || len(old.Plateaus.Known()) != 0 {
		t.Fatalf("old model: %+v %v", old, err)
	}
	old.Plateaus.Tick(day0, &ShortfallSample{At: day0, Charge: true, BatteryW: 2000})
}
