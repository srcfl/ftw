# Subetha — design history

**Status:** **implemented** as `go/internal/subetha` + `go/cmd/ftw-subetha`.
This doc captures the original proposal and trade-off analysis that led to
adopting this transport over the fowl/magic-wormhole approach. Kept for
historical context. Live implementation details live in
`go/internal/subetha/` and `docs/subetha-deploy.md`.

## Context

`ftw-pair` currently uses the Python `fowl` tool (built on magic-wormhole's
Dilation protocol) as the cross-host TCP transport. The owner's Pi runs
`fowld` as a subprocess of the pair sidecar; the friend's Mac runs another
`fowld` for the receiver end. Both connect to the public
`relay.magic-wormhole.io` rendezvous server and establish an
end-to-end-encrypted tunnel via PAKE-derived key from a short shareable code.

This works. It's also the canonical reference implementation of
magic-wormhole. But it carries two costs:

- **Python runtime dep** on both peers (`uv tool install 'fowl==25.4.0'`).
- **Upstream protocol churn.** Pinning prevents drift (see PR #321), but
  upgrading to support newer `fowl` versions requires refactoring our
  permission-control commands. We've already seen this once — the
  `danger-disable-permission-check` command was removed between 25.4.0 and
  25.10.0.

If either of those becomes more than a minor friction, this doc lays out an
alternative.

## Proposal: Sourceful-operated relay + simple PAKE-free token

Replace fowl entirely with a small Go relay + client that:

- Both peers connect outbound (TCP/443) to a Sourceful-operated relay
  (`pair-relay.sourceful.energy` or similar)
- A shared random token identifies the session — both peers send the token
  during connection setup; the relay matches them and pipes bytes between
  them
- Traffic is end-to-end encrypted with an AEAD (chacha20-poly1305) keyed
  from the token via HKDF — the relay sees only ciphertext
- Tokens are **6 random English words** (e.g.
  `garage-coffee-river-bicycle-window-cat`) — ~80 bits of entropy, no need
  for PAKE because the token IS the key material

## Architecture sketch

```
                   pair-relay.sourceful.energy
                              |
                  +-----------+-----------+
                  |                       |
              outbound                 outbound
                  |                       |
            OWNER's Pi              FRIEND's Mac
       (ftw-pair sidecar)        (ftw-connect)
```

### Relay server (~300 LOC Go)

- TCP listener on `:443` (TLS via cert-manager or LetsEncrypt)
- For each incoming connection: read a 32-byte token prefix, lookup pending
  matches
- When two connections share a token: pipe `io.Copy` in both directions,
  remove from match table
- Idle timeout (5 min for unmatched, 4 h for matched), connection count
  limits per source IP

### Client (~200 LOC Go, embedded in ftw-pair + ftw-connect)

- Generate token = 6 random words from a deterministic dictionary
  (PGP wordlist or BIP39 subset)
- HKDF-SHA256(token) → AEAD key + nonce-prefix
- Dial relay, send token prefix, then start AEAD-framed bidirectional copy
- On connection drop: simple reconnect (we accept brief gaps; this is not
  Dilation's durable-stream property)

No NAT-traversal, no direct P2P — always relay. Acceptable because:

- The use case is a short-lived 4h pair session, not a long-running connection
- Relay bandwidth cost for our scale (1–10 active sessions/day) is
  negligible (~€5/month VPS)
- Removes ~10x complexity vs Dilation

## Trade-offs

| Property | fowl/magic-wormhole (now) | Own relay (proposal) |
|---|---|---|
| Pure Go binaries | No (Python dep) | Yes |
| Third-party dependency | `relay.magic-wormhole.io` | None (Sourceful relay) |
| Code we maintain | Thin Go wrapper around fowld | Full relay + client + crypto |
| Code review surface | ~600 LOC Go wrapper | ~500 LOC Go new code |
| Operational burden | None | One small VPS to monitor |
| Token UX | `3-retraction-sawdust` (short) | `garage-coffee-river-bicycle-window-cat` (long) |
| Security review | Done by magic-wormhole project | Falls on us |
| P2P direct connection | Yes, when NAT allows | No, always via relay |
| Bandwidth efficiency | Direct when possible | Always through relay |
| Resilience to client mobility | High (Dilation reconnect) | Low (simple reconnect only) |

## Effort estimate

- Relay server in Go: 1 day implementation + 0.5 day deployment (Docker,
  systemd, certs) + 0.5 day monitoring + alarms = **~2 days**
- Client integration in `go/internal/wormhole`: 1 day (replace fowld
  subprocess wrapper with direct dial + AEAD frame loop) = **~1 day**
- Tests (integration against real relay, fuzz on framing): **~0.5 day**
- Docs + migration story: **~0.5 day**

Total: **~4 days of focused work** to fully replace fowl.

## When to pull the trigger

Specific signals that would justify the migration:

- Pinning fowl forces us behind on multiple security patches and we can't
  safely upgrade
- Pi-image size becomes a constraint and the Python+fowl stack (~80 MB)
  matters
- Operators consistently hit installation friction on the friend side
  ("uv tool install fowl" trips up non-developers)
- Sourceful starts running other peer-to-peer flows (e.g. site-to-site Nova
  federation requiring TCP tunnels) where a shared relay is already
  warranted

Until one of these is real, the current pinned-fowl approach is the better
trade — leverage 10 years of magic-wormhole protocol work for free in
exchange for a small Python install.

## Open questions for revisit

- Is the longer token (6 words vs `7-foo-bar`) actually a problem in
  practice? PGP-wordlist-style is more secure but less ergonomic.
- Could we keep the `7-foo-bar` short-code by implementing SPAKE2 ourselves
  (~500 LOC additional)? That gets us most of the wormhole UX without the
  Dilation complexity.
- Does the relay need authentication (rate-limit by IP only) or should
  Sourceful-paired operators authenticate against Nova?
