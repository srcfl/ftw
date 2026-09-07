package forecasting

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/sunpos"
)

const hourMS = int64(time.Hour / time.Millisecond)

var cumulativeHours = [...]int{1, 3, 6, 12, 24}

// LeadBucket separates the dominant short-term and weather horizons.
func LeadBucket(origin, start int64) int {
	h := float64(start-origin) / float64(hourMS)
	switch {
	case h < 1:
		return 0
	case h < 3:
		return 1
	case h < 6:
		return 2
	case h < 12:
		return 3
	case h < 24:
		return 4
	default:
		return 5
	}
}

type ErrorSample struct {
	Series        string  `json:"series"`
	ConfigVersion string  `json:"config_version"`
	IssueID       string  `json:"issue_id"`
	OriginMS      int64   `json:"origin_ms"`
	IssuedAtMS    int64   `json:"issued_at_ms"`
	StartMS       int64   `json:"start_ms"`
	EndMS         int64   `json:"end_ms"`
	AvailableAtMS int64   `json:"available_at_ms"`
	Lead          int     `json:"lead_bucket"`
	PVErrorW      float64 `json:"pv_error_w"`
	LoadErrorW    float64 `json:"load_error_w"`
	PVKnown       bool    `json:"pv_known"`
	Daylight      bool    `json:"daylight"`
	LoadKnown     bool    `json:"load_known"`
	Prediction    Point   `json:"prediction"`
}

func (e ErrorSample) Validate() error {
	if e.Series == "" || len(e.Series) > 80 || e.ConfigVersion == "" || len(e.ConfigVersion) > 128 ||
		e.IssueID == "" || len(e.IssueID) > 128 || e.OriginMS <= 0 || e.IssuedAtMS < e.OriginMS ||
		e.IssuedAtMS > e.StartMS || !validInterval(e.StartMS, e.EndMS) || e.AvailableAtMS < e.EndMS ||
		e.Lead != LeadBucket(e.OriginMS, e.StartMS) || !finite(e.PVErrorW) || !finite(e.LoadErrorW) ||
		!validPoint(e.Prediction) || e.Prediction.StartMS != e.StartMS || e.Prediction.EndMS != e.EndMS {
		return errors.New("invalid forecast error sample")
	}
	return nil
}

type observationIndex map[string]map[int64][]Observation

func indexObservations(observations []Observation, asOf int64) observationIndex {
	index := make(observationIndex)
	for _, o := range observations {
		if o.Validate() != nil || o.AvailableAtMS > asOf {
			continue
		}
		byStart := index[o.ConfigVersion]
		if byStart == nil {
			byStart = make(map[int64][]Observation)
			index[o.ConfigVersion] = byStart
		}
		byStart[o.StartMS] = append(byStart[o.StartMS], o)
	}
	for _, byStart := range index {
		for start := range byStart {
			sort.Slice(byStart[start], func(i, j int) bool {
				a, b := byStart[start][i], byStart[start][j]
				if a.EndMS != b.EndMS {
					return a.EndMS < b.EndMS
				}
				if a.AvailableAtMS != b.AvailableAtMS {
					return a.AvailableAtMS < b.AvailableAtMS
				}
				if a.PVW != b.PVW {
					return a.PVW < b.PVW
				}
				return a.LoadW < b.LoadW
			})
		}
	}
	return index
}

type coveredObservation struct {
	pvWeighted   float64
	loadWeighted float64
	pvKnown      bool
	loadKnown    bool
	available    int64
}

func coverPoint(index observationIndex, config string, point Point) (coveredObservation, bool) {
	byStart := index[config]
	if byStart == nil {
		return coveredObservation{}, false
	}
	type result struct {
		coverage coveredObservation
		ok       bool
	}
	memo := make(map[int64]result)
	var cover func(int64) result
	cover = func(cursor int64) result {
		if cursor == point.EndMS {
			return result{coverage: coveredObservation{pvKnown: true, loadKnown: true}, ok: true}
		}
		if cached, ok := memo[cursor]; ok {
			return cached
		}
		for _, o := range byStart[cursor] {
			if o.EndMS > point.EndMS {
				continue
			}
			tail := cover(o.EndMS)
			if !tail.ok {
				continue
			}
			duration := float64(o.EndMS - o.StartMS)
			coverage := tail.coverage
			coverage.pvWeighted += o.PVW * duration
			coverage.loadWeighted += o.LoadW * duration
			coverage.pvKnown = coverage.pvKnown && o.PVKnown
			coverage.loadKnown = coverage.loadKnown && o.LoadKnown
			coverage.available = max(coverage.available, o.AvailableAtMS)
			memo[cursor] = result{coverage: coverage, ok: true}
			return memo[cursor]
		}
		memo[cursor] = result{}
		return memo[cursor]
	}
	r := cover(point.StartMS)
	return r.coverage, r.ok
}

