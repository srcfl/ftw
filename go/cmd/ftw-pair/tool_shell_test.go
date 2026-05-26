package main

import (
	"context"
	"strings"
	"testing"
)

func TestToolRunCommandEcho(t *testing.T) {
	repo, sc, _ := setupScope(t)
	rc := NewRunCommandTool(sc)
	out, err := rc.Handle(context.Background(), map[string]any{
		"cmd":     "echo hello-from-pair",
		"workdir": repo,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := out.(map[string]any)
	if !strings.Contains(m["stdout"].(string), "hello-from-pair") {
		t.Fatalf("expected stdout, got %v", m)
	}
	if m["exit_code"].(int) != 0 {
		t.Fatalf("expected exit 0, got %v", m["exit_code"])
	}
}

func TestToolRunCommandWorkdirOutOfScope(t *testing.T) {
	_, sc, _ := setupScope(t)
	rc := NewRunCommandTool(sc)
	if _, err := rc.Handle(context.Background(), map[string]any{
		"cmd":     "ls",
		"workdir": "/etc",
	}); err == nil {
		t.Fatal("expected scope reject for /etc")
	}
}
