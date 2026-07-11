package main

import (
	"net/http"
	"testing"
	"time"
)

// ratelimit_test.go — FIX-C: the per-source-IP token bucket and source-IP
// extraction. These guard that one abusive IP is bounded without affecting a
// different IP, and that the IP key can't be spoofed via a request header.

func TestIPRateLimiter_PerIPIsolation(t *testing.T) {
	l := newIPRateLimiter(3, 1) // cap 3, refill 1/s

	// Drain the attacker's bucket.
	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("attacker burst token %d should be allowed", i)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("attacker's 4th immediate request should be throttled")
	}
	// A DIFFERENT IP is completely unaffected — its bucket is full.
	for i := 0; i < 3; i++ {
		if !l.Allow("9.9.9.9") {
			t.Fatalf("victim IP request %d must be allowed despite attacker flood", i)
		}
	}
}

func TestIPRateLimiter_Refills(t *testing.T) {
	l := newIPRateLimiter(1, 100) // cap 1, refill 100/s → ~10ms per token
	if !l.Allow("a") {
		t.Fatal("first request should pass")
	}
	if l.Allow("a") {
		t.Fatal("second immediate request should be throttled")
	}
	time.Sleep(30 * time.Millisecond) // >= one token refilled
	if !l.Allow("a") {
		t.Fatal("request after refill window should be allowed")
	}
}

func TestIPRateLimiter_GC(t *testing.T) {
	l := newIPRateLimiter(2, 1)
	l.Allow("x")
	l.Allow("y")
	if n := l.GC(0); n != 2 {
		t.Fatalf("GC(0) removed %d, want 2", n)
	}
	// After GC the IP starts fresh (full bucket).
	if !l.Allow("x") {
		t.Fatal("post-GC IP should start with a fresh full bucket")
	}
}

// TestClientIP_UsesRemoteAddr_NotHeaders proves the rate-limit key comes from the
// transport RemoteAddr (un-spoofable) and IGNORES client-supplied X-Forwarded-For
// / CF-Connecting-IP — otherwise an attacker forges a fresh source per request and
// bypasses the per-IP limit entirely.
func TestClientIP_UsesRemoteAddr_NotHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "/signal/site/offer", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("CF-Connecting-IP", "10.0.0.2")
	if got := clientIP(req); got != "203.0.113.7" {
		t.Fatalf("clientIP = %q, want 203.0.113.7 (RemoteAddr host, headers ignored)", got)
	}
	// A bare RemoteAddr without a port is returned verbatim.
	req.RemoteAddr = "198.51.100.9"
	if got := clientIP(req); got != "198.51.100.9" {
		t.Fatalf("clientIP(bare) = %q, want 198.51.100.9", got)
	}
}
