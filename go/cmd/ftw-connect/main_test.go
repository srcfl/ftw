package main

import (
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	p := buildPrompt("http://127.0.0.1:54321")

	// The local URL must appear so the agent knows where to curl.
	if !strings.Contains(p, "http://127.0.0.1:54321") {
		t.Fatal("local base URL missing")
	}
	// Discovery: curl /tools must be taught.
	if !strings.Contains(p, "/tools") {
		t.Fatal("missing /tools discovery instruction")
	}
	// Invocation: POST /tools/<name> must be taught.
	if !strings.Contains(p, "POST") || !strings.Contains(p, "/tools/ftw_api") {
		t.Fatal("missing POST /tools/<name> example")
	}
	// First-action guidance: agent must be told to fetch intent + state via ftw_api.
	if !strings.Contains(p, "/api/pair/status") {
		t.Fatal("missing instruction to fetch /api/pair/status")
	}
	if !strings.Contains(p, "/api/status") {
		t.Fatal("missing instruction to read /api/status")
	}
	// Wrap-up: snapshot, clone, gh pr create, session_log, pair-session.md.
	if !strings.Contains(p, "gh pr create") {
		t.Fatal("missing instruction to open PR via gh pr create")
	}
	if !strings.Contains(p, "git clone") {
		t.Fatal("missing instruction to clone the repo locally")
	}
	if !strings.Contains(p, "session_log") {
		t.Fatal("missing instruction to call session_log")
	}
	if !strings.Contains(p, "pair-session.md") {
		t.Fatal("missing reference to the pair-session.md PR template")
	}
	// The new prompt must NOT mention claude mcp / MCP server — that's the
	// whole point of this pivot (tool-agnostic, no friend-side config writes).
	if strings.Contains(p, "claude mcp add") {
		t.Fatal("prompt still mentions claude mcp add — should be agent-agnostic curl")
	}
	if strings.Contains(p, "MCP server") {
		t.Fatal("prompt still mentions MCP server — should be agent-agnostic curl")
	}
}
