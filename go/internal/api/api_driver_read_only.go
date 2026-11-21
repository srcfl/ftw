package api

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// driverReadOnly is a configured driver whose device FTW reads but cannot
// control, with the reason the owner reads, when there is one.
type driverReadOnly struct {
	Reason string
}

// observeOnlyReason is the reason when the owner keeps FTW from steering the
// device with Observe only in Settings → Devices.
const observeOnlyReason = "Observe only is on for this device in Settings → Devices."

// readOnlyDrivers maps each configured driver name to why FTW cannot control
// its device. A charger added through such a driver is accepted and shown,
// and FTW never steers it; the owner needs to be told so where they look at
// the charger, not left to guess.
//
// The fact comes from what runs: the registry's loaded driver and the
// owner's observe_only. The catalog entry of the file the driver runs gives
// the reason, and the fact too while the driver is not loaded.
func (s *Server) readOnlyDrivers() map[string]driverReadOnly {
	out := map[string]driverReadOnly{}
	if s.deps == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil {
		return out
	}
	s.deps.CfgMu.RLock()
	configured := append([]config.Driver(nil), s.deps.Cfg.Drivers...)
	s.deps.CfgMu.RUnlock()
	for _, d := range configured {
		if d.Name == "" {
			continue
		}
		if d.ObserveOnly {
			out[d.Name] = driverReadOnly{Reason: observeOnlyReason}
			continue
		}
		readOnly, loaded := false, false
		if s.deps.Registry != nil {
			readOnly, loaded = s.deps.Registry.DeclaresReadOnly(d.Name)
		}
		if loaded && !readOnly {
			continue
		}
		entry, found := cachedCatalogEntry(s.driverFile(d.Lua))
		if !loaded {
			readOnly = found && entry.ReadOnly
		}
		if readOnly {
			out[d.Name] = driverReadOnly{Reason: entry.ReadOnlyReason}
		}
	}
	return out
}

// driverFile is the file a configured lua path runs. A running config holds
// resolved absolute paths; a relative "drivers/..." path is looked up the
// way config.ResolveDriverPaths does: local override, then the owner's
// signed driver, then the release's own.
func (s *Server) driverFile(lua string) string {
	if lua == "" || filepath.IsAbs(lua) {
		return lua
	}
	bundled := s.deps.DriverDir
	if bundled == "" {
		bundled = filepath.Join(filepath.Dir(s.deps.ConfigPath), "drivers")
	}
	if rel, ok := strings.CutPrefix(filepath.ToSlash(lua), "drivers/"); ok {
		for _, dir := range []string{s.deps.UserDriverDir, s.managedDriverDir()} {
			if dir == "" {
				continue
			}
			candidate := filepath.Join(dir, filepath.FromSlash(rel))
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		return filepath.Join(bundled, filepath.FromSlash(rel))
	}
	return filepath.Join(filepath.Dir(s.deps.ConfigPath), lua)
}

// catalogFiles holds each driver file's DRIVER metadata by path, size and
// modification time. The loadpoint list is read on every status poll; a
// file is parsed again only after it changes, so an edit, a version switch
// or a new install shows on the next poll without scanning every driver.
var catalogFiles = struct {
	sync.Mutex
	byPath map[string]cachedCatalogFile
}{byPath: map[string]cachedCatalogFile{}}

type cachedCatalogFile struct {
	modTime time.Time
	size    int64
	entry   drivers.CatalogEntry
	ok      bool
}

// parseCatalogFile is drivers.ParseCatalogFile; tests count its calls.
var parseCatalogFile = drivers.ParseCatalogFile

func cachedCatalogEntry(path string) (drivers.CatalogEntry, bool) {
	if path == "" {
		return drivers.CatalogEntry{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return drivers.CatalogEntry{}, false
	}
	catalogFiles.Lock()
	defer catalogFiles.Unlock()
	if c, ok := catalogFiles.byPath[path]; ok && c.modTime.Equal(info.ModTime()) && c.size == info.Size() {
		return c.entry, c.ok
	}
	entry, err := parseCatalogFile(path)
	c := cachedCatalogFile{modTime: info.ModTime(), size: info.Size(), entry: entry, ok: err == nil}
	catalogFiles.byPath[path] = c
	return c.entry, c.ok
}
