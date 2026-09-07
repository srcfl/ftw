package forecasting

import "fmt"

// LearningRestartPendingError means the boundary is durable, but one of the
// model saves still needs retry. Callers must not report the intent as rejected.
type LearningRestartPendingError struct{ Err error }

func (e *LearningRestartPendingError) Error() string {
	return fmt.Sprintf("Learning period saved; model restart pending: %v", e.Err)
}
func (e *LearningRestartPendingError) Unwrap() error { return e.Err }

// LearningStatus describes the model serving one forecast signal. Historical
// measurements and issued forecasts remain available across learning periods.
type LearningStatus struct {
	Engine           string `json:"engine"`
	Status           string `json:"status"`
	StartedMS        int64  `json:"started_ms"`
	LatestTrainingMS int64  `json:"latest_training_ms"`
	ResetAvailable   bool   `json:"reset_available"`
}
