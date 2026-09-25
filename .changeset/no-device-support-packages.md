---
"ftw": patch
---

FTW no longer reads Device Support driver packages. Drivers come from the
release and from FTW's own signed driver channel, as before. A site that still
lists a Device Support package source keeps starting: the source is removed
from its settings with one warning, and a package that was active is switched
off at startup so the release's own driver runs. A driver `control` opt-in has
no effect any more; it is removed with a warning instead of stopping that
driver from starting. The driver diagnostics report now names a driver
installed from the channel as `managed`.
