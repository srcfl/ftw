package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// Editing a driver on a live battery should not be a decision you make once
// and forget. A draft runs for a fixed window and puts the old file back on its
// own unless it is kept, so the failure mode of walking away is the driver you
// started with.
const (
	draftDirName       = ".ftw-draft"
	draftMinWindow     = time.Minute
	draftMaxWindow     = 60 * time.Minute
	draftDefaultWindow = 10 * time.Minute
)

// draftRecord is written next to the saved original so a restart can undo a
// draft whose timer died with the process.
type draftRecord struct {
	DriverID    string `json:"driver_id"`
	Filename    string `json:"filename"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
	HadOriginal bool   `json:"had_original"`
}

type driverDrafts struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

func newDriverDrafts() *driverDrafts {
	return &driverDrafts{timers: map[string]*time.Timer{}}
}

func (d *driverDrafts) arm(filename string, in time.Duration, revert func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.timers[filename]; ok {
		existing.Stop()
	}
	d.timers[filename] = time.AfterFunc(in, revert)
}

func (d *driverDrafts) disarm(filename string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.timers[filename]; ok {
		existing.Stop()
		delete(d.timers, filename)
	}
}

func (s *Server) draftDir() string {
	if s.deps.UserDriverDir == "" {
		return ""
	}
	return filepath.Join(s.deps.UserDriverDir, draftDirName)
}

// draftPaths names the three files a draft touches: the live overlay copy, the
// saved original, and the record describing it.
func (s *Server) draftPaths(filename string) (live, original, record string) {
	return filepath.Join(s.deps.UserDriverDir, filename),
		filepath.Join(s.draftDir(), filename+".original"),
		filepath.Join(s.draftDir(), filename+".json")
}

func (s *Server) handleDriverDraft(w http.ResponseWriter, r *http.Request) {
	if s.deps.UserDriverDir == "" || s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "custom drivers are not enabled on this gateway"})
		return
	}
	var body struct {
		Lua     string `json:"lua"`
		Minutes int    `json:"minutes,omitempty"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(body.Lua) == "" {
		writeJSON(w, 400, map[string]string{"error": "the draft is empty"})
		return
	}
	if len(body.Lua) > maxDriverSourceBytes {
		writeJSON(w, 400, map[string]string{"error": errDriverSourceTooLarge.Error()})
		return
	}
	window := draftDefaultWindow
	if body.Minutes > 0 {
		window = time.Duration(body.Minutes) * time.Minute
	}
	if window < draftMinWindow || window > draftMaxWindow {
		writeJSON(w, 400, map[string]string{"error": "the window must be between 1 and 60 minutes"})
		return
	}

	id := r.PathValue("id")
	entry, err := s.catalogEntryByID(id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	// A draft that does not compile, or that renames itself into another
	// driver's slot, never reaches the overlay.
	if err := validateDraftSource(body.Lua, entry); err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}

	if !s.driverUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "another driver update is in progress"})
		return
	}
	defer s.driverUpdateMu.Unlock()

	filename := driverFilename(entry)
	live, original, record := s.draftPaths(filename)
	if err := os.MkdirAll(s.draftDir(), 0o750); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	// Save whatever the overlay held, so reverting restores an operator's own
	// override rather than merely deleting the draft on top of it.
	hadOriginal := false
	if existing, err := os.ReadFile(live); err == nil {
		if err := os.WriteFile(original, existing, 0o600); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		hadOriginal = true
	} else if !errors.Is(err, os.ErrNotExist) {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	expiresAt := time.Now().Add(window)
	meta, _ := json.Marshal(draftRecord{
		DriverID: entry.ID, Filename: filename,
		ExpiresAtMS: expiresAt.UnixMilli(), HadOriginal: hadOriginal,
	})
	if err := os.WriteFile(record, meta, 0o600); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err := replaceFile(live, []byte(body.Lua)); err != nil {
		_ = os.Remove(record)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	if err := s.restartDriversUsing(r.Context(), filename); err != nil {
		// The draft is what broke it, so undo before reporting.
		s.revertDraft(filename)
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}

	s.drafts.arm(filename, window, func() { s.expireDraft(filename) })
	writeJSON(w, 200, map[string]any{
		"status":        "running",
		"driver_id":     entry.ID,
		"expires_at_ms": expiresAt.UnixMilli(),
		"minutes":       int(window / time.Minute),
	})
}

// handleDriverDraftKeep stops the clock. The draft stays as an ordinary local
// override, which already shadows the channel and already reports when a newer
// version exists.
func (s *Server) handleDriverDraftKeep(w http.ResponseWriter, r *http.Request) {
	entry, err := s.catalogEntryByID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	filename := driverFilename(entry)
	_, original, record := s.draftPaths(filename)
	if _, err := os.Stat(record); err != nil {
		writeJSON(w, 409, map[string]string{"error": "no draft is running for this driver"})
		return
	}
	s.drafts.disarm(filename)
	_ = os.Remove(original)
	_ = os.Remove(record)
	writeJSON(w, 200, map[string]any{"status": "kept", "driver_id": entry.ID})
}

func (s *Server) handleDriverDraftRevert(w http.ResponseWriter, r *http.Request) {
	entry, err := s.catalogEntryByID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	filename := driverFilename(entry)
	if _, _, record := s.draftPaths(filename); !fileExists(record) {
		writeJSON(w, 409, map[string]string{"error": "no draft is running for this driver"})
		return
	}
	if !s.driverUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "another driver update is in progress"})
		return
	}
	defer s.driverUpdateMu.Unlock()
	s.drafts.disarm(filename)
	if err := s.revertDraftAndRestart(r.Context(), filename); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "reverted", "driver_id": entry.ID})
}

