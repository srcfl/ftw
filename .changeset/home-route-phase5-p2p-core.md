---
"forty-two-watts": patch
---

Home route Phase 5 groundwork: add the CI-verifiable P2P transport core
(`go/internal/p2p`). A `Bridge` reads `tunnel.TunneledRequest` JSON frames off
an open WebRTC `DataChannel`, replays each against the local HTTP handler, and
writes back a `ResponseFrame` — the same tunnel protocol the relay long-poll
uses, so the Pi's mux is unchanged. Proven by an in-process pion↔pion loopback
test (DTLS DataChannel, no browser/network). Pure-Go (`pion/webrtc/v4`, no
CGo). Not yet wired to any user-facing surface — the relay signaling endpoints
and browser `p2pClient` are later slices that need a browser harness.
