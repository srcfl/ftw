---
"forty-two-watts": minor
---

Local docker E2E harness — tier 2: container-side P2P + passkey proof.

Adds an automated, fully-in-docker browser test that drives the real home route
through the relay and proves the WebRTC DataChannel forms **directly**
container-to-container (where there is no NAT, unlike a Mac-host browser):

- A headless-Chromium (Playwright) container on the tier-1 bridge net
  (`docker-compose.e2e-tier2.yml`, profile `tier2`) enrolls and logs in with a
  passkey via a **CDP virtual WebAuthn authenticator** (unattended), asserts
  `window.ftwP2P.state()` reaches `direct` and that the selected ICE candidate
  pair is host/srflx (never `relay`), then makes one authenticated owner API
  call (`/api/status`) over `p2pFetch`.
- New `make e2e-docker-tier2` target brings the stack up, runs the test, and
  exits non-zero on failure.
- New `FTW_P2P_STUN` env knob on the main app: unset keeps the production STUN
  set; `none`/`off` gathers host candidates only (correct + fast on a closed
  shared-L2 network like the docker bridge); a comma-separated list overrides
  the default. No behaviour change when unset.

The harness runs the Pi with `FTW_OWNER_ACCESS_RPID=home.fortytwowatts.localhost`
so the WebAuthn origin check passes against the `*.localhost` secure-context home
host. Docs: `docs/local-e2e-docker.md`.
