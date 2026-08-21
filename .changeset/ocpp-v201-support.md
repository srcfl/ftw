---
"ftw": minor
---

Serve OCPP 2.0.1 alongside 1.6J, so newer chargers connect without a driver too.

Each version listens on its own port. A charger picks its dialect during the
WebSocket handshake, before any message is sent, and the underlying library
keeps one message handler per listener — so a single port cannot serve both.
Set `ocpp.port_v201` to enable 2.0.1; leaving it unset keeps 1.6J only.

Only the message encoding differs. Both dialects share one charger map, one
telemetry path and one control path, so a 2.0.1 charger is metered, throttled
and paused exactly like a 1.6 one, and dispatch cannot tell them apart.

2.0.1 restructures the messages more than the names suggest: StartTransaction
and StopTransaction collapse into a single TransactionEvent, transaction ids
become strings, connector status loses its charging meaning, and meter samples
arrive inside transaction events as well as on their own. The new handler
normalises all of that back to the same charger state.

OCPP 2.1 is not supported. No production-grade Go implementation of it exists:
the library FTW uses covers 1.6 and 2.0.1 and has no 2.1 support, and the Go
projects that do claim 2.1 are early-stage validators and emulators rather than
servers. Adding it later is one more handler and one more listener; the
version-neutral core does not change.
