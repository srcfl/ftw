---
"ftw": minor
---

Restore the OCPP 1.6J Central System so EV chargers can connect to FTW directly
instead of through a vendor cloud.

`go/internal/ocpp` was retired as unused in #578. Bringing it back is the only
integration path for several common chargers: Charge Amps has no FTW driver at
all and every current model speaks OCPP 1.6J, while Easee and Zaptec can both be
commissioned once through their vendor portal and then run against a local
central system with no cloud in the runtime path.

The package is restored unchanged and is not yet wired into the process, so this
release carries no behaviour change on its own.
