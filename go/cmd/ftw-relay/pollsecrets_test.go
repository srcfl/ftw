package main

import (
	"errors"
	"testing"
)

// mustIssue mints a poll secret for hostID under a per-test principal, failing
// the test on the (FIX-1) principal-binding error path. Used by the relay tests
// that just need a host to authenticate its polls.
func mustIssue(t *testing.T, p *PollSecrets, hostID string) string {
	t.Helper()
	s, err := p.Issue(hostID, "test:"+hostID)
	if err != nil {
		t.Fatalf("Issue(%q): %v", hostID, err)
	}
	return s
}

func TestPollSecretsIssueCheckGC(t *testing.T) {
	p := NewPollSecrets()
	s1, err := p.Issue("h1", "principal-a")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if s1 == "" {
		t.Fatal("Issue returned an empty secret")
	}
	if s2, err := p.Issue("h1", "principal-a"); err != nil || s2 != s1 {
		t.Fatalf("Issue not stable for a host: %q != %q (err %v)", s2, s1, err)
	}
	if !p.Check("h1", s1) {
		t.Fatal("correct token must verify")
	}
	if p.Check("h1", "wrong") {
		t.Fatal("wrong token must NOT verify")
	}
	if p.Check("unknown-host", s1) {
		t.Fatal("unknown host must NOT verify")
	}
	if p.Check("h1", "") {
		t.Fatal("empty token must NOT verify")
	}
	// GC(0): every entry's last-seen is in the past, so all are evicted.
	if n := p.GC(0); n < 1 {
		t.Fatalf("GC(0) removed %d, want >= 1", n)
	}
	if p.Check("h1", s1) {
		t.Fatal("token must be gone after GC")
	}
}

// TestPollSecrets_PrincipalBinding proves a secret minted by one principal is
// never handed to a caller presenting a DIFFERENT principal for the same
// host_id — the FIX-1 disclosure guard. An empty principal is also refused.
func TestPollSecrets_PrincipalBinding(t *testing.T) {
	p := NewPollSecrets()
	owner, err := p.Issue("owner-h", "site:OWNERKEY")
	if err != nil {
		t.Fatalf("owner Issue: %v", err)
	}
	// A different principal (e.g. the friend path's pair token) must NOT be able
	// to retrieve the owner's secret for the same host_id.
	got, err := p.Issue("owner-h", "pair:attacker-token")
	if !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("cross-principal Issue err = %v, want ErrPrincipalMismatch", err)
	}
	if got == owner {
		t.Fatal("cross-principal Issue disclosed the owner secret")
	}
	// An empty principal is rejected so the binding can't be bypassed.
	if _, err := p.Issue("owner-h", ""); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("empty-principal Issue err = %v, want ErrPrincipalMismatch", err)
	}
	// The legitimate principal still gets the same stable secret.
	again, err := p.Issue("owner-h", "site:OWNERKEY")
	if err != nil || again != owner {
		t.Fatalf("same-principal Issue = %q (err %v), want stable %q", again, err, owner)
	}
}
