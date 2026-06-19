package p2p

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// manager_unauth_flood_test.go — FIX-C (Pi side): the fast unauth reap must keep a
// connect-but-never-login (or never-connect) flood from denying a fresh owner. A
// peer that never reaches a live DTLS connection is freed at unauthConnectGrace;
// a connected-but-never-authed peer at unauthReapAfter — both well before the
// owner gives up.

// TestReapIfUnconnected_FreesStalledPeer proves the per-peer connect-grace timer
// closes an un-authed peer that never connected, but keeps an authed one.
func TestReapIfUnconnected_FreesStalledPeer(t *testing.T) {
	mgr := NewManager(nil, nil)
	defer mgr.Close()

	mgr.inject(t, "stalled", unauthConnectGrace+time.Second, false) // never connected, past grace
	mgr.inject(t, "authed-fast", 0, true)                           // authed, exempt

	mgr.reapIfUnconnected("stalled")
	mgr.reapIfUnconnected("authed-fast")

	mgr.mu.Lock()
	_, stalled := mgr.sessions["stalled"]
	_, authed := mgr.sessions["authed-fast"]
	mgr.mu.Unlock()
	if stalled {
		t.Error("FIX-C: a never-connected un-authed peer survived the connect grace")
	}
	if !authed {
		t.Error("an authed peer was closed by the connect-grace reaper")
	}
}

// TestReap_FastConnectGrace proves reap() closes never-connected un-authed peers
// past the SHORT connect grace (not waiting the full login window), so a cheap
// flood frees fast.
func TestReap_FastConnectGrace(t *testing.T) {
	mgr := NewManager(nil, nil)
	defer mgr.Close()

	// Past connect grace but well under the (longer) login window: still reaped,
	// because it never connected.
	mgr.inject(t, "stalled", unauthConnectGrace+time.Second, false)
	// Within connect grace: kept (might still be completing the handshake).
	mgr.inject(t, "warming", time.Second, false)

	mgr.reap()

	mgr.mu.Lock()
	_, stalled := mgr.sessions["stalled"]
	_, warming := mgr.sessions["warming"]
	mgr.mu.Unlock()
	if stalled {
		t.Error("FIX-C: never-connected peer past the connect grace survived reap")
	}
	if !warming {
		t.Error("a peer still within the connect grace was reaped (should warm up)")
	}
}

// TestUnauthFlood_DoesNotDenyFreshOwner is the FIX-C denial guard: a flood of
// stalled (never-connected) un-authed peers fills the unauth cap, but once they
// pass the connect grace, reap (driven by the next Answer) frees them so a fresh
// owner offer is admitted rather than 429'd out.
func TestUnauthFlood_DoesNotDenyFreshOwner(t *testing.T) {
	mgr := NewManager(nil, nil)
	mgr.SetLocalAPI(http.NewServeMux()) // so Answer doesn't reject on no-API
	defer mgr.Close()

	// Fill the unauth cap with stalled peers that drained slots but never connected
	// AND are already past the connect grace (the attacker's offers landed a while
	// ago).
	for i := 0; i < mgr.maxUnauth; i++ {
		mgr.inject(t, fmtID("flood", i), unauthConnectGrace+time.Second, false)
	}
	mgr.mu.Lock()
	if mgr.unauthCountLocked() != mgr.maxUnauth {
		mgr.mu.Unlock()
		t.Fatalf("setup: unauth count = %d, want %d", mgr.unauthCountLocked(), mgr.maxUnauth)
	}
	mgr.mu.Unlock()

	// The owner's fresh offer arrives. Answer reaps first; the stalled flood is past
	// the connect grace, so it is freed and the owner is admitted (the answer is
	// produced and tracked) rather than refused at the unauth cap.
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout+2*time.Second)
	defer cancel()
	_, err := mgr.Answer(ctx, ownerOfferSDP(t), nil)
	if err != nil {
		t.Fatalf("FIX-C: fresh owner offer was denied despite a stalled unauth flood: %v", err)
	}

	// The stalled flood is gone; only the owner's peer remains tracked.
	mgr.mu.Lock()
	flood := 0
	for id := range mgr.sessions {
		if len(id) >= 5 && id[:5] == "flood" {
			flood++
		}
	}
	mgr.mu.Unlock()
	if flood != 0 {
		t.Errorf("stalled flood peers survived the owner's reap: %d remain", flood)
	}
}

// ownerOfferSDP builds a minimal but valid offer SDP the host-only PeerConnection
// can answer (no real network — host candidates only).
func ownerOfferSDP(t *testing.T) string {
	t.Helper()
	pc, err := NewPeer(nil)
	if err != nil {
		t.Fatalf("new offer peer: %v", err)
	}
	defer pc.Close()
	_, err = pc.CreateDataChannel("ftw", nil)
	if err != nil {
		t.Fatalf("create dc: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	select {
	case <-gather:
	case <-time.After(5 * time.Second):
		t.Fatal("offer ICE gather timeout")
	}
	return pc.LocalDescription().SDP
}
