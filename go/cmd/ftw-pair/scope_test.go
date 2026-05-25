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

	// Build a traversal path that escapes both `repo` AND `os.TempDir()`
	// on any OS by going up 20 levels (POSIX clamps further `..` at root).
	// Linux: `/tmp/TestX/NNN` is 3 deep, macOS `/var/folders/.../T/TestX/NNN`
	// is ~7 deep; 20 covers either.
	escapingTraversal := filepath.Join(repo, strings.Repeat("../", 20)+"etc/passwd")

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
