package main

import (
	"context"
	"testing"
	"time"
)

func TestSessionExpiresAtTTL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := NewSession(ctx, SessionConfig{
		TTL:    50 * time.Millisecond,
		Intent: "test driver",
	})
	if s.Remaining() <= 0 {
		t.Fatal("expected positive remaining at start")
	}
	select {
	case <-s.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("session did not expire")
	}
	if s.Remaining() > 0 {
		t.Fatalf("expected 0 remaining after expiry, got %s", s.Remaining())
	}
	if s.ExitReason() != "ttl_expired" {
		t.Fatalf("expected ttl_expired, got %q", s.ExitReason())
	}
}

func TestSessionEarlyAbort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := NewSession(ctx, SessionConfig{TTL: time.Hour, Intent: "x"})
	s.End("aborted_by_owner")

	select {
	case <-s.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("session did not end")
	}
	if s.ExitReason() != "aborted_by_owner" {
		t.Fatalf("expected aborted_by_owner, got %q", s.ExitReason())
	}
}
