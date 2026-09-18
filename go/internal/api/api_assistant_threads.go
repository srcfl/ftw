package api

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// Ask why history. The box stores the conversations because the box is the
// record: a question asked from a laptop is readable from a phone, and a
// closed dialog no longer throws the answer away.

const assistantThreadIDLen = 16

// newAssistantThreadID returns a random hex id. Ids are opaque to the UI.
func newAssistantThreadID() string {
	var b [assistantThreadIDLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// validAssistantThreadID accepts only what newAssistantThreadID produces, so
// a client cannot steer the id towards anything surprising.
func validAssistantThreadID(id string) bool {
	id = strings.TrimSpace(id)
	if len(id) != assistantThreadIDLen {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

// recordAssistantTurn appends the question and its answer to the thread the
// request names, or starts a new one. It returns the thread id the reply
// should carry; an empty string means nothing was stored.
//
// History comes from the request rather than from the row: the client is
// what decides which turns the model was given, and storage must not
// change that. Failing to store is not worth failing the answer over — the
// operator already has it on screen.
func (s *Server) recordAssistantTurn(req assistantAskRequest, answer, model string) string {
	if s.deps.State == nil {
		return ""
	}
	id := strings.TrimSpace(req.ThreadID)
	started := int64(0)
	var turns []state.AssistantTurn
	if validAssistantThreadID(id) {
		if prev, ok, err := s.deps.State.AssistantThreadByID(id); err == nil && ok {
			turns = prev.Turns
			started = prev.StartedMs
		} else {
			// The thread was pruned or never existed. Keep the id so the
			// open dialog and the row agree from here on.
			turns = nil
		}
	} else {
		id = newAssistantThreadID()
	}
	if id == "" {
		return ""
	}
	now := time.Now().UnixMilli()
	turns = append(turns,
		state.AssistantTurn{Role: "user", Text: req.Question, TsMs: now},
		state.AssistantTurn{Role: "assistant", Text: answer, TsMs: now},
	)
	if err := s.deps.State.SaveAssistantThread(state.AssistantThread{
		ID:        id,
		StartedMs: started,
		Model:     model,
		Turns:     turns,
	}); err != nil {
		slog.Warn("assistant thread not stored", "err", err)
		return ""
	}
	return id
}

func (s *Server) handleAssistantThreads(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, http.StatusOK, map[string]any{"threads": []state.AssistantThread{}})
		return
	}
	limit := 0
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	threads, err := s.deps.State.RecentAssistantThreads(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read Ask why history"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

func (s *Server) handleAssistantThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validAssistantThreadID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad thread id"})
		return
	}
	if s.deps.State == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such conversation"})
		return
	}
	t, ok, err := s.deps.State.AssistantThreadByID(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read the conversation"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such conversation"})
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleAssistantThreadDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validAssistantThreadID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad thread id"})
		return
	}
	if s.deps.State == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	if err := s.deps.State.DeleteAssistantThread(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete the conversation"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleAssistantThreadsClear(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
		return
	}
	if err := s.deps.State.DeleteAllAssistantThreads(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not clear Ask why history"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}
