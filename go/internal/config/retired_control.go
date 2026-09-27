package config

import (
	"encoding/json"
	"fmt"
)

// retiredControlSettings reads two removed control settings from YAML or a
// stored document. The typed Config no longer has a place for either.
//
// planner.use_energy_dispatch was the inverse of planner.legacy_dispatch. A
// site that set it chose a dispatch path on purpose, so loading moves the
// choice onto legacy_dispatch instead of dropping it. The old key won over
// legacy_dispatch when both were set, and it still does.
//
// batteries.<name>.weight was meant for the removed weighted mode, which never
// received it. It steered nothing, so it is dropped without a notice.
type retiredControlSettings struct {
	Planner *struct {
		UseEnergyDispatch *bool `yaml:"use_energy_dispatch" json:"use_energy_dispatch"`
	} `yaml:"planner" json:"planner"`
}

// applyRetiredControl moves a use_energy_dispatch choice onto
// legacy_dispatch and says so once.
func (c *Config) applyRetiredControl(legacy retiredControlSettings) {
	if legacy.Planner == nil || legacy.Planner.UseEnergyDispatch == nil {
		return
	}
	if c.Planner == nil {
		c.Planner = &Planner{}
	}
	c.Planner.LegacyDispatch = !*legacy.Planner.UseEnergyDispatch
	c.Retired = append(c.Retired, fmt.Sprintf(
		"planner.use_energy_dispatch was removed; it is kept as planner.legacy_dispatch: %t.",
		c.Planner.LegacyDispatch))
}

// storedRetiredControl names the removed control settings a stored document
// still carries.
func storedRetiredControl(saved map[string]json.RawMessage) []string {
	var removed []string
	var planner map[string]json.RawMessage
	if json.Unmarshal(saved["planner"], &planner) == nil {
		if _, ok := planner["use_energy_dispatch"]; ok {
			removed = append(removed, "planner.use_energy_dispatch, now planner.legacy_dispatch")
		}
	}
	var batteries map[string]map[string]json.RawMessage
	if json.Unmarshal(saved["batteries"], &batteries) == nil {
		for _, battery := range batteries {
			if _, ok := battery["weight"]; ok {
				removed = append(removed, "battery weights of the removed weighted mode")
				break
			}
		}
	}
	return removed
}
