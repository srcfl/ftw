# Seamless home-route UX — design sketch (deferred, post-initial-deploy)

**Status:** designed + chosen direction locked; build DEFERRED until after the
initial P2P-only home route is deployed and proven. This is the "feels like magic"
polish layer on top of the working transport.

**Vision (Fredrik):** one URL — `home.fortytwowatts.com` — one Touch ID, home or
away, identical, "just works." Touch ID should be the *exception*, not the rule.

## Chosen direction (decided 2026-06-05)
1. **Inline login** — the dashboard IS the door. No separate `login.html` redirect
   (which today spawns a fresh channel with no session → re-auth). The dashboard
   shell opens the channel and unlocks itself.
2. **Discreet transport indicator** — a small dot (`● direct` / `◐ relayed` /
   `◌ connecting`); tap reveals "Direct & encrypted between your browser and your
   Pi. The relay sees nothing." Invisible by default — magic shouldn't show.
3. **Device-key proof-of-possession (device-key)** — truly silent re-auth: after a
   device is enrolled once (Touch ID), the channel itself proves the device, so
   new pages / reconnects / visits re-authenticate WITHOUT Touch ID.

## State machine (channel ↔ device-PoP ↔ session ↔ UI)
```
   open home.fortytwowatts.com
        │
        ▼
   CONNECTING ──(Pi not registered)──► OFFLINE ──auto-retry──┐
        │  signed-fingerprint verified                        │
        ├──(device key present)──► device-PoP ──ok──► AUTHED ◄─┐
        │                                                      │
        └──(no device key)──► NEW DEVICE ─Touch ID─► enroll ─► AUTHED
                                                              │
   AUTHED ──channel drops──► RECONNECTING ──silent PoP─────► AUTHED
   AUTHED ──sensitive action──► STEP-UP ─Touch ID─► (do it)─► AUTHED
```

## Wireframes
```
CONNECTING                         NEW DEVICE (first time / key evicted)
┌────────────────────────┐         ┌────────────────────────┐
│ ⚡ 42W           ◌      │         │ ⚡ 42W                 │
│     Reaching home…     │         │  👋 Unlock your home   │
└────────────────────────┘         │     [ 👆 Unlock ]      │
                                   └────────────────────────┘
AUTHED                             RECONNECTING (subtle overlay)
┌────────────────────────┐         ┌────────────────────────┐
│ ⚡ 42W              ●   │         │ ⚡ 42W              ◌   │  dashboard stays;
│ 2.4 kW · Battery 78%   │         │ (frozen, reconnecting) │  dot pulses
└────────────────────────┘         └────────────────────────┘
OFFLINE: "Reaching home…" → after N min "Your home hasn't checked in for N min."
STEP-UP: a focused Touch ID prompt scoped to the one sensitive action.
```

## Device-key-PoP protocol (sketch)
- **Enroll (once per device, at first Touch ID):** browser generates a
  non-extractable device keypair (WebCrypto, IndexedDB); the Pi pins the device
  PUBLIC key against the wallet `W` (new `device_pubkey` column on
  `trusted_devices`), bound to the WebAuthn enroll assertion so it can't be swapped.
- **Per channel open:** after the signed-fingerprint (Pi proves *itself*), the
  browser proves the *device* — signs a fresh server challenge (or the DTLS
  exporter / a nonce) with the device private key. The Pi verifies against the
  pinned device pubkey → mints the owner session (proof-of-possession). No WebAuthn
  needed in the common case.
- **Touch ID frequency — OPEN SUB-DECISION for build time:**
  - **(A) once per visit** — passkey (PRF) unwraps the device key into memory on
    arrival; silent for all channels/reconnects during the visit; Touch ID again
    next visit. Balanced. *Recommended default.*
  - **(B) once per device, then never** — device key usable silently
    (trusted-device); revoke the device to undo. Most magic; trusts the device.
  - Recommend: **A by default + a "trust this device" toggle for B.**
- **Step-up:** sensitive actions (remove device/credential, add device) always
  require fresh Touch ID, regardless of A/B.
- **Revocation:** the dashboard lists devices; revoking removes the pinned device
  key → that device's next channel fails PoP → must re-enroll (Touch ID).

## Build scope (it's a real feature, not pure polish)
- **Browser:** device keypair + enroll + the PoP handshake on channel open; the
  inline-login dashboard shell + the state machine; the discreet indicator.
- **Pi:** pin `device_pubkey` on `trusted_devices`; PoP-verify on channel open →
  mint session; step-up gating on sensitive endpoints; device revocation.
- This is the "device-key PoP session" the v1 spec
  (`2026-06-04-p2p-only-home-route-v1-design.md`) explicitly DEFERRED — now the
  agreed next layer, to build after the v1 transport is deployed and proven.

## Also captured for later
- Remote add-a-device (v2): one-time high-entropy token in the URL `#fragment`
  (never reaches the relay) to enroll a fresh device while away.
- iOS/Safari IndexedDB eviction (ITP ~7d) re-enroll UX (falls into NEW DEVICE
  gracefully).
- `whoami` over the channel so the sign-out affordance shows on the public route
  (today it's a plain GET, so the button can stay hidden remotely — UX nuance).
