# Hardened relay + device-key remote access — design

**Status:** direction approved by Fredrik (2026-06-05). This spec firms it up for
review before implementation. Build with the home-route's rigor: slices → tests →
**Codex security GO** before anything goes live.

## The principle (non-negotiable)

> The relay must **never** let an unauthenticated internet party reach, ping,
> fetch from, or probe a home Pi. The **only** path to a Pi is a **device-key born
> on that Pi's own LAN**. The relay is a dumb, hard broker: it serves the sign-in
> bootstrap *itself*, and forwards to the Pi *only* signaling proven by a
> device-key it can verify.

Confidentiality of owner data already meets a hard stop today (every `/api/*` →
403, double-enforced at relay + Pi gate). This redesign closes the remaining
**contact** surface: today the relay *forwards anonymous GETs* (the static shell +
`/api/identity`) to the Pi, so an anonymous visitor can cause the relay to touch a
home Pi. That ends here.

## What reaches a Pi today vs after this change

| Surface | Today (v1) | After |
|---|---|---|
| Owner data/control `/api/*` | 403 at relay + Pi gate (no leak) | unchanged — 403, only over an authed channel |
| Static app shell `GET /` etc. | **forwarded to the Pi** | **served by the relay** from its own embedded copy — Pi not touched |
| `GET /api/identity` | **forwarded to the Pi** | **served by the relay** from its pinned `-home-pubkey` — Pi not touched |
| Signaling `POST /signal/.../offer` | forwarded to the Pi (capped, anonymous) | forwarded **only** if it carries a valid **device-key proof**; anonymous → rejected at the relay, Pi never sees it |
| The P2P channel | unauthenticated at open, owner auth over it | opened only by a device-key-proven peer; owner auth (or silent device-key PoP) over it |

Net: **no anonymous internet request ever reaches the Pi.** The Pi is reachable
only by a browser that holds a device-key minted on the Pi's LAN.

## Architecture

### 1. The relay serves the bootstrap itself (closes the GET surface)
- `go:embed web/` into `ftw-relay`; the home host's static GETs are served from
  the embedded bundle, **not** forwarded to the Pi. (The bundle is the same app
  for every home host — per-Pi difference is only identity + data.)
- `GET /api/identity` is answered by the relay from the **pinned `-home-pubkey`**
  it already holds (+ site_id, curve). The Pi is not contacted. (The browser still
  TOFU-pins this and verifies the Pi's *signed DTLS fingerprint* over the channel,
  so a lying relay still can't MITM — the anti-MITM proof is unchanged.)
- Version coupling: the relay bundle is built from the same repo at release time,
  so `relay@X.Y.Z` ships the `X.Y.Z` web. The release pipeline already builds both.

### 2. The device-key gates signaling (closes the contact surface)
- **Mint (LAN-only, once per device):** during LAN enrollment (passkey + LAN PIN,
  the Pi serving enroll directly), the browser generates a **non-extractable**
  ECDSA P-256 device keypair (WebCrypto, stored in IndexedDB). The Pi pins the
  device **public** key on `trusted_devices` (new `device_pubkey` column), bound
  to the wallet and the enroll assertion so it can't be swapped.
- **Publish:** the Pi pushes its set of trusted device-pubkeys to the relay on
  `/me/register` (alongside the site key). The relay stores them per-site. These
  are **public** keys — same posture as the site key the relay already holds.
- **Gate (every channel open):**
  1. Browser `GET /signal/{site}/challenge` → fresh nonce.
  2. Browser signs the nonce with the device key.
  3. Browser `POST /signal/{site}/offer` with `{offer, device_pubkey, sig}`.
  4. Relay verifies `device_pubkey ∈ registered set` **and** `sig` over the nonce
     (ES256 — the relay already does ES256-verify for `/me/register`, so this is
     the same capability, not new trust). Valid → forward to the Pi's mailbox.
     Invalid/absent → **reject; the Pi never sees it.**
- First-ever remote contact is therefore impossible without a prior **LAN**
  enrollment. Remote-first onboarding is intentionally unsupported (more secure;
  matches the existing LAN-PIN-first model).

### 3. The device-key also mints the session (silent re-auth)
Over the opened channel the browser proves the device-key to the **Pi** (PoP over
a Pi challenge / the DTLS exporter). The Pi verifies against the pinned
`device_pubkey` → mints the owner session **silently** (no passkey). So "remember
the browser / stay signed in" falls out for free, and it does **not** weaken the
hard stop: the device-key *is* the owner's credential (LAN-born, wallet-bound).
- **Step-up:** sensitive actions (add/remove device, remove credential) always
  require a fresh passkey, regardless.
- **Revoke:** removing a device drops its `device_pubkey` on the Pi **and** the
  relay's set → that device can no longer sign in *or even signal*; it must
  re-enroll on the LAN.

## Threat model (surface by surface)
- **Anonymous internet visitor:** gets the static shell + identity from the
  *relay*; cannot signal (no device-key → relay rejects); **cannot reach the Pi at
  all.** ✓
- **Stolen device-key:** non-extractable (can't be exfiltrated from the browser);
  revocable; step-up for sensitive actions.
- **LAN attacker:** the LAN remains the trust boundary (existing LAN-bypass model,
  unchanged) — that's where device-keys are minted.
- **Compromised relay:** holds only public keys; can DoS or pass a forged offer
  through, but the channel is E2E and the session needs the device-key/passkey, so
  it gets **no data** — same confidentiality posture as today (availability was
  never the relay's to guarantee).

## Build slices (each its own reviewable step)
1. **Relay serves the shell** — `go:embed web/`; home host static from the bundle,
   not forwarded. Smoke: anonymous `GET /` hits the relay (Pi receives nothing).
2. **Relay serves `/api/identity`** from the pinned key. Smoke: identity returned
   with the Pi powered off.
3. **Device-key mint + pin** — browser keygen + IndexedDB; Pi `device_pubkey`
   column + bind at enroll/finish.
4. **Relay signaling gate** — Pi publishes device-pubkeys on `/me/register`; relay
   `/signal/{site}/challenge` + verify on `/offer`; reject unproven.
5. **Silent session via device-key PoP** over the channel → Pi mints session.
6. **Revocation** end-to-end (Pi + relay drop the key).
7. **Tests + Codex security GO** — adversarial pass on every surface in the table
   above before it goes live.

## Open question for Fredrik (the one trade-off)
The signaling gate makes the relay verify a device-key signature, so the relay
learns the set of device **public** keys per site (it already holds the site
pubkey, so this is the same kind of data — public, non-secret). The alternative —
keeping the relay totally blind and letting the Pi cheap-reject anonymous offers —
would leave anonymous offers *reaching* the Pi (bounded + leak-free, but a
contact). This spec chooses the **hard** option (no anonymous contact) at the cost
of the relay holding public device-keys. Confirm that trade is the one you want.
