package battery

import (
	"math"
	"sort"
	"time"
)

// Plateaus learns what a battery can do when it gives less than its order.
// While a battery falls short in the right direction, something is usually
// flat: grid import (the inverter's import cap), grid export (an export
// limit), the battery's DC current (a BMS or converter limit) or its power.
// A flat run that comes back on more than one day is what this device does,
// so a later shortfall that sits on it is a limit, not lost control.
//
// It only explains Core's verdict. Dispatch, fuse protection, clamps and
// commands never read it. The caller holds the model lock.
type Plateaus struct {
	Series []PlateauSeries `json:"series"`
	// open is the shortfall run under way. It is not stored: a restart
	// loses at most one run.
	open *plateauRun
}

// What a shortfall can sit on.
const (
	LimitGridImport     = "grid_import"
	LimitGridExport     = "grid_export"
	LimitBatteryCurrent = "battery_current"
	LimitBatteryPower   = "battery_power"
)

const (
	// PlateauMinShortfallW is the least battery power a sample needs. Below
	// it a reading says more about noise and standby draw than about a limit.
	PlateauMinShortfallW = 300.0
	// A run needs a minute of 5 s journal samples. A break up to
	// plateauMaxGap does not end it; a run is cut at plateauMaxRun so that
	// memory stays bounded and a long night still counts as more than one.
	plateauMinSamples = 12
	plateauMaxGap     = 30 * time.Second
	plateauMaxRun     = 2 * time.Hour
	// Each series keeps its last plateauKeepMax runs of the last
	// plateauKeepFor.
	plateauKeepFor = 60 * 24 * time.Hour
	plateauKeepMax = 50
	// A plateau is trusted after plateauTrustRuns runs on two days. Runs that
	// start plateauDayApart or more apart are on different days, so one night
	// across midnight counts once.
	plateauTrustRuns = 3
	plateauDayApart  = 12 * time.Hour
	// Temperatures key plateaus in bands this wide, so a cold-weather limit
	// never explains a warm-weather shortfall.
	plateauTempBandC = 10
)

// Spread and match limits. A run is flat when the middle half of its samples
// lies within plateauFlatShare of their median, or within the floor: the
// inverter's grid regulation and a BMS current limit hold well inside that
// (a home Sungrow held its grid import at 4,276-4,344 W, under 2 %). Runs
// agree, and a sample sits on a plateau, within plateauNearShare: one
// sample's noise is wider than a run's middle half (its DC current read
// 21.6-23.1 A on a 22.4 A limit, under 4 %). The floors cover meter and
// register resolution at small values.
const (
	plateauFlatShare = 0.03
	plateauNearShare = 0.05
	plateauFloorW    = 100.0
	plateauFloorA    = 0.5
)

// ShortfallSample is one settled journal sample of a battery short of its
// order in the right direction. Amounts are positive: watts or amps in the
// order's direction. GridW is grid import while charging or export while
// discharging, from a site meter Core trusts; nil when it has none.
type ShortfallSample struct {
	At       time.Time
	Charge   bool
	BatteryW float64
	BatteryA *float64
	GridW    *float64
	TempC    *float64
}

// PlateauRun is one flat run: the median of the flat amount and when it held.
type PlateauRun struct {
	StartMs int64   `json:"start_ms"`
	EndMs   int64   `json:"end_ms"`
	Median  float64 `json:"median"`
	Samples int     `json:"samples"`
}

// PlateauSeries keeps the recent flat runs of one amount, for one direction
// and temperature band.
type PlateauSeries struct {
	Direction string `json:"direction"`
	Kind      string `json:"kind"`
	// TempBandC is the lower edge of a 10 °C band; nil when the battery
	// reports no temperature.
	TempBandC *int         `json:"temp_band_c,omitempty"`
	Runs      []PlateauRun `json:"runs"`
}

// Plateau is a limit the battery has shown on more than one day. Value is in
// W, or in A for battery_current. Earlier is the plateau it replaced.
type Plateau struct {
	Direction string   `json:"direction"`
	Kind      string   `json:"kind"`
	TempBandC *int     `json:"temp_band_c,omitempty"`
	Value     float64  `json:"value"`
	Unit      string   `json:"unit"`
	Seen      int      `json:"seen"`
	SinceMs   int64    `json:"since_ms"`
	LastMs    int64    `json:"last_ms"`
	Earlier   *Plateau `json:"earlier,omitempty"`
}

type plateauRun struct {
	charge      bool
	band        *int
	start, last time.Time
	n           int
	values      map[string][]float64
}

