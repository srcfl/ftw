package p2p

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/srcfl/ftw/go/internal/tunnel"
)

// TestManager_Answer_RoundTrip drives the production Manager.Answer with a pion
// "browser" peer: the browser creates the offer + the "ftw" DataChannel, the
// Manager answers and serves the channel with a Bridge over a stub local API,
// and the browser gets the handler's 200 back over the channel. No HTTP, no
// browser, no network — host candidates over loopback. This is the CI guard for
// the Pi-side P2P orchestration.
func TestManager_Answer_RoundTrip(t *testing.T) {
	local := http.NewServeMux()
	local.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"auto","ok":true}`)
	})

	mgr := NewManager(nil, nil) // nil stun => host candidates only (loopback)
	mgr.SetLocalAPI(local)
	defer mgr.Close()

	browser, err := NewPeer(nil)
	if err != nil {
		t.Fatalf("browser peer: %v", err)
	}
	defer browser.Close()

	respCh := make(chan ResponseFrame, 1)
	dc, err := browser.CreateDataChannel("ftw", nil)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	dc.OnOpen(func() {
		frame, _ := json.Marshal(tunnel.TunneledRequest{ReqID: "s1", Method: http.MethodGet, Path: "/api/status"})
		_ = dc.Send(frame)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var rf ResponseFrame
		if json.Unmarshal(msg.Data, &rf) == nil {
			respCh <- rf
		}
	})

	// Browser creates the offer, gathers ICE, hands the SDP to Manager.Answer,
	// applies the returned answer — exactly what the JS client + the
	// /api/p2p/offer handler do over the wire.
	offer, err := browser.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(browser)
	if err := browser.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather

	answerSDP, err := mgr.Answer(context.Background(), browser.LocalDescription().SDP, nil)
	if err != nil {
		t.Fatalf("manager answer: %v", err)
	}
	if err := browser.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answerSDP,
	}); err != nil {
		t.Fatalf("set remote answer: %v", err)
	}

	select {
	case rf := <-respCh:
		if rf.ReqID != "s1" {
			t.Errorf("ReqID = %q, want s1", rf.ReqID)
		}
		if rf.Response.Status != http.StatusOK {
			t.Errorf("status = %d, want 200", rf.Response.Status)
		}
		if got := string(rf.Response.Body); !strings.Contains(got, `"mode"`) {
			t.Errorf("body = %q, want it to contain \"mode\"", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for the p2p response over the Manager-answered channel")
	}

	if n := mgr.ActiveCount(); n != 1 {
		t.Errorf("ActiveCount = %d, want 1 after a successful answer", n)
	}
}

// TestManager_Answer_NoLocalAPI rejects offers before the local API is wired —
// the Bridge would otherwise serve a nil handler.
func TestManager_Answer_NoLocalAPI(t *testing.T) {
	mgr := NewManager(nil, nil)
	if _, err := mgr.Answer(context.Background(), "v=0\r\n", nil); err == nil {
		t.Fatal("expected an error when the local API is not wired")
	}
}
