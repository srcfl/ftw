---
"ftw": patch
---

The weather map's MapLibre GL JS is vendored on the box instead of loaded
from unpkg, so the location picker works without internet access and the UI
executes no third-party CDN JavaScript — the same policy that vendored
Leaflet, whose now-unused copy is removed.

Static assets are also served with pinned Content-Types instead of whatever
the host OS's MIME table says: on a Windows host whose registry maps `.mjs`
to text/plain, the browser (correctly, under `nosniff`) refused the vendored
module and the map died with "failed to fetch dynamically imported module".
