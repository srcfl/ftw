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
	"sync"
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
	RatedW     float64 `json:"rated_w"`
	TiltDeg    float64 `json:"tilt_deg"`
	AzimuthDeg float64 `json:"azimuth_deg"`
	AreaM2     float64 `json:"area_m2"`
	SegmentID  string  `json:"segment_id"`
}

// BuildingList is what `--mode buildings` emits: GeoJSON features a map can
// draw directly, nearest first. Geometry is passed through as raw JSON because
// core has no business interpreting a polygon -- it only ferries it to the UI.
type BuildingList struct {
	SchemaVersion int `json:"schema_version"`
	Site          struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"site"`
	Buildings []json.RawMessage `json:"buildings"`
}

// Building records which footprint a model was derived from, and how much of
// the surrounding cloud survived the clip -- the honest measure of whether the
// footprint and the scan actually agree.
type Building struct {
	BuildingID      string          `json:"building_id"`
	AreaM2          float64         `json:"area_m2"`
	Footprint       json.RawMessage `json:"footprint"`
	ReturnsUsed     int             `json:"returns_used"`
	ReturnsInRadius int             `json:"returns_in_radius"`
}

// Model is the versioned document the module emits.
type Model struct {
	SchemaVersion int     `json:"schema_version"`
	Arrays        []Array `json:"arrays"`
	PlanesFound   int     `json:"planes_found"`
	// Building is null when the whole search radius was segmented rather than
	// one picked footprint.
	Building *Building `json:"building"`
	Site     struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		RadiusM   float64 `json:"radius_m"`
	} `json:"site"`
	Source struct {
		Provider        string `json:"provider"`
		Collection      string `json:"collection"`
		ItemCount       int    `json:"item_count"`
		DatasetDatetime string `json:"dataset_datetime"`
		// Fetch is "copc-window" when only the picked building's neighbourhood
		// was pulled from the LiDAR tile, or "whole-tile" when the whole thing
		// came across. It also qualifies Building.ReturnsInRadius below, whose
		// denominator is the fetched window in the first case.
		Fetch string `json:"fetch"`
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
	mu  sync.RWMutex
	cfg *config.RoofModel
}

// FromConfig returns a Service even when the module is disabled or
// unconfigured, so a later Reconfigure can enable it without a restart. A
// Service without a usable config — and a nil Service — is safe to call:
// every method reports ErrDisabled.
func FromConfig(cfg *config.RoofModel) *Service {
	return &Service{cfg: cfg}
}

// Reconfigure swaps the module's config so enablement or credentials saved
// through the API count on the next call rather than the next restart. The
// service used to keep its boot-time snapshot, which silently ignored
// Geotorget credentials typed into Settings while GET /api/roofmodel —
// reading the live config — reported them present.
func (s *Service) Reconfigure(cfg *config.RoofModel) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// config returns the current snapshot. Loaded config structs are never
// mutated in place — a reload builds a fresh one and Reconfigure swaps the
// pointer — so the snapshot stays coherent for the length of a call.
func (s *Service) config() *config.RoofModel {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Enabled reports whether derives are possible.
func (s *Service) Enabled() bool {
	c := s.config()
	return c != nil && c.Enabled
}

func timeoutFrom(cfg *config.RoofModel) time.Duration {
	if cfg.TimeoutS > 0 {
		return time.Duration(cfg.TimeoutS) * time.Second
	}
	return defaultTimeout
}

func commandFrom(cfg *config.RoofModel) string {
	if cfg.Command != "" {
		return cfg.Command
	}
	return defaultCommand
}

func radiusFrom(cfg *config.RoofModel) float64 {
	if cfg.RadiusM > 0 {
		return cfg.RadiusM
	}
	return defaultRadiusM
}

func packingFactorFrom(cfg *config.RoofModel) float64 {
	if cfg.PackingFactor > 0 {
		return cfg.PackingFactor
	}
	return defaultPackingFactor
}

// Buildings lists candidate building footprints near a site, nearest first, so
// the operator can pick the one their panels are going on.
//
// Without this step a derive segments everything inside its radius: the
// neighbour's roof, the garage, the trees. Worse, RANSAC fits infinite planes,
// so a second building sharing the ridge orientation lands inside the first
// one's inlier band however far away it is and steals its returns.
func (s *Service) Buildings(ctx context.Context, lat, lon float64) (*BuildingList, error) {
	out, err := s.run(ctx, lat, lon, "buildings", "", nil)
	if err != nil {
		return nil, err
	}
	var list BuildingList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("roof model returned unreadable output: %w", err)
	}
	if list.SchemaVersion != 1 {
		return nil, fmt.Errorf("roof model schema_version %d is not supported", list.SchemaVersion)
	}
	slog.Info("roof model buildings", "lat", lat, "lon", lon, "found", len(list.Buildings))
	return &list, nil
}

// Derive runs the module for one site.
//
// buildingID is optional; pass one from Buildings to clip the LiDAR to that
// footprint before segmenting, which is what makes the derived tilt and azimuth
// belong to the operator's own roof rather than to whatever else stood in range.
// footprint is the hand-drawn alternative — a [lon, lat] ring traced on the
// map, for catalogs that publish no building dataset to pick from. It wins
// over buildingID.
//
// Coverage and credentials are checked before spawning anything: a site outside
// Sweden can never succeed, and a missing credential fails the same way every
// time, so neither is worth an interpreter start and a network round trip.
func (s *Service) Derive(ctx context.Context, lat, lon float64, buildingID string, footprint [][]float64) (*Model, error) {
	out, err := s.run(ctx, lat, lon, "derive", buildingID, footprint)
	if err != nil {
		return nil, err
	}
	var m Model
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("roof model returned unreadable output: %w", err)
	}
	if m.SchemaVersion != 1 {
		return nil, fmt.Errorf("roof model schema_version %d is not supported", m.SchemaVersion)
	}
	slog.Info("roof model derived",
		"lat", lat, "lon", lon, "arrays", len(m.Arrays),
		"planes", m.PlanesFound, "building", buildingID)
	return &m, nil
}

// run spawns the module and returns its stdout.
func (s *Service) run(ctx context.Context, lat, lon float64, mode, buildingID string, footprint [][]float64) ([]byte, error) {
	cfg := s.config()
	if cfg == nil || !cfg.Enabled {
		return nil, ErrDisabled
	}
	// The Sweden gate belongs to the default Lantmäteriet catalog only. An
	// operator pointing at another country's STAC catalog knows what it
	// covers; FTW does not, so it stops pretending to.
	if cfg.StacBaseURL == "" && !coverage.Covers("lantmateriet", lat, lon) {
		return nil, fmt.Errorf("%w: (%.4f, %.4f) is not in Sweden", ErrOutsideCoverage, lat, lon)
	}
	// Lantmäteriet always needs the operator's own Geotorget credentials. A
	// custom catalog may be open — many national STAC catalogs are — so with a
	// base URL set, absent credentials mean anonymous access, not a mistake.
	if cfg.StacBaseURL == "" && (cfg.StacUser() == "" || cfg.StacPass() == "") {
		return nil, ErrNoCredentials
	}

	ctx, cancel := context.WithTimeout(ctx, timeoutFrom(cfg))
	defer cancel()

	args := []string{
		"-m", "ftw_roofmodel",
		"--mode", mode,
		"--lat", fmt.Sprintf("%.6f", lat),
		"--lon", fmt.Sprintf("%.6f", lon),
		"--radius-m", fmt.Sprintf("%.1f", radiusFrom(cfg)),
		"--packing-factor", fmt.Sprintf("%.3f", packingFactorFrom(cfg)),
	}
	if buildingID != "" {
		args = append(args, "--building-id", buildingID)
	}
	if len(footprint) > 0 {
		// Marshalling [][]float64 cannot fail; the module validates the shape.
		fp, _ := json.Marshal(footprint)
		args = append(args, "--footprint-json", string(fp))
	}
	if u := cfg.StacUser(); u != "" {
		args = append(args, "--username", u)
	}
	if p := cfg.StacPass(); p != "" {
		args = append(args, "--password", p)
	}
	// A custom catalog replaces the Lantmäteriet defaults piecewise; anything
	// left empty falls back to the module's own Geotorget defaults.
	if cfg.StacBaseURL != "" {
		args = append(args, "--stac-base-url", cfg.StacBaseURL)
	}
	if cfg.StacBuildingsCollection != "" {
		args = append(args, "--buildings-collection", cfg.StacBuildingsCollection)
	}
	if cfg.StacLidarCollection != "" {
		args = append(args, "--lidar-collection", cfg.StacLidarCollection)
	}
	if cfg.StacBboxEPSG != 0 {
		args = append(args, "--bbox-epsg", fmt.Sprintf("%d", cfg.StacBboxEPSG))
	}
	cmd := exec.CommandContext(ctx, commandFrom(cfg), args...)
	if cfg.ModuleDir != "" {
		cmd.Env = append(cmd.Environ(), "PYTHONPATH="+cfg.ModuleDir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("roof model timed out after %s", timeoutFrom(cfg))
	}
	if err != nil {
		// The module reports failures as JSON on stderr so an operator sees a
		// reason ("credentials rejected") rather than a Python traceback. It
		// is not alone on that stream: third-party libraries write warnings
		// there too (requests' RequestsDependencyWarning buried the real
		// message behind a generic "exit status 1" in live testing), so when
		// the whole stream doesn't parse, the module's contract — one JSON
		// document as the final line — still does.
		var me moduleError
		errOut := bytes.TrimSpace(stderr.Bytes())
		if jsonErr := json.Unmarshal(errOut, &me); jsonErr != nil || me.Error == "" {
			if i := bytes.LastIndexByte(errOut, '\n'); i >= 0 {
				_ = json.Unmarshal(bytes.TrimSpace(errOut[i+1:]), &me)
			}
		}
		if me.Error != "" {
			return nil, fmt.Errorf("roof model: %s", me.Error)
		}
		return nil, fmt.Errorf("roof model failed: %w", err)
	}
	if stdout.Len() > maxOutputBytes {
		return nil, fmt.Errorf("roof model returned %d bytes, refusing", stdout.Len())
	}
	return stdout.Bytes(), nil
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
			RatedW:     a.RatedW,
			TiltDeg:    &tiltDeg,
			AzimuthDeg: &azimuthDeg,
		})
	}
	return out
}
