// Package p2p establishes a direct, end-to-end-encrypted WebRTC DataChannel
// between the browser dashboard and the Pi, carrying the same
// tunnel.TunneledRequest / tunnel.TunneledResponse frames the relay long-poll
// already uses. Routing one endpoint (live /api/status) over the DataChannel
// first proves direct + DTLS-E2E P2P without a full HTTP-over-DataChannel
// shim; the relay stays the default transport and the signaling/fallback path.
//
// This file is the CI-verifiable core: Bridge (DataChannel frame <-> local
// HTTP replay) plus a thin peer constructor. The relay signaling endpoints and
// the browser p2pClient are later slices that need a real browser + live
// network (ICE/STUN/DTLS) to verify end to end — see
// docs/superpowers/specs/2026-06-03-home-route-phase5-transport-design.md.
//
// pion/webrtc is pure Go (no CGo), honoring the project's no-CGo rule, and can
// play both peers in an in-process test (p2p_test.go) — that test is the CI
// guard for everything in this file.
package p2p

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/pion/webrtc/v4"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// DefaultSTUNServers is the public STUN set used to discover server-reflexive
// candidates for NAT traversal (Phase 5 P5-5). TURN fallback is deferred; even
// a TURN-relayed DataChannel carries DTLS ciphertext only.
var DefaultSTUNServers = []string{"stun:stun.l.google.com:19302"}

// ResponseFrame is one response written back over the DataChannel: the
// originating request's ReqID (so a client with several in-flight requests can
// correlate concurrent responses) plus the reused tunnel.TunneledResponse
// payload. It marshals to {"req_id":…,"response":{"status":…,"headers":…,
// "body_b64":…}} — the shape the browser p2pClient mirrors.
type ResponseFrame struct {
	ReqID    string                  `json:"req_id"`
	Response tunnel.TunneledResponse `json:"response"`
}

// Bridge serves tunneled HTTP over one open DataChannel. Each inbound message
// is a tunnel.TunneledRequest (JSON); the Bridge replays it against handler —
// the Pi's existing local mux — and writes back a ResponseFrame (JSON) on the
// same channel. The DataChannel is DTLS-encrypted end to end, so the data
// plane is ciphertext even over a TURN relay, which closes the "cloud sees
// plaintext" gap for everything routed over P2P.
// maxInflight bounds concurrent handler execution on one DataChannel so a busy
// or hostile peer can't fan out unbounded goroutines on the Pi.
const maxInflight = 16

// tunnelMarkerHeader is the canonicalised X-FTW-Tunnel header. A client must
// never be able to supply it — only the trusted offer-time auth context may.
const tunnelMarkerHeader = "X-Ftw-Tunnel"

type Bridge struct {
	handler http.Handler
	dc      *webrtc.DataChannel
	auth    http.Header   // trusted offer-time auth context, stamped on each replay
	log     *slog.Logger
	sem     chan struct{} // caps concurrent in-flight replays
}

// NewBridge attaches a Bridge to an open DataChannel, the local handler (the
// GATED api.Server handler), and the trusted offer-time auth context to stamp
// on every replayed request. A nil log defaults to slog.Default(). The caller
// owns the DataChannel and PeerConnection lifecycle.
func NewBridge(dc *webrtc.DataChannel, handler http.Handler, auth http.Header, log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	b := &Bridge{handler: handler, dc: dc, auth: auth, log: log, sem: make(chan struct{}, maxInflight)}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		// Replay off the pion read loop so a slow handler can't
		// head-of-line-block, but bound concurrency: acquiring the semaphore
		// here applies natural backpressure once maxInflight replays run. Each
		// ResponseFrame carries the ReqID, so out-of-order completion is fine.
		b.sem <- struct{}{}
		go func(data []byte) {
			defer func() { <-b.sem }()
			b.serve(data)
		}(msg.Data)
	})
	return b
}

// serve replays one request frame and writes its response frame back. A
// malformed frame is logged and dropped; a panic in the handler is recovered
// so one bad request can never tear down the channel pump.
func (b *Bridge) serve(frame []byte) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("p2p: panic serving request frame", "recover", r)
		}
	}()
	var req tunnel.TunneledRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		b.log.Warn("p2p: malformed request frame", "err", err)
		return
	}
	out, err := json.Marshal(ResponseFrame{ReqID: req.ReqID, Response: b.replay(req)})
	if err != nil {
		b.log.Error("p2p: marshal response", "err", err, "req_id", req.ReqID)
		return
	}
	if err := b.dc.Send(out); err != nil {
		b.log.Error("p2p: send response", "err", err, "req_id", req.ReqID)
	}
}

// replay runs one tunneled request against the local handler entirely in
// memory and captures the response.
func (b *Bridge) replay(req tunnel.TunneledRequest) tunnel.TunneledResponse {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	path := req.Path
	if path == "" {
		path = "/"
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hr := httptest.NewRequest(method, path, body)
	// Client-supplied headers first — but never honour a client-supplied tunnel
	// marker: that header is the relay's remote-vs-LAN trust signal, and only
	// the trusted offer-time auth context (b.auth) may set it.
	for k, vs := range req.Header {
		if http.CanonicalHeaderKey(k) == tunnelMarkerHeader {
			continue
		}
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	// Then stamp the trusted auth context authoritatively (overwriting any
	// client value) so the replayed request carries the owner's real trust tier
	// — the same the relay path grants, never a forged local console.
	for k, vs := range b.auth {
		hr.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, hr)
	res := rec.Result()
	defer res.Body.Close()
	payload, _ := io.ReadAll(res.Body)
	return tunnel.TunneledResponse{
		Status: res.StatusCode,
		Header: res.Header,
		Body:   payload,
	}
}

// NewPeer creates an RTCPeerConnection configured with the given STUN/TURN
// server URLs. Pass nil for host-candidate-only (e.g. an in-process loopback
// test); pass DefaultSTUNServers for real NAT traversal.
func NewPeer(iceServers []string) (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{}
	if len(iceServers) > 0 {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: iceServers}}
	}
	return webrtc.NewPeerConnection(cfg)
}
