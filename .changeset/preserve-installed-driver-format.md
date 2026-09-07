---
"ftw": patch
---

Keep each managed driver's metadata format through restarts and rollback. A
Device Support package now requires its verified runtime policy even when
control is not selected, its repository is removed, or its envelope is missing.
Verified legacy installs retain their normal autonomous default. Older installs
with no recorded format need matching verified metadata; restore their repository
or reinstall them if that metadata is unavailable.
