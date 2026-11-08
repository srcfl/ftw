package mpc

import (
	"context"
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
	// PVNowcast moves a slot's selected PV forecast toward the generation
	// measured just before this capture. It takes the in-flight slot's
	// forecast, the slot's forecast and the time from issue to the slot's
	// middle, all in generation watts. Nil keeps the forecast as selected.
	PVNowcast func(inflightW, forecastW float64, dt time.Duration) float64
	// Resolve selects primary forecasts for the complete price horizon. It runs
	// once during replanning, outside the service lock, under a bounded context.
	// Only PVW and LoadW may change; the host retains a frozen legacy shadow.
	Resolve func(context.Context, []Slot) []Slot
	// Risk may replace the legacy PV margin using calibrated joint net errors.
	// It must preserve slot times/prices/limits and never add forecast PV.
	Risk func(base, planning []Slot, k float64)
	// Record runs only for a published plan, outside the service lock.
	// Implementations enqueue bounded immutable work instead of disk/network I/O.
	// recoveredLoad marks the base loads Core replaced after the selected
	// forecast failed; the live meter's in-flight load is not one of them.
	Record func(base, planning []Slot, recoveredLoad []bool, decisionID string, issuedAtMS int64)
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
