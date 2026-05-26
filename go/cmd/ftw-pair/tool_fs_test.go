package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupScope(t *testing.T) (string, *Scope, *Audit) {
	t.Helper()
	repo := t.TempDir()
	state := t.TempDir()
	return repo, NewScope(repo, state), NewAudit()
}

func TestToolReadWriteRoundTrip(t *testing.T) {
	repo, sc, a := setupScope(t)
	w := NewWriteFileTool(sc, a)
	r := NewReadFileTool(sc)

	p := filepath.Join(repo, "x.txt")
	if _, err := w.Handle(context.Background(), map[string]any{"path": p, "content": "hello"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := r.Handle(context.Background(), map[string]any{"path": p})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m := out.(map[string]any)
	if m["content"] != "hello" {
		t.Fatalf("expected hello, got %v", m["content"])
	}
}

func TestToolFSOutOfScopeRejected(t *testing.T) {
	_, sc, a := setupScope(t)
	w := NewWriteFileTool(sc, a)
	if _, err := w.Handle(context.Background(), map[string]any{"path": "/etc/passwd", "content": ":"}); err == nil {
		t.Fatal("expected scope reject")
	}
}

func TestToolWriteRecordsDiff(t *testing.T) {
	repo, sc, a := setupScope(t)
	p := filepath.Join(repo, "y.txt")
	_ = os.WriteFile(p, []byte("OLD"), 0o644)
	w := NewWriteFileTool(sc, a)
	if _, err := w.Handle(context.Background(), map[string]any{"path": p, "content": "NEW"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	md := a.RenderMarkdown()
	if !strings.Contains(md, "-OLD") || !strings.Contains(md, "+NEW") {
		t.Fatalf("expected diff recorded, got:\n%s", md)
	}
}

func TestToolListDirectory(t *testing.T) {
	repo, sc, _ := setupScope(t)
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte{}, 0o644)
	_ = os.Mkdir(filepath.Join(repo, "sub"), 0o755)
	ls := NewListDirectoryTool(sc)
	out, err := ls.Handle(context.Background(), map[string]any{"path": repo})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	entries := out.(map[string]any)["entries"].([]map[string]any)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}
