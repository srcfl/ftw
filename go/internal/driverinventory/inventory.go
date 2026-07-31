// Package driverinventory builds a bounded, secret-free snapshot of the Lua
// code that FTW has configured and loaded.
package driverinventory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const (
	SchemaVersion = "sourceful.driver-inventory/v1"
	RuntimeABI    = "gopher-lua-source-v1"
	HostAPI       = 1
	MaxDrivers    = 128
)

var driverIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type Host struct {
	Product       string `json:"product"`
	Version       string `json:"version"`
	UpdateChannel string `json:"update_channel"`
	Target        string `json:"target"`
	RuntimeABI    string `json:"runtime_abi"`
	HostAPI       int    `json:"host_api"`
}

type Health struct {
	OK       int `json:"ok"`
	Degraded int `json:"degraded"`
	Offline  int `json:"offline"`
	Unknown  int `json:"unknown"`
}

type Driver struct {
	DriverID            string `json:"driver_id"`
	Version             string `json:"version"`
	Source              string `json:"source"`
	PackageID           string `json:"package_id,omitempty"`
	RepositoryID        string `json:"repository_id,omitempty"`
	PackageChannel      string `json:"package_channel,omitempty"`
	ArtifactSHA256      string `json:"artifact_sha256,omitempty"`
	SourceSHA256        string `json:"source_sha256,omitempty"`
	ControlClass        string `json:"control_class"`
	ConfiguredInstances int    `json:"configured_instances"`
	RunningInstances    int    `json:"running_instances"`
	Health              Health `json:"health"`
}

