---
"forty-two-watts": patch
---

Dashboard: route every state-changing owner/CONTROL `/api/*` call through the
fail-closed strict transport (FIX-B).

The owner-access ceremony pages already rode `ownerFetch` (strict P2P, fail-closed
503 on a public / `/me/<site>` origin with no DataChannel, raw relay fetch ONLY on
a genuine LAN). This extends the SAME behaviour to the dashboard's classic scripts
+ web components so a state-changing call's body + owner session can never traverse
the untrusted relay in cleartext on the public home route:

- `p2p.js` now exposes `window.ownerFetch = p2pFetchStrict` — the dashboard's one
  shared, fail-closed entry point (not a fork; the identical function the ceremony
  pages use).
- Converted the remaining bare state-changing calls: config save / restart
  (`settings.js`), self-tune start/cancel (`models.js`), load-twin profile switch
  + PV/load twin reset (`twins.js`), MPC replan (`plan.js`), EV-charger probe /
  Tesla verify / driver test (`settings/tabs/devices.js`), battery + PV manual-hold
  install/clear, pair start/abort, notification test, self-update trigger, and the
  update-badge snapshot-delete + skip/unskip/rollback/update POSTs.
- Read-only GETs stay plain (no body; the relay strips the owner cookie on the
  P2P-only route, so they can't leak).
- New web tests: a static guard that fails if any public-route module bare-fetches
  a state-changing `/api` call, plus an `ownerFetch` wiring test. The tier-2 docker
  e2e gains a `window.ownerFetch` fail-closed step + a relay-leak tripwire.
