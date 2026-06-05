package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/frahlg/forty-two-watts/go/internal/p2p"
	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// TestHandleP2POffer_RoundTrip drives the real POST /api/p2p/offer handler with
// a pion "browser": it posts an SDP offer over the gated handler (authorized by
// LAN bypass), applies the answer, and confirms the resulting DataChannel
// replays a request against the local API and returns 200. This is the CI-safe
// end-to-end for the Pi side — the only untested layer is the browser JS.
func TestHandleP2POffer_RoundTrip(t *testing.T) {
	local := http.NewServeMux()
	local.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"auto"}`)
	})
	mgr := p2p.NewManager(nil, nil) // nil stun => loopback host candidates
	mgr.SetLocalAPI(local)
	defer mgr.Close()

	deps := &Deps{
		TunnelMarker:         "test-marker",
		OwnerAccessLANBypass: true, // non-tunnelled httptest requests are authorized
		P2P:                  mgr,
	}
	srv := New(deps)
	ts := httptest.NewServer(srv.Handler()) // gated handler — the real path
	defer ts.Close()

	browser, err := p2p.NewPeer(nil)
	if err != nil {
		t.Fatalf("browser peer: %v", err)
	}
	defer browser.Close()

	respCh := make(chan p2p.ResponseFrame, 1)
	dc, err := browser.CreateDataChannel("ftw", nil)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	dc.OnOpen(func() {
		frame, _ := json.Marshal(tunnel.TunneledRequest{ReqID: "h1", Method: http.MethodGet, Path: "/api/status"})
		_ = dc.Send(frame)
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var rf p2p.ResponseFrame
		if json.Unmarshal(m.Data, &rf) == nil {
			respCh <- rf
		}
	})

	offer, err := browser.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(browser)
	if err := browser.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather

	reqBody, _ := json.Marshal(map[string]string{"type": "offer", "sdp": browser.LocalDescription().SDP})
	offerReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/p2p/offer", bytes.NewReader(reqBody))
	offerReq.Header.Set("Content-Type", "application/json")
	// Loopback is no longer auto-trusted by the gate, so authorize the offer
	// POST with a real owner session, exactly as an authenticated browser would.
	offerReq.AddCookie(ownerSessionCookie(t, srv))
	resp, err := http.DefaultClient.Do(offerReq)
	if err != nil {
		t.Fatalf("post offer: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("offer status %d: %s", resp.StatusCode, b)
	}
	var ans struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	resp.Body.Close()
	if ans.SDP == "" {
		t.Fatal("empty answer sdp")
	}
	if err := browser.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: ans.SDP,
	}); err != nil {
		t.Fatalf("set remote: %v", err)
	}

	select {
	case rf := <-respCh:
		if rf.Response.Status != http.StatusOK {
			t.Errorf("status = %d, want 200", rf.Response.Status)
		}
		if !strings.Contains(string(rf.Response.Body), `"mode"`) {
			t.Errorf("body = %q, want it to contain \"mode\"", rf.Response.Body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout: no p2p response after the real /api/p2p/offer handshake")
	}
}

// TestHandleP2POffer_Unavailable returns 503 when no manager is wired.
func TestHandleP2POffer_Unavailable(t *testing.T) {
	srv := New(&Deps{TunnelMarker: "m", OwnerAccessLANBypass: true}) // P2P nil
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body, _ := json.Marshal(map[string]string{"type": "offer", "sdp": "v=0"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/p2p/offer", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ownerSessionCookie(t, srv)) // loopback no longer auto-trusted
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestHandleP2POffer_Unauthorized confirms a remote (tunnelled) caller with no
// owner session is rejected at the gate — only authenticated owners may open a
// P2P channel.
func TestHandleP2POffer_Unauthorized(t *testing.T) {
	mgr := p2p.NewManager(nil, nil)
	mgr.SetLocalAPI(http.NewServeMux())
	deps := &Deps{TunnelMarker: "m", OwnerAccessLANBypass: false, P2P: mgr}
	srv := New(deps)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"type": "offer", "sdp": "v=0"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/p2p/offer", bytes.NewReader(body))
	req.Header.Set("X-FTW-Tunnel", "m") // remote/tunnelled, no ftw_owner session
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// A friend pair-flow request reaches the Pi from loopback, unmarked, with no
// owner session. It must NOT be able to open a P2P DataChannel — that channel
// would outlive the time-boxed pair grant. Only a real session or genuine LAN
// source may.
func TestHandleP2POffer_FriendLoopbackRejected(t *testing.T) {
	mgr := p2p.NewManager(nil, nil)
	mgr.SetLocalAPI(http.NewServeMux())
	deps := &Deps{TunnelMarker: "m", OwnerAccessLANBypass: true, P2P: mgr}
	srv := New(deps)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body, _ := json.Marshal(map[string]string{"type": "offer", "sdp": "v=0"})
	// httptest client connects from loopback; no X-FTW-Tunnel, no cookie.
	resp, err := http.Post(ts.URL+"/api/p2p/offer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("friend loopback P2P offer: got %d, want 401", resp.StatusCode)
	}
}

// ownerSessionCookie issues a real owner session on srv and returns its cookie.
// The P2P tests drive a real loopback server (httptest.NewServer), and a
// loopback source is no longer auto-trusted by the gate, so the offer POST must
// carry a valid session exactly as an authenticated browser would.
func ownerSessionCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := srv.issueOwnerSession(rec, []byte("p2p-test-cred")); err != nil {
		t.Fatalf("issue owner session: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no owner session cookie issued")
	}
	return cookies[0]
}
