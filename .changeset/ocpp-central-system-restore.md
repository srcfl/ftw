---
"ftw": minor
---

Add OCPP 1.6J support so EV chargers connect to FTW directly instead of through
a vendor cloud. An OCPP charger needs no driver: the protocol is vendor-neutral,
so one server in core handles every charger that speaks it.

Chargers dial FTW rather than the other way round, so there is nothing to add
under `drivers:`. A charge point becomes a device on its first BootNotification,
keyed by the last segment of the URL it connected to, and dispatch treats it
like any other EV reading.

This reinstates `go/internal/ocpp`, retired as unused in #578, and wires it into
the process behind a new `ocpp` config section. It matters because Charge Amps
has no FTW driver at all and every current model speaks OCPP, while Easee and
Zaptec can be commissioned once through their vendor portal and then run with no
cloud in the runtime path.

The server is off by default. Enabling it requires a username and password, and
FTW refuses to start without them: the OCPP library builds its listen address
from the port alone, so the socket is reachable on every interface and basic
auth is the only gate. Keep the port closed at your router.

Phase 1 is read-only — chargers are metered and monitored, but FTW does not yet
start, stop or throttle a charge.
