# ADR 0001 — WebAuthn RP-ID for owner remote access

- Status: Accepted (2026-06-03)
- Context: `docs/superpowers/specs/2026-06-03-home-route-passkey-design.md`

## Decision

The production WebAuthn Relying Party ID for owner passkeys is the dedicated
host **`home.fortytwowatts.com`** — never the apex `fortytwowatts.com`.

## Why this is a one-way door

The RP-ID is cryptographically baked into every passkey at creation. Changing
it later silently invalidates every enrolled credential and forces full
re-enrollment. Therefore:

1. **Never set RP-ID to the apex.** An apex RP-ID would place the credential on
   `fortytwowatts.com` and make it presentable on every sibling subdomain —
   exactly what the project's dedicated-domain rule forbids. A host is trivially
   a registrable-domain-suffix of its own origin, so `home.fortytwowatts.com`
   satisfies the WebAuthn suffix rule on its own.
2. **Do NOT enroll real owner passkeys under `relay.fortytwowatts.com`.** A
   passkey created with RP-ID `relay.*` will not work at `home.*`. Phase 1 runs
   on the existing `relay.fortytwowatts.com/me/<site_id>` path purely for
   security-floor hardening; production passkey enrollment begins in Phase 4 on
   the `home.fortytwowatts.com` host.

## Sequencing

- **Phase 1–3:** RP-ID stays `relay.fortytwowatts.com` (the host actually
  serving the page). `FTW_OWNER_ACCESS_RPID` remains the override knob.
- **Phase 4:** when `home.fortytwowatts.com` exists (host + wildcard TLS +
  routing), flip the default to `home.fortytwowatts.com` and serve enrollment
  from that origin. This is the moment real passkeys are first enrolled.
