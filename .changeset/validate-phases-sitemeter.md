---
"ftw": patch
---

Config validation now rejects `fuse.phases` above 3 (previously silently truncated to 3 by the dispatch freshness gate, understating fuse limits) and more than one driver with `is_site_meter: true` (previously the first match silently won). Both were already misconfigurations with surprising behavior; they now fail loudly at load time with a clear message instead.
