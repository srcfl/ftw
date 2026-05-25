package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopeAllowsRepoStateAndTmp(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	sc := NewScope(repo, state)

	if _, err := sc.Resolve(filepath.Join(repo, "drivers", "foo.lua")); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if _, err := sc.Resolve(filepath.Join(state, "state.db")); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if _, err := sc.Resolve(filepath.Join(os.TempDir(), "x")); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestScopeRejectsTraversal(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	sc := NewScope(repo, state)

	// Build a traversal path that actually escapes past os.TempDir() by
	// climbing above every component of the resolved TempDir root.
	// t.TempDir() nests as T/TestName.../NNN so we need enough ".." to
	// reach above the entire TempDir base (which on macOS is 6+ levels deep).
	escapingTraversal := repo
	for range strings.Split(strings.TrimPrefix(filepath.Clean(os.TempDir()), string(filepath.Separator)), string(filepath.Separator)) {
		escapingTraversal = filepath.Join(escapingTraversal, "..")
	}
	escapingTraversal = filepath.Join(escapingTraversal, "etc", "passwd")

	cases := []string{
		"/etc/passwd",
		escapingTraversal,
		"~/.ssh/id_rsa",
		"",
	}
	for _, p := range cases {
		if _, err := sc.Resolve(p); err == nil {
			t.Fatalf("expected scope-reject for %q", p)
		}
	}
}

func TestScopeRejectsSymlinkEscape(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	sc := NewScope(repo, state)

	link := filepath.Join(repo, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := sc.Resolve(filepath.Join(link, "passwd")); err == nil {
		t.Fatal("expected reject on symlink-escape")
	}
}
