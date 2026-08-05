# FTW roadmap

This roadmap is a delivery boundary, not a feature inventory. **NOW** contains
work already in implementation or acceptance. **NEXT** has a defined contract
and entry gates. **LATER** items have no delivery promise; each must satisfy its
promotion gate before it can move forward.

The permanent rules do not move between lanes: core owns safety and dispatch,
the site sign convention is unchanged, and local operation never depends on an
optional service.

## NOW — close the P0 control and product loop

NOW is complete only when these four tracks are implemented, tested together,
and understandable from the local UI:

| Track | P0 outcome | Exit evidence |
|---|---|---|
| Access boundary | One admission policy covers state-changing requests during setup, boot, normal API operation and local development. Trusted local access remains recoverable; non-local mutation fails closed. The separate site-controller identity remains read-only. | Positive and negative tests cover every lifecycle phase, origin/host handling, credential enforcement and local recovery. |
| Energy ledger and history | One durable ledger records import and export separately, with interval, source and quality/freshness attached. Daily and settlement-period views are derived from that record across hot, warm and cold history; control never offsets earlier import with later export. | Tier-boundary, restart, rolloff and reconciliation tests agree with the site sign convention and retain provenance. |
| Battery-to-EV lease | Battery support for EV charging is an explicit local lease with a bounded site/loadpoint scope, start, expiry and observable state. Expiry, stale required telemetry or loss of the controlled session releases it; all normal SoC, fuse, power and slew limits still apply. | Tests cover grant, replacement, expiry, restart policy, stale-data release, optimizer interpretation and local operator priority. |
| Mobile and optimizer UX | The local experience works at narrow widths and explains current action, next planned action, fallback state, freshness and active battery-to-EV lease without requiring diagnostic knowledge. | Viewport checks and UI tests cover normal planning, optimizer unavailable/invalid, stale telemetry and lease expiry. |

The active access-boundary and read-only site-controller work are inputs to
this lane, not parallel remote products. Their contracts must converge on one
rule: identity can establish who or what is speaking, but only core can admit a
mutation and validate its effect.

## NEXT — the FTW app

The optional FTW Home Link was built, shipped and then removed whole; see
[ADR 0006](adr/0006-app-uplink.md). The remote lane is now one thing: the FTW
app at `app.ftw.energy`, talking to the box over its own protocol. Pairing or
relay availability must not change local control, setup, history or fallback
planning, and does not.

### Identity and pairing

- The box holds three secrets with three lifetimes: a Noise static X25519 key
  that never changes, a rotatable 32-byte rendezvous secret, and a single-use
  pairing code with a ten-minute life. None of them is in SQLite.
- Trust reaches the app optically. The QR code is a URL fragment carrying the
  static public key, the pairing code, a LAN hint and the rendezvous secret.
  A fragment is never sent in an HTTP request, so nothing the app trusts ever
  passes through a server.
- The box is not a WebAuthn relying party. It authenticates a phone by the
  pairing code in the first handshake message and afterwards by the app's
  pinned static key. The passkey lives in the app against `app.ftw.energy`,
  where it gates enrollment and privileged commands rather than reading.
- The machine identity in [`go/internal/gatewayidentity`](../go/internal/gatewayidentity)
  is unchanged and unrelated: hardware-protected P-256 where the hardware
  exists, a bound software key otherwise, and the same deterministic
  adjective-color-animal display name derived from the stable 18-hex gateway
  ID. It identifies the machine; it authorises nothing.
- Multi-site means several independently paired boxes. There is no central
  user-to-site directory. A consolidated multi-site view stays gated in LATER.

### Connection and authorization contract

The box holds one outbound WSS connection to `wss://relay.ftw.energy`, joining
under a handle derived per epoch from the rendezvous secret with HKDF-SHA256.
The handle rotates hourly, so the relay operator cannot follow a household from
one hour to the next, and there is no DNS alias or other stable per-box name.
An epoch correction from the relay is read as a clock correction and clamped;
it is never an order.

The relay forwards encrypted frames and holds no keys. Up to four phones share
one uplink; the relay broadcasts, and the box lets the AEAD decide which
session a frame belongs to, because asking the relay would require the relay to
know. Lane 0 frames are constant in length and cadence, because a
variable-length 1 Hz power stream leaks a household's load pattern through
perfect encryption.

Commands carry an expiry and preconditions, and core revalidates against fresh
state before acting. A queued command is never replayed silently. Site mode
changes go through `control.ApplyMode` from every door. Stale telemetry, local
limits and local operator actions remain authoritative.

The public `srcfl/device-drivers` release channel remains separate from
pairing and authentication.

Open work before this lane is finished:

- an on-box pairing surface, so the QR payload can be seen without a terminal;
- per-device revocation, so one lost phone does not require rotating the
  rendezvous secret and re-pairing the household;
- push, history and the plan surface over the same protocol.

### Conditional Apple EnergyKit native companion

Apple EnergyKit is a conditional native companion initiative, never an FTW
core implementation. The base framework, electricity guidance, and EV/HVAC
load events require iOS/iPadOS 26 or later. Named load devices, EV
status/reasons/targets and Home presentation belong to the OS 27 beta line.
EnergyKit remains officially limited to the contiguous United States, so a
Sweden pilot is blocked by both region and the stability of the beta APIs.

The native app owns the entitlement, consent, venue mapping, guidance token and
offline event submission. For every venue, the person explicitly opts in to a
binding between their local passkey identity, the site-controller public key
and that venue. Person identity and site identity never collapse into one key.

Core owns a versioned, vendor-neutral venue/guidance/load-event flow and a
durable neutral EV event journal. Guidance is advisory input and passes the
same completeness, freshness and safety validation as every other planner
output. Adapter absence, denial or regional unavailability leaves FTW planning
and local operation unchanged.

This initiative cannot graduate until regional availability includes the
target site, the OS/API surface is stable, the neutral EV event journal is
durable, and the consent, retention and deletion model has passed privacy
review. See Apple's official [EnergyKit overview](https://developer.apple.com/energykit/)
and [EnergyKit updates](https://developer.apple.com/documentation/updates/energykit)
for the platform boundary.

## LATER — promote only from evidence

These are bounded follow-on directions, not scheduled commitments:

| Direction | Promotion gate |
|---|---|
| OCPP gateway | The EV lease/action model and stable charger identity are proven locally, including disconnect and autonomous-default behavior. |
| External grid constraints | A versioned constraint record has provenance, effective window, expiry, conflict handling and an audit trail; it can never weaken physical site limits. |
| Active heat | Neutral thermal capabilities, comfort bounds and a safe autonomous default are demonstrated before dispatch is enabled. |
| Native widgets and richer multi-site views | The app protocol's read schema, per-site pairing and privacy budget are stable in production. |
| V2X automation | Bidirectional capability, metering, lease ownership, interlocks and fallback are proven for the complete local actuation path. |
| General vehicle snapshot adapter | A minimal vendor-neutral snapshot has stable vehicle identity, freshness and consent semantics without becoming a second control path. |

## Later — Device Support package promotion

Device Support may later consume an exact `srcfl/device-drivers` commit for
another product or a higher support level. That work must not create a second
editable source or replace FTW's public default channel. Core will consume only
packages that pass its host contract, signature, compatibility, activation and
rollback checks.

The architectural decision for NEXT is recorded in
[ADR 0005](adr/0005-outbound-site-link.md).
