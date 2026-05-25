package main

import (
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	p := buildPrompt("write a goodwe driver", "3h45m")

	// MCP server name must appear so Claude knows which tools belong to this server.
	if !strings.Contains(p, "ftw-remote") {
		t.Fatal("server name missing")
	}
	// First-action guidance: Claude must be told to fetch intent + state via ftw_api.
	if !strings.Contains(p, "/api/pair/status") {
		t.Fatal("missing instruction to fetch /api/pair/status")
	}
	if !strings.Contains(p, "/api/status") {
		t.Fatal("missing instruction to read /api/status")
	}
	// Wrap-up guidance: Claude must snapshot the changes, clone locally,
	// open the PR from the friend's own machine via gh.
	if !strings.Contains(p, "gh pr create") {
		t.Fatal("missing instruction to open PR via gh pr create")
	}
	if !strings.Contains(p, "git clone") {
		t.Fatal("missing instruction to clone the repo locally on the friend's machine")
	}
	if !strings.Contains(p, "session_log") {
		t.Fatal("missing instruction to call session_log before wrap-up")
	}
	if !strings.Contains(p, "pair-session.md") {
		t.Fatal("missing reference to the pair-session.md PR template")
	}
}
