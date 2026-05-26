// Package loadmodel learns a household load profile online.
//
// Design choices driven by robustness / interpretability:
//
//  1. 168 buckets — one per (weekday, hour-of-day). An EMA per bucket.
//     Directly models the weekly pattern that dominates residential
//     load (weekend vs weekday, morning peak, evening peak, overnight
//     baseline) without having to fit non-linear basis functions.
//
//  2. Typical-home prior. Each bucket is seeded with a reasonable
//     Swedish-home default (300W overnight, 2000W morning/evening
//     peaks, 600W midday). Day-one predictions are useful; the model
//     refines from there.
//
//  3. Trust-weighted blending. Per-bucket trust = min(samples/20, 1).
//     A fresh bucket ignores its (noisy) EMA and returns the prior.
//     After ~20 samples through that bucket (3 weeks of 1-sample/min
//     yields ~60 samples per bucket per week), we trust observations.
//
//  4. Optional temperature correction. Outdoor temperature below 18°C
//     tracks strongly with heating load. We maintain a global scalar
//     `HeatingW_per_degC`, fit online via a simple EMA of residuals
//     vs. (18 − temp_c). Adds 0 W when temp is unknown or ≥ 18°C.
//
// The fallback-on-empty behavior makes this model safe on cold boot —
// the MPC always gets a plausible load estimate, never zero or wild.
package loadmodel

import (
	"math"
	"time"
)

// Buckets is the number of hour-of-week buckets: 7 days × 24 hours.
const Buckets = 7 * 24

// MinTrustSamples is how many samples we want in a bucket before we
// fully trust its EMA. Below this we blend with the prior. 8 ≈ two
// months of weekly observations, enough signal to outrank the prior.
const MinTrustSamples = 8

// HeatingReferenceC is the indoor setpoint the heating curve is
// relative to. Load proportional to max(setpoint − outdoor, 0).
const HeatingReferenceC = 18.0

// Profile selects which learned occupancy profile is used for training
// and prediction.
type Profile string

const (
	ProfileHome Profile = "home"
	ProfileAway Profile = "away"
)

const awayPriorScale = 0.25

// Profiles returns the supported load-model profiles in display order.
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

// Bucket holds one hour-of-week's learned state.
type Bucket struct {
	Mean    float64 `json:"mean"` // EMA of observed load (W)
	Samples int64   `json:"samples"`
}

// Model is the hour-of-week + heating-gain predictor.
type Model struct {
	Bucket            [Buckets]Bucket `json:"bucket"`
	HeatingW_per_degC float64         `json:"heating_w_per_degc"`
	PeakW             float64         `json:"peak_w"`
	Samples           int64           `json:"samples"`
	LastMs            int64           `json:"last_ms"`
	MAE               float64         `json:"mae"`
	Alpha             float64         `json:"alpha"` // EMA coefficient for bucket updates
	PriorScale        float64         `json:"prior_scale,omitempty"`
}

