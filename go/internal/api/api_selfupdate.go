package api

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/selfupdate"
)

// handleVersionCheck returns the cached self-update state. ?force=1 bypasses
// the cache and contacts GitHub directly. All fields in selfupdate.Info are
// passed through verbatim so the UI does the rendering.
func (s *Server) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if force {
		info, err := s.deps.SelfUpdate.Check(r.Context(), true)
		if err != nil {
			// Return the full Info schema with Err populated so the UI has
			// one shape to handle (not a special error envelope).
			info.Err = err.Error()
			writeJSON(w, 502, info)
			return
		}
	}
	writeJSON(w, 200, s.deps.SelfUpdate.Info())
}

// handleVersionChannel persists the selected release stream. Changing the
// channel only clears the cached target; image pulls still require the normal
// update endpoint and its pre-update snapshot.
func (s *Server) handleVersionChannel(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	var body struct {
		Channel string `json:"channel"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	channel, err := selfupdate.ParseChannel(body.Channel)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
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
	if err := s.deps.SelfUpdate.SetChannel(channel); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, s.deps.SelfUpdate.Info())
}

func versionUpdateInFlight(state string) bool {
	switch state {
	case "starting", "snapshotting", "pulling", "restarting", "restoring":
		return true
	default:
		return false
	}
}

// handleVersionSkip persists a dismissed version. A subsequent /check with a
// NEWER release resurfaces the notification automatically — Skip only hides
// the version passed in the body, not everything above it.
func (s *Server) handleVersionSkip(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := s.deps.SelfUpdate.Skip(body.Version); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "skipped": true, "version": body.Version})
}

// handleVersionUnskip clears the persisted skip. Called from the UI's
// "Check for updates" action so a user who skipped vX.Y.Z can resurface it
// without waiting for a newer release.
func (s *Server) handleVersionUnskip(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	if err := s.deps.SelfUpdate.Unskip(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "skipped": false})
}

// handleVersionUpdate signals the sidecar to pull the latest image + compose
// up the main service. Returns as soon as the sidecar acknowledges; the UI
// polls /api/version/update/status for progress.
//
// Before handing off to the sidecar we capture a rollback-point snapshot
// (state.db + config.yaml) into SnapshotDir. A failed snapshot aborts
// the update — the whole point of offering "Update" is that the user
// knows they can back out, and shipping without the safety net breaks
// that promise. Two exceptions skip the snapshot:
//
//   - SnapshotDir is empty (operator opted out at deploy time).
//   - The request body sets {"skip_snapshot": true} (operator opted
//     out for this specific update via the UI checkbox, typically
//     because the existing 5 retained snapshots already cover them).
//
// Both exceptions return \`snapshot_skipped: true\` in the response so
// the UI can differentiate "no snapshot taken on purpose" from "no
// snapshot field because the field was elided".
func (s *Server) handleVersionUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}

	// Body is optional — empty body / null JSON yields the zero value
	// (SkipSnapshot false), so pre-checkbox UIs keep getting the snapshot.
	var body struct {
		SkipSnapshot bool `json:"skip_snapshot,omitempty"`
	}
	// readJSON caps at 1 MB (api.go:153). Errors here include EOF on an
	// empty body, which we treat as the operator using the legacy no-body
	// path.
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
			return
		}
	}

	info := s.deps.SelfUpdate.Info()
	if !info.SidecarReady {
		writeJSON(w, 502, map[string]string{"error": "selfupdate: sidecar socket not ready"})
		return
	}
	if info.Latest == "" {
		writeJSON(w, 409, map[string]string{"error": "no update target available"})
		return
	}
	if !s.versionUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "update already in progress"})
		return
	}

	startedAt := time.Now()
	s.writeVersionUpdateStatus(selfupdate.UpdateStatus{
		State:     "starting",
		Action:    "update",
		Target:    info.Latest,
		StartedAt: startedAt,
		UpdatedAt: time.Now(),
		Message:   "starting update",
	})

	go s.runVersionUpdate(startedAt, info.Current, info.Latest, body.SkipSnapshot)

	resp := map[string]any{"status": "started", "action": "update", "target": info.Latest}
	if body.SkipSnapshot || s.deps.SnapshotDir == "" {
		resp["snapshot_skipped"] = true
	}
	writeJSON(w, 202, resp)
}

func (s *Server) runVersionUpdate(startedAt time.Time, current, latest string, skipSnapshot bool) {
	defer s.versionUpdateMu.Unlock()

	writeUpdateStatus := func(state, message string) {
		s.writeVersionUpdateStatus(selfupdate.UpdateStatus{
			State:     state,
			Action:    "update",
			Target:    latest,
			StartedAt: startedAt,
			UpdatedAt: time.Now(),
			Message:   message,
		})
	}

	snapshotSkipped := skipSnapshot || s.deps.SnapshotDir == ""
	if !snapshotSkipped {
		writeUpdateStatus("snapshotting", "creating backup snapshot")
		if _, err := s.createPreUpdateSnapshot("update", current, latest); err != nil {
			writeUpdateStatus("failed", "snapshot failed: "+err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.deps.SelfUpdate.Trigger(ctx, "update", latest); err != nil {
		writeUpdateStatus("failed", err.Error())
		return
	}
}

func (s *Server) writeVersionUpdateStatus(st selfupdate.UpdateStatus) {
	if s.deps.SelfUpdate == nil {
		return
	}
	if err := s.deps.SelfUpdate.WriteStatus(st); err != nil {
		slog.Warn("selfupdate: write status failed", "state", st.State, "action", st.Action, "err", err)
	}
}

// handleVersionRollback restores a specific snapshot over the main
// container's data volume ("soft" rollback — state.db + config.yaml only,
// image unchanged). Before triggering the sidecar we capture a
// *pre-rollback* safety snapshot so the operator can roll forward again
// if the restored state misbehaves. That extra snapshot is tagged as
// action="pre-rollback" so the UI can distinguish it from the routine
// pre-update set.
//
// Request body: {"snapshot_id": "<id>"}. The id must match a directory
// inside SnapshotDir; the validation rules mirror handleVersionSnapshotDelete.
//
// Scope of this endpoint (#152):
//   - Soft rollback only. Image version stays on the currently-running
//     tag. If the snapshot predates a state-schema change, a forward
//     rollback to the same version (or an explicit image pin) is needed
//     — tracked as a follow-up in #140 Phase 3.
//   - Pre-rollback safety snapshot is always created; no opt-out (unlike
//     the routine #149 opt-out). If disk is tight, delete older
//     snapshots via DELETE /api/version/snapshots/{id} first.
func (s *Server) handleVersionRollback(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	if s.deps.SnapshotDir == "" {
		writeJSON(w, 503, map[string]string{"error": "snapshots disabled (no SnapshotDir)"})
		return
	}
	var body struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if body.SnapshotID == "" {
		writeJSON(w, 400, map[string]string{"error": "snapshot_id required"})
		return
	}
	// Same id-shape validation as DELETE — cheap defence in depth.
	if containsTraversal(body.SnapshotID) {
		writeJSON(w, 400, map[string]string{"error": "invalid snapshot id"})
		return
	}
	// Validate the snapshot exists and looks plausible (has meta.json +
	// at least one file we can restore).
	snapDir := filepath.Join(s.deps.SnapshotDir, body.SnapshotID)
	meta, err := readSnapshotMeta(snapDir)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "snapshot not found or unreadable: " + err.Error()})
		return
	}
	if len(meta.Files) == 0 {
		writeJSON(w, 400, map[string]string{"error": "snapshot has no files recorded; cannot restore safely"})
		return
	}

	// Safety-net snapshot before we swap files. If the rolled-back
	// state turns out to be wrong, this is the forward-rollback point.
	info := s.deps.SelfUpdate.Info()
	safetyID := ""
	if safety, serr := s.createPreUpdateSnapshot("pre-rollback", info.Current, body.SnapshotID); serr != nil {
		writeJSON(w, 500, map[string]string{
			"error": "failed to capture pre-rollback safety snapshot: " + serr.Error(),
			"hint":  "Rollback aborted. Check SnapshotDir free space and retry, or delete stale snapshots first.",
		})
		return
	} else {
		safetyID = safety.ID
	}

	if err := s.deps.SelfUpdate.TriggerRollback(r.Context(), body.SnapshotID, meta.Files); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{
		"status":             "started",
		"action":             "rollback",
		"snapshot":           body.SnapshotID,
		"files":              meta.Files,
		"safety_snapshot_id": safetyID,
	})
}

// containsTraversal rejects ids that could escape SnapshotDir.
// Extracted so the rollback + delete handlers share the exact same
// rule — diverging them silently would open a path-traversal CVE.
func containsTraversal(id string) bool {
	if strings.ContainsAny(id, "/\\") {
		return true
	}
	return id == "." || id == ".."
}

// handleVersionRestart signals the sidecar to pull + force-recreate the
// main service regardless of whether a newer image exists. Exists so the
// full update flow can be exercised end-to-end in dev / CI before cutting
// a real release.
func (s *Server) handleVersionRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	if err := s.deps.SelfUpdate.Trigger(r.Context(), "restart", ""); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{"status": "started", "action": "restart"})
}

// handleVersionUpdateStatus passes through the sidecar's state.json. The
// shared Docker volume makes this survive the main container being recreated:
// the new container reads the same file written by the (still-running)
// sidecar and serves the last transition (pulling → restarting → done) to
// the UI which is still polling from the browser.
func (s *Server) handleVersionUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	writeJSON(w, 200, s.deps.SelfUpdate.Status())
}
