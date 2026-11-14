package pvmodel

import (
	"math"
	"time"
)

const (
	// NowcastWindow is how far back the measured level is averaged. It is
	// short because PV changes within minutes, and a few samples long so one
	// bad reading cannot steer the plan.
	NowcastWindow = 5 * time.Minute
	// nowcastMinSamples needs at least this many of the 60 s samples.
	nowcastMinSamples = 3
)

// MeasuredNowW returns the mean measured generation over the window ending at
// origin, in generation watts. False when too few samples back it.
func (s ForecastSnapshot) MeasuredNowW(origin time.Time) (float64, bool) {
	cutoff := origin.Add(-NowcastWindow)
	var sum float64
	n := 0
	for _, p := range s.Residuals {
		if p.At.Before(cutoff) || p.At.After(origin) || !finite(p.ActualW) {
			continue
		}
		sum += p.ActualW
		n++
	}
	if n < nowcastMinSamples {
		return 0, false
	}
	return sum / float64(n), true
}

// NowcastW moves forecastW toward measuredW by the gap between measured
// generation and the forecast for the in-flight slot, fading with dt, the time
// from issue to the slot's middle. A slot stays between its forecast and the
// measured level: a forecast that has already caught up keeps its value, and
// the correction can only draw it toward what the panels make now.
func NowcastW(measuredW, inflightW, forecastW float64, dt time.Duration) float64 {
	moved := forecastW + residualFadeFactor(dt)*(measuredW-inflightW)
	low, high := math.Min(forecastW, measuredW), math.Max(forecastW, measuredW)
	return math.Min(high, math.Max(low, moved))
}
