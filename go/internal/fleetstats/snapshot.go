package fleetstats

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type SnapshotInput struct {
	CoreVersion string
	Channel     string
	Optimizer   *OptimizerStats
	Drivers     []config.Driver
	Catalog     []drivers.CatalogEntry
	Health      map[string]telemetry.DriverHealth
}

// BuildSnapshot deliberately joins health by the local config name but emits
// only public catalog IDs. User-chosen names and all config values disappear at
// this boundary.
func BuildSnapshot(input SnapshotInput) Payload {
	catalog := make(map[string]drivers.CatalogEntry, len(input.Catalog))
	for _, entry := range input.Catalog {
		catalog[entry.Filename] = entry
	}
	type groupKey struct{ id, version, source, status string }
	groups := make(map[groupKey]DriverStats)
	payload := Payload{Core: CoreStats{Version: input.CoreVersion, Channel: input.Channel}, Optimizer: input.Optimizer}
	for _, configured := range input.Drivers {
		status := configuredDriverStatus(configured.Disabled, input.Health[configured.Name])
		// Site health is a coarse boolean and is safe to retain even when the
		// site meter is a custom local driver whose identity must be discarded.
		if configured.IsSiteMeter && status == "healthy" {
			payload.SiteMeterHealthy = true
		}
		entry, ok := catalog[filepath.Base(configured.Lua)]
		// A local custom driver's ID is operator-authored and could itself carry
		// identifying information. Count it, but never serialize its metadata.
		if !ok || entry.ID == "" || entry.Version == "" || entry.Source == "local" {
			payload.UnidentifiedDriverCount++
			continue
		}
		version := entry.InstalledVersion
		if version == "" {
			version = entry.Version
		}
		source := entry.Source
		if source == "" {
			source = "bundled"
		}
		key := groupKey{entry.ID, version, source, status}
		stats := groups[key]
		stats.ID, stats.Version, stats.Source, stats.Status = entry.ID, version, source, status
		stats.HostAPIMin, stats.HostAPIMax = entry.HostAPIMin, entry.HostAPIMax
		stats.Instances++
		stats.Kinds = append([]string(nil), entry.Capabilities...)
		sort.Strings(stats.Kinds)
		groups[key] = stats
	}
	for _, stats := range groups {
		payload.Drivers = append(payload.Drivers, stats)
	}
	return payload
}

func configuredDriverStatus(disabled bool, health telemetry.DriverHealth) string {
	if disabled {
		return "disabled"
	}
	if health.LastSuccess == nil {
		return "starting"
	}
	if health.DeviceFault || health.Status == telemetry.StatusDegraded {
		return "degraded"
	}
	if health.Status == telemetry.StatusOffline {
		return "offline"
	}
	if strings.EqualFold(health.Status.String(), "ok") {
		return "healthy"
	}
	return "starting"
}
