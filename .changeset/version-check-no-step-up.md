---
"ftw": patch
---

An owner's phone can now ask the box whether a newer Core exists
(`GET /api/version/check`) without Face ID, so the app can show the newer
version and its notes. Installing it (`POST /api/version/update`) still asks
for Face ID, and a viewer still cannot read the check.
