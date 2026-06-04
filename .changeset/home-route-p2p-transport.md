---
"forty-two-watts": minor
---

Home route Phase 5: **direct browser↔Pi P2P transport**. The dashboard at
home.fortytwowatts.com now opens a direct, DTLS-end-to-end-encrypted WebRTC
DataChannel to the Pi and routes its live `/api/status` poll over it, bypassing
the relay on the data path. A `Direct / Relay` indicator in the header shows
which transport is live; if the DataChannel can't open (hard NAT, no STUN
reachability) it falls back to the relay fetch invisibly.

- **Signaling rides the existing authenticated owner tunnel** — `POST
  /api/p2p/offer` is owner-gated, so only an authenticated owner can open a
  channel. No relay changes.
- **Pi side**: `p2p.Manager` answers SDP offers and serves the channel with a
  `Bridge` over the local API mux; pure Go (`pion/webrtc/v4`, no CGo), with
  PeerConnection lifecycle reaping and a connection cap.
- The DataChannel carries the existing `tunnel.TunneledRequest/Response`
  frames, so the Pi's mux is unchanged. The data plane is ciphertext even over
  a future TURN relay — closing the "cloud sees plaintext" gap for P2P-routed
  traffic. STUN-only for now; TURN deferred.