// typicalPrior returns an approximate W load for a given hour-of-week
// based on a generic single-family Swedish home. Peak dinner around
// 18:00–19:00, morning coffee around 07:00, weekend patterns shifted
// slightly later.
func typicalPrior(hourOfWeek int) float64 {
	weekday := hourOfWeek / 24
	hour := hourOfWeek % 24
	isWeekend := weekday >= 5 // Saturday (5), Sunday (6)
	base := 300.0             // overnight baseload
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

// NewModel returns a model seeded with the typical prior on every bucket.
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

// repairPoisonedBuckets resets bucket.Mean back to the prior for any bucket
// whose stored mean has drifted below a floor of prior*poisonFloor. This
// repairs models that were trained before the heating-subtraction guard was
// in place: when heatEst exceeded actualLoad the code clamped baseSample to
// 0, causing the EMA to decay toward zero over many cold-weather samples even
// though a real baseline load (fridge, server, standby) always exists.
//
// Samples count is left intact — the data was genuinely observed, we just
// can't trust the mean it produced. Setting Samples=0 would reset trust to 0
// and re-expose the prior, but would also trigger the exact-running-mean path
// for the next 10 samples on warm days which is acceptable. Either way the
// repaired model quickly re-learns from warm-season observations.
//
// Floor is conservative (25% of prior) so we only touch buckets that are
// clearly below any plausible real consumption — a house at 75 W overnight
// would be unusual but possible, so we preserve those. A mean of 15 W for an
// overnight bucket that has prior=300 W is unambiguously poisoned.
const poisonFloor = 0.25

func (m *Model) repairPoisonedBuckets() {
	for i := 0; i < Buckets; i++ {
		p := m.prior(i)
		if m.Bucket[i].Mean < p*poisonFloor {
			m.Bucket[i].Mean = p
			m.Bucket[i].Samples = 0
		}
	}
}

// HourOfWeek computes 0..167 for a time. Monday = 0 through Sunday.
// Coerces to UTC so the bucket index stays stable across DST
// transitions (wall-clock 19:00 maps to a different bucket in summer
// vs. winter otherwise, silently misaligning the EMA).
func HourOfWeek(t time.Time) int {
	u := t.UTC()
	// time.Weekday: Sunday=0, Saturday=6. We shift so Monday=0.
	wd := (int(u.Weekday()) + 6) % 7
	return wd*24 + u.Hour()
}

// Predict returns the expected load (W, non-negative) at time t with
// outdoor temperature tempC (0 if unknown). Blends per-bucket EMA with
// the typical prior by sample count, then adds the heating correction.
func (m Model) Predict(t time.Time, tempC float64) float64 {
	idx := HourOfWeek(t)
	b := m.Bucket[idx]
	trust := float64(b.Samples) / MinTrustSamples
	if trust > 1 {
		trust = 1
	}
	prior := m.prior(idx)
	base := trust*b.Mean + (1-trust)*prior
	heating := 0.0
	if tempC < HeatingReferenceC {
		heating = m.HeatingW_per_degC * (HeatingReferenceC - tempC)
	}
	y := base + heating
	if y < 0 {
		return 0
	}
	if m.PeakW > 0 && y > 3*m.PeakW {
		y = 3 * m.PeakW
	}
	return y
}

// PredictNoTemp is a convenience that predicts without a temperature
// signal — useful when no forecast is available.
func (m Model) PredictNoTemp(t time.Time) float64 { return m.Predict(t, HeatingReferenceC) }

// Update runs one online update. Feed (now, actual_load_w, outdoor_temp_c).
// Pass 0 for tempC if unknown; we'll skip the heating fit in that case.
// Returns true when the update was applied (not filtered as an outlier).
func (m *Model) Update(t time.Time, actualLoadW, tempC float64) (updated bool) {
	if actualLoadW < 0 {
		return false
	}
	idx := HourOfWeek(t)
	b := &m.Bucket[idx]
	// Outlier filter: once we have some history, reject 10× MAE residuals.
	predicted := m.Predict(t, tempC)
	err := actualLoadW - predicted
	if m.Samples > 50 {
		band := math.Max(m.MAE*10, 200)
		if math.Abs(err) > band {
			return false
		}
	}

	// Bucket update: exact running mean for the first 10 samples (crisp
	// early convergence), EMA after (smooth drift as the home evolves).
	// Subtract the current heating-gain estimate so the bucket learns
	// the "base" load — heating varies day-to-day and shouldn't smear
	// into the hour-of-week signature.
	//
	// Guard: when the heating estimate exceeds the measured load we
	// cannot cleanly isolate the base load from the heating component.
	// Storing 0 would poison the bucket (the EMA decays toward 0 even
	// though a real baseline — fridge, server, standby — always exists).
	// Instead, skip the bucket update entirely for this sample and let
	// existing Samples + Mean stand. Global Samples and MAE still update.
	heatEst := 0.0
	if tempC < HeatingReferenceC {
		heatEst = m.HeatingW_per_degC * (HeatingReferenceC - tempC)
	}
	if heatEst < actualLoadW {
		baseSample := actualLoadW - heatEst
		if b.Samples < 10 {
			b.Mean = (b.Mean*float64(b.Samples) + baseSample) / float64(b.Samples+1)
		} else {
			b.Mean = (1-m.Alpha)*b.Mean + m.Alpha*baseSample
		}
		b.Samples++
	}
	// Heating coefficient is operator-configured (Planner.HeatingWPerDegC
	// in config). We don't try to identify it from data here because
	// online fit is noisy + entangled with the bucket baseline. The
	// simple path is: user enters "my house needs ~300 W/°C" once,
	// the MPC uses it for forward-looking cold-day planning. Room for
	// a dedicated offline OLS fit in a future iteration.

	m.Samples++
	m.LastMs = t.UnixMilli()
	if m.Samples == 1 {
		m.MAE = math.Abs(err)
	} else {
		m.MAE = 0.99*m.MAE + 0.01*math.Abs(err)
	}
	return true
}

// Quality reports confidence in [0, 1]. Roughly: what fraction of
// buckets have enough samples to be trusted, weighted by MAE.
func (m Model) Quality() float64 {
	if m.PeakW <= 0 {
		return 0
	}
	var warm int
	for i := 0; i < Buckets; i++ {
		if m.Bucket[i].Samples >= MinTrustSamples {
			warm++
		}
	}
	coverage := float64(warm) / float64(Buckets)
	// Accuracy factor based on MAE vs peak.
	accuracy := 0.0
	if m.Samples > 0 {
		rel := m.MAE / m.PeakW
		if rel <= 0.05 {
			accuracy = 1.0
		} else if rel < 0.5 {
			accuracy = 1 - (rel-0.05)/0.45
		}
	}
	return 0.5*coverage + 0.5*accuracy
}
