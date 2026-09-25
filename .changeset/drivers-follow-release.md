---
"ftw": patch
---

A driver installed from the driver channel no longer keeps running after an
update brings a copy of the same or a newer version. At the first start of a new
release, Core retires such an install and runs the release's driver. An older
version chosen on purpose stays until the next release. `ftw status` now lists
the version each configured driver runs and names any driver that is not the
release's own copy.
