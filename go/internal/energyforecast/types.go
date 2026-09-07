// Package energyforecast adapts Core's observations and issued weather to the
// versioned Energyplan forecast worker. All learned model code stays in Rust.
package energyforecast

import (
	"context"
	"encoding/json"
)

const ProtocolVersion = 1
const MaxPayloadBytes = 2 * 1024 * 1024
const MaxStateBytes = 1024 * 1024
const MaxObservations = 4096
const MaxHorizon = 512

// RoundTripper must stop I/O when the context expires. The existing MPC
// ProcessTransport satisfies this interface without a package dependency.
type RoundTripper interface {
	RoundTrip(context.Context, []byte) ([]byte, error)
}

type PVConfig struct {
	LatitudeDeg  float64  `json:"latitude_deg"`
	LongitudeDeg float64  `json:"longitude_deg"`
	ACLimitW     *float64 `json:"ac_limit_w,omitempty"`
}

type LoadConfig struct{}

type Config struct {
	PV   *PVConfig   `json:"pv,omitempty"`
	Load *LoadConfig `json:"load,omitempty"`
}

type RequestContext struct {
	RequestID      string          `json:"request_id"`
	SiteID         string          `json:"site_id"`
	ConfigRevision string          `json:"config_revision"`
	OriginMs       int64           `json:"origin_ms"`
	Config         Config          `json:"config"`
	State          json.RawMessage `json:"state,omitempty"`
}

type Interval struct {
	ValidStartMs int64 `json:"valid_start_ms"`
	ValidEndMs   int64 `json:"valid_end_ms"`
}

// Features describe the civil schedule and the weather available at origin.
// LocalDay counts civil dates since 1970; Monday is weekday zero.
type Features struct {
	LocalDay             int64    `json:"local_day"`
	LocalWeekday         int      `json:"local_weekday"`
	LocalMinute          int      `json:"local_minute"`
	Home                 *bool    `json:"home,omitempty"`
	GHIWm2               *float64 `json:"ghi_w_m2,omitempty"`
	CloudPct             *float64 `json:"cloud_pct,omitempty"`
	TempC                *float64 `json:"temp_c,omitempty"`
	WeatherAvailableAtMs *int64   `json:"weather_available_at_ms,omitempty"`
}

type ObservationQuality string

const (
	QualityGood       ObservationQuality = "good"
	QualityMissing    ObservationQuality = "missing"
	QualityStale      ObservationQuality = "stale"
	QualityIncomplete ObservationQuality = "incomplete"
	QualityCurtailed  ObservationQuality = "curtailed"
	QualityClipped    ObservationQuality = "clipped"
)

// Observation uses interval-mean, generation-positive PV watts. Core converts
// its site-sign PV before this boundary. Missing power stays absent, not zero.
// Ordinary inverter AC clipping is Good; Clipped means a censored sensor reading.
type Observation struct {
	Interval
	Features
	AvailableAtMs  int64              `json:"available_at_ms"`
	HouseholdLoadW *float64           `json:"household_load_w,omitempty"`
	PVAvailableW   *float64           `json:"pv_available_w,omitempty"`
	LoadQuality    ObservationQuality `json:"load_quality"`
	PVQuality      ObservationQuality `json:"pv_quality"`
}

type HorizonSlot struct {
	Interval
	Features
}

type UpdateRequest struct {
	RequestContext
	Observations []Observation `json:"observations"`
}
type PredictRequest struct {
	RequestContext
	Horizon []HorizonSlot `json:"horizon"`
}

type ReplyContext struct {
	Op                  string          `json:"op"`
	Version             int             `json:"version"`
	Action              string          `json:"action"`
	RequestID           string          `json:"request_id"`
	SiteID              string          `json:"site_id"`
	ConfigRevision      string          `json:"config_revision"`
	OriginMs            int64           `json:"origin_ms"`
	OK                  bool            `json:"ok"`
	Error               json.RawMessage `json:"error,omitempty"`
	ModelRevision       uint64          `json:"model_revision"`
	LatestTrainingMs    LatestInput     `json:"latest_training_ms"`
	LatestInputMs       LatestInput     `json:"latest_input_ms"`
	LatestAvailableAtMs LatestInput     `json:"latest_available_at_ms"`
}

type UpdateCount struct {
	Applied int `json:"applied"`
	Skipped int `json:"skipped"`
}
type UpdateCounts struct {
	PV   *UpdateCount `json:"pv,omitempty"`
	Load *UpdateCount `json:"load,omitempty"`
}
type UpdateReply struct {
	ReplyContext
	State   json.RawMessage `json:"state"`
	Updates UpdateCounts    `json:"updates"`
}
type LatestInput struct {
	PV   *int64 `json:"pv,omitempty"`
	Load *int64 `json:"load,omitempty"`
}

// Estimate bounds are provisional model output, not calibrated quantiles.
// Unknown is explicit and cannot turn into a zero-watt forecast on decode.
type Estimate struct {
	Known       bool     `json:"known"`
	PointW      *float64 `json:"point_w,omitempty"`
	LowerW      *float64 `json:"lower_w,omitempty"`
	UpperW      *float64 `json:"upper_w,omitempty"`
	Quality     string   `json:"quality"`
	Uncertainty string   `json:"uncertainty"`
	Coverage    float64  `json:"coverage"`
}

type Prediction struct {
	Interval
	PV   *Estimate `json:"pv,omitempty"`
	Load *Estimate `json:"load,omitempty"`
}
type PredictReply struct {
	ReplyContext
	Predictions []Prediction `json:"predictions"`
}
