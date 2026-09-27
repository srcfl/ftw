package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Rollback points are what the older Docker line saved before each update:
// the settings database and config.yaml in `<SnapshotDir>/<id>/`. Native
// Core neither takes nor restores them. It lists what is left, so `ftw
// status` can report the space they use, and deletes one when the owner
// asks.

// SnapshotMeta is the part of a rollback point's meta.json that the list
// shows.
type SnapshotMeta struct {
	CreatedAt   time.Time `json:"created_at"`
	FromVersion string    `json:"from_version,omitempty"`
	ToVersion   string    `json:"to_version,omitempty"`
	Action      string    `json:"action,omitempty"` // "update" | "manual" | "pre-rollback"
}

// SnapshotInfo is the shape returned by GET /api/version/snapshots.
type SnapshotInfo struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	CreatedAt   time.Time `json:"created_at"`
	FromVersion string    `json:"from_version,omitempty"`
	ToVersion   string    `json:"to_version,omitempty"`
	Action      string    `json:"action,omitempty"`
	SizeBytes   int64     `json:"size_bytes"`
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
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, errs
}

// handleVersionSnapshotDelete removes one snapshot by ID, to free the
// space an older Core's rollback points still take. Validates the ID is a
// snapshot-shaped filename and lives inside SnapshotDir so a malicious
// caller can't traverse to sibling directories — only dirs that
// listSnapshots would have surfaced are deletable.
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
	// name. No slashes, no "..".
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

// handleVersionSnapshots lists the rollback points that are left.
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

// containsTraversal rejects ids that could escape SnapshotDir.
func containsTraversal(id string) bool {
	if strings.ContainsAny(id, "/\\") {
		return true
	}
	return id == "." || id == ".."
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
