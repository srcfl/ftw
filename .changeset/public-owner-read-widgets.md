---
"forty-two-watts": minor
---

Route public dashboard price, history, savings, plan, and loadpoint reads through the strict owner P2P transport so they render on the relay-backed home route.

Remember multiple browser-local device keys for a synced passkey, so Safari on Mac can silent-login after an iPhone-enrolled passkey has already pinned its own key.

Add a Settings Access tab for the opt-in remote access switch, passkey management, remembered browser keys, and active session revocation.

Make `remote_access.enabled` the only remote-access opt-in; `FTW_REMOTE_ACCESS_ENABLED` no longer enables remote access by itself.

Keep the home-route sign-in gate retrying while the owner P2P channel is still opening, and give Chrome/WebRTC a longer handshake window before retrying.

Clear stale browser-local `ftw.p2p=off` toggles from older builds on the public home route, where strict owner traffic must use the P2P channel.

Generate or rotate to a persistent high-entropy owner `site_id`; guessable `site:<name>` routing is not preserved.

Stop bootstrapping fresh public browsers to `site:Home`; without a cached directory the home route now shows the setup/sign-in landing instead of guessing a site.

Stop serving the dashboard app bundle from the relay in multi-tenant mode. The relay now serves only a small remote loader/login allowlist; after the browser decrypts its directory, static app GETs are routed to the selected Pi while owner APIs remain P2P-only.

Publish `ftw-relay-web.tar.gz` as a minimal relay bootstrap asset so relay deploys no longer copy the Pi dashboard `web/` bundle.

Cache Pi-routed static dashboard assets privately in the browser and pause advanced-panel polling until Advanced is visible, improving repeat-load and remote-route responsiveness.
