# Terra Draw, vendored

Terra Draw 1.32.2 and terra-draw-maplibre-gl-adapter 1.4.1, MIT. See
`LICENSE` (the npm packages ship no license file; the text comes from the
project repository, which covers both packages).

Vendored rather than pulled from a CDN, for the same reason as
`/vendor/maplibre/` next door: the box UI must not execute third-party JS
from a CDN, and the PV-array drawing tools have to load even when the
gateway cannot reach the internet.

| File | Why |
|---|---|
| `terra-draw.umd.js` | the drawing engine (self-contained UMD build) |
| `terra-draw-maplibre-gl-adapter.umd.js` | binds Terra Draw to a MapLibre map |

To upgrade: bump the versions in this file, replace the two files from the
packages' `dist/`, and update `web/settings/tabs/weather.js` if the layout
changed. `web/terra-draw-vendor.test.mjs` pins the contract.
