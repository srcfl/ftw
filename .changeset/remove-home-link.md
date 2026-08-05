---
"ftw": major
---

Home Link is removed. Remote access is the FTW app.

**What you lose.** Every passkey enrolled with Home Link stops working, and
there is no migration: the box is no longer a WebAuthn relying party, so those
credentials now verify against nothing. The browser route is gone with them —
your `<name>.home.sourceful.energy` address, the page it served and the relay
behind it no longer exist. The local UI's Remote tab and the
`/api/home-link/*` endpoints are gone. If you were reading your house from a
browser away from home, that stops working on this release and the app is the
only way back.

**What you get.** One remote path instead of two. The app pairs by scanning the
box's QR code, which carries the box's key optically so no server ever sees it,
and afterwards reconnects on its own key — a photographed code cannot pair a
second phone. The relay holds no keys and cannot read a frame, and the name the
box joins under changes every hour, so the relay operator cannot tell which
household is online or follow one across the day. Nothing about local control,
setup, history or fallback planning changes.

**What is not there yet.** The box mints its pairing code at boot but nothing
renders it in the local UI, so pairing a phone still needs the terminal.
Revoking one phone means rotating the rendezvous secret, which re-pairs the
whole household; per-device revocation is owed.

An existing `home_link:` block in your `config.yaml` is ignored — the box boots
without you editing anything. Home Link's rows in `state.db` are left where
they are and authorise nothing. `ftw home-link-adopt` is now
`ftw gateway-identity-adopt`; it does the same thing to the same files, and the
machine identity it binds is untouched by any of this.
