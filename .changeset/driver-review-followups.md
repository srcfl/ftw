---
"ftw": patch
---

A driver install that Core could not retire at the start of a new release is
retried at the next start instead of shadowing the release's driver until the
following release. `ftw status` no longer credits a driver version to a device
that runs an operator's own file of the same name from another directory.