func scoreAll(issues []Issue, observations []Observation, asOf int64) []ErrorSample {
	index := indexObservations(observations, asOf)
	out := make([]ErrorSample, 0)
	for _, issue := range issues {
		if issue.IssuedAtMS > asOf || issue.Validate() != nil {
			continue
		}
		for _, series := range issue.Series {
			for _, p := range series.Points {
				if p.StartMS < issue.IssuedAtMS || p.EndMS > asOf {
					continue
				}
				coverage, ok := coverPoint(index, issue.ConfigVersion, p)
				if !ok {
					continue
				}
				duration := float64(p.EndMS - p.StartMS)
				pvKnown := p.PVKnown && coverage.pvKnown
				loadKnown := p.LoadKnown && coverage.loadKnown
				if !pvKnown && !loadKnown {
					continue
				}
				e := ErrorSample{
					Series:        series.Name,
					ConfigVersion: issue.ConfigVersion,
					IssueID:       issue.ID,
					OriginMS:      issue.OriginMS,
					IssuedAtMS:    issue.IssuedAtMS,
					StartMS:       p.StartMS,
					EndMS:         p.EndMS,
					AvailableAtMS: coverage.available,
					Lead:          LeadBucket(issue.OriginMS, p.StartMS),
					PVErrorW:      coverage.pvWeighted/duration - p.PVW,
					LoadErrorW:    coverage.loadWeighted/duration - p.LoadW,
					PVKnown:       pvKnown,
					Daylight:      intervalDaylight(issue, p.StartMS, p.EndMS),
					LoadKnown:     loadKnown,
					Prediction:    p,
				}
				if e.Validate() == nil {
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// intervalDaylight never uses the forecast or observed PV power to choose
// evaluation rows: a daytime zero forecast must still count as an error.
func intervalDaylight(issue Issue, start, end int64) bool {
	midpoint := start + (end-start)/2
	if site := issue.Site; site != nil && site.HasLocation {
		return sunpos.At(time.UnixMilli(midpoint), site.Latitude, site.Longitude).ZenithDeg < 90
	}
	for _, w := range issue.Weather {
		if midpoint >= w.StartMS && midpoint < w.EndMS && w.GHIWm2 != nil && *w.GHIWm2 > 20 {
			return true
		}
	}
	return false
}

func laterIssue(a, b ErrorSample) bool {
	if a.OriginMS != b.OriginMS {
		return a.OriginMS > b.OriginMS
	}
	if a.IssuedAtMS != b.IssuedAtMS {
		return a.IssuedAtMS > b.IssuedAtMS
	}
	return a.IssueID > b.IssueID
}

// Errors scores only forecasts available before the entire target interval,
// and outcomes available by asOf. Partial or gapped truth is not interpolated.
// Per target/lead bucket, the latest eligible issue wins: repeated replans
// cannot create independent evidence from the same outcome.
func Errors(issues []Issue, observations []Observation, asOf int64) []ErrorSample {
	type key struct {
		series, config string
		start, end     int64
		lead           int
	}
	chosen := make(map[key]ErrorSample)
	for _, e := range scoreAll(issues, observations, asOf) {
		k := key{e.Series, e.ConfigVersion, e.StartMS, e.EndMS, e.Lead}
		if old, ok := chosen[k]; !ok || laterIssue(e, old) {
			chosen[k] = e
		}
	}
	out := make([]ErrorSample, 0, len(chosen))
	for _, e := range chosen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.StartMS != b.StartMS {
			return a.StartMS < b.StartMS
		}
		if a.Series != b.Series {
			return a.Series < b.Series
		}
		if a.ConfigVersion != b.ConfigVersion {
			return a.ConfigVersion < b.ConfigVersion
		}
		if a.Lead != b.Lead {
			return a.Lead < b.Lead
		}
		return a.IssueID < b.IssueID
	})
	return out
}

type Metric struct {
	Series      string  `json:"series"`
	Signal      string  `json:"signal"`
	Lead        int     `json:"lead_bucket"`
	Samples     int     `json:"samples"`
	Days        int     `json:"days"`
	MAEW        float64 `json:"mae_w"`
	BiasW       float64 `json:"bias_w"`
	EnergyMAEWh float64 `json:"interval_energy_mae_wh"`
	BandSamples int     `json:"band_samples"`
	Coverage80  float64 `json:"coverage_80"`
	Pinball80   float64 `json:"pinball_10_90"`
}

func pinball(err, q float64) float64 {
	if err >= 0 {
		return q * err
	}
	return (q - 1) * err
}

func signalError(e ErrorSample, signal string) (err float64, known bool, band Band, prediction float64) {
	switch signal {
	case "pv":
		return e.PVErrorW, e.PVKnown, e.Prediction.PVBand, e.Prediction.PVW
	case "pv_daylight":
		return e.PVErrorW, e.PVKnown && e.Daylight, e.Prediction.PVBand, e.Prediction.PVW
	case "load":
		return e.LoadErrorW, e.LoadKnown, e.Prediction.LoadBand, e.Prediction.LoadW
	case "net":
		return e.LoadErrorW - e.PVErrorW, e.LoadKnown && e.PVKnown, e.Prediction.NetBand, e.Prediction.LoadW - e.Prediction.PVW
	default:
		return 0, false, Band{}, 0
	}
}

func Metrics(samples []ErrorSample) []Metric {
	type accumulator struct {
		metric Metric
		days   map[int64]bool
	}
	all := make(map[string]*accumulator)
	for _, e := range samples {
		if e.Validate() != nil {
			continue
		}
		for _, signal := range []string{"pv", "pv_daylight", "load", "net"} {
			err, known, band, prediction := signalError(e, signal)
			if !known {
				continue
			}
			key := e.Series + "/" + signal + "/" + string(rune('0'+e.Lead))
			a := all[key]
			if a == nil {
				a = &accumulator{metric: Metric{Series: e.Series, Signal: signal, Lead: e.Lead}, days: map[int64]bool{}}
				all[key] = a
			}
			m := &a.metric
			m.Samples++
			m.MAEW += math.Abs(err)
			m.BiasW += err
			m.EnergyMAEWh += math.Abs(err) * float64(e.EndMS-e.StartMS) / 3.6e6
			a.days[e.StartMS/(24*hourMS)] = true
			if band.Method == BandMethodEmpirical {
				m.BandSamples++
				actual := prediction + err
				if actual >= band.LowW && actual <= band.HighW {
					m.Coverage80++
				}
				m.Pinball80 += pinball(actual-band.LowW, .1) + pinball(actual-band.HighW, .9)
			}
		}
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Metric, 0, len(keys))
	for _, k := range keys {
		a := all[k]
		m := a.metric
		n := float64(m.Samples)
		m.MAEW /= n
		m.BiasW /= n
		m.EnergyMAEWh /= n
		m.Days = len(a.days)
		if m.BandSamples > 0 {
			m.Coverage80 /= float64(m.BandSamples)
			m.Pinball80 /= float64(m.BandSamples)
		}
		out = append(out, m)
	}
	return out
}

type calibrationKey struct {
	series   string
	lead     int
	duration int64
	signal   string
}

type calibrationCell struct {
	values  []float64
	samples int
	days    int
}

// Calibrator is an immutable, bounded snapshot of the residuals that were
// available at one forecast origin. Band lookups do not scan or sort history.
type Calibrator struct {
	origin int64
	cells  map[calibrationKey]calibrationCell
}

// NewCalibrator keeps at most eight recently active series and 256
// hour-aligned targets per series, lead, duration and signal. Replans for one
// target count once. Different interval lengths have separate error spreads.
func NewCalibrator(history []ErrorSample, config string, origin int64) *Calibrator {
	type targetKey struct {
		series   string
		lead     int
		duration int64
		start    int64
	}
	byTarget := make(map[targetKey]ErrorSample)
	lastBySeries := make(map[string]int64)
	for _, e := range history {
		if e.Validate() != nil || e.ConfigVersion != config || e.AvailableAtMS > origin ||
			e.EndMS > origin || e.StartMS%hourMS != 0 {
			continue
		}
		key := targetKey{e.Series, e.Lead, e.EndMS - e.StartMS, e.StartMS}
		if old, ok := byTarget[key]; !ok || laterIssue(e, old) {
			byTarget[key] = e
		}
		if e.StartMS > lastBySeries[e.Series] {
			lastBySeries[e.Series] = e.StartMS
		}
	}
	series := make([]string, 0, len(lastBySeries))
	for name := range lastBySeries {
		series = append(series, name)
	}
	sort.Slice(series, func(i, j int) bool {
		if lastBySeries[series[i]] != lastBySeries[series[j]] {
			return lastBySeries[series[i]] > lastBySeries[series[j]]
		}
		return series[i] < series[j]
	})
	if len(series) > MaxSeries {
		series = series[:MaxSeries]
	}
	allowed := make(map[string]bool, len(series))
	for _, name := range series {
		allowed[name] = true
	}
	type residual struct {
		start int64
		value float64
	}
	residuals := make(map[calibrationKey][]residual)
	for key, e := range byTarget {
		if !allowed[key.series] {
			continue
		}
		for _, signal := range []string{"pv", "load", "net"} {
			value, known, _, _ := signalError(e, signal)
			if known {
				cellKey := calibrationKey{key.series, key.lead, key.duration, signal}
				residuals[cellKey] = append(residuals[cellKey], residual{key.start, value})
			}
		}
	}
	cells := make(map[calibrationKey]calibrationCell, len(residuals))
	for key, values := range residuals {
		sort.Slice(values, func(i, j int) bool { return values[i].start > values[j].start })
		if len(values) > 256 {
			values = values[:256]
		}
		days := make(map[int64]bool)
		sortedValues := make([]float64, len(values))
		for i, value := range values {
			days[value.start/(24*hourMS)] = true
			sortedValues[i] = value.value
		}
		sort.Float64s(sortedValues)
		cells[key] = calibrationCell{values: sortedValues, samples: len(sortedValues), days: len(days)}
	}
	return &Calibrator{origin: origin, cells: cells}
}

// Band is the one-hour compatibility form of BandForInterval.
func (c *Calibrator) Band(series, signal string, start int64, prediction float64) Band {
	return c.BandForInterval(series, signal, start, start+hourMS, prediction)
}

// BandForInterval returns a calibrated band for one target interval. The
// cold-start width still depends on this target's prediction and lead time.
func (c *Calibrator) BandForInterval(series, signal string, start, end int64, prediction float64) Band {
	lead := LeadBucket(c.origin, start)
	cell := c.cells[calibrationKey{series, lead, end - start, signal}]
	b := Band{Samples: cell.samples, Days: cell.days}
	if cell.samples < 48 || cell.days < 7 {
		width := math.Max(250, math.Abs(prediction)*(.35+.05*float64(lead)))
		b.LowW = prediction - width
		b.HighW = prediction + width
		b.Method = BandMethodColdStart
	} else {
		b.LowW = prediction + cell.values[int(math.Floor(.1*float64(cell.samples-1)))]
		b.HighW = prediction + cell.values[int(math.Ceil(.9*float64(cell.samples-1)))]
		b.Method = BandMethodEmpirical
	}
	if signal != "net" {
		b.LowW = math.Max(0, b.LowW)
		b.HighW = math.Max(0, b.HighW)
	}
	return b
}

// Calibrate is the compatibility wrapper for one-off callers. Issue creation
// should build one Calibrator and reuse it across all series and slots.
func Calibrate(history []ErrorSample, series, config, signal string, origin, start int64, prediction float64) Band {
	return NewCalibrator(history, config, origin).Band(series, signal, start, prediction)
}

// CumulativeEnergySample is one net-load energy error from a single issue.
// Each interval in the window has complete load and available-PV truth.
type CumulativeEnergySample struct {
	Series        string  `json:"series"`
	ConfigVersion string  `json:"config_version"`
	IssueID       string  `json:"issue_id"`
	OriginMS      int64   `json:"origin_ms"`
	IssuedAtMS    int64   `json:"issued_at_ms"`
	StartMS       int64   `json:"start_ms"`
	EndMS         int64   `json:"end_ms"`
	AvailableAtMS int64   `json:"available_at_ms"`
	Lead          int     `json:"lead_bucket"`
	Hours         int     `json:"hours"`
	NetErrorWh    float64 `json:"net_error_wh"`
}

func (s CumulativeEnergySample) valid() bool {
	validHours := false
	for _, hours := range cumulativeHours {
		validHours = validHours || s.Hours == hours
	}
	return s.Series != "" && s.ConfigVersion != "" && s.IssueID != "" && s.OriginMS > 0 &&
		s.IssuedAtMS >= s.OriginMS && s.IssuedAtMS <= s.StartMS && s.AvailableAtMS >= s.EndMS &&
		s.EndMS-s.StartMS == int64(s.Hours)*hourMS && s.Lead == LeadBucket(s.OriginMS, s.StartMS) &&
		validHours && finite(s.NetErrorWh)
}

// CumulativeNetEnergy returns contiguous 1, 3, 6, 12 and 24 hour net-energy
// errors. A window always comes from one issue and starts on a UTC hour.
func CumulativeNetEnergy(issues []Issue, observations []Observation, asOf int64) []CumulativeEnergySample {
	type groupKey struct {
		issue, series, config string
		origin, issued        int64
	}
	groups := make(map[groupKey][]ErrorSample)
	for _, e := range scoreAll(issues, observations, asOf) {
		key := groupKey{e.IssueID, e.Series, e.ConfigVersion, e.OriginMS, e.IssuedAtMS}
		groups[key] = append(groups[key], e)
	}
	type windowKey struct {
		series, config string
		start, end     int64
		lead, hours    int
	}
	chosen := make(map[windowKey]CumulativeEnergySample)
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool { return group[i].StartMS < group[j].StartMS })
		for i := range group {
			first := group[i]
			if first.StartMS%hourMS != 0 {
				continue
			}
			for _, hours := range cumulativeHours {
				targetEnd := first.StartMS + int64(hours)*hourMS
				cursor := first.StartMS
				available := int64(0)
				errorWh := 0.0
				ok := true
				for j := i; cursor < targetEnd; j++ {
					if j >= len(group) || group[j].StartMS != cursor || group[j].EndMS > targetEnd ||
						!group[j].PVKnown || !group[j].LoadKnown {
						ok = false
						break
					}
					e := group[j]
					errorWh += (e.LoadErrorW - e.PVErrorW) * float64(e.EndMS-e.StartMS) / 3.6e6
					available = max(available, e.AvailableAtMS)
					cursor = e.EndMS
				}
				if !ok || cursor != targetEnd || !finite(errorWh) {
					continue
				}
				sample := CumulativeEnergySample{
					Series: first.Series, ConfigVersion: first.ConfigVersion, IssueID: first.IssueID,
					OriginMS: first.OriginMS, IssuedAtMS: first.IssuedAtMS, StartMS: first.StartMS,
					EndMS: targetEnd, AvailableAtMS: available, Lead: first.Lead, Hours: hours, NetErrorWh: errorWh,
				}
				k := windowKey{first.Series, first.ConfigVersion, first.StartMS, targetEnd, first.Lead, hours}
				old, exists := chosen[k]
				if !exists || sample.OriginMS > old.OriginMS ||
					(sample.OriginMS == old.OriginMS && (sample.IssuedAtMS > old.IssuedAtMS ||
						(sample.IssuedAtMS == old.IssuedAtMS && sample.IssueID > old.IssueID))) {
					chosen[k] = sample
				}
			}
		}
	}
	out := make([]CumulativeEnergySample, 0, len(chosen))
	for _, sample := range chosen {
		out = append(out, sample)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.StartMS != b.StartMS {
			return a.StartMS < b.StartMS
		}
		if a.Series != b.Series {
			return a.Series < b.Series
		}
		if a.Hours != b.Hours {
			return a.Hours < b.Hours
		}
		return a.Lead < b.Lead
	})
	return out
}

