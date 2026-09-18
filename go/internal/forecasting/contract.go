// Package forecasting defines portable forecast records and causal evaluation.
// It has no clock, telemetry, database, network or planner dependencies.
package forecasting

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	Schema = 1
	// A rolling 48-hour forecast includes 193 quarters when the current one
	// is already underway. Keep that partial interval and the final quarter.
	MaxPoints          = 48*4 + 1
	MaxSeries          = 8
	MaxPayloadBytes    = 1 << 20
	MaxModelStateBytes = 1 << 20

	BandMethodColdStart = "cold_start_prior"
	BandMethodEmpirical = "empirical"

	ModelQualityColdStart = "cold_start"
	ModelQualityLearning  = "learning"
	ModelQualityReady     = "ready"
	ModelQualityWarm      = "warm"
)

// Band is an empirical central 80% prediction interval. Prior bands are
// explicitly uncalibrated; zero history never claims certainty.
type Band struct {
	LowW    float64 `json:"low_w"`
	HighW   float64 `json:"high_w"`
	Method  string  `json:"method"`
	Samples int     `json:"samples"`
	Days    int     `json:"days"`
}

// ModelEstimateEvidence preserves a model's own provisional range. It stays
// separate from Band because it has not earned empirical coverage yet.
type ModelEstimateEvidence struct {
	LowerW      float64 `json:"lower_w"`
	UpperW      float64 `json:"upper_w"`
	Uncertainty string  `json:"uncertainty"`
	Coverage    float64 `json:"coverage"`
}

// Point contains interval mean power in W. PV is available AC generation,
// positive here; Load excludes independently planned EV and battery flows.
// Net load is LoadW-PVW. This contract never contains hardware commands.
// PredictionStartMS is set when watts describe only the remaining part of a
// planner interval. Such points are excluded from full-interval evaluation.
type Point struct {
	PredictionStartMS int64   `json:"prediction_start_ms,omitempty"`
	StartMS           int64   `json:"valid_start_ms"`
	EndMS             int64   `json:"valid_end_ms"`
	PVW               float64 `json:"pv_available_w"`
	LoadW             float64 `json:"household_load_w"`
	PVKnown           bool    `json:"pv_known"`
	LoadKnown         bool    `json:"load_known"`
	PVQuality         string  `json:"pv_quality"`
	LoadQuality       string  `json:"load_quality"`
	// Sources identify the selected model per signal, including fallback.
	// Empty sources remain valid for archives written before provenance existed.
	PVSource   string                 `json:"pv_source,omitempty"`
	LoadSource string                 `json:"load_source,omitempty"`
	PVBand     Band                   `json:"pv_band"`
	LoadBand   Band                   `json:"load_band"`
	NetBand    Band                   `json:"net_band"`
	ModelPV    *ModelEstimateEvidence `json:"model_pv,omitempty"`
	ModelLoad  *ModelEstimateEvidence `json:"model_load,omitempty"`
}

type Series struct {
	Name         string  `json:"name"`
	ModelVersion string  `json:"model_version"`
	Points       []Point `json:"points"`
}

type Weather struct {
	StartMS       int64    `json:"valid_start_ms"`
	EndMS         int64    `json:"valid_end_ms"`
	AvailableAtMS int64    `json:"available_at_ms"`
	Source        string   `json:"source"`
	GHIWm2        *float64 `json:"ghi_w_m2,omitempty"`
	DirectPVW     *float64 `json:"direct_pv_w,omitempty"`
	EstimatedPVW  *float64 `json:"estimated_pv_w,omitempty"`
	CloudPct      *float64 `json:"cloud_pct,omitempty"`
	TempC         *float64 `json:"temp_c,omitempty"`
}

type ModelState struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Quality     string          `json:"quality"`
	UpdatedAtMS int64           `json:"updated_at_ms"`
	StateID     string          `json:"state_id,omitempty"`
	State       json.RawMessage `json:"state,omitempty"`
}

// SiteContext preserves the site inputs needed to replay an issued horizon.
// LearningRevision identifies the physical boundary independently of code builds.
type SiteContext struct {
	SiteID           string  `json:"site_id"`
	LearningRevision string  `json:"learning_revision"`
	Timezone         string  `json:"timezone"`
	HasLocation      bool    `json:"has_location"`
	Latitude         float64 `json:"latitude"`
	Longitude        float64 `json:"longitude"`
}

