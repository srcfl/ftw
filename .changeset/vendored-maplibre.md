---
"ftw": patch
---

The Settings location picker moves from Leaflet to MapLibre GL JS 6.9.0,
vendored on the box: the map keeps the same OpenStreetMap raster tiles and
attribution, but the UI now executes no third-party CDN JavaScript and the
picker loads even when the gateway cannot reach the internet — the same
policy as `/vendor/three` and `/vendor/ace`. Leaflet's now-unused copy is
removed. If WebGL is unavailable the numeric latitude/longitude fields stay
authoritative, exactly as before.

Static assets are also served with pinned Content-Types instead of whatever
the host OS's MIME table says: on a Windows host whose registry maps `.mjs`
to text/plain, the browser (correctly, under `nosniff`) refuses the vendored
ES module and the map dies with "failed to fetch dynamically imported
module".
