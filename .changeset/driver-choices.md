---
"ftw": patch
---

An older driver version chosen on purpose now stays after a Core update. Only
a newer version installed early is replaced once a release catches up. "Use
bundled" works again for drivers whose bundled file spells the id differently
from the channel (easee-cloud, easee_cloud), and installing such a driver
reaches the running device without a restart. `ftw status` names the release's
own version beside an override. The beta driver channel honours
`device_repository.enabled: false`, and Update Center no longer offers an older
beta driver as an update.
