package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func pinHash(pin string) string { h := sha256.Sum256([]byte(pin)); return hex.EncodeToString(h[:]) }

func TestBootstrapStore_PutClaimBurn(t *testing.T) {
	s := NewBootstrapStore(64, 2048)
	desc := []byte(`{"site_id":"site:A","pi_pubkey":"x","label":"Home","sig":"y"}`)
	if err := s.Put("site:A", desc, pinHash("123456"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, site, ok := s.Claim(pinHash("123456"))
	if !ok || site != "site:A" || string(got) != string(desc) {
		t.Fatalf("Claim = %q,%q,%v want the descriptor for site:A", got, site, ok)
	}
	if _, _, ok := s.Claim(pinHash("000000")); ok {
		t.Fatal("wrong pin must miss")
	}
	s.Burn("site:A")
	if _, _, ok := s.Claim(pinHash("123456")); ok {
		t.Fatal("burned blob must be gone")
	}
}

func TestBootstrapStore_TTLAndGC(t *testing.T) {
	s := NewBootstrapStore(64, 2048)
	_ = s.Put("site:T", []byte("d"), pinHash("1"), -time.Second)
	if _, _, ok := s.Claim(pinHash("1")); ok {
		t.Fatal("expired blob must not claim")
	}
	if n := s.GC(); n != 1 {
		t.Fatalf("GC removed %d, want 1", n)
	}
}

func TestBootstrapStore_Caps(t *testing.T) {
	s := NewBootstrapStore(4, 2)
	if err := s.Put("site:A", []byte("toolong!"), pinHash("1"), time.Minute); err != ErrBootstrapTooLarge {
		t.Fatalf("oversize: %v want ErrBootstrapTooLarge", err)
	}
	_ = s.Put("a", []byte("d"), pinHash("1"), time.Minute)
	_ = s.Put("b", []byte("d"), pinHash("2"), time.Minute)
	if err := s.Put("c", []byte("d"), pinHash("3"), time.Minute); err != ErrTooManyBootstraps {
		t.Fatalf("over cap: %v want ErrTooManyBootstraps", err)
	}
	if err := s.Put("a", []byte("e"), pinHash("9"), time.Minute); err != nil {
		t.Fatalf("refresh existing: %v", err)
	}
	if !strings.Contains("x", "x") { t.Fatal("unreachable") }
}

func TestBootstrapStore_Live(t *testing.T) {
	s := NewBootstrapStore(64, 8)
	_ = s.Put("site:L", []byte("d"), pinHash("42"), time.Minute)
	if !s.Live("site:L", pinHash("42")) { t.Fatal("live entry with matching pin must report Live") }
	if s.Live("site:L", pinHash("99")) { t.Fatal("wrong pin must not be Live") }
	if s.Live("site:none", pinHash("42")) { t.Fatal("unknown site must not be Live") }
}
