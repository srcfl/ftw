---
"forty-two-watts": patch
---

Dashboard: fix the 5–10 s first-load stall and collapse duplicate request
storms — three related changes to how the live dashboard talks to the backend.

**P2P no longer stalls the first paint.** The Phase 5 P2P transport
(`window.p2pFetch`) awaited the WebRTC handshake on the first request, so the
first `/api/status` poll — which gates the whole live render — blocked on the
8 s `CONNECT_TIMEOUT_MS` before falling back to plain `fetch`. Two fixes:
(1) `p2pFetch` is now non-blocking — it uses the DataChannel only once it's open
and otherwise serves the request over the relay immediately while connecting in
the background, so no request ever waits on the handshake (on the relay path
either); (2) P2P is skipped entirely on a direct-LAN connection — detected by
host (`isDirectLAN`: localhost, private/CGNAT IP, single-label or `*.local`
name), not by the pathname. The bare-host relay (e.g. `home.fortytwowatts.com`)
is a public FQDN reached through the relay, so it is correctly treated as a
remote context and keeps P2P — the earlier `apiBase() === ""` gate wrongly
disabled it there. On a direct-LAN visit the transport indicator stays hidden
instead of showing an un-toggleable "Relay" badge.

**Live 24 h history is deduped.** `/api/history?range=24h&points=288` was
fetched on boot, the 1-min poll, and every (undebounced) window resize, so a
first-load layout resize storm fanned out into many identical requests. A small
in-flight-coalescing + 15 s-TTL cache (`fetchHistory`, mirroring
`ftw-history-card`'s `dailyFetchCache`) now shares one payload across those
triggers; the periodic poll forces a fresh sample.

**Notification-history badge is deduped.** `<ftw-notif-history>` now shares one
in-flight request and a short-TTL cache for `/api/notifications/history` across
the badge poll and modal open, collapsing transient bursts to a single request.
The modal's manual Refresh button forces a fresh fetch, and non-OK responses are
never cached.
