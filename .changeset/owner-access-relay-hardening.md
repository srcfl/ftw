---
"forty-two-watts": minor
---

Owner remote-access + relay hardening (pre-release security pass) — closes the
home.* exposure and the issues a multi-agent audit surfaced around it.

**Security**

- **Authenticated relay registration.** `POST /me/register` is now ES256-signed:
  the Pi signs `(site_id, host_id, ts)` with its self-sovereign site identity and
  the relay verifies it, pins the key per site (trust-on-first-use, or an
  operator-provisioned `-home-pubkey` for the internet-exposed home host), and
  refuses a conflicting key or a stale timestamp. Previously any internet client
  could repoint a site's tunnel mapping to a host it controlled (owner-session
  theft + dashboard MITM).
- **No friend-flow enrollment hijack.** The PIN-less LAN enrollment bootstrap and
  the enrollment-PIN endpoint now require a genuine private-range LAN source, so a
  relay/sidecar request arriving from loopback (the friend pair-flow path) can
  never bootstrap-enroll itself as the owner or mint the PIN. The owner-remote
  gate continues to key off the unforgeable tunnel marker, and a new end-to-end
  test (`TestOwnerGateThroughRelay`) regression-guards that an unauthenticated
  home-host request is refused.
- **Relay reflected-XSS fix.** The pair-session landing page now charset-validates
  the routing token on registration and JSON-encodes it into the page, so a token
  planted via the open `POST /tunnel/register` can't break out of the page.
- **On-host liveness.** Loopback `GET /api/health` probes (deploy/CI/docker
  HEALTHCHECK) are exempt from the gate without exposing health detail remotely.

**Robustness**

- Relay caps tunneled body size, bounds each forwarded request with a timeout so a
  dead-but-registered host fails fast instead of pinning a goroutine, and GCs
  expired/revoked pair tokens.
- `home.*` now serves a calm, self-contained **offline page** (with auto-retry)
  when the Pi is offline or hasn't checked in recently, instead of a raw timeout.

**Onboarding & UX**

- A persistent **"Run setup wizard"** control in the dashboard (re-run setup
  without a fresh install), an in-UI **"Show enrollment PIN"** affordance on the
  LAN (with copy + live countdown) so first-passkey enrollment isn't a dead end,
  and `/setup?step=N` deep-links now navigate to the requested step.
- **EV charger setup fix:** the provider dropdown is populated and the
  username/field-id mismatch that made the whole EV section non-functional is
  corrected.

Note for operators: upgrade the relay and the Pi together — the hardened relay
requires the signed registration the updated Pi sends.
