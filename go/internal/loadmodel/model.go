// Package loadmodel maintains the existing local household-load predictor.
package loadmodel

import (
	"math"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
)

const Buckets = 7 * 24

const MinTrustSamples = 8

const HeatingReferenceC = 18.0

const HeatingAlpha = 0.01

const HeatingMinDeltaT = 3.0

const HeatingCoefMaxW = 1500.0

const PlausibleLoadHeadroom = 1.25

type Profile string

const (
	ProfileHome Profile = "home"
	ProfileAway Profile = "away"
)

const awayPriorScale = 0.25

func Profiles() []Profile {
	return []Profile{ProfileHome, ProfileAway}
}

func (p Profile) valid() bool {
	switch p {
	case ProfileHome, ProfileAway:
		return true
	default:
		return false
	}
}

type Bucket struct {
	Mean         float64 `json:"mean"` // Day-weighted observed load (W)
	Samples      int64   `json:"samples"`
	Days         int64   `json:"days"`
	LastDay      string  `json:"last_day"`
	LastMs       int64   `json:"last_ms"`
	DaySum       float64 `json:"day_sum"`
	DaySamples   int64   `json:"day_samples"`
	PreviousMean float64 `json:"previous_mean"`
}

type Model struct {
	ConfigRevision    string          `json:"config_revision,omitempty"`
	LearningStartedMS int64           `json:"learning_started_ms,omitempty"`
	Timezone          string          `json:"timezone,omitempty"`
	LastTemperatureC  float64         `json:"last_temperature_c"`
	HasTemperature    bool            `json:"has_temperature"`
	Bucket            [Buckets]Bucket `json:"bucket"`
	HeatingW_per_degC float64         `json:"heating_w_per_degc"`
	PeakW             float64         `json:"peak_w"`
	Samples           int64           `json:"samples"`
	LastMs            int64           `json:"last_ms"`
	MAE               float64         `json:"mae"`
	Alpha             float64         `json:"alpha"` // Retained for state compatibility
	PriorScale        float64         `json:"prior_scale,omitempty"`

	// MaxPlausibleW remains for stored-state/API compatibility. Grid limits do not cap gross household load.
	MaxPlausibleW float64 `json:"max_plausible_w,omitempty"`
}

func typicalPrior(hourOfWeek int) float64 {
	weekday := hourOfWeek / 24
	hour := hourOfWeek % 24
	isWeekend := weekday >= 5 // Saturday (5), Sunday (6)
	base := 650.0             // overnight baseload — typical Swedish house today, not 2010
	morning := 2000.0 * math.Exp(-0.5*math.Pow(float64(hour-7)/1.2, 2))
	midday := 600.0 * math.Exp(-0.5*math.Pow(float64(hour-13)/2.5, 2))
	eveningH := 18.5
	if isWeekend {
		eveningH = 19.0
		morning *= 0.7 // sleep-in
	}
	evening := 2500.0 * math.Exp(-0.5*math.Pow((float64(hour)-eveningH)/1.3, 2))
	return base + morning + midday + evening
}

func NewModel(peakW float64) *Model {
	return newModel(peakW, 1)
}

func newProfileModel(peakW float64, profile Profile) *Model {
	scale := 1.0
	if profile == ProfileAway {
		scale = awayPriorScale
	}
	return newModel(peakW, scale)
}

func newModel(peakW, priorScale float64) *Model {
	m := &Model{
		PeakW:      peakW,
		Alpha:      0.1, // new sample gets 10% weight in EMA
		PriorScale: priorScale,
	}
	if m.PeakW <= 0 {
		m.PeakW = 5000
	}
	for i := 0; i < Buckets; i++ {
		m.Bucket[i].Mean = m.prior(i)
		m.Bucket[i].Samples = 0
	}
	return m
}

func (m Model) prior(hourOfWeek int) float64 {
	scale := m.PriorScale
	if scale <= 0 {
		scale = 1
	}
	return typicalPrior(hourOfWeek) * scale
}

const poisonFloor = 0.25

func (m *Model) repairPoisonedBuckets() {
	for i := range m.Bucket {
		if math.IsNaN(m.Bucket[i].Mean) || math.IsInf(m.Bucket[i].Mean, 0) || m.Bucket[i].Mean < 0 {
			m.Bucket[i] = Bucket{Mean: m.prior(i)}
		}
	}
}
func (m Model) localTime(t time.Time) time.Time {
	return t.In(siteLocation(m.Timezone))
}
func (m Model) hourOfWeek(t time.Time) int {
	t = m.localTime(t)
	return ((int(t.Weekday())+6)%7)*24 + t.Hour()
}

// HourOfWeek retains UTC indexing for older external callers. Model methods use their stored site timezone.
func HourOfWeek(t time.Time) int {
	u := t.UTC()
	wd := (int(u.Weekday()) + 6) % 7
	return wd*24 + u.Hour()
}

func heatingGain(coefWPerDegC, tempC float64) float64 {
	if tempC >= HeatingReferenceC {
		return 0
	}
	return coefWPerDegC * (HeatingReferenceC - tempC)
}

const featureSemantics = "loadmodel/2 independent_days local_site_clock raw_complete_balance load=site_w_less_pv_bat_ev_v2x temp=outdoor_c target=house_w"