type Snapshot struct {
	SchemaVersion string    `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Host          Host      `json:"host"`
	Drivers       []Driver  `json:"drivers"`
}

// RepositoryArtifact describes one activated repository file. PackageID and
// Channel are set only when a signed package supplied them.
type RepositoryArtifact struct {
	LogicalPath    string
	InstalledPath  string
	DriverID       string
	Version        string
	SHA256         string
	RepositoryID   string
	PackageID      string
	PackageChannel string
	ControlClass   string
}

type Input struct {
	HostVersion       string
	Drivers           []config.Driver
	RunningNames      []string
	Health            map[string]telemetry.DriverHealth
	UserDriverDir     string
	ManagedDriverDir  string
	BundledDriverDir  string
	RepositoryDrivers []RepositoryArtifact
}

func Build(now time.Time, in Input) (Snapshot, error) {
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now.UTC(),
		Host: Host{
			Product:       "ftw",
			Version:       hostVersion(in.HostVersion),
			UpdateChannel: updateChannel(in.HostVersion),
			Target:        "ftw-core",
			RuntimeABI:    RuntimeABI,
			HostAPI:       HostAPI,
		},
		Drivers: []Driver{},
	}

	running := make(map[string]bool, len(in.RunningNames))
	for _, name := range in.RunningNames {
		running[name] = true
	}
	artifacts := artifactByPath(in.RepositoryDrivers, in.ManagedDriverDir)
	grouped := make(map[string]*Driver)
	configured := 0
	for _, cfg := range in.Drivers {
		if cfg.Disabled {
			continue
		}
		configured++
		if configured > MaxDrivers {
			return Snapshot{}, fmt.Errorf("driver inventory exceeds %d configured instances", MaxDrivers)
		}
		row, err := inspectDriver(cfg.Lua, in, artifacts)
		if err != nil {
			return Snapshot{}, fmt.Errorf("inspect driver %q: %w", cfg.Name, err)
		}
		key := identityKey(row)
		item := grouped[key]
		if item == nil {
			copyRow := row
			grouped[key] = &copyRow
			item = &copyRow
		}
		item.ConfiguredInstances++
		if running[cfg.Name] {
			item.RunningInstances++
		}
		addHealth(&item.Health, in.Health[cfg.Name], running[cfg.Name], hasHealth(in.Health, cfg.Name))
	}

	for _, row := range grouped {
		snapshot.Drivers = append(snapshot.Drivers, *row)
	}
	sort.Slice(snapshot.Drivers, func(i, j int) bool {
		return identityKey(snapshot.Drivers[i]) < identityKey(snapshot.Drivers[j])
	})
	return snapshot, nil
}

func inspectDriver(path string, in Input, artifacts map[string]RepositoryArtifact) (Driver, error) {
	if strings.TrimSpace(path) == "" {
		return Driver{}, errors.New("Lua path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Driver{}, err
	}
	sum := sha256.Sum256(data)
	loadedSHA := hex.EncodeToString(sum[:])
	metadata, _ := drivers.ParseCatalogFile(path)
	id := metadata.ID
	if !driverIDRE.MatchString(id) {
		// File and instance names may contain customer or site details. Keep a
		// fixed fallback and let the source hash distinguish unknown drivers.
		id = "unknown"
	}
	version := metadata.Version
	if version == "" {
		version = "unknown"
	}
	row := Driver{DriverID: id, Version: version, ControlClass: "unknown"}
	if metadata.ReadOnlyDeclared {
		if metadata.ReadOnly {
			row.ControlClass = "read_only"
		} else {
			row.ControlClass = "control"
		}
	}

	if within(path, in.ManagedDriverDir) {
		rel, _ := filepath.Rel(in.ManagedDriverDir, path)
		artifact, ok := artifacts[filepath.ToSlash(rel)]
		if ok && strings.EqualFold(artifact.SHA256, loadedSHA) {
			row.DriverID = artifact.DriverID
			row.Version = artifact.Version
			row.RepositoryID = artifact.RepositoryID
			row.ArtifactSHA256 = strings.ToLower(artifact.SHA256)
			if artifact.ControlClass == "read_only" || artifact.ControlClass == "control" {
				row.ControlClass = artifact.ControlClass
			}
			if artifact.PackageID != "" && (artifact.PackageChannel == "beta" || artifact.PackageChannel == "stable") {
				row.Source = "managed"
				row.PackageID = artifact.PackageID
				row.PackageChannel = artifact.PackageChannel
			} else {
				row.Source = "legacy_repository"
			}
			return row, nil
		}
		// A missing or changed activation must not claim signed provenance.
		row.Source = "local"
		row.SourceSHA256 = loadedSHA
		return row, nil
	}
	if within(path, in.UserDriverDir) {
		row.Source = "local"
	} else if within(path, in.BundledDriverDir) {
		row.Source = "bundled"
	} else {
		row.Source = "local"
	}
	row.SourceSHA256 = loadedSHA
	return row, nil
}

func artifactByPath(items []RepositoryArtifact, managedDir string) map[string]RepositoryArtifact {
	out := make(map[string]RepositoryArtifact, len(items)*2)
	for _, item := range items {
		logical := filepath.ToSlash(strings.TrimPrefix(item.LogicalPath, "drivers/"))
		if logical != "" {
			out[logical] = item
		}
		if item.InstalledPath != "" && within(item.InstalledPath, managedDir) {
			rel, err := filepath.Rel(managedDir, item.InstalledPath)
			if err == nil {
				out[filepath.ToSlash(rel)] = item
			}
		}
	}
	return out
}

func addHealth(out *Health, health telemetry.DriverHealth, running, exists bool) {
	if !exists || !running {
		out.Unknown++
		return
	}
	if health.DeviceFault {
		out.Degraded++
		return
	}
	switch health.Status {
	case telemetry.StatusOk:
		out.OK++
	case telemetry.StatusDegraded:
		out.Degraded++
	case telemetry.StatusOffline:
		out.Offline++
	default:
		out.Unknown++
	}
}

func hasHealth(health map[string]telemetry.DriverHealth, name string) bool {
	_, ok := health[name]
	return ok
}

func identityKey(row Driver) string {
	return strings.Join([]string{
		row.DriverID, row.Version, row.Source, row.PackageID, row.RepositoryID,
		row.PackageChannel, row.ArtifactSHA256, row.SourceSHA256, row.ControlClass,
	}, "\x00")
}

func within(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func hostVersion(version string) string {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if version == "" {
		return "dev"
	}
	if len(version) > 64 {
		return version[:64]
	}
	return version
}

func updateChannel(version string) string {
	version = strings.ToLower(version)
	if strings.Contains(version, "-beta") {
		return "beta"
	}
	if version != "" && version != "dev" && !strings.Contains(version, "-") {
		return "stable"
	}
	return "unknown"
}
