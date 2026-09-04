---
"ftw": patch
---

Deriving a picked building no longer fails with "not found near this site"
in the two ways it could. The derive now searches where the picker searched:
the same (possibly dragged, unsaved) map coordinates are sent with the
request instead of silently using the stored site. And the derive's re-find
of the picked footprint now reaches as far as the picker's own search did —
it used to re-search with the 40 m LiDAR radius, so anything the 150 m picker
had found beyond that reach vanished on derive. The LiDAR tile lookup also
centres on the picked building rather than the pin. Verified live on the
reported case: a 522 m² barn picked via an unsaved pin now derives 2 arrays
from 2 roof planes.
