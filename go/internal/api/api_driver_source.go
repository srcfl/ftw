package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/srcfl/ftw/go/internal/drivers"
)

var errDriverSourceTooLarge = errors.New("driver file is larger than the channel allows")

// maxDriverSourceBytes bounds what this endpoint will read into memory. The
// channel refuses to publish anything larger, so a file above it is not a
// driver we shipped.
const maxDriverSourceBytes = 512 * 1024

// handleDriverSource returns the Lua actually running for a driver, and says
// which of the three overlays it came from.
//
// A driver is one Lua file and the repository is the source of truth, but from
// the gateway there was no way to look at the code that is running. Reading it
// is the first half of being able to fix it.
func (s *Server) handleDriverSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "driver id is required"})
		return
	}
	catalog, err := drivers.LoadCatalogMulti(s.deps.UserDriverDir, s.managedDriverDir(), s.deps.DriverDir)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": err.Error()})
		return
	}
	var entry *drivers.CatalogEntry
	for i := range catalog {
		if catalog[i].ID == id {
			entry = &catalog[i]
			break
		}
	}
	if entry == nil {
		writeJSON(w, 404, map[string]string{"error": "no driver with that id"})
		return
	}

	path, source := s.resolveDriverSourcePath(entry)
	if path == "" {
		writeJSON(w, 404, map[string]string{"error": "the driver's file is not on disk"})
		return
	}
	raw, err := readDriverSourceFile(path)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	digest := sha256.Sum256(raw)

	writeJSON(w, 200, map[string]any{
		"id":       entry.ID,
		"path":     entry.Path,
		"filename": entry.Filename,
		"version":  entry.Version,
		"source":   source,
		"sha256":   hex.EncodeToString(digest[:]),
		"bytes":    len(raw),
		"lua":      string(raw),
		// Where this driver lives upstream, so the operator can read the
		// history behind the file rather than only its current contents.
		"repository_url": driverRepositoryURL(entry.Path),
		"read_only":      entry.ReadOnly,
	})
}

// resolveDriverSourcePath walks the same overlays the driver resolver does, in
// the same order, and reports which one answered.
func (s *Server) resolveDriverSourcePath(entry *drivers.CatalogEntry) (string, string) {
	name := entry.Filename
	if name == "" {
		name = filepath.Base(entry.Path)
	}
	for _, overlay := range []struct {
		dir    string
		source string
	}{
		{s.deps.UserDriverDir, "local"},
		{s.managedDriverDir(), "managed"},
		{s.deps.DriverDir, "bundled"},
	} {
		if overlay.dir == "" {
			continue
		}
		candidate := filepath.Join(overlay.dir, name)
		// A managed entry is a symlink into the content-addressed store.
		// EvalSymlinks keeps the read inside the directory we meant to read.
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			if info, err := os.Stat(resolved); err == nil && info.Mode().IsRegular() {
				return resolved, overlay.source
			}
		}
	}
	return "", ""
}

// readDriverSourceFile refuses anything past the limit rather than truncating
// it: half a driver shown as the whole driver is worse than an error.
func readDriverSourceFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxDriverSourceBytes {
		return nil, errDriverSourceTooLarge
	}
	return os.ReadFile(path)
}

const driverRepositoryBase = "https://github.com/srcfl/device-drivers/blob/main/drivers/lua/"

// driverRepositoryURL points at the shared source a driver was built from.
// The published artifact carries generated metadata the repository copy does
// not, so this is a link to the file's history, not to identical bytes.
func driverRepositoryURL(logicalPath string) string {
	name := filepath.Base(filepath.ToSlash(logicalPath))
	if name == "" || name == "." || name == "/" || !strings.HasSuffix(name, ".lua") {
		return ""
	}
	return driverRepositoryBase + name
}
