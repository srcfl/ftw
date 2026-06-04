package p2p

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// manager_unauth_test.go — FIX-4b guards: the Manager admits not-yet-authed peers
// against a SEPARATE lower cap (maxUnauth < maxOpen) and reaps un-authenticated
// peers past a short grace, so an unauthenticated offer flood can't fill maxOpen
// and deny the owner. These tests drive the cap/auth/reap logic deterministically
// by injecting cheap host-only PeerConnections (no network handshake), which is
// the unit Codex flagged as the slot-exhaustion surface.

// inject adds a tracked session with the given age and authed flag, using a real
// (but unconnected) PeerConnection so reap()'s ConnectionState()/Close() work.
func (m *Manager) inject(t *testing.T, id string, age time.Duration, authed bool) {
	t.Helper()
	pc, err := NewPeer(nil)
	if err != nil {
		t.Fatalf("new peer: %v", err)
	}
	m.mu.Lock()
	m.sessions[id] = &pcSession{pc: pc, created: time.Now().Add(-age), authed: authed}
	m.mu.Unlock()
}

// TestManager_UnauthCapBlocksFlood proves a flood of un-authenticated peers stops
// at maxUnauth, leaving maxOpen-maxUnauth slots for the owner — and that marking
// one authed frees an unauth slot.
func TestManager_UnauthCapBlocksFlood(t *testing.T) {
	mgr := NewManager(nil, nil)
	mgr.SetLocalAPI(http.NewServeMux()) // so Answer doesn't reject on no-API
	defer mgr.Close()

	// Fill the unauth cap with fresh (within-grace) un-authed peers.
	for i := 0; i < mgr.maxUnauth; i++ {
		mgr.inject(t, fmtID("flood", i), 0, false)
	}
	mgr.mu.Lock()
	unauth := mgr.unauthCountLocked()
	mgr.mu.Unlock()
	if unauth != mgr.maxUnauth {
		t.Fatalf("unauth count = %d, want %d", unauth, mgr.maxUnauth)
	}

	// A new offer is refused at the unauth cap (NOT maxOpen — there are free slots
	// there). Answer reaps first; these peers are within grace, so none are freed.
	if _, err := mgr.Answer(context.Background(), "v=0\r\n", nil); err == nil {
		t.Fatal("expected refusal at the unauth cap with a flood of un-authed peers")
	}

	// Mark one peer authenticated — it no longer counts against the unauth cap.
	mgr.markAuthed(fmtID("flood", 0))
	mgr.mu.Lock()
	unauth = mgr.unauthCountLocked()
	mgr.mu.Unlock()
	if unauth != mgr.maxUnauth-1 {
		t.Fatalf("after markAuthed, unauth count = %d, want %d", unauth, mgr.maxUnauth-1)
	}
}

// TestManager_ReapsStaleUnauthPeers proves reap() closes un-authenticated peers
// past the grace window but keeps authed ones — so a flood that never logs in is
// cleaned up.
func TestManager_ReapsStaleUnauthPeers(t *testing.T) {
	mgr := NewManager(nil, nil)
	defer mgr.Close()

	mgr.inject(t, "stale-unauth", unauthReapAfter+time.Second, false) // past grace, unauth
	mgr.inject(t, "fresh-unauth", time.Second, false)                 // within grace, unauth
	mgr.inject(t, "stale-authed", unauthReapAfter+time.Second, true)  // past grace but authed

	mgr.reap()

	if mgr.ActiveCount() != 2 {
		t.Fatalf("ActiveCount after reap = %d, want 2", mgr.ActiveCount())
	}
	mgr.mu.Lock()
	_, staleUnauth := mgr.sessions["stale-unauth"]
	_, freshUnauth := mgr.sessions["fresh-unauth"]
	_, staleAuthed := mgr.sessions["stale-authed"]
	mgr.mu.Unlock()
	if staleUnauth {
		t.Error("stale un-authenticated peer survived reap (should be closed)")
	}
	if !freshUnauth {
		t.Error("fresh un-authenticated peer was reaped (within grace, should survive)")
	}
	if !staleAuthed {
		t.Error("authed peer reaped by the unauth grace (should be exempt)")
	}
}

// TestManager_ReapIfUnauthed proves the per-peer grace timer closes a peer iff it
// is still un-authenticated when it fires.
func TestManager_ReapIfUnauthed(t *testing.T) {
	mgr := NewManager(nil, nil)
	defer mgr.Close()

	mgr.inject(t, "never-logged-in", 0, false)
	mgr.inject(t, "logged-in", 0, true)

	mgr.reapIfUnauthed("never-logged-in")
	mgr.reapIfUnauthed("logged-in")

	mgr.mu.Lock()
	_, never := mgr.sessions["never-logged-in"]
	_, logged := mgr.sessions["logged-in"]
	mgr.mu.Unlock()
	if never {
		t.Error("un-authenticated peer survived its grace timer (should be reaped)")
	}
	if !logged {
		t.Error("authenticated peer reaped by its grace timer (should be kept)")
	}
}

func fmtID(prefix string, i int) string {
	return prefix + "-" + string(rune('0'+i))
}
