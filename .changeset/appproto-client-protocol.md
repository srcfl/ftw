---
"ftw": patch
---

The box can now speak the FTW web app's client protocol. A new `appproto` package handles the handshake, the telemetry stream, commands and the dispatch plan: version negotiation degrades an old app to a frozen field subset instead of refusing it, telemetry rides a fixed-size lane that sends a tick even when nothing changed, source freshness travels in deltas so a device that goes quiet mid-session is visible immediately, and commands are separated into a receipt from the dispatcher and a result read back from the driver. Shared names — field ids, capabilities, scopes and error codes — are generated from `contract/registry.yaml` rather than typed out, and a test fails when the two drift. Nothing is wired into the running process yet; this is the protocol layer on its own.
