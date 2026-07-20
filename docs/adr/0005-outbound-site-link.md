# ADR 0005: Outbound FTW Home Link

- Status: accepted direction; implementation is gated by the roadmap's NOW
  exit evidence
- Date: 2026-07-20
- Supersedes: historical ADR 0003's placement of user passkeys at a Sourceful
  account boundary, plus its replacement-deferred and transport-specific
  follow-up direction
- Reconsiders: historical ADR 0001 only to permit a new local WebAuthn verifier
  under the `home.sourceful.energy` relying-party boundary
- Preserves: ADR 0003's retirement of the former remote-access implementation;
  no former domain, protocol, credential store or connection path returns

## Context

FTW needs an optional, simple remote experience without moving control or
safety authority away from the site. Current P0 work establishes two necessary
boundaries: one centralized admission policy for mutations, and a distinct
read-only site-controller identity and snapshot contract.

Historical ADR 0003 correctly retired the former implementation. It also moved
passkey authentication to a Sourceful user-account boundary, deferred a
dedicated replacement and tied the possible follow-up too closely to a
particular federation path. Product direction now requires local FTW
authentication through a much smaller relay boundary. This ADR changes those
parts honestly; it does not restore the retired design.

## Decision

FTW will use an optional **FTW Home Link** with four separations:

1. **User identity:** passkey authentication terminates at the local FTW
   instance through the tunnel. FTW stores only credential ID, public key,
   signature counter and a local credential label. It never receives or stores
   a passkey private key, password or shared access key. The relay stores no
   user credential material.
2. **Gateway identity:** every FTW installation has a separate permanent
   gateway identity for the site/machine. A compatible hardware-protected
   P-256 secure element is tried first. Its existing private key, public key and
   normalized 18-hex serial are used; the private key never leaves the chip.
   Without compatible hardware, FTW creates and securely, atomically persists a
   local P-256 identity with the same wire contract: a stable 18-hex ID, raw
   64-byte `X || Y` public key and raw 64-byte `r || s` signatures over SHA-256.
   Possession identifies the gateway but does not authorize a mutation.
3. **Transport:** FTW initiates one outbound-only, long-lived TLS connection.
   The relay sends a challenge and FTW signs it with the site/machine key; no
   long-lived bearer or shared tunnel token exists. A small versioned
   multiplexing contract carries concurrent request/response and event streams
   with bounded flow control and explicit reconnect rules.
4. **Authority:** FTW creates short-lived access proofs only after local
   passkey verification. Each proof is bound to the gateway identity, local
   credential and exact scope, and FTW verifies it again before admitting a
   request. Core then applies its normal intent validation, leases, clamps and
   safety checks.

First passkey enrollment starts from the LAN UI with a short-lived,
single-use, high-entropy QR/deep-link secret. FTW validates and consumes the
secret locally; there is no PIN. Later passkey challenges, assertions and
requests are routed through the Home Link, while their authentication remains
local to FTW.

The domain boundary is:

- `<adjective>-<color>-<animal>.home.sourceful.energy` as the deterministic
  human address for one gateway;
- `home.sourceful.energy` as the stable WebAuthn relying-party ID;
- `uplink.home.sourceful.energy` as the FTW machine connection endpoint.

The three-word gateway name is derived only from the stable gateway ID in the
order adjective–color–animal. Existing compatible Sourceful Energy Gateways
must retain the exact same derived name. The derivation has known ID-to-name
compatibility vectors and no manually assigned exceptions. The name is a human
display value and DNS alias, never an authentication factor or secret.

Relay state is limited to the derived alias to public gateway-identity mapping,
active routing, status and timestamps. The gateway public key and 18-hex ID are
authoritative; the relay stores no user credential.

Read-only status, health, plan preview and history summaries are the first
allowed scopes. A later control scope can carry only semantic, expiring intents
that core can accept or reject. The link cannot expose arbitrary device writes
or a generic connection to the local UI or LAN.

## Consequences

- FTW retains full setup, history, planning, control and recovery while the
  Home Link relay is unavailable.
- Revoking a local passkey or the machine identity stops its remote authority
  without changing local operation.
- The central mutation boundary remains mandatory even after a proof verifies;
  transport authentication is not control authorization.
- One connection can support independent bounded streams without creating
  several competing connection lifecycles on the FTW machine.
- The service needs explicit threat modeling for pairing, proof issuance,
  relying-party/origin handling, deterministic-name collisions, replay,
  revocation, stream isolation, buffering and audit before mutation scopes can
  ship.
- The read-only site-controller direction remains compatible: it can supply a
  bounded pairing statement and snapshot without gaining command authority.

## App adapters

Platform-specific energy frameworks stay in their native app. In particular,
an Apple EnergyKit companion owns entitlement, per-venue consent and mapping,
guidance tokens, platform types and offline submission. The person explicitly
binds their local passkey identity and the separate site-controller public key
to each venue; those identities never collapse.

The base guidance and EV/HVAC load-event surface requires iOS/iPadOS 26 or
later. Named load devices, EV status/reasons/targets and Home presentation are
in the OS 27 beta line. Current availability is limited to the contiguous
United States, so a Sweden pilot remains blocked.

Go core contains only a versioned, vendor-neutral venue/guidance/load-event
flow and durable neutral EV event journal. Guidance remains advisory and is
validated like every planner output. Adapter absence, denial, unstable beta
surface or regional unavailability is a cleanly unavailable input, not a
planner or control failure.

## Compatibility tests

Identity tests must prove hardware-first selection and software fallback with
the same P-256 contract. They cover normalized 18-hex gateway IDs, exact public
key and signature byte encodings, signature verification over SHA-256, atomic
software-key persistence, and known gateway-ID-to-three-word-name vectors.
There are no manual namespace special cases.

## Non-decisions

This ADR does not enable remote mutation, choose a transport library, define a
support channel, add cloud user accounts, alter driver packages, or change any
local control invariant. Those require their own reviewed contracts and the
roadmap promotion gates.