type CumulativeMetric struct {
	Series        string  `json:"series"`
	ConfigVersion string  `json:"config_version"`
	Lead          int     `json:"lead_bucket"`
	Hours         int     `json:"hours"`
	Samples       int     `json:"samples"`
	Days          int     `json:"days"`
	MAEWh         float64 `json:"mae_wh"`
	BiasWh        float64 `json:"bias_wh"`
}

// CumulativeMetrics summarizes the matched-origin cumulative samples without
// mixing site config versions, lead buckets or horizons.
func CumulativeMetrics(samples []CumulativeEnergySample) []CumulativeMetric {
	type key struct {
		series, config string
		lead, hours    int
	}
	type accumulator struct {
		metric CumulativeMetric
		days   map[int64]bool
	}
	all := make(map[key]*accumulator)
	for _, sample := range samples {
		if !sample.valid() {
			continue
		}
		k := key{sample.Series, sample.ConfigVersion, sample.Lead, sample.Hours}
		a := all[k]
		if a == nil {
			a = &accumulator{metric: CumulativeMetric{
				Series: sample.Series, ConfigVersion: sample.ConfigVersion, Lead: sample.Lead, Hours: sample.Hours,
			}, days: map[int64]bool{}}
			all[k] = a
		}
		a.metric.Samples++
		a.metric.MAEWh += math.Abs(sample.NetErrorWh)
		a.metric.BiasWh += sample.NetErrorWh
		a.days[sample.StartMS/(24*hourMS)] = true
	}
	keys := make([]key, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.series != b.series {
			return a.series < b.series
		}
		if a.config != b.config {
			return a.config < b.config
		}
		if a.hours != b.hours {
			return a.hours < b.hours
		}
		return a.lead < b.lead
	})
	out := make([]CumulativeMetric, 0, len(keys))
	for _, k := range keys {
		a := all[k]
		n := float64(a.metric.Samples)
		a.metric.MAEWh /= n
		a.metric.BiasWh /= n
		a.metric.Days = len(a.days)
		out = append(out, a.metric)
	}
	return out
}

