# ADR 0006: The FTW app uplink replaces Home Link

- Status: accepted and implemented
- Date: 2026-08-05
- Supersedes: [ADR 0005](0005-outbound-site-link.md) in full. Home Link's
  packages, HTTP endpoints, config block, relay binary, web pages and deploy
  script are deleted, not deprecated
- Preserves: ADR 0003's retirement of the earlier remote-access implementation.
  Nothing from it returns

## Context

Home Link worked. A person enrolled a passkey from the LAN UI, the box held an
outbound TLS connection to `uplink.home.sourceful.energy`, and a browser at
`<adjective>-<color>-<animal>.home.sourceful.energy` read four bounded views.

It was also the wrong shape for the product. Three things settled that.

**The remote surface is an app, not a page.** What people want on a phone is
their house at a glance, live, from a cold start, offline-tolerant, with push.
Home Link's browser page could not be that: every open was a fresh WebAuthn
ceremony against the box, nothing was cached, and there was no home screen to
return to. Building the app on top of Home Link would have meant a second
protocol next to it, not a use of it.

**The box was a WebAuthn relying party.** That put an authenticator ceremony,
a credential store, a signature counter, revocation tombstones and an
on-disk fail-closed fence into a box whose actual job is to run a house at
1 Hz. It also pinned the relying-party ID to a Sourceful domain, which made the
DNS name part of the security boundary.

**The relay learned the household.** A stable per-gateway alias in DNS and a
stable route handle on the relay meant the relay operator could see which house
was online, for how long, and from where — through perfect encryption.

The app protocol (`go/internal/appwire`, `appproto`, `appuplink`,
`appenroll`) answers all three. Running both would have meant two remote paths,
two pairing stories and two things to get right at 1 Hz, for one product.

## Decision

**Home Link is removed, and the app uplink is the only remote path.**

1. **Trust reaches the app optically.** The QR code carries the box's Noise
   static public key, a single-use pairing code, a LAN hint and the rendezvous
   secret, all in a URL fragment. A fragment is never sent in an HTTP request,
   so a hostile or compelled `app.ftw.energy` can deny service but cannot
   present itself as this box.

2. **Noise IK replaces WebAuthn on the box.** The app authenticates the box by
   the static key it pinned from the QR code; the box authorises the app by the
   pairing code in handshake message 1, then remembers that app's static key so
   a reconnect needs no new code. The box is no longer a relying party, stores
   no credential public keys, and runs no ceremony. A passkey still exists, but
   in the app and against `app.ftw.energy` — it gates enrollment and privileged
   commands, not reading.

3. **The relay is `wss://relay.ftw.energy` and holds no keys.** It forwards
   encrypted frames and cannot read or attribute them. Up to four sessions
   share one uplink; the relay broadcasts, and the box lets the AEAD decide
   which session a frame came from, because asking the relay would mean the
   relay had to know.

4. **The box's name on the relay rotates hourly.** The join handle is derived
   per epoch from the rendezvous secret with HKDF-SHA256, so the relay operator
   cannot follow one household from hour to hour. There is no DNS alias, no
   three-word host, and nothing stable for the relay to key a household on.

5. **Authority is unchanged, which is the point.** A command carries an expiry
   and preconditions; core revalidates against fresh state before acting. Site
   mode changes go through `control.ApplyMode` from every door. The planner's
   output still never reaches hardware directly, and stale site-meter data
   still stops dispatch.

## What is lost

Stated plainly, because this makes remote access unusable for anyone who had
Home Link working:

- **Every enrolled Home Link passkey stops working.** There is no migration.
  The box is no longer a relying party, so the credentials verify against
  nothing. Their rows are left in `state.db` and authorise nothing.
- **The browser route is gone.** `<name>.home.sourceful.energy` and the
  standalone `home-link.html` page no longer exist, and neither does the relay
  that served them. Remote access is the app or nothing.
- **`GET /api/home-link/status`, `POST /api/home-link/pairing` and
  `POST /api/home-link/passkeys/revoke` are gone**, along with the LAN UI's
  Remote tab.
- **There is no pairing surface on the box's own UI yet.** The QR payload is
  minted at boot but nothing renders it. Until that lands, a site that turns on
  `app_link` cannot pair a phone from the local UI.
- **Revocation is coarser.** Home Link could revoke one passkey and leave the
  others. Today the box can rotate the rendezvous secret, which moves every
  paired phone at once. Per-device revocation is owed.
- **WebAuthn as an on-box capability is gone**, including the signature-counter
  clone check and the emergency fail-closed markers beside `state.db`. If a
  future feature needs a local relying party, it starts from nothing.

## What is kept, and why

- **`go/internal/gatewayidentity` stays.** It is the machine's identity, not
  Home Link's: `nova-claim` uses it, and the hardware binding is what stops a
  box with incomplete sidecar state from quietly minting a replacement
  `nova.key` and losing its Nova claim. The `home-link-adopt` subcommand is
  renamed `gateway-identity-adopt`; the `nova.key.home-link.{state,json}`
  sidecar names are not, because they exist on every adopted box.
- **The three-word name stays.** It is a display label derived from the stable
  gateway ID, and existing Sourceful Energy Gateways must keep deriving the
  same one. `RouteHandle` does not stay: it was an address for a relay that no
  longer exists.
- **The Home Link credential tables stay in `state.db` on boxes that have
  them.** They are dropped from the migration list, so a new box never creates
  them, and existing rows are left alone. A migration that deletes rows is the
  one kind that cannot be undone if this turns out to have been wrong.
- **An existing `home_link:` block in `config.yaml` is ignored, not rejected.**
  Config parsing is non-strict and a test covers it, so an upgraded box boots
  without anyone editing a file first.

## Consequences

- Local control, setup, history and fallback planning are untouched by any of
  this. A box with `app_link.enabled: false` behaves exactly as before.
- One remote path means one thing to threat-model, one pairing story and one
  place freshness can be got wrong.
- The relay is now a dumb frame forwarder. It can deny service and it can count
  connections per handle for an hour. It cannot identify a household, read a
  reading, or name a device.
- Per-device revocation and an on-box pairing surface are open work, and the
  app uplink is not finished until they land.
