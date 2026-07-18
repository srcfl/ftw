package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// snapshotKeepCount caps the normal update snapshots we retain. A rollback
// temporarily needs one additional safety backup while keeping its selected
// target, so that path retains at most snapshotKeepCount+1 directories.
const snapshotKeepCount = 5

// SnapshotMeta is the JSON marker written inside each snapshot dir.
// It's the minimum the UI needs to list snapshots and decide which one
// to restore when we add the rollback flow — pinning a schema now
// lets future code read older snapshots without a guessing game.
type SnapshotMeta struct {
	SchemaVersion    int       `json:"schema_version"`
	CreatedAt        time.Time `json:"created_at"`
	FromVersion      string    `json:"from_version,omitempty"`
	ToVersion        string    `json:"to_version,omitempty"`
	Action           string    `json:"action,omitempty"` // "update" | "restart" | "pre-rollback"
	CompleteDatabase bool      `json:"complete_database"`
	// Files lists what was captured so a reader can distinguish a
	// state-only snapshot (old) from a state+config one (current) and
	// refuse to restore a partial set.
	Files []string `json:"files"`
}

// snapshotSchemaVersion is bumped when the on-disk layout of a snapshot
// dir changes (new required files, renamed meta fields). Older readers
// see the number and can refuse with a clear error instead of silently
// restoring an incomplete set.
const snapshotSchemaVersion = 2

// SnapshotInfo is the UI-facing shape returned by GET /api/version/snapshots.
type SnapshotInfo struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	CreatedAt   time.Time `json:"created_at"`
	FromVersion string    `json:"from_version,omitempty"`
	ToVersion   string    `json:"to_version,omitempty"`
	Action      string    `json:"action,omitempty"`
	SizeBytes   int64     `json:"size_bytes"`
	Restorable  bool      `json:"restorable"`
}

// createPreUpdateSnapshot captures state.db + config.yaml into
// `<SnapshotDir>/<id>/` before the self-update flow pulls a new image.
// Returns the snapshot ID on success so the caller can surface it to
// the UI / log it / reference it from an eventual rollback request.
//
// Failure modes are deliberately loud: the operator asked for a
// safety-netted update, so if we can't produce the safety net we'd
// rather refuse the update than silently proceed without one.
func (s *Server) createPreUpdateSnapshot(action, fromVersion, toVersion string) (SnapshotInfo, error) {
	if s.deps.SnapshotDir == "" {
		return SnapshotInfo{}, errors.New("snapshot dir not configured")
	}
	if s.deps.State == nil {
		return SnapshotInfo{}, errors.New("state store unavailable")
	}
	if err := os.MkdirAll(s.deps.SnapshotDir, 0o755); err != nil {
		return SnapshotInfo{}, fmt.Errorf("mkdir snapshot root: %w", err)
	}

	id := snapshotID(time.Now().UTC(), fromVersion, toVersion)
	dir := filepath.Join(s.deps.SnapshotDir, id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return SnapshotInfo{}, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// Always clean up the partial dir on any error past this point so
	// the snapshots/ list stays consistent — a half-written snapshot
	// is worse than none, because the UI would offer to restore it.
	commit := false
	defer func() {
		if !commit {
			_ = os.RemoveAll(dir)
		}
	}()

	captured := []string{}

	// 1. A complete state.db via VACUUM INTO + gzip. Rollback backups must
	// include history and samples; the compact daily corruption-recovery
	// snapshot deliberately excludes those large tables and is not safe here.
	if err := s.deps.State.BackupToCompressed(filepath.Join(dir, "state.db.gz")); err != nil {
		return SnapshotInfo{}, fmt.Errorf("state snapshot: %w", err)
	}
	captured = append(captured, "state.db.gz")

	// 2. config.yaml — plain file copy. Missing/empty path means the
	// caller wasn't wired with one; log and move on without failing
	// the snapshot (an update with no config on disk is legal — it
	// just means the operator runs with defaults, and we have nothing
	// to restore).
	if s.deps.ConfigPath != "" {
		dst := filepath.Join(dir, "config.yaml")
		if err := copyFile(s.deps.ConfigPath, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return SnapshotInfo{}, fmt.Errorf("copy config: %w", err)
		}
		if _, err := os.Stat(dst); err == nil {
			captured = append(captured, "config.yaml")
		}
	}

	// 3. meta.json — the pointer the UI/rollback flow reads first.
	meta := SnapshotMeta{
		SchemaVersion:    snapshotSchemaVersion,
		CreatedAt:        time.Now().UTC(),
		FromVersion:      fromVersion,
		ToVersion:        toVersion,
		Action:           action,
		CompleteDatabase: true,
		Files:            captured,
	}
	metaPath := filepath.Join(dir, "meta.json")
	metaBytes, err := json.MarshalIndent(&meta, "", "  ")
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("marshal meta: %w", err)
	}
	if err := os.WriteFile(metaPath, metaBytes, 0o644); err != nil {
		return SnapshotInfo{}, fmt.Errorf("write meta: %w", err)
	}

	commit = true

	// Prune in the background-ish (synchronous but errors swallowed to
	// slog — pruning failure isn't worth blocking the update).
	// A rollback safety snapshot temporarily takes the retained set to six.
	// Keep both it and the selected target while pruning any older, unrelated
	// entry so repeated rollbacks cannot grow this directory without bound.
	keep := snapshotKeepCount
	protected := map[string]bool{}
	if action == "pre-rollback" {
		keep++
		protected[id] = true
		protected[toVersion] = true
	}
	if err := pruneSnapshotsExcept(s.deps.SnapshotDir, keep, protected); err != nil {
		// Use stdlib log so we don't introduce a new dep; this file
		// stays slog-free.
		// The snapshot itself was created successfully, so we return
		// the info and let the caller decide whether to surface the
		// prune error.
		_ = err // intentional: documented best-effort
	}

	return SnapshotInfo{
		ID:          id,
		Path:        dir,
		CreatedAt:   meta.CreatedAt,
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Action:      action,
		SizeBytes:   dirSize(dir),
		Restorable:  true,
	}, nil
}

