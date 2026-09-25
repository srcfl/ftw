package driverrepo

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// Replacement is a device moved from a retired catalog driver to the
// release's driver that replaces it.
type Replacement struct {
	Driver      string `json:"driver"`
	From        string `json:"from"`
	To          string `json:"to"`
	LogicalPath string `json:"logical_path"`
}

// MigrateReplaced moves devices off a catalog driver that one of this
// release's drivers declares it replaces (DRIVER.replaces), such as
// esphome-dsmr folded into esphome_dsmr. A configured driver that runs the
// owner's selection of the retired driver now names the release's file, and
// the old selection ends. An operator's own file of the old name in userDir
// is left alone. cfg must hold portable driver paths ("drivers/x.lua"); the
// caller saves the configuration when anything moved.
func (m *Manager) MigrateReplaced(cfg *config.Config, userDir string) []Replacement {
	if m.store == nil || m.bundledDir == "" || cfg == nil {
		return nil
	}
	entries, err := drivers.LoadCatalog(m.bundledDir)
	if err != nil {
		return nil
	}
	replacing := make(map[string]drivers.CatalogEntry)
	for _, entry := range entries {
		for _, old := range entry.Replaces {
			replacing[old] = entry
		}
	}
	if len(replacing) == 0 {
		return nil
	}
	active, err := m.store.ActiveDriverRepoInstalls()
	if err != nil {
		slog.Warn("driver repository: read active state", "err", err)
		return nil
	}
	var moved []Replacement
	for _, installed := range active {
		replacement, ok := replacing[installed.DriverID]
		if !ok {
			continue
		}
		rel := strings.TrimPrefix(installed.LogicalPath, "drivers/")
		if userDir != "" {
			if _, err := os.Stat(filepath.Join(userDir, filepath.FromSlash(rel))); err == nil {
				continue
			}
		}
		target := "drivers/" + replacement.Filename
		var names []string
		for i := range cfg.Drivers {
			if filepath.ToSlash(cfg.Drivers[i].Lua) == installed.LogicalPath {
				cfg.Drivers[i].Lua = target
				names = append(names, cfg.Drivers[i].Name)
			}
		}
		if err := m.Deactivate(installed.LogicalPath); err != nil {
			slog.Warn("driver repository: end replaced selection", "path", installed.LogicalPath, "err", err)
		}
		for _, name := range names {
			slog.Info("driver repository: device moved to the release's replacing driver",
				"driver", name, "from", installed.DriverID, "to", replacement.ID, "path", target)
			moved = append(moved, Replacement{Driver: name, From: installed.DriverID, To: replacement.ID, LogicalPath: target})
		}
	}
	return moved
}