func featureProbe() []float64 {
	out := []float64{float64(Buckets), HeatingReferenceC}
	zone := time.FixedZone("probe", 2*60*60)
	for _, t := range []time.Time{
		time.Date(2024, 1, 1, 0, 30, 0, 0, time.UTC),
		time.Date(2024, 3, 31, 1, 30, 0, 0, zone),
		time.Date(2024, 6, 21, 23, 45, 0, 0, zone),
		time.Date(2024, 10, 27, 0, 15, 0, 0, zone),
		time.Date(2024, 12, 24, 18, 0, 0, 0, time.UTC),
	} {
		out = append(out, float64(HourOfWeek(t)))
	}
	for _, tempC := range []float64{-20, -3, 0, 10, 17.5, 18, 25} {
		out = append(out, heatingGain(1, tempC))
	}
	return out
}

var featureHash = sync.OnceValue(func() string {
	return modelstate.Fingerprint(featureSemantics, featureProbe())
})

func FeatureHash() string { return featureHash() }

// Predict uses site local time. NaN temperature means unknown and retains the last known heat estimate.
func (m Model) Predict(t time.Time, tempC float64) float64 {
	idx := m.hourOfWeek(t)
	b := m.Bucket[idx]
	trust := m.Coverage(t)
	prior := m.prior(idx)
	base := trust*b.Mean + (1-trust)*prior
	if math.IsNaN(tempC) || math.IsInf(tempC, 0) {
		if m.HasTemperature {
			tempC = m.LastTemperatureC
		} else {
			tempC = HeatingReferenceC
		}
	}
	y := base + heatingGain(m.HeatingW_per_degC, tempC)
	if math.IsNaN(y) || math.IsInf(y, 0) {
		y = prior
	}
	if y < 0 {
		return 0
	}
	return y
}

func (m Model) PredictNoTemp(t time.Time) float64 { return m.Predict(t, math.NaN()) }

// Update requires a valid complete electrical balance. Zero degrees is real weather; NaN is unknown.
func (m *Model) Update(t time.Time, actualLoadW, tempC float64) (updated bool) {
	if actualLoadW < 0 || math.IsNaN(actualLoadW) || math.IsInf(actualLoadW, 0) || (m.LastMs > 0 && t.UnixMilli() <= m.LastMs) {
		return false
	}

	idx := m.hourOfWeek(t)
	b := &m.Bucket[idx]
	if b.LastMs > 0 && t.UnixMilli() <= b.LastMs {
		return false
	}
	knownTemp := !math.IsNaN(tempC) && !math.IsInf(tempC, 0)
	if knownTemp {
		m.LastTemperatureC = tempC
		m.HasTemperature = true
	} else if m.HasTemperature {
		tempC = m.LastTemperatureC
	} else {
		tempC = HeatingReferenceC
	}
	predicted := m.Predict(t, tempC)
	err := actualLoadW - predicted

	if knownTemp && tempC < HeatingReferenceC-HeatingMinDeltaT && b.Days >= MinTrustSamples {
		deltaT := HeatingReferenceC - tempC
		elapsedHours := 1.0
		if m.LastMs > 0 {
			elapsedHours = math.Min(1, math.Max(0, t.Sub(time.UnixMilli(m.LastMs)).Hours()))
		}
		alpha := 1 - math.Pow(1-HeatingAlpha, elapsedHours)
		m.HeatingW_per_degC += alpha * err / deltaT
		if m.HeatingW_per_degC < 0 {
			m.HeatingW_per_degC = 0
		}
		if m.HeatingW_per_degC > HeatingCoefMaxW {
			m.HeatingW_per_degC = HeatingCoefMaxW
		}
	}

	heatEst := heatingGain(m.HeatingW_per_degC, tempC)
	if heatEst <= actualLoadW {
		baseSample := actualLoadW - heatEst
		day := m.localTime(t).Format("2006-01-02")
		if day != b.LastDay {
			b.Days++
			b.LastDay = day
			b.PreviousMean = b.Mean
			b.DaySum = 0
			b.DaySamples = 0
		}
		b.DaySum += baseSample
		b.DaySamples++
		b.Samples++
		// Average within a day before applying its weight to the weekly hour.
		// Polling more often cannot give that day more structural influence.
		weight := 1 / math.Min(float64(b.Days), 10)
		b.Mean = (1-weight)*b.PreviousMean + weight*b.DaySum/float64(b.DaySamples)
		b.LastMs = t.UnixMilli()
	}

	m.Samples++
	m.LastMs = t.UnixMilli()
	if m.Samples == 1 {
		m.MAE = math.Abs(err)
	} else {
		m.MAE = 0.99*m.MAE + 0.01*math.Abs(err)
	}
	return true
}

func (m Model) Quality() float64 {
	if m.PeakW <= 0 {
		return 0
	}
	var warm int
	for i := 0; i < Buckets; i++ {
		if m.Bucket[i].Days >= MinTrustSamples {
			warm++
		}
	}
	coverage := float64(warm) / float64(Buckets)
	accuracy := 0.0
	if m.Samples > 0 {
		rel := m.MAE / m.PeakW
		if rel <= 0.05 {
			accuracy = 1.0
		} else if rel < 0.5 {
			accuracy = 1 - (rel-0.05)/0.45
		}
	}
	return coverage * accuracy
}

var locationCache sync.Map

func siteLocation(zone string) *time.Location {
	if loc, ok := locationCache.Load(zone); ok {
		return loc.(*time.Location)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	locationCache.Store(zone, loc)
	return loc
}

// Coverage measures independent local days at this hour. Unseen seasons lose
// trust after thirty days without observations; training error alone is not trust.
func (m Model) Coverage(t time.Time) float64 {
	b := m.Bucket[m.hourOfWeek(t)]
	trust := math.Min(1, float64(b.Days)/MinTrustSamples)
	if b.LastMs > 0 {
		ageDays := t.Sub(time.UnixMilli(b.LastMs)).Hours() / 24
		trust *= math.Exp(-math.Max(0, ageDays-30) / 60)
	}
	return trust
}
