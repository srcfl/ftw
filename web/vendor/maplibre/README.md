# MapLibre GL JS, vendored

MapLibre GL JS 6.9.0, BSD-3-Clause. See `LICENSE.txt`.

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
`LICENSE.txt` from its root), refresh the hashes below, and update
`web/settings/tabs/weather.js` if the file layout changed.
`web/maplibre-vendor.test.mjs` pins the contract.

## Provenance

Taken unmodified from `maplibre-gl-6.9.0.tgz` on the npm registry, whose
signed integrity is

    sha512-vFMwMK0Zs+NM/rOMSdtu8bO30DIexhBEVi5KC6f70/XtI+L/K2wC3LsDCAXFZ4s8ik5gAuDugfCNbpllpdJ9bA==

To re-verify without trusting this tree: download the tarball
(`npm pack maplibre-gl@6.9.0` or
`curl -O https://registry.npmjs.org/maplibre-gl/-/maplibre-gl-6.9.0.tgz`),
check that integrity, extract, and compare SHA-256 per file:

| File | SHA-256 |
|---|---|
| `maplibre-gl.mjs` | `0197eee4c6e8fd5d8b68f5f94a23e1c77e0c8fd054c377a280bcd08117aadfc4` |
| `maplibre-gl-shared.mjs` | `0aa7432c2d4644e8f46158cc99d0a05e0bf5db98afe6ded2b258a68dcc91bb90` |
| `maplibre-gl-worker.mjs` | `836616cb05bef91f9c7f3920ee2aeef648b81cee0645a037e70365286009ac98` |
| `maplibre-gl.css` | `8e2dbbab312dc57656fbb76e9fa5308c75c9d7c7ba5808a7d55bcdb64cc813fa` |
| `LICENSE.txt` | `ee5fc05a0677eaf69601d2c7db0d9ecd6cc27c3abc1d0733bc9ed34707cf8ef2` |
