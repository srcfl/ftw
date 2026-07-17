# Sourceful Energy app integration

Status: product and implementation plan, 2026-07-17.

## Decision

FTW should appear in the Sourceful Energy app as a **software site
coordinator**, implemented through Novacore's normal gateway, site, device and
DER model. Attachment is always an explicit owner action; detecting or adding
a driver must never silently enable cloud publishing.

There are two onboarding modes:

- **Zap-anchored (preferred when a local Zap is present):** FTW is attached to
  the same Sourceful `site_id` as the physical Zap. Zap remains a physical
  gateway and FTW becomes a separate coordinator member of that site.
- **Standalone:** when no Zap is present, the owner explicitly selects or
  creates a Sourceful site and registers FTW as its software gateway.

“Same site” must not mean “same credential”. FTW never copies Zap's private
key, assumes Zap's gateway ID or publishes with Zap's MQTT identity. Each keeps
its own revocable P-256 identity; Novacore records their relationship.

The local FTW instance remains the control authority and continues operating
without cloud access. Novacore is the consent, federation and mobile
presentation plane. A physical Zap is the preferred site anchor when present,
but it is not a requirement for using the app.

This direction reuses the existing opt-in `internal/nova` publisher, but the
current CLI claim flow and legacy wire adapter are foundations rather than a
finished mobile integration.

## Product experience

The entry point belongs under **Settings → Integrations → Sourceful Energy**,
outside the required device/setup flow. Successful Zap discovery may show a
non-blocking **Add FTW to this Zap's Sourceful site** card, but it performs no
cloud or signing operation until the owner presses **Connect**.

### With a Zap

1. FTW shows the detected Zap serial and fingerprint from `GET /api/crypto`.
2. The owner presses **Connect**. FTW creates a short-lived pairing session
   bound to the Zap fingerprint and FTW's own persistent public key.
3. With a single-use Novacore challenge, FTW asks the local Zap to sign a
   domain-separated attachment proof through `POST /api/crypto/sign`.
4. FTW displays a QR code and **Open in Sourceful Energy** deep link. The app
   resolves the Zap to its existing site and shows “Add FTW coordinator to
   _Home_”, both device fingerprints and requested permissions.
5. The owner approves. If the Zap has not yet been claimed, the app first asks
   the owner to select or create its site; it never guesses one.
6. Novacore adds FTW as a separate coordinator gateway under the same site and
   returns FTW's own broker registration and durable mappings.
7. The app shows the existing energy site with **Zap** as the physical gateway
   and **FTW** as coordinator. Either membership can be revoked independently.

### Without a Zap

1. The same integration page offers **Connect without Zap**; it is never run
   automatically on first boot.
2. FTW displays the short-lived Sourceful Energy approval link.
3. The owner explicitly selects an existing site or creates a new one.
4. Novacore registers FTW's persistent public key as an `ftw` software gateway,
   provisions its devices and returns broker configuration.
5. The app shows **Connected via FTW** and offers revoke from either side.

No shell command, pasted JWT, organization ID or site ID belongs in this user
journey.

## Architecture

```text
Zap-anchored site

Energy devices ──► Zap ── direct DER telemetry ───────────────► Novacore
                    │                                             ▲
                    └── local API ──► FTW coordinator ────────────┘
                                      strategy, health, plans,
                                      non-overlapping FTW devices

Novacore site membership: [physical Zap gateway] + [FTW coordinator gateway]
```

```text
Standalone site

Energy devices ── native FTW drivers ──► FTW coordinator ──► Novacore
                                                                  │
                         Sourceful Energy app ◄────────────────────┘
```

| Site member | Role | Runtime identity | Default published data |
|---|---|---|---|
| Physical Zap | `physical_gateway` | Zap hardware P-256 key | Its directly attached DER telemetry |
| FTW with Zap | `site_coordinator` | FTW instance P-256 key | Strategy, health, plans and non-overlapping devices |
| FTW without Zap | `site_coordinator` + software gateway | FTW instance P-256 key | FTW-provisioned DER telemetry plus coordinator state |

