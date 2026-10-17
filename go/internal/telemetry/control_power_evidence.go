package telemetry

import (
	"encoding/json"
	"time"
)

// ControlPowerEvidence captures the same EV reading used by the budget, not a later status
// snapshot. It is diagnostic only and never supplies replacement credit.
type ControlPowerEvidence struct {
	ReportedSourceAt string    `json:"reported_source_at"`
	Driver           string    `json:"driver"`
	Reason           string    `json:"reason"`
	RawW             float64   `json:"raw_w"`
	PowerW           float64   `json:"control_power_w"`
	ReceivedAt       time.Time `json:"received_at"`
	SourceAt         time.Time `json:"source_at"`
	ConfirmedAt      time.Time `json:"confirmed_at"`
	SourceValid      bool      `json:"source_valid"`
	Qualified        bool      `json:"qualified"`
	TelemetryLive    bool      `json:"telemetry_live"`
}

// DescribeControlPower describes the supplied decision snapshot, including rejected
// or offline readings, for diagnostics only. It never authorizes control.
func DescribeControlPower(driver string, r *DerReading, health *DriverHealth, p ControlObservation, qualified bool, now time.Time) ControlPowerEvidence {
	e := ControlPowerEvidence{Driver: driver, Reason: "missing_reading", Qualified: qualified}
	if r == nil {
		return e
	}
	e.RawW, e.ReceivedAt = r.RawW, r.UpdatedAt
	// Keep the source timestamp even when qualification rejected the reading.
	var reported struct {
		ControlPowerObservedAt string `json:"control_power_observed_at"`
		PowerObservedAt        string `json:"power_observed_at"`
	}
	_ = json.Unmarshal(r.Data, &reported)
	e.ReportedSourceAt = reported.ControlPowerObservedAt
	if e.ReportedSourceAt == "" {
		e.ReportedSourceAt = reported.PowerObservedAt
	}
	source, valid := ControlPowerObservation(r.RawW, r.Data, r.UpdatedAt)
	e.PowerW, e.SourceAt, e.ConfirmedAt, e.SourceValid = source.PowerW, source.At, source.ConfirmedAt, valid
	if !valid {
		e.SourceAt, e.ConfirmedAt = time.Time{}, time.Time{}
	}
	e.TelemetryLive = health != nil && health.TelemetryLive()
	switch {
	case !valid:
		e.Reason = "invalid_or_unavailable_source"
	case r.UpdatedAt.After(now) || now.Sub(r.UpdatedAt) > ControlPowerMaxAge(r.Data):
		e.Reason = "stale_or_future_receipt"
	case !qualified:
		e.Reason = "stale_or_future_control_observation"
	case !e.TelemetryLive:
		e.Reason = "telemetry_not_live"
	case p.PowerW <= 0:
		e.Reason = "observed_nonpositive_power"
	default:
		e.Reason = "qualified"
	}
	return e
}
