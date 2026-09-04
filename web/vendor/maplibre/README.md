# MapLibre GL JS, vendored

MapLibre GL JS 6.7.0, BSD-3-Clause. See `LICENSE.txt`.

Vendored rather than pulled from a CDN, for the same reason as
`/vendor/three/`, `/vendor/ace/` and the Leaflet copy this replaced: the box
UI must not execute third-party JS from a CDN, and the weather map has to
load even when the gateway cannot reach the internet.

v6 ships ESM only and is code-split, so this is the entry module plus the
two files it loads by relative URL, taken from the `maplibre-gl` npm package
(`dist/`):

| File | Why |
|---|---|
| `maplibre-gl.mjs` | the map library (ESM entry; no default export in v6) |
| `maplibre-gl-shared.mjs` | code-split chunk the entry imports |
| `maplibre-gl-worker.mjs` | the worker the entry spawns for tile parsing |
| `maplibre-gl.css` | default controls, marker and popup styles |

Dev builds and source maps are not vendored. To upgrade: bump the version in
this file, replace the four files from the npm package's `dist/` (and
`LICENSE.txt` from its root), and update `web/settings/tabs/weather.js` if
the file layout changed. `web/maplibre-vendor.test.mjs` pins the contract.
