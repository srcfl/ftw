package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time checks.
var _ Tool = (*SessionLogTool)(nil)
var _ Tool = (*SessionRemainingTool)(nil)
var _ Tool = (*SessionEndTool)(nil)

// firstNonEmpty returns the first non-empty string from the arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── SessionLogTool ────────────────────────────────────────────────────────────

// SessionLogTool renders a markdown session report including the audit log.
type SessionLogTool struct {
	sess  *Session
	audit *Audit
}

func NewSessionLogTool(s *Session, a *Audit) *SessionLogTool {
	return &SessionLogTool{sess: s, audit: a}
}

func (t *SessionLogTool) Name() string { return "session_log" }

func (t *SessionLogTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "session_log",
		Description: "Return a markdown report of the current session state and full tool-call audit log.",
		InputSchema: map[string]any{"type": "object"},
	}
}

func (t *SessionLogTool) Handle(_ context.Context, _ map[string]any) (any, error) {
	exitReason := firstNonEmpty(t.sess.ExitReason(), "in_progress")

	var b strings.Builder
	fmt.Fprintf(&b, "# Session report\n\n")
	fmt.Fprintf(&b, "| Field | Value |\n")
	fmt.Fprintf(&b, "|---|---|\n")
	fmt.Fprintf(&b, "| session_id | %s |\n", t.sess.ID)
	fmt.Fprintf(&b, "| intent | %s |\n", t.sess.Intent())
	if friend := t.sess.As(); friend != "" {
		fmt.Fprintf(&b, "| friend | %s |\n", friend)
	}
	fmt.Fprintf(&b, "| started_at | %s |\n", t.sess.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| exit_reason | %s |\n", exitReason)
	fmt.Fprintf(&b, "\n")
	b.WriteString(t.audit.RenderMarkdown())

	return map[string]any{"markdown": b.String()}, nil
}

// ── SessionRemainingTool ──────────────────────────────────────────────────────

// SessionRemainingTool returns the number of seconds left in the session TTL.
type SessionRemainingTool struct {
	sess *Session
}

func NewSessionRemainingTool(s *Session) *SessionRemainingTool {
	return &SessionRemainingTool{sess: s}
}

func (t *SessionRemainingTool) Name() string { return "session_remaining" }

func (t *SessionRemainingTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "session_remaining",
		Description: "Return the number of seconds remaining in the current session TTL.",
		InputSchema: map[string]any{"type": "object"},
	}
}

func (t *SessionRemainingTool) Handle(_ context.Context, _ map[string]any) (any, error) {
	return map[string]any{"seconds": int(t.sess.Remaining().Seconds())}, nil
}

// ── SessionEndTool ────────────────────────────────────────────────────────────

// SessionEndTool terminates the session gracefully on behalf of the friend.
type SessionEndTool struct {
	sess *Session
}

func NewSessionEndTool(s *Session) *SessionEndTool {
	return &SessionEndTool{sess: s}
}

func (t *SessionEndTool) Name() string { return "session_end" }

func (t *SessionEndTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "session_end",
		Description: "End the current pair session. Call when the task is complete or the friend explicitly wants to close the session.",
		InputSchema: map[string]any{"type": "object"},
	}
}

func (t *SessionEndTool) Handle(_ context.Context, _ map[string]any) (any, error) {
	t.sess.End("ended_by_friend")
	return map[string]any{"ok": true}, nil
}
