package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Device Support packages were a second driver format with a per-driver
// control opt-in. FTW no longer reads either. Loading drops what a site still
// stores so it cannot steer that site, and says so once.
const retiredDriverRepositoryFormat = "sourceful.driver-index/v1"

// retiredDriverSettings reads the one removed driver field whose presence is
// reported. The typed Driver no longer has a place for it.
type retiredDriverSettings struct {
	Drivers []struct {
		Name    string `yaml:"name" json:"name"`
		Control *struct {
			Enabled bool `yaml:"enabled" json:"enabled"`
		} `yaml:"control" json:"control"`
	} `yaml:"drivers" json:"drivers"`
}

// dropRetired removes Device Support repositories and records a notice for
// them and for any driver that still opted into package control.
func (c *Config) dropRetired(legacy retiredDriverSettings) {
	if repo := c.DeviceRepository; repo != nil && len(repo.Repositories) > 0 {
		kept := make([]DriverRepositorySource, 0, len(repo.Repositories))
		for _, source := range repo.Repositories {
			if source.Format != retiredDriverRepositoryFormat {
				kept = append(kept, source)
				continue
			}
			c.Retired = append(c.Retired, fmt.Sprintf(
				"device_repository %q was removed: FTW no longer reads the Device Support package format %s. FTW's own signed driver channel is unaffected.",
				source.ID, source.Format))
		}
		if len(kept) == 0 {
			// The same default a site with no listed source gets.
			kept = []DriverRepositorySource{defaultDriverRepository()}
		}
		repo.Repositories = kept
	}
	var optedIn []string
	for _, d := range legacy.Drivers {
		if d.Control != nil && d.Control.Enabled {
			optedIn = append(optedIn, fmt.Sprintf("%q", d.Name))
		}
	}
	if len(optedIn) > 0 {
		c.Retired = append(c.Retired, fmt.Sprintf(
			"driver control opt-in has no effect and was removed from %s: it applied only to Device Support packages, which FTW no longer runs.",
			strings.Join(optedIn, ", ")))
	}
}

// storedRetiredSettings names the removed settings a stored document still
// carries.
func storedRetiredSettings(saved map[string]json.RawMessage) []string {
	var removed []string
	if _, ok := saved["assistant"]; ok {
		removed = append(removed, "Ask why settings and API key")
	}
	var repo struct {
		Repositories []struct {
			Format string `json:"format"`
		} `json:"repositories"`
	}
	if json.Unmarshal(saved["device_repository"], &repo) == nil {
		for _, source := range repo.Repositories {
			if source.Format == retiredDriverRepositoryFormat {
				removed = append(removed, "Device Support package repositories")
				break
			}
		}
	}
	var drivers []map[string]json.RawMessage
	if json.Unmarshal(saved["drivers"], &drivers) == nil {
		for _, d := range drivers {
			if _, ok := d["control"]; ok {
				removed = append(removed, "driver control opt-ins")
				break
			}
		}
	}
	return removed
}
