package api

import (
	"path/filepath"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// readOnlyDrivers maps each configured driver name to its catalog entry when
// the driver reads its device but cannot control it. A charger added through
// such a driver is accepted and shown, and FTW never steers it; the owner
// needs to be told so where they look at the charger, not left to guess.
func (s *Server) readOnlyDrivers() map[string]drivers.CatalogEntry {
	out := map[string]drivers.CatalogEntry{}
	if s.deps == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil {
		return out
	}
	s.deps.CfgMu.RLock()
	configured := append([]config.Driver(nil), s.deps.Cfg.Drivers...)
	s.deps.CfgMu.RUnlock()
	if len(configured) == 0 {
		return out
	}
	dir := s.deps.DriverDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(s.deps.ConfigPath), "drivers")
	}
	// Local override > owner's selected signed driver > the release's own
	// driver, the order the registry runs them in.
	catalog, err := drivers.LoadCatalogMulti(s.deps.UserDriverDir, s.managedDriverDir(), dir)
	if err != nil {
		return out
	}
	for _, d := range configured {
		if e, ok := drivers.FindCatalogEntry(catalog, d.Lua); ok && e.ReadOnly {
			out[d.Name] = e
		}
	}
	return out
}
