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
	"strings"
	"sync"

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

// ownerSessionCookieName is the owner session cookie the channel-scoped session
// captures from a login-finish response and replays on subsequent frames. Kept
// as a literal (not imported from api) so the p2p package stays free of the api
// dependency; it must match api.ownerAccessCookieName.
const ownerSessionCookieName = "ftw_owner"

type Bridge struct {
	handler http.Handler
	dc      *webrtc.DataChannel
	auth    http.Header // trusted offer-time auth context, stamped on each replay
	log     *slog.Logger
	sem     chan struct{} // caps concurrent in-flight replays

	// mu guards the per-channel session captured when the browser logs in OVER
	// this DataChannel. In the P2P-only home route the offer carries NO owner
	// cookie (the channel starts unauthenticated, marked remote); the browser
	// runs the WebAuthn ceremony over the channel, and login-finish returns a
	// Set-Cookie: ftw_owner=<token>. We capture that token here and stamp it as
	// Cookie: ftw_owner=<token> on every later frame, so the session lives ONLY
	// inside DTLS and is never visible to the relay or to JS. cookieSet guards
	// against overwriting a live session with a malformed later header.
	mu      sync.Mutex
	sessTok string // captured ftw_owner token, "" until login-over-channel succeeds
}

// NewReplayer builds a DataChannel-less Bridge that can replay frames against
// handler with the given trusted auth context. It exposes the SAME replay +
// channel-scoped-session machinery NewBridge uses, but with no pion DataChannel,
// so the fail-closed gate behaviour (marker stamping, login-over-channel session
// capture) is testable in the api package — which imports p2p and cannot reach
// the unexported replay path otherwise. Production code always uses NewBridge.
func NewReplayer(handler http.Handler, auth http.Header) *Bridge {
	return &Bridge{handler: handler, auth: auth, log: slog.Default(), sem: make(chan struct{}, maxInflight)}
}

// Replay runs one tunneled request through the Bridge's replay path (auth +
// marker stamping, channel-session capture/stamp) and returns the response. It
// is the exported entry point NewReplayer-built Bridges drive in tests; the
// production message pump calls the unexported replay directly.
func (b *Bridge) Replay(req tunnel.TunneledRequest) tunnel.TunneledResponse {
	return b.replay(req)
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
	// Send a TEXT frame (not binary): the browser's DataChannel onmessage does
	// JSON.parse(ev.data), which only works when ev.data is a string. dc.Send
	// would emit a BINARY frame, which the browser surfaces as a Blob, so
	// JSON.parse fails silently and the response is dropped. SendText mirrors the
	// browser, which sends its request frames as text (dc.send(JSON.stringify)).
	if err := b.dc.SendText(string(out)); err != nil {
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
	// marker OR a client-supplied owner-session cookie: both are server-trusted
	// signals. The marker is the relay's remote-vs-LAN trust signal; the
	// ftw_owner cookie is the channel session, which ONLY the Bridge may set
	// from a captured login-finish. A browser could otherwise forge either by
	// putting it in the frame's headers.
	for k, vs := range req.Header {
		ck := http.CanonicalHeaderKey(k)
		if ck == tunnelMarkerHeader {
			continue
		}
		if ck == "Cookie" {
			// Drop any client-supplied ftw_owner; keep other cookies (none are
			// trusted by the gate, but stripping just the owner cookie is the
			// minimal, least-surprising rule).
			if v := stripOwnerCookie(vs); v != "" {
				hr.Header.Set("Cookie", v)
			}
			continue
		}
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	// Then stamp the trusted auth context authoritatively (overwriting any
	// client value) so the replayed request carries the owner's real trust tier
	// — remote (marker stamped) for the signaling path, never a forged local
	// console.
	for k, vs := range b.auth {
		hr.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	// Stamp the channel-scoped session cookie (captured when the browser logged
	// in over THIS channel). It is appended to any surviving non-owner cookies.
	if tok := b.session(); tok != "" {
		appendCookie(hr.Header, ownerSessionCookieName+"="+tok)
	}
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, hr)
	res := rec.Result()
	defer res.Body.Close()
	// Capture an ftw_owner session minted by a login-finish over this channel, so
	// every subsequent frame is authorized by the gate's ownerSession check —
	// without the cookie ever leaving DTLS or being readable by JS.
	b.captureSession(res.Cookies())
	payload, _ := io.ReadAll(res.Body)
	return tunnel.TunneledResponse{
		Status: res.StatusCode,
		Header: res.Header,
		Body:   payload,
	}
}

// session returns the captured channel session token, or "" if the browser has
// not yet logged in over this channel.
func (b *Bridge) session() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessTok
}

// captureSession scans a replayed response's Set-Cookie headers for ftw_owner
// and records it as this channel's session. A clearing cookie (MaxAge<0 / empty
// value — emitted by logout) drops the captured session so a sign-out over the
// channel actually de-authorizes subsequent frames.
func (b *Bridge) captureSession(cookies []*http.Cookie) {
	for _, c := range cookies {
		if c.Name != ownerSessionCookieName {
			continue
		}
		b.mu.Lock()
		if c.Value == "" || c.MaxAge < 0 {
			b.sessTok = "" // logout-over-channel
		} else {
			b.sessTok = c.Value
		}
		b.mu.Unlock()
	}
}

// stripOwnerCookie returns the Cookie header value(s) with any ftw_owner pair
// removed, so a client can never smuggle a forged session through the frame
// headers. Other cookies are preserved verbatim.
func stripOwnerCookie(vs []string) string {
	var kept []string
	for _, v := range vs {
		for _, pair := range strings.Split(v, ";") {
			p := strings.TrimSpace(pair)
			if p == "" {
				continue
			}
			name := p
			if i := strings.IndexByte(p, '='); i >= 0 {
				name = strings.TrimSpace(p[:i])
			}
			if name == ownerSessionCookieName {
				continue
			}
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "; ")
}

// appendCookie adds a "name=value" pair to the request's Cookie header,
// preserving any existing pairs.
func appendCookie(h http.Header, pair string) {
	existing := h.Get("Cookie")
	if existing == "" {
		h.Set("Cookie", pair)
		return
	}
	h.Set("Cookie", existing+"; "+pair)
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
