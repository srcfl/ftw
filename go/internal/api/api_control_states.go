package api

import (
	"time"

	"github.com/srcfl/ftw/go/internal/notifications"
)

// ControlStates is the box's answer to "Are we in control?" for the
// control.not_following notification rule: the same verdicts GET
// /api/status serves in control_feedback, so a push never says what the
// box's page does not.
func (s *Server) ControlStates(now time.Time) []notifications.ControlState {
	rows := s.controlFeedback(now)
	out := make([]notifications.ControlState, 0, len(rows))
	for _, f := range rows {
		out = append(out, notifications.ControlState{Driver: f.Driver, Kind: f.Kind, Status: f.Status})
	}
	return out
}
