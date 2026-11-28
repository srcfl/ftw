package api

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
)

// FirstRunProbes serves the device test and the network scan before a box
// has any settings, so the setup guide can name what the scan finds and test
// each device as the owner adds it. They run the code Settings runs: the
// same probe, the same fingerprints. With no saved settings there are no
// secrets to restore and no running driver to restart; nothing is written.
//
// No password exists yet, so anyone on the LAN can reach these. Two limits
// keep that small: a probe runs only a driver file from the box's own driver
// directories, and one probe or scan runs at a time. A second caller gets
// 429 until the first ends.
//
// deps needs ConfigPath, DriverDir, UserDriverDir and the protocol
// factories.
func FirstRunProbes(deps Deps) (driverTest, scan http.HandlerFunc) {
	s := &Server{deps: &deps}
	busy := make(chan struct{}, 1)
	one := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			select {
			case busy <- struct{}{}:
				defer func() { <-busy }()
				next(w, r)
			default:
				writeJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": "FTW is already testing a device or scanning. Try again when it ends.",
				})
			}
		}
	}
	driverTest = func(w http.ResponseWriter, r *http.Request) {
		var cfg config.Driver
		if err := readJSON(r, &cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid driver config: " + err.Error()})
			return
		}
		if !s.firstRunDriverFile(cfg.Lua) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "driver must be one of this box's drivers"})
			return
		}
		s.runDriverTest(w, r, cfg, nil)
	}
	return one(driverTest), one(s.handleScan)
}

// firstRunDriverFile reports whether lua resolves, as runDriverTest resolves
// it, to a .lua file inside the box's bundled or user driver directory.
// Anything else, such as /etc/hosts, would let a caller run or read a file
// the box never meant to load.
func (s *Server) firstRunDriverFile(lua string) bool {
	if strings.TrimSpace(lua) == "" {
		return true // runDriverTest answers a missing path itself
	}
	resolved := config.Config{Drivers: []config.Driver{{Lua: lua}}}
	resolved.ResolveDriverPaths(filepath.Dir(s.deps.ConfigPath))
	path := realPath(resolved.Drivers[0].Lua)
	if path == "" || !strings.EqualFold(filepath.Ext(path), ".lua") {
		return false
	}
	for _, dir := range []string{s.deps.DriverDir, s.deps.UserDriverDir} {
		root := realPath(dir)
		if root == "" {
			continue
		}
		if rel, err := filepath.Rel(root, path); err == nil && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

// realPath is an absolute path with symlinks followed, or "" when it does
// not exist.
func realPath(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	followed, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	return followed
}
