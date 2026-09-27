package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/selfupdate"
)

// handleVersionCheck returns self-update state. A normal GET still asks the
// checker so a previous GitHub 5xx can be retried; the checker itself skips
// the network when a successful result is younger than half the interval.
// ?force=1 always contacts GitHub. All fields in selfupdate.Info are passed
// through verbatim so the UI does the rendering.
func (s *Server) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	force := r.URL.Query().Get("force") == "1"
	info, err := s.deps.SelfUpdate.Check(r.Context(), force)
	if err != nil {
		// Return the full Info schema with Err populated so the UI has
		// one shape to handle (not a special error envelope).
		info.Err = err.Error()
		if force {
			writeJSON(w, 502, info)
			return
		}
		writeJSON(w, 200, info)
		return
	}
	writeJSON(w, 200, s.deps.SelfUpdate.Info())
}

// handleVersionChannel persists the selected release stream. Changing the
// channel only clears the cached target; installing a release still needs
// the update endpoint.
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
	case "starting", "pulling", "checking", "restarting":
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

// handleVersionUpdate starts a native update to the latest release on the
// saved channel. It returns once the run is accepted; the caller follows
// /api/version/update/status. A native update takes no rollback point
// (ADR 0007, decision 3): binary rollback keeps the data in place, and a
// release that changes the state schema is refused until it has its own
// backup path.
func (s *Server) handleVersionUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	info := s.deps.SelfUpdate.Info()
	if !info.UpdateAvailable {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no newer native release is available on this channel"})
		return
	}
	// A release that failed on this box is not installed again by an
	// unattended caller, whatever client it uses; {"retry": true} asks.
	if info.LastFailed != "" && info.LastFailed == info.Latest {
		var request struct {
			Retry bool `json:"retry"`
		}
		_ = readJSON(r, &request) // an empty body asks for no retry
		if !request.Retry {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": info.Latest + ` failed on this box; send {"retry": true} to try it again`,
			})
			return
		}
	}
	// Refuse before the download: the slot swap refuses a state-schema
	// change only after the package is on disk, and nothing records that
	// refusal, so a scheduled caller would download it again every time.
	if info.FullBackupRequired {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf(
			"%s changes stored data (state schema %d -> %d), which a native update cannot take yet; %s stays installed",
			info.Latest, info.CurrentStateSchema, info.TargetStateSchema, info.Current)})
		return
	}
	if !info.InstallReady {
		writeJSON(w, 502, map[string]string{"error": "selfupdate: native release slot not ready"})
		return
	}
	if !s.versionUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "update already in progress"})
		return
	}

	startedAt := time.Now()
	start := selfupdate.UpdateStatus{
		State:          "starting",
		Action:         "update",
		Component:      "core",
		Target:         info.Latest,
		StartedAt:      startedAt,
		PhaseStartedAt: startedAt,
		UpdatedAt:      startedAt,
		Message:        "starting update",
		TotalSteps:     selfupdate.NativeUpdateSteps,
	}
	s.writeVersionUpdateStatus(start)
	s.recordComponentStatus(start, info.Current)

	go s.runVersionUpdate(startedAt, info.Latest)

	writeJSON(w, 202, map[string]any{"status": "started", "action": "update", "target": info.Latest})
}

func (s *Server) runVersionUpdate(startedAt time.Time, latest string) {
	defer s.versionUpdateMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := s.deps.SelfUpdate.TriggerUpdate(ctx, latest, startedAt); err != nil {
		now := time.Now()
		s.writeVersionUpdateStatus(selfupdate.UpdateStatus{
			State:          "failed",
			Action:         "update",
			Component:      "core",
			Target:         latest,
			StartedAt:      startedAt,
			PhaseStartedAt: now,
			UpdatedAt:      now,
			Message:        err.Error(),
			TotalSteps:     selfupdate.NativeUpdateSteps,
		})
	}
}

func (s *Server) writeVersionUpdateStatus(st selfupdate.UpdateStatus) {
	if s.deps.SelfUpdate == nil {
		return
	}
	if err := s.deps.SelfUpdate.WriteStatus(st); err != nil {
		slog.Warn("selfupdate: write status failed", "state", st.State, "action", st.Action, "err", err)
	}
	s.recordComponentStatus(st, "")
}

// handleVersionRestart restarts the installed Core. No other version is
// selected.
func (s *Server) handleVersionRestart(w http.ResponseWriter, _ *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	if versionUpdateInFlight(s.deps.SelfUpdate.Status().State) || !s.versionUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "update already in progress"})
		return
	}
	defer s.versionUpdateMu.Unlock()
	if err := s.deps.SelfUpdate.TriggerRestart(); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{"status": "started", "action": "restart"})
}

func (s *Server) handleVersionBinaryRollback(w http.ResponseWriter, _ *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "native version rollback is unavailable"})
		return
	}
	if versionUpdateInFlight(s.deps.SelfUpdate.Status().State) || !s.versionUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "update already in progress"})
		return
	}
	defer s.versionUpdateMu.Unlock()
	previous, err := s.deps.SelfUpdate.TriggerNativeRollback()
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{"status": "started", "action": "rollback", "target": previous})
}

// handleVersionUpdateStatus passes through the saved update status. The
// file sits beside the release slots, so the Core that starts next serves
// the last transition (pulling → restarting → done) to a client that is
// still polling.
func (s *Server) handleVersionUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	status := s.deps.SelfUpdate.Status()
	s.recordComponentStatus(status, "")
	writeJSON(w, 200, status)
}
