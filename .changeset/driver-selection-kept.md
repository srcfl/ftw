---
"ftw": patch
---

A driver version picked under Settings → Devices is no longer lost when a Core
update brings a newer copy of that driver. The release's newer driver runs, and
if the update falls back or you run `ftw rollback`, the older release runs your
picked version again. Going back from a newer driver version to an older one
now counts as a choice and stays across updates. `ftw status` no longer credits
a driver version to a device that runs your own file of the same name from
another directory.
