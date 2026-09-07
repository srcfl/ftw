package pvmodel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// ResidualObservation is the portable inference state for the short forecast
// correction. Both powers are non-negative generation watts.
type ResidualObservation struct {
	At         time.Time `json:"at"`
	PredictedW float64   `json:"predicted_w"`
	ActualW    float64   `json:"actual_w"`
}

// ForecastSnapshot freezes the existing RLS model and its residual window for
// one issued horizon. Weather and clear-sky inputs come from the same caller's
// frozen forecast. Revision hashes inference state, excluding capture time.
type ForecastSnapshot struct {
	Model       Model                 `json:"model"`
	Residuals   []ResidualObservation `json:"residuals"`
	Revision    string                `json:"revision"`
	LatestInput time.Time             `json:"latest_input"`
	CapturedAt  time.Time             `json:"captured_at"`
}

func (s *Service) ForecastSnapshot() ForecastSnapshot {
	var out ForecastSnapshot
	if s == nil {
		return out
	}
	s.mu.RLock()
	if s.model != nil {
		out.Model = *s.model
	}
	if s.Residuals != nil {
		s.Residuals.mu.Lock()
		for _, p := range s.Residuals.samples {
			out.Residuals = append(out.Residuals, ResidualObservation{p.t.UTC(), p.predicted, p.actual})
		}
		s.Residuals.mu.Unlock()
	}
	out.CapturedAt = time.Now().UTC()
	s.mu.RUnlock()
	if out.Model.LastMs > 0 {
		out.LatestInput = time.UnixMilli(out.Model.LastMs).UTC()
	}
	for _, p := range out.Residuals {
		if p.At.After(out.LatestInput) {
			out.LatestInput = p.At
		}
	}
	state, err := json.Marshal(struct {
		Model     Model
		Residuals []ResidualObservation
	}{out.Model, out.Residuals})
	if err == nil {
		out.Revision = fmt.Sprintf("pv-legacy/%s/%x", FeatureHash(), sha256.Sum256(state))
	}
	return out
}

func (s ForecastSnapshot) Structural(target time.Time, clearSkyWm2, cloudPct float64) float64 {
	return s.Model.Predict(clearSkyWm2, cloudPct, target)
}

func (s ForecastSnapshot) residualStats(origin time.Time) (n int, mean, std float64) {
	cutoff := origin.Add(-ResidualBufferWindow)
	var m2 float64
	for _, p := range s.Residuals {
		if p.At.Before(cutoff) || p.At.After(origin) || !finite(p.ActualW) || !finite(p.PredictedW) {
			continue
		}
		n++
		x := p.ActualW - p.PredictedW
		delta := x - mean
		mean += delta / float64(n)
		m2 += delta * (x - mean)
	}
	if n > 0 {
		std = math.Sqrt(math.Max(0, m2/float64(n)))
	}
	return
}

func (s ForecastSnapshot) ResidualCorrect(origin, target time.Time, base float64) float64 {
	_ = base
	dt := target.Sub(origin)
	if dt <= 0 || dt > residualFadeEnd {
		return 0
	}
	n, mean, std := s.residualStats(origin)
	if n < residualMinSamples || math.Abs(mean) < residualEpsilonW || std/math.Max(1, math.Abs(mean)) > residualMaxCoVar {
		return 0
	}
	return mean * residualFadeFactor(dt)
}

func (s ForecastSnapshot) RelativeUncertainty() float64 { return s.Model.RelMAE }

func (s ForecastSnapshot) ResidualStdW(origin time.Time) float64 {
	_, _, std := s.residualStats(origin)
	return std
}
