package p2p

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// TestBridge_PionToPion proves the Phase 5 core — a DataChannel carrying the
// tunnel protocol to the local HTTP stack — with two in-process pion peers and
// no browser or external network. Peer A plays the browser; peer B plays the
// Pi. This is the CI guard described in the Phase 5 design (§"verifiable-in-Go
// core"): if it passes, the heart of the P2P data plane works.
func TestBridge_PionToPion(t *testing.T) {
	// Pi-role local mux: 200 {"ok":true} at /api/test, 404 elsewhere — stands
	// in for the real api.Server handler.
	handler := http.NewServeMux()
	handler.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	})

	pcA, err := NewPeer(nil) // browser; nil => host candidates only (loopback)
	if err != nil {
		t.Fatalf("create peer A: %v", err)
	}
	defer pcA.Close()
	pcB, err := NewPeer(nil) // Pi
	if err != nil {
		t.Fatalf("create peer B: %v", err)
	}
	defer pcB.Close()

	// Pi side: when the "ftw" channel arrives, serve it with a Bridge.
	pcB.OnDataChannel(func(dc *webrtc.DataChannel) {
		NewBridge(dc, handler, nil, nil)
	})

	// Browser side: open the channel; on open, send one TunneledRequest.
	respCh := make(chan ResponseFrame, 1)
	errCh := make(chan error, 2)
	dc, err := pcA.CreateDataChannel("ftw", nil)
	if err != nil {
		t.Fatalf("create data channel: %v", err)
	}
	dc.OnOpen(func() {
		req := tunnel.TunneledRequest{ReqID: "r1", Method: http.MethodGet, Path: "/api/test"}
		frame, err := json.Marshal(req)
		if err != nil {
			errCh <- err
			return
		}
		if err := dc.Send(frame); err != nil {
			errCh <- err
		}
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var resp ResponseFrame
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	})

	// Stand in for the relay's signaling path: a full offer/answer exchange.
	if err := connectPeers(pcA, pcB); err != nil {
		t.Fatalf("connect peers: %v", err)
	}

	select {
	case resp := <-respCh:
		if resp.ReqID != "r1" {
			t.Errorf("ReqID = %q, want %q (response correlation broken)", resp.ReqID, "r1")
		}
		if resp.Response.Status != http.StatusOK {
			t.Errorf("Status = %d, want 200", resp.Response.Status)
		}
		if got := string(resp.Response.Body); !strings.Contains(got, `"ok":true`) {
			t.Errorf("Body = %q, want it to contain %q", got, `"ok":true`)
		}
	case err := <-errCh:
		t.Fatalf("channel error: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for the tunneled response over the DataChannel")
	}
}

// TestBridge_NotFoundPropagates confirms the Bridge faithfully relays a
// non-200 status from the local handler (the data plane is transparent, not a
// 200-only happy path).
func TestBridge_NotFoundPropagates(t *testing.T) {
	pcA, err := NewPeer(nil)
	if err != nil {
		t.Fatalf("create peer A: %v", err)
	}
	defer pcA.Close()
	pcB, err := NewPeer(nil)
	if err != nil {
		t.Fatalf("create peer B: %v", err)
	}
	defer pcB.Close()

	pcB.OnDataChannel(func(dc *webrtc.DataChannel) {
		NewBridge(dc, http.NewServeMux(), nil, nil) // empty mux => 404 everywhere
	})

	respCh := make(chan ResponseFrame, 1)
	dc, err := pcA.CreateDataChannel("ftw", nil)
	if err != nil {
		t.Fatalf("create data channel: %v", err)
	}
	dc.OnOpen(func() {
		frame, _ := json.Marshal(tunnel.TunneledRequest{ReqID: "r2", Method: http.MethodGet, Path: "/api/missing"})
		_ = dc.Send(frame)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var resp ResponseFrame
		if err := json.Unmarshal(msg.Data, &resp); err == nil {
			respCh <- resp
		}
	})

	if err := connectPeers(pcA, pcB); err != nil {
		t.Fatalf("connect peers: %v", err)
	}

	select {
	case resp := <-respCh:
		if resp.ReqID != "r2" {
			t.Errorf("ReqID = %q, want %q", resp.ReqID, "r2")
		}
		if resp.Response.Status != http.StatusNotFound {
			t.Errorf("Status = %d, want 404", resp.Response.Status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for the 404 response over the DataChannel")
	}
}

// connectPeers performs a full (non-trickle) offer/answer exchange between two
// in-process peers, standing in for the relay's signaling path. It waits for
// ICE gathering to complete so the exchanged SDP carries host candidates — no
// external STUN is needed for a loopback connection.
func connectPeers(a, b *webrtc.PeerConnection) error {
	offer, err := a.CreateOffer(nil)
	if err != nil {
		return err
	}
	gatherA := webrtc.GatheringCompletePromise(a)
	if err := a.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gatherA
	if err := b.SetRemoteDescription(*a.LocalDescription()); err != nil {
		return err
	}
	answer, err := b.CreateAnswer(nil)
	if err != nil {
		return err
	}
	gatherB := webrtc.GatheringCompletePromise(b)
	if err := b.SetLocalDescription(answer); err != nil {
		return err
	}
	<-gatherB
	return a.SetRemoteDescription(*b.LocalDescription())
}

// TestBridge_replay_AuthContext verifies the security boundary: the Bridge
// stamps the trusted offer-time auth context (X-FTW-Tunnel + Cookie) on every
// replayed request, overriding any client-forged values, while passing benign
// client headers through. This is what stops a DataChannel request from forging
// the remote-vs-LAN trust signal or swapping in a different owner session.
func TestBridge_replay_AuthContext(t *testing.T) {
	var seen http.Header
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	})
	auth := http.Header{}
	auth.Set("X-FTW-Tunnel", "real-marker")
	auth.Set("Cookie", "ftw_owner=sess1")
	b := &Bridge{handler: h, auth: auth}

	req := tunnel.TunneledRequest{Method: http.MethodGet, Path: "/api/x"}
	req.Header = http.Header{}
	req.Header.Set("X-FTW-Tunnel", "forged")       // client forgery attempt
	req.Header.Set("Cookie", "ftw_owner=attacker") // client session-swap attempt
	req.Header.Set("Accept", "application/json")   // benign, should pass through

	b.replay(req)

	if got := seen.Get("X-Ftw-Tunnel"); got != "real-marker" {
		t.Errorf("X-Ftw-Tunnel = %q, want \"real-marker\" (client forgery must be overridden)", got)
	}
	if got := seen.Get("Cookie"); got != "ftw_owner=sess1" {
		t.Errorf("Cookie = %q, want the offer-time cookie, not the client's", got)
	}
	if got := seen.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want the benign client header preserved", got)
	}
}
