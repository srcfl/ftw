package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// claimKeyFor mirrors the relay/browser claimKey = hex(sha256(bootstrap_id)).
// In tests we feed an arbitrary string in place of the raw bootstrap_id.
func claimKeyFor(bootstrapID string) string {
	h := sha256.Sum256([]byte(bootstrapID))
	return hex.EncodeToString(h[:])
}

func TestBootstrapStore_PutClaimBurn(t *testing.T) {
	s := NewBootstrapStore(64, 2048)
	desc := []byte(`{"site_id":"site:A","pi_pubkey":"x","label":"Home","sig":"y"}`)
	if err := s.Put("site:A", desc, claimKeyFor("bootstrap-A"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, site, ok := s.Claim(claimKeyFor("bootstrap-A"))
	if !ok || site != "site:A" || string(got) != string(desc) {
		t.Fatalf("Claim = %q,%q,%v want the descriptor for site:A", got, site, ok)
	}
	if _, _, ok := s.Claim(claimKeyFor("bootstrap-WRONG")); ok {
		t.Fatal("wrong claim key must miss")
	}
	s.Burn("site:A")
	if _, _, ok := s.Claim(claimKeyFor("bootstrap-A")); ok {
		t.Fatal("burned blob must be gone")
	}
}

func TestBootstrapStore_TTLAndGC(t *testing.T) {
	s := NewBootstrapStore(64, 2048)
	_ = s.Put("site:T", []byte("d"), claimKeyFor("1"), -time.Second)
	if _, _, ok := s.Claim(claimKeyFor("1")); ok {
		t.Fatal("expired blob must not claim")
	}
	if n := s.GC(); n != 1 {
		t.Fatalf("GC removed %d, want 1", n)
	}
}

func TestBootstrapStore_Caps(t *testing.T) {
	s := NewBootstrapStore(4, 2)
	if err := s.Put("site:A", []byte("toolong!"), claimKeyFor("1"), time.Minute); err != ErrBootstrapTooLarge {
		t.Fatalf("oversize: %v want ErrBootstrapTooLarge", err)
	}
	_ = s.Put("a", []byte("d"), claimKeyFor("1"), time.Minute)
	_ = s.Put("b", []byte("d"), claimKeyFor("2"), time.Minute)
	if err := s.Put("c", []byte("d"), claimKeyFor("3"), time.Minute); err != ErrTooManyBootstraps {
		t.Fatalf("over cap: %v want ErrTooManyBootstraps", err)
	}
	if err := s.Put("a", []byte("e"), claimKeyFor("9"), time.Minute); err != nil {
		t.Fatalf("refresh existing: %v", err)
	}
	if !strings.Contains("x", "x") {
		t.Fatal("unreachable")
	}
}

func TestBootstrapStore_Live(t *testing.T) {
	s := NewBootstrapStore(64, 8)
	_ = s.Put("site:L", []byte("d"), claimKeyFor("42"), time.Minute)
	if !s.Live("site:L", claimKeyFor("42")) {
		t.Fatal("live entry with matching claim key must report Live")
	}
	if s.Live("site:L", claimKeyFor("99")) {
		t.Fatal("wrong claim key must not be Live")
	}
	if s.Live("site:none", claimKeyFor("42")) {
		t.Fatal("unknown site must not be Live")
	}
}

func TestBootstrapStore_Consume(t *testing.T) {
	// Happy path: Consume returns the descriptor and atomically removes it,
	// so a following Claim/Consume for the same site misses.
	t.Run("consumes and removes", func(t *testing.T) {
		s := NewBootstrapStore(128, 8)
		desc := []byte(`{"site_id":"site:C","pi_pubkey":"x"}`)
		if err := s.Put("site:C", desc, claimKeyFor("boot-C"), time.Minute); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, ok := s.Consume("site:C", claimKeyFor("boot-C"))
		if !ok || string(got) != string(desc) {
			t.Fatalf("Consume = %q,%v want the descriptor for site:C", got, ok)
		}
		if _, ok := s.Consume("site:C", claimKeyFor("boot-C")); ok {
			t.Fatal("second Consume must miss after consumption")
		}
		if _, _, ok := s.Claim(claimKeyFor("boot-C")); ok {
			t.Fatal("Claim must miss after Consume removed the entry")
		}
	})

	// Non-matching claim key: Consume returns false and leaves the entry
	// intact so the correct claim key can still Consume it.
	t.Run("non-matching key leaves entry", func(t *testing.T) {
		s := NewBootstrapStore(128, 8)
		desc := []byte(`{"site_id":"site:C","pi_pubkey":"x"}`)
		if err := s.Put("site:C", desc, claimKeyFor("boot-C"), time.Minute); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, ok := s.Consume("site:C", claimKeyFor("boot-WRONG")); ok {
			t.Fatal("non-matching claim key must not consume")
		}
		got, ok := s.Consume("site:C", claimKeyFor("boot-C"))
		if !ok || string(got) != string(desc) {
			t.Fatalf("Consume after miss = %q,%v want the descriptor still claimable", got, ok)
		}
	})

	// Expired entry never consumes.
	t.Run("expired never consumes", func(t *testing.T) {
		s := NewBootstrapStore(128, 8)
		_ = s.Put("site:E", []byte("d"), claimKeyFor("boot-E"), -time.Second)
		if _, ok := s.Consume("site:E", claimKeyFor("boot-E")); ok {
			t.Fatal("expired entry must not consume")
		}
	})

	// Unknown site never consumes.
	t.Run("unknown site never consumes", func(t *testing.T) {
		s := NewBootstrapStore(128, 8)
		if _, ok := s.Consume("site:none", claimKeyFor("boot-E")); ok {
			t.Fatal("unknown site must not consume")
		}
	})
}

func TestBootstrapStore_Reserve(t *testing.T) {
	// Atomicity: N goroutines race to Reserve the same (site, claimKey); exactly
	// one wins. This is the invariant bootstrapEnrollForward relies on so a
	// concurrent second finish is refused BEFORE any Pi side effect.
	t.Run("atomic exactly-one wins", func(t *testing.T) {
		s := NewBootstrapStore(256, 8)
		const site = "site:R"
		ck := claimKeyFor("boot-reserve")
		desc := []byte(`{"site":"site:R"}`)
		if err := s.Put(site, desc, ck, time.Minute); err != nil {
			t.Fatalf("Put: %v", err)
		}
		const racers = 16
		var wg sync.WaitGroup
		var oks int32
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if d, ok := s.Reserve(site, ck); ok {
					if string(d) != string(desc) {
						t.Errorf("Reserve descriptor = %q, want %q", d, desc)
					}
					atomic.AddInt32(&oks, 1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if oks != 1 {
			t.Fatalf("concurrent Reserve succeeded %d times, want exactly 1", oks)
		}
	})

	// A reserved entry is still Claimable (a probing read does not depend on the
	// reserved flag) but a second Reserve refuses it.
	t.Run("reserved entry still claimable, second reserve refused", func(t *testing.T) {
		s := NewBootstrapStore(256, 8)
		const site = "site:R"
		ck := claimKeyFor("boot-reserve2")
		desc := []byte(`{"site":"site:R"}`)
		if err := s.Put(site, desc, ck, time.Minute); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, ok := s.Reserve(site, ck); !ok {
			t.Fatal("first Reserve must succeed")
		}
		if got, gotSite, ok := s.Claim(ck); !ok || gotSite != site || string(got) != string(desc) {
			t.Fatalf("Claim on a reserved entry = %q,%q,%v want it still claimable", got, gotSite, ok)
		}
		if _, ok := s.Reserve(site, ck); ok {
			t.Fatal("second Reserve on an already-reserved entry must refuse")
		}
	})

	// Release clears the flag so Reserve succeeds again (the Pi-rejected retry path).
	t.Run("release re-opens for reserve", func(t *testing.T) {
		s := NewBootstrapStore(256, 8)
		const site = "site:R"
		ck := claimKeyFor("boot-reserve3")
		if err := s.Put(site, []byte("d"), ck, time.Minute); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, ok := s.Reserve(site, ck); !ok {
			t.Fatal("first Reserve must succeed")
		}
		if _, ok := s.Reserve(site, ck); ok {
			t.Fatal("second Reserve before Release must refuse")
		}
		s.Release(site, ck)
		if _, ok := s.Reserve(site, ck); !ok {
			t.Fatal("Reserve after Release must succeed")
		}
	})

	// Release with a non-matching claim_key must not clear the flag (a stray
	// rollback from the wrong key can't re-open a window someone else reserved).
	t.Run("release ignores non-matching key", func(t *testing.T) {
		s := NewBootstrapStore(256, 8)
		const site = "site:R"
		ck := claimKeyFor("boot-reserve4")
		if err := s.Put(site, []byte("d"), ck, time.Minute); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, ok := s.Reserve(site, ck); !ok {
			t.Fatal("first Reserve must succeed")
		}
		s.Release(site, claimKeyFor("boot-WRONG"))
		if _, ok := s.Reserve(site, ck); ok {
			t.Fatal("Reserve must still refuse after a non-matching Release")
		}
	})

	// Reserve on an expired entry refuses.
	t.Run("expired never reserves", func(t *testing.T) {
		s := NewBootstrapStore(256, 8)
		const site = "site:RE"
		ck := claimKeyFor("boot-reserve-exp")
		if err := s.Put(site, []byte("d"), ck, -time.Second); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, ok := s.Reserve(site, ck); ok {
			t.Fatal("expired entry must not reserve")
		}
	})

	// Reserve on an unknown site refuses.
	t.Run("unknown site never reserves", func(t *testing.T) {
		s := NewBootstrapStore(256, 8)
		if _, ok := s.Reserve("site:none", claimKeyFor("boot-reserve-x")); ok {
			t.Fatal("unknown site must not reserve")
		}
	})

	// A constant-time non-matching key refuses AND leaves the entry un-reserved
	// (so the correct key can still Reserve it).
	t.Run("non-matching key refuses and leaves un-reserved", func(t *testing.T) {
		s := NewBootstrapStore(256, 8)
		const site = "site:R"
		ck := claimKeyFor("boot-reserve5")
		if err := s.Put(site, []byte("d"), ck, time.Minute); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, ok := s.Reserve(site, claimKeyFor("boot-WRONG")); ok {
			t.Fatal("non-matching claim key must not reserve")
		}
		if _, ok := s.Reserve(site, ck); !ok {
			t.Fatal("correct key must still Reserve after a non-matching attempt")
		}
	})
}
