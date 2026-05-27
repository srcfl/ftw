package main

import (
	"errors"
	"testing"
	"time"
)

func TestTokenRegisterStartsPending(t *testing.T) {
	r := NewTokenRegistry()
	tok, err := r.Register(TokenRegistration{
		HostID:       "host-a",
		Token:        "garage-coffee-river-bicycle-window-cat",
		TTL:          1 * time.Hour,
		ApprovalCode: "4827",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if tok.State() != TokenPending {
		t.Fatalf("expected pending, got %v", tok.State())
	}
	if tok.ApprovalCode() != "4827" {
		t.Fatalf("approval code lost")
	}
}

func TestTokenApproveActivates(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: time.Hour, ApprovalCode: "1234"})
	if err := r.Approve("tok1", "1234"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	tok, _ := r.Get("tok1")
	if tok.State() != TokenActive {
		t.Fatalf("expected active, got %v", tok.State())
	}
}

func TestTokenApproveWrongCodeFails(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: time.Hour, ApprovalCode: "1234"})
	if err := r.Approve("tok1", "9999"); !errors.Is(err, ErrBadApprovalCode) {
		t.Fatalf("want ErrBadApprovalCode, got %v", err)
	}
	tok, _ := r.Get("tok1")
	if tok.State() != TokenPending {
		t.Fatalf("wrong code must not activate")
	}
}

func TestTokenExpiresOnTTL(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: 30 * time.Millisecond, ApprovalCode: "1234"})
	time.Sleep(80 * time.Millisecond)
	tok, _ := r.Get("tok1")
	if tok.State() != TokenExpired {
		t.Fatalf("expected expired, got %v", tok.State())
	}
}

func TestTokenRevokeImmediate(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: time.Hour, ApprovalCode: "1234"})
	_ = r.Approve("tok1", "1234")
	r.Revoke("tok1")
	tok, _ := r.Get("tok1")
	if tok.State() != TokenRevoked {
		t.Fatalf("expected revoked, got %v", tok.State())
	}
}

func TestTokenRegisterRejectsDuplicate(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "dup", TTL: time.Hour, ApprovalCode: "1"})
	if _, err := r.Register(TokenRegistration{HostID: "host-b", Token: "dup", TTL: time.Hour, ApprovalCode: "2"}); !errors.Is(err, ErrTokenExists) {
		t.Fatalf("want ErrTokenExists, got %v", err)
	}
}

func TestTokenApprovalRateLimit(t *testing.T) {
	r := NewTokenRegistry()
	_, _ = r.Register(TokenRegistration{HostID: "host-a", Token: "tok1", TTL: time.Hour, ApprovalCode: "1234"})
	for i := 0; i < MaxApprovalAttempts; i++ {
		if err := r.Approve("tok1", "9999"); !errors.Is(err, ErrBadApprovalCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Next attempt — even with correct code — must fail.
	if err := r.Approve("tok1", "1234"); !errors.Is(err, ErrApprovalLocked) {
		t.Fatalf("want ErrApprovalLocked, got %v", err)
	}
}