// handleDriverDraftStatus lets the panel show a countdown that survives a
// reload of the page.
func (s *Server) handleDriverDraftStatus(w http.ResponseWriter, r *http.Request) {
	entry, err := s.catalogEntryByID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	_, _, record := s.draftPaths(driverFilename(entry))
	raw, err := os.ReadFile(record)
	if err != nil {
		writeJSON(w, 200, map[string]any{"running": false})
		return
	}
	var rec draftRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		writeJSON(w, 200, map[string]any{"running": false})
		return
	}
	writeJSON(w, 200, map[string]any{
		"running":       true,
		"expires_at_ms": rec.ExpiresAtMS,
	})
}

func (s *Server) expireDraft(filename string) {
	s.driverUpdateMu.Lock()
	defer s.driverUpdateMu.Unlock()
	s.drafts.disarm(filename)
	if err := s.revertDraftAndRestart(context.Background(), filename); err != nil {
		slog.Warn("driver draft expired but could not be reverted", "filename", filename, "err", err)
	}
}

func (s *Server) revertDraftAndRestart(ctx context.Context, filename string) error {
	if !s.revertDraft(filename) {
		// Nothing was running, so there is nothing to restart back onto.
		return nil
	}
	return s.restartDriversUsing(ctx, filename)
}

// revertDraft puts the overlay back exactly as it was: an operator's own
// override if there was one, otherwise nothing at all so the channel or the
// bundled copy resolves again.
//
// The record is what says a draft is running, and it is checked first. Keeping
// a draft removes the record, so a timer that fires in the same instant would
// otherwise delete the file the operator just chose to keep.
func (s *Server) revertDraft(filename string) bool {
	live, original, record := s.draftPaths(filename)
	if !fileExists(record) {
		return false
	}
	if saved, err := os.ReadFile(original); err == nil {
		_ = replaceFile(live, saved)
	} else {
		_ = os.Remove(live)
	}
	_ = os.Remove(original)
	_ = os.Remove(record)
	return true
}

