# Home Route — Phase 5: P2P Transport — Design

- **Date:** 2026-06-03
- **Status:** Design approved. **BUILT** on `home-route-phase5`: the verifiable-in-Go core (`go/internal/p2p` `Bridge`/`NewPeer`, pion↔pion loopback tests) **and** the full browser path — `p2p.Manager` (offer→answer + lifecycle), the owner-gated `POST /api/p2p/offer` signaling (rides the authenticated owner tunnel; no relay changes), and the browser `web/p2p.js` client (`p2pFetch` over the DataChannel with relay fallback + a `Direct/Relay` indicator). CI-safe Go tests drive a pion "browser" through the real handler (`manager_test.go`, `api_p2p_test.go`); the browser JS is verified on the live deploy. **Deferred:** STUN→TURN fallback for hard NATs, WebTransport/QUIC, widening past `/api/status`.
- **Builds on:** Phases 1–3 (`home-route-phase1`), master spec §10.

## One-liner

Keep the dashboard on the proven relay tunnel; bring up a **direct WebRTC DataChannel (DTLS-E2E)** browser↔Pi and route **one endpoint over it first** (live `/api/status`), proving direct + end-to-end-encrypted P2P, then widen. The relay is signaling + fallback only; the DataChannel carries the existing `tunnel.TunneledRequest/Response` frames, so the Pi's reverse-proxy is unchanged.

## Approved decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| P5-1 | Browser transport rollout | **Incremental opt-in fast path** — dashboard stays on the relay tunnel; route ONE endpoint over the DataChannel first, then widen | Smallest blast radius; ships the P2P plumbing without a full HTTP-over-DataChannel shim; "robust over feature-rich" |
| P5-2 | Wire format over the DataChannel | **Reuse `tunnel.TunneledRequest/Response`** (JSON frames) | Pi reverse-proxy unchanged; one serialization for both transports; the DataChannel just swaps in for the long-poll |
| P5-3 | Peer library (Pi side) | **`pion/webrtc` (pure Go, no CGo)** | The de-facto Go WebRTC; honors the no-CGo rule; can be both peers in an in-process test |
| P5-4 | Signaling | **Over the relay, post-auth** (SDP offer/answer + ICE candidates), ephemeral | The relay is already the authenticated path; signaling is transient, not state |
| P5-5 | NAT traversal | **Public STUN** for v1; **TURN (coturn) as fallback**, deferred | STUN is free + covers most NATs; TURN is a real ops cost — add when hard-NAT cases appear (still ciphertext-only) |
| P5-6 | QUIC | **WebTransport/QUIC auto-upgrade** where the Pi is directly reachable — **deferred to a later slice** | Real QUIC needs reachability; not the v1 critical path |

## Architecture

```
Browser (dashboard)                Relay (signaling + fallback)            Pi
  fetch("/api/status")  ──relay tunnel (existing, default)───────────────▶ :8080
       │
       │  opt-in: p2pClient.fetch("/api/status")
       ▼
  RTCPeerConnection ──SDP/ICE via relay signaling (post-auth)──▶  pion peer
       │                                                              │
       └════ DataChannel "ftw" (DTLS 1.3, E2E) ════════════════════▶ bridge
              TunneledRequest(JSON) ───────────────────────────────▶  replay → :8080
              TunneledResponse(JSON) ◀───────────────────────────────  ◀ response
```

The DataChannel is **DTLS-encrypted end-to-end** — even a TURN-relayed fallback carries ciphertext only, which is what makes the relay untrusted on the data path too (closes the "cloud sees plaintext" gap for everything routed over P2P).

## The verifiable-in-Go core (build this first, TDD)

**New package `go/internal/p2p`** (isolates the pion dependency to one package):

- `Bridge` — given an open `*webrtc.DataChannel` and an `http.Handler` (the local mux), reads `tunnel.TunneledRequest` JSON frames, replays each against the handler via `httptest.NewRecorder` (or a real `127.0.0.1:8080` round-trip), and writes back `tunnel.TunneledResponse`. No signaling, no browser — pure frame↔HTTP.
- `dialPeer` / `answerPeer` helpers for setting up a `*webrtc.PeerConnection` with a DataChannel and the STUN config.

**The test that proves it (`p2p_test.go`, pion↔pion, no browser):**
1. Create two `webrtc.PeerConnection`s in-process; wire their ICE candidates to each other directly (callbacks); exchange offer/answer.
2. Peer A (browser role) opens DataChannel `ftw`; on open, sends `TunneledRequest{Method:"GET", Path:"/api/test"}`.
3. Peer B (Pi role) runs `Bridge` over its DataChannel with a test handler that returns `200 {"ok":true}`.
4. Assert peer A receives a `TunneledResponse{Status:200, Body:…"ok":true…}`.

This verifies the heart of Phase 5 (a DataChannel carrying the tunnel protocol to the local HTTP stack) with `go test` alone — no browser, no network.

## The browser-dependent remainder (needs a real harness — design now, build with a browser)

- **Relay signaling endpoints** (post-auth): `POST /signal/offer`, long-poll `GET /signal/answer`, ICE trickle. Stateless/ephemeral. *(HTTP handlers are unit-testable; end-to-end needs a browser.)*
- **Browser `p2pClient`** — a tiny JS module: open `RTCPeerConnection` (STUN config), exchange SDP/ICE via the relay, open the `ftw` DataChannel, expose `p2pFetch(path)` that frames a `TunneledRequest`, sends it, awaits the `TunneledResponse`. The dashboard opts in for **`/api/status` only** at first.
- **Fallback** — if the DataChannel doesn't open within N seconds (hard NAT, no TURN), `p2pFetch` falls back to the normal relay `fetch`. Invisible to the user.
- **STUN/TURN provisioning** — public STUN URL in config; coturn on the relay VM when TURN is needed.

## What's reused vs new

**Reused:** `tunnel.TunneledRequest/Response` (P5-2); the Pi's local mux + reverse-proxy semantics; the authenticated relay path (for signaling); the auth-gate (the DataChannel-delivered requests still hit the gated mux — they carry the owner cookie/marker semantics).

**New:** the `pion/webrtc` dependency; `go/internal/p2p` (Bridge + peer helpers); relay signaling endpoints; the browser `p2pClient`; STUN/TURN config.

## Risks

- **TURN is a real ops cost** (one research agent flagged this). Mitigated: STUN-only v1 covers most NATs; TURN deferred; even TURN carries ciphertext.
- **Auth on the P2P path** — a DataChannel-delivered request must still pass the Phase-1 auth-gate. Decide: does the DataChannel ride the authenticated session (the browser proved its passkey to open the channel post-auth) so its replayed requests are treated as authenticated, or does each replayed request re-present the `ftw_owner` cookie? Resolve in the build plan; default to "the post-auth DataChannel is an authenticated session, marked like the tunnel."
- **Browser coverage** — Conditional UI ≠ WebRTC coverage; `p2pFetch` must always degrade to the relay `fetch` cleanly.
- **Cannot be CI-verified end-to-end** — the pion↔pion core test is the CI guard; the browser path needs a manual/Playwright harness, documented as such (no silent "it works" claims).

## Sequencing for the build (when ready)

1. `go/internal/p2p` Bridge + pion↔pion test (verifiable, CI-safe). ← start here
2. Relay signaling endpoints (+ handler tests).
3. Browser `p2pClient` + opt-in `/api/status` over P2P + relay fallback.
4. STUN config; manual browser verification; document the harness.
5. (later) Widen beyond `/api/status`; TURN; WebTransport/QUIC upgrade.
