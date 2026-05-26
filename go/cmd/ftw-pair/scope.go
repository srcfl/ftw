package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type Scope struct {
	roots []string // canonicalized
}

func NewScope(repoDir, stateDir string) *Scope {
	roots := []string{repoDir, stateDir, os.TempDir()}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if abs, err := filepath.Abs(r); err == nil {
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				out = append(out, resolved)
			} else {
				out = append(out, abs)
			}
		}
	}
	return &Scope{roots: out}
}

var ErrOutOfScope = errors.New("path is outside the allowed scope (repo, state-dir, /tmp)")

// Resolve canonicalizes p (resolving symlinks of all *existing* prefix
// components) and verifies the result is under one of the configured
// roots. Returns the canonical absolute path. Use this output for any
// subsequent fs op so callers can't trick us by passing a different
// path representation.
func (s *Scope) Resolve(p string) (string, error) {
	if p == "" {
		return "", ErrOutOfScope
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	// Walk down existing components and EvalSymlinks the deepest
	// existing one — this catches "symlink to /etc + /passwd" without
	// requiring the file to exist before the write.
	dir := abs
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolvedDir = dir
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return "", err
	}
	canonical := filepath.Join(resolvedDir, rel)

	for _, root := range s.roots {
		if canonical == root || strings.HasPrefix(canonical, root+string(filepath.Separator)) {
			return canonical, nil
		}
	}
	return "", ErrOutOfScope
}
