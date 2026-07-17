# ADR 0003 — Retire legacy FTW remote access

- Status: Accepted (2026-07-17)
- Supersedes: [ADR 0001](0001-passkey-rp-id.md)'s active WebAuthn decision and
  ADR 0002's planned relay/domain migration
- Detail: [Sourceful remote access decision](../sourceful-remote-access-v2.md)

## Decision

Retire the legacy FTW relay, TURN/WebRTC path, owner-passkey portal and
`ftw-pair` support tunnel. Do not migrate them to Sourceful domains.

The full FTW dashboard and API are LAN-local. Operators who want that complete
interface remotely may use a VPN or private overlay that they operate and
trust. This is a community-supported self-hosting path and must not be exposed
by public port forwarding.

The planned managed remote experience is the Sourceful Energy app through an
explicit, optional Novacore integration. FTW initiates outbound connections,
remains the local control and safety authority, and continues operating when
Sourceful services are unavailable. Mobile integration starts read-only;
future control uses narrow, expiring intents that FTW validates locally.

Passkeys, if adopted, belong to the Sourceful user/account boundary rather
than a separate FTW owner portal. FTW retains its persistent P-256 machine key
for Novacore gateway identity but no longer stores or verifies user passkeys.

## Consequences

- `ftw-relay`, `ftw-pair`, Pion/WebRTC, tunnel, owner-access, passkey, legacy
  fleet-reporter and related release/deployment surfaces are removed.
- New databases no longer create owner-session or trusted-browser tables.
  Existing tables are left untouched during upgrade to avoid destructive state
  migration.
- Obsolete `remote_access` and `fleet_statistics` YAML keys are ignored.
- No Sourceful-hosted endpoint provides the full local administration UI.
- Any future direct-access service requires a new requirement, threat model,
  abuse controls, recovery design and security review.
