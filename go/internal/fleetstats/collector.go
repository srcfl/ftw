package fleetstats

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const defaultMaxInstallations = 100_000
const recordRetention = 30 * 24 * time.Hour

type FleetRecord struct {
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Payload   Payload   `json:"payload"`
}

type Collector struct {
	path string
	max  int
	now  func() time.Time

	mu      sync.Mutex
	records map[string]FleetRecord
}

func NewCollector(path string, maxInstallations int) (*Collector, error) {
	if path == "" {
		return nil, errors.New("fleet collector path is required")
	}
	if maxInstallations <= 0 {
		maxInstallations = defaultMaxInstallations
	}
	c := &Collector{path: path, max: maxInstallations, now: time.Now, records: make(map[string]FleetRecord)}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &c.records); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return c, nil
}

func (c *Collector) Record(payload Payload) error {
	if err := payload.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	current := c.records[payload.InstallationID]
	if current.FirstSeen.IsZero() {
		current.FirstSeen = now
	}
	current.LastSeen, current.Payload = now, payload
	c.records[payload.InstallationID] = current
	c.pruneLocked(now)
	return c.persistLocked()
}

type DriverAggregate struct {
	ActiveInstallations int            `json:"active_installations"`
	HealthyInstances    int            `json:"healthy_instances"`
	ConfiguredInstances int            `json:"configured_instances"`
	Versions            map[string]int `json:"versions"`
}

type Aggregate struct {
	GeneratedAt       time.Time                  `json:"generated_at"`
	Active24H         int                        `json:"active_24h"`
	Active7D          int                        `json:"active_7d"`
	Healthy24H        int                        `json:"healthy_24h"`
	HealthySiteMeters int                        `json:"healthy_site_meters_24h"`
	CoreVersions      map[string]int             `json:"core_versions_24h"`
	CoreChannels      map[string]int             `json:"core_channels_24h"`
	OptimizerVersions map[string]int             `json:"optimizer_versions_24h"`
	OptimizerStatuses map[string]int             `json:"optimizer_statuses_24h"`
	Drivers           map[string]DriverAggregate `json:"drivers_24h"`
}

func (c *Collector) Aggregate() Aggregate {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	out := Aggregate{
		GeneratedAt: now, CoreVersions: make(map[string]int), CoreChannels: make(map[string]int),
		OptimizerVersions: make(map[string]int), OptimizerStatuses: make(map[string]int),
		Drivers: make(map[string]DriverAggregate),
	}
	for _, record := range c.records {
		if now.Sub(record.LastSeen) <= 7*24*time.Hour {
			out.Active7D++
		}
		if now.Sub(record.LastSeen) > 24*time.Hour {
			continue
		}
		out.Active24H++
		out.CoreVersions[record.Payload.Core.Version]++
		if record.Payload.Core.Channel != "" {
			out.CoreChannels[record.Payload.Core.Channel]++
		}
		if record.Payload.SiteMeterHealthy {
			out.Healthy24H++
			out.HealthySiteMeters++
		}
		if record.Payload.Optimizer != nil {
			out.OptimizerStatuses[record.Payload.Optimizer.Status]++
			version := record.Payload.Optimizer.Version
			if version == "" {
				version = record.Payload.Optimizer.Status
			}
			out.OptimizerVersions[version]++
		}
		seenDrivers := make(map[string]bool)
		seenVersions := make(map[string]bool)
		for _, driver := range record.Payload.Drivers {
			aggregate := out.Drivers[driver.ID]
			if aggregate.Versions == nil {
				aggregate.Versions = make(map[string]int)
			}
			if !seenDrivers[driver.ID] {
				aggregate.ActiveInstallations++
				seenDrivers[driver.ID] = true
			}
			aggregate.ConfiguredInstances += driver.Instances
			if driver.Status == "healthy" {
				aggregate.HealthyInstances += driver.Instances
			}
			versionKey := driver.ID + "\x00" + driver.Version
			if !seenVersions[versionKey] {
				aggregate.Versions[driver.Version]++
				seenVersions[versionKey] = true
			}
			out.Drivers[driver.ID] = aggregate
		}
	}
	return out
}

func (c *Collector) pruneLocked(now time.Time) {
	for id, record := range c.records {
		if now.Sub(record.LastSeen) > recordRetention {
			delete(c.records, id)
		}
	}
	if len(c.records) <= c.max {
		return
	}
	type seen struct {
		id string
		at time.Time
	}
	ordered := make([]seen, 0, len(c.records))
	for id, record := range c.records {
		ordered = append(ordered, seen{id: id, at: record.LastSeen})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].at.Before(ordered[j].at) })
	for i := 0; i < len(ordered)-c.max; i++ {
		delete(c.records, ordered[i].id)
	}
}

func (c *Collector) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o750); err != nil {
		return err
	}
	raw, err := json.Marshal(c.records)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".fleet-stats-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, c.path)
}