// Tick feeds one journal sample; s is nil when the battery is not short of
// its order now. A run ends after a gap over plateauMaxGap, on a change of
// direction or temperature band, or at plateauMaxRun.
func (p *Plateaus) Tick(now time.Time, s *ShortfallSample) {
	if p == nil {
		return
	}
	if o := p.open; o != nil {
		ended := now.Sub(o.last) > plateauMaxGap
		if s != nil {
			ended = s.At.Sub(o.last) > plateauMaxGap || s.Charge != o.charge ||
				!sameBand(tempBand(s.TempC), o.band) || s.At.Sub(o.start) >= plateauMaxRun
		}
		if ended {
			p.close()
		}
	}
	p.prune(now)
	if s == nil || !finite(s.BatteryW) || s.BatteryW < PlateauMinShortfallW {
		return
	}
	if p.open == nil {
		p.open = &plateauRun{charge: s.Charge, band: tempBand(s.TempC), start: s.At, values: map[string][]float64{}}
	}
	o := p.open
	o.n++
	o.last = s.At
	add := func(kind string, v *float64) {
		if v != nil && finite(*v) && *v > 0 {
			o.values[kind] = append(o.values[kind], *v)
		}
	}
	grid := LimitGridImport
	if !s.Charge {
		grid = LimitGridExport
	}
	add(grid, s.GridW)
	add(LimitBatteryCurrent, s.BatteryA)
	add(LimitBatteryPower, &s.BatteryW)
}

// close keeps each amount that stayed flat over the open run. An amount
// needs most of the run's samples, so a meter that came and went proves
// nothing.
func (p *Plateaus) close() {
	o := p.open
	p.open = nil
	if o == nil || o.n < plateauMinSamples {
		return
	}
	for _, kind := range []string{LimitGridImport, LimitGridExport, LimitBatteryCurrent, LimitBatteryPower} {
		v := o.values[kind]
		if len(v) < plateauMinSamples || float64(len(v)) < 0.8*float64(o.n) {
			continue
		}
		median, iqr := middle(v)
		if iqr > math.Max(plateauFlatShare*median, floor(kind)) {
			continue
		}
		run := PlateauRun{StartMs: o.start.UnixMilli(), EndMs: o.last.UnixMilli(), Median: median, Samples: len(v)}
		series := p.series(directionOf(o.charge), kind, o.band)
		series.Runs = append(series.Runs, run)
	}
}

// prune drops runs older than plateauKeepFor, keeps the last plateauKeepMax
// of each series and drops empty series.
func (p *Plateaus) prune(now time.Time) {
	cutoff := now.Add(-plateauKeepFor).UnixMilli()
	kept := p.Series[:0]
	for _, s := range p.Series {
		runs := s.Runs[:0]
		for _, r := range s.Runs {
			if r.EndMs >= cutoff {
				runs = append(runs, r)
			}
		}
		if len(runs) > plateauKeepMax {
			runs = runs[len(runs)-plateauKeepMax:]
		}
		if len(runs) > 0 {
			s.Runs = runs
			kept = append(kept, s)
		}
	}
	p.Series = kept
}

func (p *Plateaus) series(direction, kind string, band *int) *PlateauSeries {
	for i := range p.Series {
		s := &p.Series[i]
		if s.Direction == direction && s.Kind == kind && sameBand(s.TempBandC, band) {
			return s
		}
	}
	p.Series = append(p.Series, PlateauSeries{Direction: direction, Kind: kind, TempBandC: band})
	return &p.Series[len(p.Series)-1]
}

// Explain returns the trusted plateau a shortfall sits on, if any: grid first,
// since it is the outer limit, then DC current, then battery power. A sample
// well below its plateau sits on none and keeps its warning.
func (p *Plateaus) Explain(s ShortfallSample) (Plateau, bool) {
	if p == nil || !finite(s.BatteryW) || s.BatteryW < PlateauMinShortfallW {
		return Plateau{}, false
	}
	grid := LimitGridImport
	if !s.Charge {
		grid = LimitGridExport
	}
	band := tempBand(s.TempC)
	for _, c := range []struct {
		kind  string
		value *float64
	}{{grid, s.GridW}, {LimitBatteryCurrent, s.BatteryA}, {LimitBatteryPower, &s.BatteryW}} {
		if c.value == nil || !finite(*c.value) {
			continue
		}
		for i := range p.Series {
			series := &p.Series[i]
			if series.Direction != directionOf(s.Charge) || series.Kind != c.kind || !sameBand(series.TempBandC, band) {
				continue
			}
			if known, ok := series.plateau(); ok && near(*c.value, known.Value, c.kind) {
				return known, true
			}
		}
	}
	return Plateau{}, false
}

