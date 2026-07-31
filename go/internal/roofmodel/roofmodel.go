// Package roofmodel invokes the optional Lantmäteriet roof-geometry module and
// turns its output into candidate PV arrays.
//
// The module is a separate Python package (roofmodel/, sibling to optimizer/)
// reached at arm's length: core spawns it, hands it coordinates and the
// operator's Geotorget credentials on the command line, and reads one versioned
// JSON document back from stdout. Core knows nothing about STAC, LAZ or plane
// fitting, and the module knows nothing about FTW.
//
// That boundary is the point. LiDAR segmentation drags in a compiled point-cloud
// stack, runs for minutes, and is onboarding-time work rather than runtime work.
// Keeping it in a subprocess means it cannot stall the control tick, cannot leak
// memory into the daemon, and can be absent entirely — which is the normal case,
// since the data only exists for Sweden.
//
// Nothing here is authoritative. Derived arrays *pre-fill* the operator's
// editable weather.pv_arrays; the numeric editor stays the final word.
package roofmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/coverage"
)

var (
	// ErrDisabled means no roofmodel section is configured, or it is off.
	ErrDisabled = errors.New("roof model module is not enabled")
	// ErrOutsideCoverage means Lantmäteriet has no data for this site.
	ErrOutsideCoverage = errors.New("outside Lantmäteriet coverage")
	// ErrNoCredentials means Geotorget credentials are missing.
	ErrNoCredentials = errors.New("Geotorget credentials are required")
)

const (
	defaultCommand       = "python3"
	defaultRadiusM       = 40.0
	defaultPackingFactor = 0.70
	defaultTimeout       = 10 * time.Minute
	// A roof model is small; anything larger is a runaway or a wrong command.
	maxOutputBytes = 1 << 20
)

// Array is one derived candidate PV array. Field names mirror config.PVArray so
// the document can pre-fill weather.pv_arrays directly.
type Array struct {
	Name       string  `json:"name"`
	KWp        float64 `json:"kwp"`
	TiltDeg    float64 `json:"tilt_deg"`
	AzimuthDeg float64 `json:"azimuth_deg"`
	AreaM2     float64 `json:"area_m2"`
	SegmentID  string  `json:"segment_id"`
}

// Model is the versioned document the module emits.
type Model struct {
	SchemaVersion int     `json:"schema_version"`
	Arrays        []Array `json:"arrays"`
	PlanesFound   int     `json:"planes_found"`
	Site          struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		RadiusM   float64 `json:"radius_m"`
	} `json:"site"`
	Source struct {
		Provider        string `json:"provider"`
		Collection      string `json:"collection"`
		ItemCount       int    `json:"item_count"`
		DatasetDatetime string `json:"dataset_datetime"`
	} `json:"source"`
	// CapturedAtMs is when Lantmäteriet flew the LiDAR. Null while their STAC
	// datetime backfill is incomplete, which is a missing provenance date and
	// not a failure — the UI shows "capture date unknown".
	CapturedAtMs *int64 `json:"captured_at_ms"`
	DerivedAtMs  int64  `json:"derived_at_ms"`
}

// moduleError is the JSON the module writes to stderr when it fails.
type moduleError struct {
	Error string `json:"error"`
	Kind  string `json:"kind"`
}

// Service derives roof models. The zero value is unusable; use FromConfig.
type Service struct {
	cfg *config.RoofModel
}

// FromConfig returns a Service, or nil when the module is not configured. A nil
// Service is safe to call: every method reports ErrDisabled.
func FromConfig(cfg *config.RoofModel) *Service {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	return &Service{cfg: cfg}
}

// Enabled reports whether derives are possible.
func (s *Service) Enabled() bool { return s != nil && s.cfg != nil && s.cfg.Enabled }

func (s *Service) timeout() time.Duration {
	if s.cfg.TimeoutS > 0 {
		return time.Duration(s.cfg.TimeoutS) * time.Second
	}
	return defaultTimeout
}

func (s *Service) command() string {
	if s.cfg.Command != "" {
		return s.cfg.Command
	}
	return defaultCommand
}

func (s *Service) radius() float64 {
	if s.cfg.RadiusM > 0 {
		return s.cfg.RadiusM
	}
	return defaultRadiusM
}

func (s *Service) packingFactor() float64 {
	if s.cfg.PackingFactor > 0 {
		return s.cfg.PackingFactor
	}
	return defaultPackingFactor
}

// Derive runs the module for one site.
//
// Coverage and credentials are checked before spawning anything: a site outside
// Sweden can never succeed, and a missing credential fails the same way every
// time, so neither is worth an interpreter start and a network round trip.
func (s *Service) Derive(ctx context.Context, lat, lon float64) (*Model, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if !coverage.Covers("lantmateriet", lat, lon) {
		return nil, fmt.Errorf("%w: (%.4f, %.4f) is not in Sweden", ErrOutsideCoverage, lat, lon)
	}
	if s.cfg.GeotorgetUsername == "" || s.cfg.GeotorgetToken == "" {
		return nil, ErrNoCredentials
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	args := []string{
		"-m", "ftw_roofmodel",
		"--lat", fmt.Sprintf("%.6f", lat),
		"--lon", fmt.Sprintf("%.6f", lon),
		"--username", s.cfg.GeotorgetUsername,
		"--token", s.cfg.GeotorgetToken,
		"--radius-m", fmt.Sprintf("%.1f", s.radius()),
		"--packing-factor", fmt.Sprintf("%.3f", s.packingFactor()),
	}
	cmd := exec.CommandContext(ctx, s.command(), args...)
	if s.cfg.ModuleDir != "" {
		cmd.Env = append(cmd.Environ(), "PYTHONPATH="+s.cfg.ModuleDir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("roof model timed out after %s", s.timeout())
	}
	if err != nil {
		// The module reports failures as JSON on stderr so an operator sees a
		// reason ("credentials rejected") rather than a Python traceback.
		var me moduleError
		if jsonErr := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &me); jsonErr == nil && me.Error != "" {
			return nil, fmt.Errorf("roof model: %s", me.Error)
		}
		return nil, fmt.Errorf("roof model failed: %w", err)
	}
	if stdout.Len() > maxOutputBytes {
		return nil, fmt.Errorf("roof model returned %d bytes, refusing", stdout.Len())
	}

	var m Model
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("roof model returned unreadable output: %w", err)
	}
	if m.SchemaVersion != 1 {
		return nil, fmt.Errorf("roof model schema_version %d is not supported", m.SchemaVersion)
	}

	slog.Info("roof model derived",
		"lat", lat, "lon", lon, "arrays", len(m.Arrays),
		"planes", m.PlanesFound, "elapsed", elapsed)
	return &m, nil
}

// ToPVArrays converts derived arrays into config entries ready to be written
// into weather.pv_arrays.
func (m *Model) ToPVArrays() []config.PVArray {
	if m == nil {
		return nil
	}
	out := make([]config.PVArray, 0, len(m.Arrays))
	for _, a := range m.Arrays {
		// Config keeps tilt and azimuth as pointers so an omitted field cannot
		// pass for a valid 0°. A derived array always has both, so both are
		// addressed here; the locals keep each entry pointing at its own copy.
		tiltDeg, azimuthDeg := a.TiltDeg, a.AzimuthDeg
		out = append(out, config.PVArray{
			Name:       a.Name,
			KWp:        a.KWp,
			TiltDeg:    &tiltDeg,
			AzimuthDeg: &azimuthDeg,
		})
	}
	return out
}
