package main

import "testing"

func TestPollSecretsIssueCheckGC(t *testing.T) {
	p := NewPollSecrets()
	s1 := p.Issue("h1")
	if s1 == "" {
		t.Fatal("Issue returned an empty secret")
	}
	if s2 := p.Issue("h1"); s2 != s1 {
		t.Fatalf("Issue not stable for a host: %q != %q", s2, s1)
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
