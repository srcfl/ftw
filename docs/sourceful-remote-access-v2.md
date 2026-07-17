# Sourceful remote access decision

Status: legacy FTW remote stack retired; dedicated replacement deferred,
2026-07-17

This document records the decision made after retiring
`home.fortytwowatts.com` and `relay.fortytwowatts.com` and evaluating a new
Cloudflare WebRTC/TURN service against the existing Sourceful Energy app and
Novacore integration.

## Decision

Do **not** rebuild a generic FTW remote-access portal or relay now.

The supported managed remote experience should be the Sourceful Energy app,
using Novacore as its consent, identity, site, telemetry and command plane.
FTW remains the local control and safety authority and makes outbound-only
connections to Sourceful services.

The full FTW web interface stays local. Owners who want that interface remotely
may provide their own VPN or private overlay network. This is a community path,
not an officially operated or supported Sourceful remote-access service.

The previously explored Worker + Durable Objects + WebRTC + managed TURN design
is retained only as a contingency. It should be reconsidered if a concrete
product requirement later needs direct, end-to-end encrypted access to the full
local UI without sending telemetry or commands through Novacore.

This decision complements
[Sourceful Energy app integration](sourceful-energy-app-integration.md), which
defines FTW as a software site coordinator.

## Why this boundary is preferable

Sourceful already has the major parts that a second FTW remote portal would
need to duplicate:

- user identity and site authorization;
- site and DER inventory;
- live telemetry delivery;
- fleet and site presentation;
- optimizer schedules and high-level energy controls;
- revocation and a natural place for audit history.

FTW already has the other half:

- persistent ES256/P-256 machine identity;
- outbound MQTT publishing to Novacore;
- local driver, control and safety paths;
- offline operation and default-mode fallback;
- validation points for future high-level remote intents.

A dedicated relay would add a second identity system, owner directory, browser
application, signaling protocol, NAT traversal service, session lifecycle,
incident surface and on-call responsibility. It would also pull networking and
browser-authentication machinery into the HEMS core without improving local
energy control.

Using the Sourceful app keeps the FTW core smaller: local control, local UI,
device integration, safety, history and one optional outbound federation
adapter. It also gives Sourceful one official owner experience instead of two
partly overlapping products.

## Product topology

```text
Managed remote experience

Sourceful Energy app ── user auth ──► Novacore
                                         ▲
                                         │ outbound MQTT/HTTPS
                                         │ telemetry + bounded intents
                                         │
Energy devices ◄── local control ─────── FTW
                                         │
                                         └── local web UI on LAN

Community full-UI access

Owner device ══ owner-operated VPN/private overlay ══ FTW local web UI
```

There is no inbound Sourceful connection to the home, no public per-home URL,
no Sourceful-operated generic TCP tunnel and no browser-to-home TURN session in
the recommended architecture.

## What belongs in each layer

| Capability | FTW local instance | Sourceful app and Novacore | Owner VPN |
|---|---|---|---|
| Control loop and device protocols | Authoritative | Never | Transport only |
| Safety validation and fallback | Authoritative | Never bypasses FTW | Never bypasses FTW |
| Full setup and low-level administration | LAN UI | Not exposed | Optional owner access |
| Remote overview and notifications | Publishes opted-in data | Official experience | Not required |
| Remote energy actions | Validates and executes | Sends bounded intents | Same local UI as LAN |
| User and multi-site identity | No cloud user account | Authoritative | Owner-operated |
| Gateway machine identity | Local ES256 key | Verifies/revokes key | Not required |

## Authentication and passkeys

Passkeys are still a good direction, but they belong at the Sourceful account
and app boundary, not in a second FTW owner portal.

That separation produces two clear identities:

- a user authenticates to Sourceful with a passkey and receives permission for
  specific sites and actions;
- an FTW instance authenticates to Novacore with its non-exportable local
  ES256/P-256 gateway key.

One Sourceful passkey can then cover all of an owner's sites and devices. FTW
does not store user passkeys, recovery material or Sourceful sessions.

