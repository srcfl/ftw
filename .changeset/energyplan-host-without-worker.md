---
"ftw": patch
---

`make native-solver-check` no longer fails on a host the Energyplan bundle has
no worker for, such as a Mac when the bundle ships only the Linux workers. The
verifier still checks every bundled file and skips only the handshake there,
as it already did on other unsupported hosts.
