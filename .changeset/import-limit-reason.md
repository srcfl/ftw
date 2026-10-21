---
"ftw": patch
---

When the hourly import limit holds a car back, FTW now says so instead of blaming the main fuse. The charger's reason is `import_limit` ("Limited by the hourly import limit") whenever this hour's import budget leaves less room than the fuse, and a battery boost stopped by that budget says "Hourly import limit stopped boost". `fuse_limit` now means the main fuse alone, and the web page says "Limited by the main fuse".