type PairMetric struct {
	Champion         string  `json:"champion"`
	Candidate        string  `json:"candidate"`
	ConfigVersion    string  `json:"config_version"`
	Signal           string  `json:"signal"`
	Lead             int     `json:"lead_bucket"`
	Samples          int     `json:"samples"`
	Days             int     `json:"days"`
	ChampionMAEW     float64 `json:"champion_mae_w"`
	CandidateMAEW    float64 `json:"candidate_mae_w"`
	DeltaMAEW        float64 `json:"candidate_minus_champion_mae_w"`
	CandidateWinRate float64 `json:"candidate_win_rate"`
}

func sameActual(a, b ErrorSample, signal string) bool {
	aErr, aKnown, _, aPrediction := signalError(a, signal)
	bErr, bKnown, _, bPrediction := signalError(b, signal)
	if !aKnown || !bKnown {
		return false
	}
	aActual, bActual := aPrediction+aErr, bPrediction+bErr
	tolerance := 1e-9 * math.Max(1, math.Max(math.Abs(aActual), math.Abs(bActual)))
	return math.Abs(aActual-bActual) <= tolerance
}

// CompareSeries scores champion and candidate only on common targets with the
// same config and lead bucket. Mismatched outcomes are excluded.
func CompareSeries(samples []ErrorSample, champion, candidate string) []PairMetric {
	type targetKey struct {
		config     string
		start, end int64
		lead       int
	}
	championByTarget := make(map[targetKey]ErrorSample)
	candidateByTarget := make(map[targetKey]ErrorSample)
	for _, e := range samples {
		if e.Validate() != nil || (e.Series != champion && e.Series != candidate) {
			continue
		}
		key := targetKey{e.ConfigVersion, e.StartMS, e.EndMS, e.Lead}
		target := championByTarget
		if e.Series == candidate {
			target = candidateByTarget
		}
		if old, ok := target[key]; !ok || laterIssue(e, old) {
			target[key] = e
		}
	}
	type accumulator struct {
		metric PairMetric
		days   map[int64]bool
		wins   float64
	}
	all := make(map[string]*accumulator)
	for key, championSample := range championByTarget {
		candidateSample, ok := candidateByTarget[key]
		if !ok {
			continue
		}
		for _, signal := range []string{"pv", "pv_daylight", "load", "net"} {
			if !sameActual(championSample, candidateSample, signal) {
				continue
			}
			championError, _, _, _ := signalError(championSample, signal)
			candidateError, _, _, _ := signalError(candidateSample, signal)
			accKey := key.config + "/" + signal + "/" + string(rune('0'+key.lead))
			a := all[accKey]
			if a == nil {
				a = &accumulator{metric: PairMetric{
					Champion: champion, Candidate: candidate, ConfigVersion: key.config, Signal: signal, Lead: key.lead,
				}, days: map[int64]bool{}}
				all[accKey] = a
			}
			a.metric.Samples++
			a.metric.ChampionMAEW += math.Abs(championError)
			a.metric.CandidateMAEW += math.Abs(candidateError)
			a.days[key.start/(24*hourMS)] = true
			switch {
			case math.Abs(candidateError) < math.Abs(championError):
				a.wins++
			case math.Abs(candidateError) == math.Abs(championError):
				a.wins += .5
			}
		}
	}
	keys := make([]string, 0, len(all))
	for key := range all {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]PairMetric, 0, len(keys))
	for _, key := range keys {
		a := all[key]
		n := float64(a.metric.Samples)
		a.metric.ChampionMAEW /= n
		a.metric.CandidateMAEW /= n
		a.metric.DeltaMAEW = a.metric.CandidateMAEW - a.metric.ChampionMAEW
		a.metric.CandidateWinRate = a.wins / n
		a.metric.Days = len(a.days)
		out = append(out, a.metric)
	}
	return out
}