// Occupancy freezes one quarter's household profile as known at issue time.
// It is an input feature, not a later observation of whether anyone was home.
type Occupancy struct {
	StartMS       int64 `json:"valid_start_ms"`
	EndMS         int64 `json:"valid_end_ms"`
	AvailableAtMS int64 `json:"available_at_ms"`
	Home          bool  `json:"home"`
}

// ValidateOccupancy bounds portable model features and rejects future knowledge.
// Empty features remain valid for historical issues that did not record them.
func ValidateOccupancy(rows []Occupancy, originMS int64) error {
	if len(rows) > 512 {
		return errors.New("occupancy exceeds bounded horizon")
	}
	var end int64
	for _, row := range rows {
		if row.StartMS <= 0 || row.StartMS%900000 != 0 || row.EndMS-row.StartMS != 900000 || row.StartMS < end || row.AvailableAtMS <= 0 || row.AvailableAtMS > originMS {
			return errors.New("invalid or unavailable occupancy feature")
		}
		end = row.EndMS
	}
	return nil
}

func (s SiteContext) Validate() error {
	if s.SiteID == "" || len(s.SiteID) > 128 || s.LearningRevision == "" || len(s.LearningRevision) > 128 || s.Timezone == "" || len(s.Timezone) > 128 || s.Timezone == "Local" {
		return errors.New("invalid forecast site identity or timezone")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return errors.New("invalid forecast site timezone")
	}
	if !finite(s.Latitude) || !finite(s.Longitude) || (s.HasLocation && (math.Abs(s.Latitude) > 90 || math.Abs(s.Longitude) > 180)) {
		return errors.New("invalid forecast site location")
	}
	return nil
}

// Issue freezes the inputs known at OriginMS. IssuedAtMS is when the complete
// forecast became available. A later issue never replaces this one.
type Issue struct {
	Schema        int          `json:"schema"`
	Site          *SiteContext `json:"site,omitempty"`
	ID            string       `json:"id"`
	DecisionID    string       `json:"decision_id"`
	OriginMS      int64        `json:"origin_ms"`
	IssuedAtMS    int64        `json:"issued_at_ms"`
	LatestInputMS int64        `json:"latest_input_ms"`
	ConfigVersion string       `json:"config_version"`
	Models        []ModelState `json:"models,omitempty"`
	Weather       []Weather    `json:"weather,omitempty"`
	Occupancy     []Occupancy  `json:"occupancy,omitempty"`
	Series        []Series     `json:"series"`
}

