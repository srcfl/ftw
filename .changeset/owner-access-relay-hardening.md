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
- **No friend-flow owner escalation.** The friend pair-flow reverse-proxies from
  loopback and must never inherit owner authority. Owner-credential management
  (enroll an additional passkey, list/delete devices) and pairing control
  (`/api/pair/start`, `/api/pair/abort`) now require a real passkey session or a
  genuine private-range LAN source — never the loopback bypass — so a temporary
  friend grant can't be escalated into a permanent owner passkey, and a friend
  can't lock the owner out by deleting their passkeys. The PIN-less LAN bootstrap
  and the enrollment-PIN endpoint were already source-checked; this extends the
  same discipline to the post-bootstrap credential surface. The owner-remote gate
  continues to key off the unforgeable tunnel marker, regression-guarded by the
  end-to-end `TestOwnerGateThroughRelay`.
- **Mandatory home-key pin.** The relay refuses to run a public home host
  (`-home-host`/`-home-site`) without `-home-pubkey`, so the internet-exposed home
  route is never left in trust-on-first-use mode (claimable by a racer after a
  relay restart); `-home-allow-tofu` is an explicit testing-only override.
- **Correct passkey RP-ID default.** The WebAuthn RP-ID now defaults to
  `home.fortytwowatts.com` (the origin the owner visits) instead of the relay
  host, so a deploy that forgets the env var no longer enrolls passkeys bound to
  the wrong, unusable origin (a one-way door).
- **Bounded request bodies.** Every relay request body is capped (with a tighter
  ceiling on the small unauthenticated control endpoints), and the Pi's WebAuthn
  finish handlers bound the attestation/assertion body, closing memory-exhaustion
  vectors on the public JSON surfaces.
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