// Known lists the trusted plateau of every series.
func (p *Plateaus) Known() []Plateau {
	out := []Plateau{}
	if p == nil {
		return out
	}
	for i := range p.Series {
		if known, ok := p.Series[i].plateau(); ok {
			out = append(out, known)
		}
	}
	return out
}

// plateau is the series' trusted plateau: of the groups of runs that agree,
// the newest one seen at least plateauTrustRuns times on two days. A lower
// level becomes normal only once it repeats like that; until then the old
// one stands and a run at the new level does not match it.
func (s *PlateauSeries) plateau() (Plateau, bool) {
	runs := append([]PlateauRun(nil), s.Runs...)
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartMs < runs[j].StartMs })
	var groups [][]PlateauRun
	for _, r := range runs {
		best := -1
		for i, g := range groups {
			if near(r.Median, groupValue(g), s.Kind) && (best < 0 || g[len(g)-1].StartMs > groups[best][len(groups[best])-1].StartMs) {
				best = i
			}
		}
		if best < 0 {
			groups = append(groups, []PlateauRun{r})
		} else {
			groups[best] = append(groups[best], r)
		}
	}
	var trusted []Plateau
	for _, g := range groups {
		if len(g) < plateauTrustRuns || days(g) < 2 {
			continue
		}
		trusted = append(trusted, Plateau{Direction: s.Direction, Kind: s.Kind, TempBandC: s.TempBandC, Value: groupValue(g),
			Unit: unit(s.Kind), Seen: len(g), SinceMs: g[0].StartMs, LastMs: g[len(g)-1].EndMs})
	}
	if len(trusted) == 0 {
		return Plateau{}, false
	}
	sort.Slice(trusted, func(i, j int) bool { return trusted[i].LastMs > trusted[j].LastMs })
	current := trusted[0]
	if len(trusted) > 1 {
		earlier := trusted[1]
		current.Earlier = &earlier
	}
	return current, true
}

func groupValue(g []PlateauRun) float64 {
	v := make([]float64, len(g))
	for i, r := range g {
		v[i] = r.Median
	}
	m, _ := middle(v)
	return m
}

// days counts the days a group of runs (sorted by start) was seen on: a run
// starting plateauDayApart after the last counted day starts a new one.
func days(g []PlateauRun) int {
	n, anchor := 0, int64(0)
	for _, r := range g {
		if n == 0 || r.StartMs-anchor >= plateauDayApart.Milliseconds() {
			n, anchor = n+1, r.StartMs
		}
	}
	return n
}

// middle returns the median and the interquartile range.
func middle(v []float64) (median, iqr float64) {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	q := func(f float64) float64 {
		x := f * float64(len(s)-1)
		lo := int(math.Floor(x))
		hi := min(lo+1, len(s)-1)
		return s[lo] + (x-float64(lo))*(s[hi]-s[lo])
	}
	return q(0.5), q(0.75) - q(0.25)
}

func near(v, ref float64, kind string) bool {
	return math.Abs(v-ref) <= math.Max(plateauNearShare*math.Abs(ref), floor(kind))
}

func floor(kind string) float64 {
	if kind == LimitBatteryCurrent {
		return plateauFloorA
	}
	return plateauFloorW
}

func unit(kind string) string {
	if kind == LimitBatteryCurrent {
		return "A"
	}
	return "W"
}

func directionOf(charge bool) string {
	if charge {
		return "charge"
	}
	return "discharge"
}

func tempBand(c *float64) *int {
	if c == nil || !finite(*c) {
		return nil
	}
	b := int(math.Floor(*c/plateauTempBandC)) * plateauTempBandC
	return &b
}

func sameBand(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// PlateauView is what GET /api/battery_models shows: the trusted plateaus,
// each with the one it replaced, and the runs behind them.
type PlateauView struct {
	Known  []Plateau       `json:"known"`
	Series []PlateauSeries `json:"series"`
}

// View copies the plateaus for the API.
func (p *Plateaus) View() PlateauView {
	v := PlateauView{Known: p.Known(), Series: []PlateauSeries{}}
	if p == nil {
		return v
	}
	for _, s := range p.Series {
		s.Runs = append([]PlateauRun(nil), s.Runs...)
		v.Series = append(v.Series, s)
	}
	return v
}
