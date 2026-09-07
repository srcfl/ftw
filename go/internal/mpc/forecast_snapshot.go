package mpc

import (
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// ForecastInputs owns one frozen model/weather view for a whole plan. The
// host captures state once; slot inference does not read a changing model.
type ForecastInputs struct {
	Weather               []state.ForecastPoint
	PV                    PVPredictor
	PVResidualCorrect     PVResidualCorrector
	Load                  LoadPredictor
	PVWeight              func(time.Time) float64
	PVUncertaintyW        float64
	PVRelativeUncertainty float64
	// Risk may replace the legacy PV margin using calibrated joint net errors.
	// It must preserve slot times/prices/limits and never add forecast PV.
	Risk func(base, planning []Slot, k float64)
	// Record runs only for a published plan, outside the service lock.
	// Implementations enqueue bounded immutable work instead of disk/network I/O.
	Record func(base, planning []Slot, decisionID string, issuedAtMS int64)
}

const ForecastMaxAge = 12 * time.Hour

// usableForecasts rejects weather unavailable at the decision origin and rows
// whose cache age exceeds policy. Unknown receipt time cannot establish
// that cached weather was available and fresh at the decision.
func usableForecasts(rows []state.ForecastPoint, nowMS int64) []state.ForecastPoint {
	out := make([]state.ForecastPoint, 0, len(rows))
	for _, r := range rows {
		if r.FetchedAtMs <= 0 || r.FetchedAtMs > nowMS || nowMS-r.FetchedAtMs > ForecastMaxAge.Milliseconds() {
			continue
		}
		out = append(out, r)
	}
	return out
}
