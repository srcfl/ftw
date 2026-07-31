# Ace, vendored

Ace 1.44.0, BSD-3-Clause. See `LICENSE`.

Vendored rather than pulled from a CDN, for the same reason as `/vendor/three/`:
a gateway has to work without reaching the internet, and a driver editor is
most needed exactly when something is wrong.

Only the files the driver editor uses are here, taken from the `ace-builds`
npm package (`src-min-noconflict/`):

| File | Why |
|---|---|
| `ace.js` | the editor |
| `mode-lua.js` | Lua syntax highlighting |
| `theme-tomorrow_night.js` | dark theme, closest to FTW's palette |
| `worker-lua.js` | Ace's own Lua linter — it bundles luaparse |
| `ext-searchbox.js` | find and replace, which a 35 kB driver needs |

`worker-lua.js` is luaparse, a JavaScript implementation. The driver itself
runs under gopher-lua. The two can disagree, so this linter is for immediate
feedback while typing only — `POST /api/drivers/{id}/lint` asks the parser that
actually decides whether the driver will start, and that is what gates running
a draft.

## Updating

    curl -sL https://registry.npmjs.org/ace-builds/-/ace-builds-<version>.tgz -o ace.tgz
    tar xzf ace.tgz package/src-min-noconflict/{ace,mode-lua,theme-tomorrow_night,worker-lua,ext-searchbox}.js package/LICENSE
    cp package/src-min-noconflict/*.js package/LICENSE web/vendor/ace/

Tarball sha256 for 1.44.0:
`a8116a1ec64f7d99a0a0e6003b8dbd0fab158a18716f3520990927bb01d90d14`