The [current public environments](https://developer.sourceful.energy/docs/getting-started/environments)
are:

- testnet REST/MQTT: `novacore-testnet.sourceful.dev`, MQTT/TLS port 8883;
- mainnet REST/MQTT: `novacore-mainnet.sourceful.dev`, MQTT/TLS port 8883.

These values must come from a signed discovery document or release config,
not remain scattered as hard-coded defaults.

## Identity and pairing contract

There are three different trust roles and they must not be collapsed into one
key:

- Zap has a hardware-bound ES256/P-256 gateway key. `GET /api/crypto` exposes
  only its serial and public key; `POST /api/crypto/sign` can produce a local
  possession proof without exporting the private key.
- FTW already has a persistent ES256/P-256 instance key. The current Novacore
  [gateway-claim contract](https://developer.sourceful.energy/docs/guides/gateway-claiming)
  also uses ES256, so this key should remain the durable
  identity of the FTW coordinator gateway. Its private half never leaves FTW.
- Current Novacore user identities and Sourceful Energy delegation use
  Ed25519. Pairing should therefore use a short-lived Ed25519 delegate key,
  rather than changing or exporting either gateway key.

FTW creates its persistent gateway key locally. That local file alone does not
create a Nova identity or enable federation. Only the approved onboarding
transaction creates the FTW gateway record and site membership in Novacore.

Use Sourceful's documented
[Bifrost session flow](https://docs.sourceful.energy/developer/auth/) instead
of inventing a second mobile-auth protocol:

1. FTW generates an ephemeral Ed25519 delegate key and calls Bifrost
   `POST /api/auth` with its public key, an FTW integration label and the
   least-privilege permissions required for one site.
2. Bifrost returns `session_id` and `session_url`. FTW renders the URL as a QR
   code and **Open in Sourceful Energy** link. The documented session lifetime
   is at most three minutes.
3. The owner approves in the Sourceful Energy app. FTW polls
   `GET /api/auth/{session_id}` for the delegated token.
4. FTW keeps that human-approved token only in memory. In Zap-anchored mode it
   exchanges the Zap attachment proof plus its own key proof for membership in
   the Zap's existing site. In standalone mode the owner selects or creates the
   site before FTW's key is claimed as an `ftw` software gateway.
5. Novacore returns durable site, gateway, device and DER mappings plus broker
   discovery. FTW stores the identifiers; runtime MQTT authentication is made
   with the persistent gateway key.
6. FTW erases the delegated JWT and ephemeral Ed25519 private key after
   claim/provision. No human token is written to YAML or SQLite.

The public contracts do not yet define a Sourceful Energy consent scope for
claiming/provisioning an FTW software gateway, nor the `ftw` gateway type. Phase
0 must add and ratify those pieces. Scope names must follow Sourceful's current
`action:resource` convention. `read:site` and `write:control:der` already exist
as documented examples; proposed claim/provision/revoke scopes should not be
hard-coded in FTW until Novacore enforces their final names. Telemetry publishing
is authorized by the claimed gateway identity, not by a long-lived user scope.

The existing Zap signing endpoint is a LAN possession oracle, not owner
authorization. A valid attachment therefore requires all three proofs in one
single-use, short-lived session:

- Zap signs a domain-separated message containing the Bifrost session ID, a
  server nonce and the hash of FTW's public key. The existing endpoint then
  appends its own nonce, timestamp and Zap serial; Novacore verifies the exact
  returned message against the registered Zap key.
- FTW signs the same attachment ID with its own domain-separated key proof.
- The Sourceful Energy user explicitly approves the resolved site membership.

An attacker who merely shares the LAN can at most request a Zap signature; it
cannot complete owner approval. Rate limits and one-time challenges still
apply. Zap proves co-location, the app proves ownership, and FTW proves which
coordinator key is being attached.

Novacore should persist this as a first-class `coordinator_attachment`, not as
an overloaded device claim. At minimum it records the site, FTW gateway, the
optional anchor Zap, approving identity, creation time and revocation time.
Revoking FTW must leave the Zap and its site intact; revoking or replacing Zap
must not silently transfer its identity to FTW. The approved FTW membership is
durable until explicitly revoked, so the Zap is not required to remain online
for every FTW MQTT reconnect.

## Telemetry and device model

FTW must publish its clean, site-convention schema. Schema translation belongs
at the Novacore ingress boundary during migration, not in every FTW driver.
Before the app MVP, agree one versioned wire schema for meter, PV, battery, EV,
V2X, vehicle and flexible-load telemetry.

Device reconciliation uses FTW's hardware-stable `device_id`:

- `make:serial` when a native hardware serial is available;
- `mac:<mac>` for same-L2 fallback;
- `ep:<endpoint>` only as the final fallback.

Novacore returns durable `device_id` and `der_id` mappings, which FTW stores in
SQLite. Reconciliation must be idempotent and safe to repeat after a driver is
added, renamed or removed.

## Physical Zap and duplicate-source rule

A Sourceful site may already contain a physical Zap publishing the same meter
or inverter that FTW reads locally. Shipping without a source-authority rule
would double-count power and energy.

The required model is one canonical physical Device/DER with one active
telemetry-source lease:

- reconcile first on immutable make + serial;
- default a Zap-anchored onboarding to **Zap direct** for every overlapping DER;
- let FTW publish coordinator state, strategy, plans and devices that Zap does
  not already own, without republishing the same raw meter/inverter stream;
- let the owner explicitly switch an individual DER to **FTW coordinated** only
  when FTW has a distinct, hardware-stable source for it;
- record the selected source in Novacore, with an auditable handover;
- never sum duplicate sources merely because their gateway IDs differ;
- if FTW goes offline, Sourceful may fall back to Zap direct only after a
  freshness timeout and an explicit source-priority rule.

For the read-only MVP, pairing must block or exclude overlaps it cannot
resolve. Silent double counting is not an acceptable temporary behavior.

The current FTW Zap driver intentionally aggregates child DERs into one local
driver reading. That is useful for local control and dashboards but is not a
stable identity for cloud deduplication. In Zap-anchored mode those aggregates
must therefore stay local while Zap direct is the source lease. A later
per-child publisher may use downstream make+serial mappings, but it must not
invent child identities from aggregate metric names.

## Physical Zap local-control contract

The current Zap firmware already exposes semantic local REST actions for
battery power, PV curtailment and default-mode release. These are distinct from
mobile remote control: they are the final local actuator hop from FTW to a
physical device behind Zap.

The current REST response confirms only queue admission. Its commands do not
participate in the firmware's MQTT-only duration and heartbeat watchdog, and
the final hardware result is not retrievable over the local API. FTW therefore
keeps Zap telemetry-only until the firmware exposes a versioned, per-DER
capability contract, mandatory command expiry, session-loss fallback and a
queryable `executed`/`failed` acknowledgement. The implementation should reuse
the firmware's existing `ControlCommand` and `DerControlState` behavior so
cloud and LAN commands have one safety model.

This local lease is subordinate to FTW's site controller lease. A remote app
intent is first accepted or rejected by FTW; only the resulting bounded local
setpoint may be delegated to Zap. The app never talks directly to Zap control
routes while FTW is the selected coordinator.

## Remote control model

Remote control is a later, separately consented capability. The app sends
high-level, expiring intents such as:

- strategy: `self_consumption`, `arbitrage`, `cheap_charge`;
- temporary manual battery target with a deadline;
- EV departure target;
- idle/maintenance request.

It must not write inverter registers or durable raw watt setpoints through
Novacore. FTW validates every intent through its existing safety path: stale
meter guard, SoC limits, fuse guard, per-phase protection, command caps and
default mode.

There is one controller lease per site. A command envelope needs at least
`command_id`, `site_id`, `issued_at`, `expires_at`, monotonic sequence,
requested scope and actor. FTW publishes accepted/rejected/expired status and
the reason. Replays, stale commands and commands outside the active lease are
rejected before dispatch.

Local operator action always wins and can revoke the remote lease instantly.
Loss of Novacore connectivity never stops local control.

## Delivery phases

### Phase 0 — contract alignment

- Confirm the split identity contract against deployed testnet: Ed25519 for
  the user/delegation session, ES256 for the persistent software gateway.
- Define the `ftw` software-gateway type and the least-privilege Bifrost scopes
  that authorize claim, provision and revoke for one selected site.
- Define a durable `coordinator_attachment` relation with `site_id`, FTW
  gateway ID, optional anchor Zap gateway ID, approving identity, timestamps
  and independent revocation state.
- Define and test the domain-separated Zap+FTW attachment challenge. Zap proof
  establishes local possession only; Sourceful Energy approval remains the
  ownership authority.
- Confirm current Novacore REST, gateway claim, MQTT authentication and
  telemetry schemas against the deployed testnet.
- Replace legacy `core.sourceful.energy` / plaintext MQTT examples in the
  federation path with environment discovery and MQTT/TLS.
- Agree the FTW gateway type, versioned telemetry schema and source-lease
  fields across FTW, Novacore and the app.
- Add shared JSON fixtures to both repositories.
- Define Zap local-control v2: per-DER action capabilities, expiring commands,
  execution status, idempotent release and a fail-closed protocol version.

Exit: FTW publishes a simulated site to testnet and the same fixtures pass in
FTW and Novacore CI.

### Phase 1 — read-only mobile MVP

- Add the Bifrost session client, QR code and Sourceful app deep link; do not
  introduce a parallel FTW-specific authentication service.
- Add **Settings → Integrations → Sourceful Energy** status, pair, retry and
  revoke UI in FTW. Do not place federation in the required first-run flow.
- Offer Zap-anchored onboarding when one local Zap is verified. If several are
  present, require the owner to choose; if none is present, show the standalone
  path explicitly.
- Use the approved token transiently to automate the existing gateway claim
  and device provisioning, then erase it. Reconcile devices automatically and
  remove the `nova-claim` CLI requirement from the consumer journey.
- Show live meter/PV/battery/EV/V2X, health and last-seen in the app.
- Keep all control local and label the integration read-only.

Exit: a clean FTW install can be paired, viewed and revoked without CLI work;
a Zap-backed install joins the Zap's existing site without sharing credentials;
internet loss does not affect local FTW operation.

### Phase 2 — source reconciliation

- Implement physical-device matching and active telemetry-source leases.
- Make Zap direct the initial source for overlaps on a Zap-anchored site and
  add the Zap-direct versus FTW-coordinated handover to the app.
- Add automatic, idempotent reconciliation for driver add/remove/rename.
- Add source and freshness diagnostics to both FTW and the app.

Exit: a site with both physical Zap and FTW produces one canonical reading per
DER, including during source handover and reconnect.

In parallel, implement and hardware-test Zap local-control v2 in firmware and
its FTW adapter. Do not activate it merely because the legacy `202 queued`
routes are present.

### Phase 3 — consented control intents

- Add the single-controller lease and signed, expiring command envelope.
- Subscribe FTW to the control-intent stream and route accepted intents
  through normal dispatch/safety code.
- Add acknowledgement, audit and revocation views.
- Start with strategy selection and temporary manual hold; add EV/V2X only
  after their policy constraints are represented end to end.

Exit: every remote action is attributable, replay-safe, bounded in time,
visible locally and unable to bypass FTW safety.

### Phase 4 — production hardening

- Test token rotation, clock skew, offline queues, reconnect storms and
  revoked sessions.
- Load-test fleet reconnects and five-second telemetry.
- Complete privacy review, data export/deletion and support runbooks.
- Pilot on employee sites, then Zap+FTW mixed sites, before general release.

Exit: signed compatibility matrix, rollback plan, dashboards and on-call
ownership exist across FTW, Novacore and the mobile app.

## Repository work split

| Repository | Primary work |
|---|---|
| FTW | Dedicated opt-in integration UI, Zap discovery and attachment proof, standalone claim, automatic reconciliation, source-aware MQTT/TLS publisher, later command validation |
| Zap firmware | Stable identity/signing contract plus versioned per-DER local-control capabilities, leased commands, execution acknowledgements and verified default-mode fallback |
| Novacore | Zap-anchored coordinator attachment, standalone FTW claim, gateway roles, durable device mapping, source leases and command/audit stream |
| Sourceful Energy app | Approve attachment to an existing Zap site or create/select a standalone site, source selection, FTW status, revoke UX and later control intents |
| Shared contract | Versioned schemas, conformance fixtures, compatibility matrix and release gates |

## Release gates

- No user JWT or device credential is stored in FTW config YAML.
- No Zap private key, MQTT credential or gateway identity is copied into FTW.
- Adding a Zap driver alone produces no Sourceful cloud write or auth session.
- Zap-anchored onboarding resolves to the Zap's site and creates a separately
  revocable FTW coordinator membership.
- Pairing starts from the FTW LAN UI and approval/revocation works from the
  Sourceful Energy app. No dedicated FTW remote-access portal is required.
- Telemetry stays in site convention without a second sign flip.
- Physical Zap overlap cannot double-count.
- Revocation stops cloud publishing and command intake without restarting FTW.
- Remote commands cannot bypass local clamps, watchdog or default mode.
- A Sourceful/Novacore outage leaves the house operating normally.