The current Sourceful portal uses Privy email/Solana-wallet authentication and
then a Novacore JWT. Moving the Sourceful owner experience to passkeys is
therefore a separate identity product change; it is not a prerequisite for
retiring the FTW relay or for a read-only Sourceful-app MVP.

## Remote-control security model

Start with read-only monitoring. Add remote control only after the command
contract is explicit and tested.

Remote actions must be semantic, narrow and expiring, for example:

- change strategy to `self_consumption` until revoked;
- charge a battery until a specified time or state of charge;
- set an EV departure target;
- request idle or maintenance mode.

The app must not send Modbus writes, inverter registers or unrestricted raw
watt setpoints. A command envelope needs at least `command_id`, `site_id`,
`actor`, `scope`, `issued_at`, `expires_at` and a monotonic sequence. FTW
rejects replayed, stale, unauthorized or locally unsafe commands and publishes
an accepted/rejected/expired result with a reason.

Local operator action wins. Cloud loss never stops local control. FTW's stale
meter guard, SoC limits, fuse guard, per-phase protection, command clamps,
watchdog and default mode remain authoritative for every command source.

## Privacy and consent

This option is operationally simpler than TURN, but it is not the same privacy
model. Novacore and Sourceful can process the telemetry and high-level commands
that the owner has enabled; a direct WebRTC/TURN tunnel could keep owner API
payloads end-to-end encrypted from Sourceful.

The Sourceful integration must therefore be:

- disabled by default and enabled by an explicit owner action;
- clear about which telemetry and control permissions are requested;
- revocable from both FTW and the Sourceful app;
- governed by documented retention, deletion and account-recovery rules;
- unnecessary for local operation.

An owner who wants zero Sourceful cloud processing uses FTW locally, optionally
through an owner-managed VPN.

## Support and installer access

Owner remote monitoring is not the same as remote support. Do not restore the
legacy `ftw-pair` surface as part of app integration: it included broad web,
MCP, packet-capture, MQTT, Modbus and deployment capabilities.

If Sourceful later needs installer or support access, build it as a separate
managed capability with explicit owner consent, least-privilege scopes, short
expiry, named actors, complete audit history, immediate revocation and no
implicit shell or LAN access.

Self-hosters may use their own VPN and community tooling. Sourceful does not
operate, troubleshoot or warrant those networks under the community-supported
Apache-2.0 distribution.

## When to reconsider TURN

Reopen the direct-access design only if at least one validated requirement
cannot be served safely by the app model, such as:

- remote access to the complete local UI without cloud-visible telemetry;
- interactive diagnostics that cannot be represented as scoped operations;
- a customer contract explicitly requiring a Sourceful-hosted, end-to-end
  encrypted tunnel into FTW;
- a deployment where Novacore federation is unavailable but Sourceful must
  still operate remote access.

If that happens, the preferred contingency remains an outbound FTW WebSocket to
a per-site Cloudflare Durable Object for signaling, direct WebRTC where
possible, and short-lived Cloudflare Realtime TURN credentials as fallback.
That service must remain optional and outside the control loop. It requires a
fresh threat model, metadata policy, abuse controls, recovery design and
security review before implementation.

## Delivery order

1. **Complete:** retire the legacy relay and make old clients stop permanently
   on HTTP `410 Gone`.
2. Deliver explicit Sourceful-app pairing and revocation from the FTW LAN UI.
3. Ship read-only site status and telemetry in the app.
4. Add passkeys to the Sourceful account layer when the identity roadmap allows.
5. Define, test and audit high-level remote command intents before enabling
   control.
6. Document owner-managed VPN access as an optional, community-supported path.
7. Reconsider TURN only from a validated requirement, not as default platform
   infrastructure.

## Acceptance gates

- FTW runs fully when Novacore and every Sourceful service are unavailable.
- Enabling cloud integration requires explicit local owner consent.
- No inbound port or public home endpoint is required.
- App revocation stops telemetry and remote intents without affecting local
  control.
- Every remote command is attributable, bounded, replay-safe, locally validated
  and auditable.
- Full local administration is never exposed through the app accidentally.
- Community VPN guidance makes no promise of official Sourceful support.