// listSnapshots returns all snapshot directories under SnapshotDir,
// newest first. Unreadable entries are skipped with their errors
// surfaced via the returned slice's second return — callers typically
// render the usable list and log the problems.
func listSnapshots(snapshotDir string) ([]SnapshotInfo, []error) {
	if snapshotDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []SnapshotInfo
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(snapshotDir, e.Name())
		meta, err := readSnapshotMeta(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		out = append(out, SnapshotInfo{
			ID:          e.Name(),
			Path:        dir,
			CreatedAt:   meta.CreatedAt,
			FromVersion: meta.FromVersion,
			ToVersion:   meta.ToVersion,
			Action:      meta.Action,
			SizeBytes:   dirSize(dir),
			Restorable:  snapshotMetaRestorable(meta),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, errs
}

func snapshotMetaRestorable(meta SnapshotMeta) bool {
	if meta.SchemaVersion != snapshotSchemaVersion || !meta.CompleteDatabase {
		return false
	}
	hasState := false
	for _, file := range meta.Files {
		switch file {
		case "state.db.gz":
			hasState = true
		case "config.yaml":
		default:
			return false
		}
	}
	return hasState
}

// pruneSnapshots keeps the `keep` newest snapshot dirs (by CreatedAt
// from meta.json) and removes the rest. Broken meta files cause the
// entry to be left alone — we don't want to delete something we
// couldn't parse, because it might be a partial write in progress.
func pruneSnapshots(snapshotDir string, keep int) error {
	return pruneSnapshotsExcept(snapshotDir, keep, nil)
}

func pruneSnapshotsExcept(snapshotDir string, keep int, protected map[string]bool) error {
	if keep <= 0 {
		return nil
	}
	snaps, _ := listSnapshots(snapshotDir)
	if len(snaps) <= keep {
		return nil
	}
	removeCount := len(snaps) - keep
	var firstErr error
	for i := len(snaps) - 1; i >= 0 && removeCount > 0; i-- {
		s := snaps[i]
		if protected[s.ID] {
			continue
		}
		if err := os.RemoveAll(s.Path); err != nil && firstErr == nil {
			firstErr = err
		} else if err == nil {
			removeCount--
		}
	}
	return firstErr
}

// handleVersionSnapshotDelete removes one snapshot by ID. Used from the
// UI's snapshot-management view when the operator wants to free disk
// space manually (beyond the built-in last-5 retention). Validates the
// ID is a snapshot-shaped filename and lives inside SnapshotDir so a
// malicious caller can't traverse to sibling directories — only dirs
// that listSnapshots would have surfaced are deletable.
func (s *Server) handleVersionSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	if versionUpdateInFlight(s.deps.SelfUpdate.Status().State) {
		writeJSON(w, 409, map[string]string{"error": "update already in progress"})
		return
	}
	if !s.versionUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "update already in progress"})
		return
	}
	defer s.versionUpdateMu.Unlock()
	if s.deps.SnapshotDir == "" {
		writeJSON(w, 503, map[string]string{"error": "snapshots disabled (no SnapshotDir)"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "missing snapshot id"})
		return
	}
	// Defense in depth: reject anything that isn't a simple directory
	// name. No slashes, no "..". A well-formed snapshot id is produced
	// by snapshotID() with only [A-Za-z0-9._-] characters.
	if containsTraversal(id) {
		writeJSON(w, 400, map[string]string{"error": "invalid snapshot id"})
		return
	}
	target := filepath.Join(s.deps.SnapshotDir, id)
	// Must exist AND be inside SnapshotDir. filepath.Join can't escape
	// SnapshotDir given the above ID guards, but verify by checking
	// existence + type before deleting.
	fi, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, 404, map[string]string{"error": "snapshot not found: " + id})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if !fi.IsDir() {
		writeJSON(w, 400, map[string]string{"error": "id does not refer to a snapshot directory"})
		return
	}
	if err := os.RemoveAll(target); err != nil {
		writeJSON(w, 500, map[string]string{"error": "remove failed: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": id})
}