// Observation records a fully covered interval from valid, aligned readings.
// Available PV is unknown during commanded curtailment, although load may
// remain measurable. Historical rows without this evidence are not labels.
type Observation struct {
	StartMS       int64   `json:"valid_start_ms"`
	EndMS         int64   `json:"valid_end_ms"`
	AvailableAtMS int64   `json:"available_at_ms"`
	PVW           float64 `json:"pv_available_w"`
	LoadW         float64 `json:"household_load_w"`
	PVKnown       bool    `json:"pv_known"`
	LoadKnown     bool    `json:"load_known"`
	Quality       string  `json:"quality"`
	ConfigVersion string  `json:"config_version"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validInterval(start, end int64) bool {
	return start > 0 && end > start && end-start <= int64(time.Hour/time.Millisecond)
}

func validBand(b Band) bool {
	if !finite(b.LowW) || !finite(b.HighW) || b.LowW > b.HighW || b.Samples < 0 || b.Days < 0 || b.Days > b.Samples {
		return false
	}
	switch b.Method {
	case BandMethodColdStart:
		return true
	case BandMethodEmpirical:
		return b.Samples >= 48 && b.Days >= 7
	default:
		return false
	}
}

func validPoint(p Point) bool {
	return validInterval(p.StartMS, p.EndMS) &&
		(p.PredictionStartMS == 0 || (p.PredictionStartMS > p.StartMS && p.PredictionStartMS < p.EndMS)) &&
		finite(p.PVW) && finite(p.LoadW) && p.PVW >= 0 && p.LoadW >= 0 &&
		p.PVQuality != "" && p.LoadQuality != "" &&
		len(p.PVSource) <= 80 && len(p.LoadSource) <= 80 &&
		validBand(p.PVBand) && validBand(p.LoadBand) && validBand(p.NetBand) &&
		validModelEstimate(p.ModelPV) && validModelEstimate(p.ModelLoad)
}

func validModelEstimate(e *ModelEstimateEvidence) bool {
	return e == nil || (finite(e.LowerW) && finite(e.UpperW) && e.LowerW >= 0 && e.LowerW <= e.UpperW &&
		e.Uncertainty == "provisional" && finite(e.Coverage) && e.Coverage >= 0 && e.Coverage <= 1)
}

func (r Issue) Validate() error {
	if err := ValidateOccupancy(r.Occupancy, r.OriginMS); err != nil {
		return err
	}
	if r.Site != nil {
		if err := r.Site.Validate(); err != nil {
			return err
		}
	}
	if r.Schema != Schema || r.ID == "" || len(r.ID) > 128 || len(r.DecisionID) > 128 ||
		r.ConfigVersion == "" || len(r.ConfigVersion) > 128 || r.OriginMS <= 0 ||
		r.IssuedAtMS < r.OriginMS || r.LatestInputMS < 0 || r.LatestInputMS > r.OriginMS {
		return errors.New("invalid forecast issue identity or availability")
	}
	if len(r.Series) == 0 || len(r.Series) > MaxSeries || len(r.Weather) > MaxPoints || len(r.Models) > 8 {
		return errors.New("forecast issue exceeds bounded horizon")
	}
	names := map[string]bool{}
	for _, s := range r.Series {
		if s.Name == "" || len(s.Name) > 80 || s.ModelVersion == "" || len(s.ModelVersion) > 128 ||
			names[s.Name] || len(s.Points) == 0 || len(s.Points) > MaxPoints {
			return errors.New("invalid forecast series")
		}
		names[s.Name] = true
		var end int64
		for _, p := range s.Points {
			if !validPoint(p) || p.StartMS < end || (p.PredictionStartMS != 0 && p.PredictionStartMS < r.OriginMS) {
				return fmt.Errorf("invalid interval in %s", s.Name)
			}
			end = p.EndMS
		}
	}
	var weatherEnd int64
	for _, w := range r.Weather {
		if !validInterval(w.StartMS, w.EndMS) || w.AvailableAtMS <= 0 || w.AvailableAtMS > r.OriginMS ||
			w.StartMS < weatherEnd || w.Source == "" || len(w.Source) > 128 {
			return errors.New("weather unavailable at forecast origin")
		}
		weatherEnd = w.EndMS
		if w.GHIWm2 != nil && (!finite(*w.GHIWm2) || *w.GHIWm2 < 0) {
			return errors.New("invalid weather irradiance")
		}
		if w.DirectPVW != nil && (!finite(*w.DirectPVW) || *w.DirectPVW < 0) {
			return errors.New("invalid direct provider PV")
		}
		if w.EstimatedPVW != nil && (!finite(*w.EstimatedPVW) || *w.EstimatedPVW < 0) {
			return errors.New("invalid estimated PV")
		}
		if w.CloudPct != nil && (!finite(*w.CloudPct) || *w.CloudPct < 0 || *w.CloudPct > 100) {
			return errors.New("invalid weather cloud cover")
		}
		if w.TempC != nil && !finite(*w.TempC) {
			return errors.New("nonfinite weather temperature")
		}
	}
	modelNames := map[string]bool{}
	for _, m := range r.Models {
		if m.Name == "" || len(m.Name) > 80 || m.Version == "" || len(m.Version) > 128 ||
			modelNames[m.Name] || len(m.State) > MaxModelStateBytes {
			return errors.New("invalid model state")
		}
		modelNames[m.Name] = true
		hasState := len(m.State) > 0
		hasReference := m.StateID != ""
		if (hasState && !json.Valid(m.State)) || (hasReference && !validStateID(m.StateID)) || (hasState && hasReference) {
			return errors.New("model state payload or SHA-256 reference is invalid")
		}
		switch m.Quality {
		case ModelQualityColdStart:
			if m.UpdatedAtMS != 0 {
				return errors.New("cold-start model has learned state time")
			}
		case ModelQualityWarm, ModelQualityLearning, ModelQualityReady:
			if m.UpdatedAtMS <= 0 || m.UpdatedAtMS > r.OriginMS || (!hasState && !hasReference) {
				return errors.New("warm model state is unavailable at forecast origin")
			}
		default:
			return errors.New("invalid model state quality")
		}
	}
	return nil
}

func validStateID(id string) bool {
	if len(id) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 32
}

func (o Observation) Validate() error {
	if !validInterval(o.StartMS, o.EndMS) || o.AvailableAtMS < o.EndMS ||
		!finite(o.PVW) || !finite(o.LoadW) || o.PVW < 0 || o.LoadW < 0 ||
		o.Quality == "" || len(o.Quality) > 80 || o.ConfigVersion == "" || len(o.ConfigVersion) > 128 {
		return errors.New("invalid forecast observation")
	}
	return nil
}
