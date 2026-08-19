package pvperf

import (
	"math"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// Calibration is the site-specific correction learned by comparing measured
// production against the STRÅNG physics baseline. It is the payoff of scoring:
// once a site has enough closed days, its typical performance ratio *is* the
// derate the forward forecast should apply, and the spread of that ratio is a
// measured uncertainty that beats any hand-tuned constant.
type Calibration struct {
	// Factor is the representative performance ratio — multiply a loss-free
	// physics PV estimate by this to get an expected real-world figure.
	Factor float64
	// SigmaRel is the robust spread of the daily ratio. PR is dimensionless,
	// so this is already a relative uncertainty.
	SigmaRel float64
	// Days is how many scored days carried a usable ratio.
	Days int
	// Valid reports whether Factor is trustworthy enough to apply. A factor
	// outside the plausible band means the geometry or the meter is wrong,
	// not that the panels are dirty — better to leave the forecast alone.
	Valid bool
}

const (
	// minCalibrationDays is the smallest sample that can outvote a single
	// snowy or curtailed day.
	minCalibrationDays = 7

	// Plausible bounds for a real site's ratio. Below the floor points at
	// broken telemetry or an array that never ran; above the ceiling means
	// the declared rated watts understate the installation. Both are configuration
	// faults that a silent forecast rescale would only hide.
	minCalibrationFactor = 0.30
	maxCalibrationFactor = 1.30

	// maxCalibrationSigma caps the reported spread so a pathological sample
	// cannot widen the planner's uncertainty without bound.
	maxCalibrationSigma = 1.0

	// madToSigma converts a median absolute deviation into a standard
	// deviation for normally distributed data.
	madToSigma = 1.4826

	// calibrationWindowDays is how far back Service.Calibration looks.
	calibrationWindowDays = 30
)

// Calibrate summarises scored days into a site calibration. It uses the median
// and a median-absolute-deviation sigma rather than mean/stddev: a single
// snow-covered or curtailed day is a large outlier, and the mean would chase it.
func Calibrate(days []state.PVPerformanceDay) Calibration {
	ratios := make([]float64, 0, len(days))
	for _, d := range days {
		if d.PR != nil && !math.IsNaN(*d.PR) && !math.IsInf(*d.PR, 0) {
			ratios = append(ratios, *d.PR)
		}
	}
	c := Calibration{Days: len(ratios)}
	if len(ratios) < minCalibrationDays {
		return c
	}
	c.Factor = median(ratios)

	deviations := make([]float64, len(ratios))
	for i, r := range ratios {
		deviations[i] = math.Abs(r - c.Factor)
	}
	c.SigmaRel = math.Min(maxCalibrationSigma, madToSigma*median(deviations))
	c.Valid = c.Factor >= minCalibrationFactor && c.Factor <= maxCalibrationFactor
	return c
}

// median returns the middle value of xs without mutating the caller's slice.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// Calibration returns the site calibration over the recent scored window.
// A zero value (Valid false) is returned whenever the service is unwired or
// the history is too thin to draw a conclusion.
func (s *Service) Calibration() Calibration {
	if s == nil || s.Store == nil {
		return Calibration{}
	}
	now := time.Now()
	days, err := s.Store.LoadPVPerformance(
		now.AddDate(0, 0, -calibrationWindowDays).Format("2006-01-02"),
		now.Format("2006-01-02"),
	)
	if err != nil {
		return Calibration{}
	}
	return Calibrate(days)
}

// CalibrationFactor adapts Calibration to the hook shape the forecast service
// consumes. The second return is false when the factor must not be applied.
func (s *Service) CalibrationFactor() (float64, bool) {
	c := s.Calibration()
	return c.Factor, c.Valid
}
