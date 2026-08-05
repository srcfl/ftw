---
"ftw": minor
---

Pair a phone from Settings → FTW app.

The box could mint an enrollment payload but nothing showed it, so there was no
way to pair a phone at all. There is now a button that mints a single-use code
and draws it as a QR, and a local-only endpoint behind it.

The code is minted on request rather than shown continuously: a QR left on a
screen for a week is a QR that everyone who walked past has photographed. It is
never offered as copyable text either — the payload carries the pairing code and
the rendezvous secret, so the camera is the only path it should take.