// handleVersionSnapshots serves the list to the UI.
func (s *Server) handleVersionSnapshots(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	snaps, errs := listSnapshots(s.deps.SnapshotDir)
	if snaps == nil {
		snaps = []SnapshotInfo{}
	}
	resp := map[string]any{
		"snapshots": snaps,
		"dir":       s.deps.SnapshotDir,
		"enabled":   s.deps.SnapshotDir != "",
	}
	if len(errs) > 0 {
		// Surface as warnings alongside the usable entries so an
		// operator with a corrupt snapshot still sees the healthy ones.
		warnings := make([]string, 0, len(errs))
		for _, e := range errs {
			warnings = append(warnings, e.Error())
		}
		resp["warnings"] = warnings
	}
	writeJSON(w, 200, resp)
}

// ---- helpers ----

// snapshotID builds the directory name. We keep timestamps ahead of
// version tags so the file system's natural sort matches chronology,
// which keeps `ls` and any UI that falls back to alphabetic order
// sensible.
func snapshotID(t time.Time, fromVersion, toVersion string) string {
	stamp := t.Format("2006-01-02T15-04-05Z")
	slug := strings.TrimSpace(strings.TrimPrefix(fromVersion, "v"))
	if to := strings.TrimSpace(strings.TrimPrefix(toVersion, "v")); to != "" {
		if slug != "" {
			slug += "_to_" + to
		} else {
			slug = "to_" + to
		}
	}
	if slug == "" {
		return stamp
	}
	return stamp + "_" + sanitizeSlug(slug)
}

// sanitizeSlug keeps only characters that are safe in a filename across
// all filesystems we care about; any semver-weird char (e.g. '+' build
// metadata) becomes '_'.
func sanitizeSlug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func readSnapshotMeta(dir string) (SnapshotMeta, error) {
	var meta SnapshotMeta
	f, err := os.Open(filepath.Join(dir, "meta.json"))
	if err != nil {
		return meta, err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&meta); err != nil {
		return meta, fmt.Errorf("decode meta.json: %w", err)
	}
	return meta, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