// RevertDraftsOnStart undoes every draft left behind by a restart. The timer
// that would have expired them died with the process, and a driver running
// after a reboot should be the one that was chosen, not a forgotten
// experiment.
func (s *Server) RevertDraftsOnStart() {
	dir := s.draftDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec draftRecord
		if err := json.Unmarshal(raw, &rec); err != nil || rec.Filename == "" {
			continue
		}
		if filepath.Base(rec.Filename) != rec.Filename {
			continue
		}
		s.revertDraft(rec.Filename)
		slog.Info("reverted driver draft left by a restart", "filename", rec.Filename)
	}
}

// validateDraftSource refuses a draft before it can reach the overlay: it must
// compile, and it must still be the driver whose slot it is taking.
func validateDraftSource(source string, entry *drivers.CatalogEntry) error {
	L := lua.NewState()
	defer L.Close()
	if _, err := L.LoadString(source); err != nil {
		return fmt.Errorf("the draft does not compile: %w", err)
	}
	tmp, err := os.CreateTemp("", "ftw-draft-*.lua")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(source); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	meta, err := drivers.ParseCatalogFile(tmp.Name())
	if err != nil {
		return err
	}
	if meta.ID != entry.ID {
		return fmt.Errorf("the draft declares id %q, but this is %q", meta.ID, entry.ID)
	}
	return nil
}

func (s *Server) catalogEntryByID(id string) (*drivers.CatalogEntry, error) {
	if id == "" {
		return nil, errors.New("driver id is required")
	}
	catalog, err := drivers.LoadCatalogMulti(s.deps.UserDriverDir, s.managedDriverDir(), s.deps.DriverDir)
	if err != nil {
		return nil, err
	}
	for i := range catalog {
		if catalog[i].ID == id {
			return &catalog[i], nil
		}
	}
	return nil, errors.New("no driver with that id")
}

func driverFilename(entry *drivers.CatalogEntry) string {
	if entry.Filename != "" {
		return filepath.Base(entry.Filename)
	}
	return filepath.Base(entry.Path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// replaceFile writes through a temporary file in the same directory and renames
// it into place.
//
// Writing to the path directly fails when the file is owned by someone else,
// which is the normal case for an operator's own override: they copy it in as
// themselves, while the gateway runs as its own user. Rename needs write
// permission on the directory, not on the file -- and it is atomic, so a
// driver can never be caught half-written.
func replaceFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// restartDriversUsing points every configured driver with this filename at
// whichever overlay now holds it, and restarts it.
//
// The overlay search runs once, when the config is loaded, so cfg.Lua is an
// absolute path into the overlay that answered back then. Writing a draft into
// the user overlay does not move it — the driver would restart against the
// file it was already running. This is the same repointing the managed
// installer does when it activates an artifact.
func (s *Server) restartDriversUsing(ctx context.Context, filename string) error {
	target := s.resolveOverlayPath(filename)
	if target == "" {
		return fmt.Errorf("no copy of %s is on disk", filename)
	}
	s.deps.CfgMu.Lock()
	var affected []config.Driver
	for i := range s.deps.Cfg.Drivers {
		if filepath.Base(s.deps.Cfg.Drivers[i].Lua) != filename {
			continue
		}
		s.deps.Cfg.Drivers[i].Lua = target
		affected = append(affected, s.deps.Cfg.Drivers[i])
	}
	s.deps.CfgMu.Unlock()
	for _, d := range affected {
		if err := s.deps.Registry.Restart(ctx, d); err != nil {
			return fmt.Errorf("restart driver %s: %w", d.Name, err)
		}
	}
	return nil
}

// resolveOverlayPath answers with the file the driver resolver would pick,
// searching the overlays in the order it uses them.
func (s *Server) resolveOverlayPath(filename string) string {
	for _, dir := range []string{s.deps.UserDriverDir, s.managedDriverDir(), s.deps.DriverDir} {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}
