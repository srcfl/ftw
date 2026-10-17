package control

import "github.com/srcfl/ftw/go/internal/telemetry"

// Budget-specific fields accompany the immutable diagnostic snapshot.
type evFuseEvidence struct {
	telemetry.ControlPowerEvidence
	LatestCreditW float64 `json:"latest_credit_w"`
	Points        int     `json:"points"`
}
