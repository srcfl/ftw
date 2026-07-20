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

## NEXT — optional FTW Home Link

After the NOW exit evidence is complete, FTW can add an opt-in managed remote
experience. Its first releasable slice is read-only status, health, plan preview
and history summaries. Pairing or service availability must not change local
control, setup, history or fallback planning.

### Identity and pairing

- Passkey authentication terminates at the local FTW instance through the
  tunnel. There is no Sourceful user account or cloud authentication step.
- FTW stores only the public WebAuthn verifier needed for each local
  credential: credential ID, public key, signature counter and local credential
  label. It never receives or stores a passkey private key, password or shared
  access key. The relay stores no user credential material.
- Each FTW installation has a separate permanent gateway identity for the
  site/machine. A compatible hardware-protected P-256 secure element is always
  tried first: its existing key and normalized 18-hex serial become the gateway
  identity, and the private key never leaves the chip.
- Without compatible hardware, FTW creates and atomically persists a local
  P-256 identity with the same contract: a stable 18-hex gateway ID, a raw
  64-byte `X || Y` public key, and raw 64-byte `r || s` signatures over SHA-256.
  The private key remains local. Either identity authenticates the tunnel; it
  is not a user passkey and grants no control by itself.
- First passkey enrollment starts from the LAN UI. FTW creates a short-lived,
  single-use, high-entropy secret and shows one QR code/deep link. FTW validates
  and consumes the secret locally; there is no PIN or durable enrollment key.
- Later passkey ceremonies and requests use the same remote route, but FTW
  still creates the challenge and verifies the response locally.
- Multi-site means several independently paired site aliases. Each site
  authenticates locally; there is no central user-to-site directory. A richer
  consolidated multi-site view remains gated in LATER.

Proposed domain layout:

| Host | Single responsibility |
|---|---|
| `<adjective>-<color>-<animal>.home.sourceful.energy` | Deterministic human address for one FTW gateway |
| `home.sourceful.energy` | Stable WebAuthn relying-party ID; no account directory |
| `uplink.home.sourceful.energy` | FTW machine connection endpoint |

`drivers.sourceful.energy` remains the separate driver-package service. It is
not a pairing, authentication or Home Link endpoint.

The three-word name is derived from the stable gateway ID in the exact order
adjective–color–animal. It is a display name and DNS alias, never an
authentication factor or secret. Existing compatible Sourceful Energy
Gateways must derive exactly the same name. There are no chosen names or manual
namespace exceptions. The gateway's public key and 18-hex ID are authoritative.

### Connection and authorization contract

FTW opens one outbound-only, long-lived TLS connection to the machine endpoint.
The relay challenges the connection; FTW signs that challenge with its
permanent site/machine key. There is no long-lived bearer or shared tunnel
token. A small versioned multiplexing contract carries several concurrent
request/response and event streams over that connection. The contract must
define stream identity, message kind, ordering, deadlines, cancellation,
per-stream flow control, bounded buffering and reconnect behavior. It must not
expose a generic tunnel or make transport reachability equivalent to authority.

Relay state is limited to alias-to-public-gateway-identity mapping, active
routing, status and timestamps. It stores no user credentials and makes no user
authorization decision.

After a locally verified passkey ceremony, FTW creates a short-lived access
proof bound to its own site identity, the authenticated local credential and
the exact scope. FTW verifies that proof on later requests, rejects replay and
expiry, and records the outcome. Read-only scopes ship first.

Later NEXT slices may introduce only semantic, expiring intents, such as an EV
departure target or a temporary operating strategy. Intents never contain raw
device writes. Core translates an accepted intent into its own plan, clamps and
leases; stale telemetry, local limits and local operator actions remain
authoritative.

NEXT cannot graduate until tests demonstrate enrollment-secret consumption,
passkey and gateway-key revocation, challenge replay rejection, stream
isolation and backpressure, multi-site separation, bounded outage recovery, and
uninterrupted local operation while the relay is unavailable. Identity tests
must cover hardware-first selection, atomic software-key persistence, exact
wire encodings, normalized 18-hex IDs, and known gateway-ID-to-name vectors
without special cases.

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
| Native widgets and richer multi-site views | The read-only Home Link schema, per-site authentication and privacy budget are stable in production. |
| V2X automation | Bidirectional capability, metering, lease ownership, interlocks and fallback are proven for the complete local actuation path. |
| General vehicle snapshot adapter | A minimal vendor-neutral snapshot has stable vehicle identity, freshness and consent semantics without becoming a second control path. |

## Active parallel program — canonical driver packages

Canonical driver-package delivery through `drivers.sourceful.energy` is already
an active, separate program. This roadmap neither duplicates that work nor sets
a device-count target. It does not change bundled Lua recovery drivers, the
device-repository format or its defaults. Core consumes only packages that pass
the existing host-contract, signature, compatibility, activation and rollback
rules.

The architectural decision for NEXT is recorded in
[ADR 0005](adr/0005-outbound-site-link.md).
