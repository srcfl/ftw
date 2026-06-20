# MyUplink OAuth + Heating View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix MyUplink heat-pump auth (issue #496) by replacing the unsupported `client_credentials` OAuth flow with the authorization-code + refresh-token flow the MyUplink developer portal actually supports, with a built-in in-app consent flow; and surface heat-pump telemetry in a dedicated dashboard view.

**Architecture:** MyUplink uses Azure-B2C authorization-code. A one-time browser consent (Go bootstrap endpoint builds the authorize URL, a callback handler exchanges the code) yields a `refresh_token` persisted into the driver's config block as a masked secret. The Lua driver runs `grant_type=refresh_token` at runtime, holding rotation in memory and persisting any rotated token via a new generic `host.persist_secret` capability so it survives restarts. The heating view reuses the existing `/api/series` + per-driver live-metrics endpoints.

**Tech Stack:** Go 1.26 (stdlib `net/http` method mux), gopher-lua 5.1 drivers, vanilla JS web UI (no framework), SQLite TS DB.

## Global Constraints

- No CGo; pure Go + embedded Lua. `go build` must stay a static single binary.
- Driver owns protocol logic; Go owns capabilities only (CLAUDE.md).
- New API handlers live in `api_<feature>.go`, never in `api.go`; only the one-line route registration goes in `routes()` (api/CLAUDE.md).
- Config writes go through the injected `Deps.SaveConfig` / `config.SaveAtomic`, never `os.WriteFile`; take `CfgMu` before mutating `Deps.Cfg`.
- Secrets are declared in the driver `DRIVER.config_secrets` block; masked on GET via `maskDriverConfigSecrets`, restored on POST via `restoreDriverConfigSecrets`. Never echo a secret into the DOM.
- Site sign convention unaffected (read-only telemetry; no power-math).
- UI follows `DESIGN.md`: theme tokens from `web/components/theme.css`, one amber accent, mono for labels/numerics, 1px hairlines, no Google Fonts, no hard-coded hex.
- `slog` for Go logging; `host.log(level,msg)` in Lua.
- Every user-visible change needs a changeset (`npx changeset`); new driver capability + new feature = **minor**.
- All GitHub / Discord text in English (chat may be Swedish).

---

## File Structure

| File | Responsibility |
|---|---|
| `go/internal/drivers/host.go` | Add `PersistSecret func(key, value string) error` field to `HostEnv`. |
| `go/internal/drivers/lua.go` | Register `host.persist_secret(key, value)` Lua primitive → `env.PersistSecret`. |
| `go/internal/drivers/registry.go` | Carry a `SecretPersister func(driverName, key, value string) error`; wire each driver's `HostEnv.PersistSecret` to a closure binding its name. |
| `go/cmd/forty-two-watts/main.go` | Provide the `SecretPersister` impl: under `CfgMu`, set `cfg.Drivers[i].Config[key]`, `config.SaveAtomic`. |
| `drivers/myuplink.lua` | Rewrite auth: `grant_type=refresh_token`; persist rotated token; `config_secrets={client_secret,refresh_token}`; clear "not connected" idle state. |
| `go/internal/drivers/myuplink_test.go` | Update driver test to the refresh-token flow + a rotation-persist assertion. |
| `go/internal/api/api_myuplink_oauth.go` | NEW: `/api/oauth/myuplink/start` + `/api/oauth/myuplink/callback` bootstrap. |
| `go/internal/api/api_myuplink_oauth_test.go` | NEW: state lifecycle + code-exchange + config-write tests. |
| `go/internal/api/api.go` | Register the two new routes in `routes()` only. |
| `web/settings/tabs/devices.js` | MyUplink fieldset: callback-URL hint, Connect button, connection badge; fix stale `client_credentials` copy. |
| `web/heating.js` | NEW: heating dashboard view — metric cards + history chart via `/api/series`. |
| `web/index.html` | Add the heating `<section>` host + script include. |
| `docs/myuplink-oauth.md` | NEW: operator setup walkthrough (portal app, callback URL, connect). |
| `docs/writing-a-driver.md` | Document `host.persist_secret`. |
| `drivers/myuplink.lua` header + `CLAUDE.md` driver notes | Reflect new auth. |
| `.changeset/<name>.md` | minor bump entry. |

---

## Task 1: `host.persist_secret` capability (Go)

**Files:**
- Modify: `go/internal/drivers/host.go` (HostEnv struct)
- Modify: `go/internal/drivers/lua.go` (register primitive)
- Modify: `go/internal/drivers/registry.go` (wire per-driver closure)
- Modify: `go/cmd/forty-two-watts/main.go` (provide persister impl)
- Test: `go/internal/drivers/lua_persist_test.go` (new)

**Interfaces:**
- Produces: `HostEnv.PersistSecret func(key, value string) error` (nil → primitive returns `"capability not granted"`). Lua: `host.persist_secret(key, value) -> ok, err`.
- Produces: `Registry` gains `SecretPersister func(driverName, key, value string) error`; set by `main.go` before `Add`.

- [ ] **Step 1: Write the failing test** — `lua_persist_test.go`: a driver that calls `host.persist_secret("refresh_token", "RT2")` in `driver_poll`; inject a `PersistSecret` capturing the call; assert key/value received.

```go
func TestHostPersistSecret(t *testing.T) {
    tel := telemetry.NewStore()
    var gotKey, gotVal string
    env := NewHostEnv("dummy", tel)
    env.PersistSecret = func(k, v string) error { gotKey, gotVal = k, v; return nil }
    src := `function driver_init(c) end
            function driver_poll() local ok = host.persist_secret("refresh_token","RT2"); return 1000 end`
    d := newLuaDriverFromString(t, src, env) // helper mirroring existing inline-source tests
    if err := d.Init(context.Background(), map[string]any{}); err != nil { t.Fatal(err) }
    if _, err := d.Poll(context.Background()); err != nil { t.Fatal(err) }
    if gotKey != "refresh_token" || gotVal != "RT2" {
        t.Fatalf("persist got (%q,%q)", gotKey, gotVal)
    }
}
```

(If no inline-source helper exists, add one or write a tiny temp `.lua` file via `t.TempDir()`.)

- [ ] **Step 2: Run, expect FAIL** (`host.persist_secret` undefined). `cd go && go test ./internal/drivers/ -run TestHostPersistSecret`.

- [ ] **Step 3: Add the field** in `host.go` HostEnv struct:

```go
// PersistSecret, when non-nil, lets a driver durably write a config
// secret (e.g. a rotated OAuth refresh_token) back into its own config
// block so it survives restarts. nil → host.persist_secret returns an
// error. Wired by the Registry to a per-driver closure (see registry.go).
PersistSecret func(key, value string) error
```

- [ ] **Step 4: Register the primitive** in `lua.go` `registerHost` (mirror the `set_sn` shape):

```go
host.RawSetString("persist_secret", L.NewFunction(func(L *lua.LState) int {
    key := L.CheckString(1)
    val := L.CheckString(2)
    if env.PersistSecret == nil {
        L.Push(lua.LBool(false))
        L.Push(lua.LString("persist_secret: capability not granted"))
        return 2
    }
    if err := env.PersistSecret(key, val); err != nil {
        L.Push(lua.LBool(false))
        L.Push(lua.LString(err.Error()))
        return 2
    }
    L.Push(lua.LBool(true))
    L.Push(lua.LNil)
    return 2
}))
```

- [ ] **Step 5: Run, expect PASS.**

- [ ] **Step 6: Wire the Registry closure.** In `registry.go`, add `SecretPersister func(driverName, key, value string) error` to the Registry struct, and where each driver's `HostEnv` is built in `Add`, set:

```go
if reg.SecretPersister != nil {
    name := <driver name in scope>
    env.PersistSecret = func(key, value string) error {
        return reg.SecretPersister(name, key, value)
    }
}
```

- [ ] **Step 7: Provide the impl** in `main.go` where the registry is constructed:

```go
reg.SecretPersister = func(driverName, key, value string) error {
    cfgMu.Lock()
    defer cfgMu.Unlock()
    for i := range cfg.Drivers {
        if cfg.Drivers[i].Name == driverName {
            if cfg.Drivers[i].Config == nil {
                cfg.Drivers[i].Config = map[string]any{}
            }
            cfg.Drivers[i].Config[key] = value
            return config.SaveAtomic(cfgPath, cfg)
        }
    }
    return fmt.Errorf("persist_secret: driver %q not in config", driverName)
}
```

(Match the exact identifiers main.go uses for the config pointer / mutex / path — confirm before writing.)

- [ ] **Step 8: `cd go && go build ./... && go test ./internal/drivers/`. Commit.**

```bash
git add go/internal/drivers/host.go go/internal/drivers/lua.go go/internal/drivers/registry.go go/cmd/forty-two-watts/main.go go/internal/drivers/lua_persist_test.go
git commit -m "feat(drivers): add host.persist_secret capability for rotated OAuth tokens"
```

---

## Task 2: Rewrite `drivers/myuplink.lua` to refresh-token auth

**Files:**
- Modify: `drivers/myuplink.lua`
- Test: `go/internal/drivers/myuplink_test.go`

**Interfaces:**
- Consumes: `host.persist_secret` (Task 1), `host.http_post`, `host.json_decode`.
- Config keys: `client_id`, `client_secret` (secret), `refresh_token` (secret), optional `device_id`, param overrides, `base_url` (tests).

- [ ] **Step 1: Update the test** `TestMyUplinkEmitsTelemetry` so the fake server handles a refresh-token grant and returns a rotated refresh token; add `refresh_token` to config; assert telemetry still lands AND that `persist_secret` is called with the rotated token.

```go
// fake /oauth/token: assert grant_type=refresh_token, return rotated token
case "/oauth/token":
    body, _ := io.ReadAll(r.Body)
    if !strings.Contains(string(body), "grant_type=refresh_token") {
        t.Errorf("expected refresh_token grant, got %q", body)
    }
    _ = json.NewEncoder(w).Encode(map[string]any{
        "access_token": "test-token", "expires_in": 3600, "refresh_token": "RT-rotated",
    })
```
```go
var persisted = map[string]string{}
env := NewHostEnv("myuplink", tel).WithHTTP()
env.PersistSecret = func(k, v string) error { persisted[k] = v; return nil }
cfg := map[string]any{"client_id":"cid","client_secret":"csec","refresh_token":"RT-initial","device_id":"DEV1","base_url":srv.URL}
// ...after Init...
if persisted["refresh_token"] != "RT-rotated" {
    t.Errorf("rotated refresh_token not persisted: %v", persisted)
}
```

- [ ] **Step 2: Run, expect FAIL** (`go test ./internal/drivers/ -run TestMyUplink`).

- [ ] **Step 3: Rewrite `fetch_token()`** in `drivers/myuplink.lua`:

```lua
local refresh_token = nil  -- module-level, alongside access_token

local function fetch_token()
    if not refresh_token then
        host.log("error", "MyUplink: no refresh_token — complete the OAuth connect in Settings → Devices")
        return false
    end
    local body = "grant_type=refresh_token"
        .. "&client_id="     .. url_encode(client_id)
        .. "&client_secret=" .. url_encode(client_secret)
        .. "&refresh_token=" .. url_encode(refresh_token)
    local resp, err = host.http_post(
        BASE_URL .. "/oauth/token", body,
        { ["Content-Type"] = "application/x-www-form-urlencoded" })
    if err then
        host.log("error", "MyUplink: token refresh failed: " .. tostring(err))
        return false
    end
    local data = host.json_decode(resp)
    if not data or not data.access_token then
        host.log("error", "MyUplink: no access_token in refresh response")
        return false
    end
    access_token = data.access_token
    local expires_in = tonumber(data.expires_in) or 3600
    token_expires_at = host.millis() + (expires_in * 1000) - 60000
    -- Azure B2C rotates refresh tokens; persist the new one so it survives restart.
    if data.refresh_token and data.refresh_token ~= refresh_token then
        refresh_token = data.refresh_token
        local ok, perr = host.persist_secret("refresh_token", refresh_token)
        if not ok then host.log("warn", "MyUplink: could not persist rotated refresh_token: " .. tostring(perr)) end
    end
    return true
end
```

- [ ] **Step 4: Read `refresh_token` from config** in `driver_init` (alongside client_id/secret), normalize `""`→nil. Update `config_secrets = { "client_secret", "refresh_token" }` in the DRIVER block. Update the top-of-file comment + DRIVER `description` to say authorization-code/refresh-token (no `client_credentials`, and explicitly NO `mode` field — that was never implemented). If `refresh_token` is nil after init, log the "complete OAuth connect" message and leave the driver idle (poll returns 60000 and does nothing).

- [ ] **Step 5: Run, expect PASS.** Also run the full `go test ./internal/drivers/`.

- [ ] **Step 6: Commit.**

```bash
git add drivers/myuplink.lua go/internal/drivers/myuplink_test.go
git commit -m "fix(myuplink): use authorization-code/refresh-token OAuth (closes #496)"
```

---

## Task 3: OAuth bootstrap endpoints (Go)

**Files:**
- Create: `go/internal/api/api_myuplink_oauth.go`
- Create: `go/internal/api/api_myuplink_oauth_test.go`
- Modify: `go/internal/api/api.go` (routes only)

**Interfaces:**
- `GET /api/oauth/myuplink/start?driver=<name>` → `{authorize_url, redirect_uri, callback}`. Reads `client_id` from `cfg.Drivers[name].Config`; generates random `state`; stores `{driver, redirect_uri, exp}` in an in-memory map (mutex-guarded, 10-min TTL); `redirect_uri` derived from request scheme+host as `<origin>/api/oauth/myuplink/callback`.
- `GET /api/oauth/myuplink/callback?code=&state=` → validates state, POSTs to MyUplink `https://api.myuplink.com/oauth/token` with `grant_type=authorization_code,code,client_id,client_secret,redirect_uri`; on success writes `refresh_token` into the driver config via `Deps.SaveConfig` (mirroring `handlePostConfig`'s restore+save), restarts the driver, returns a small HTML success page. Base URL overridable via an unexported package var for tests.

- [ ] **Step 1: Write failing test** `api_myuplink_oauth_test.go`: `/start` returns an authorize URL containing the configured client_id, `response_type=code`, `scope=READSYSTEM offline_access`, and the derived redirect_uri; `/callback` with a valid state exchanges against a stub token server and the resulting config has `refresh_token` set. Use the existing api test harness (in-memory `SaveConfig` fake — see `api_drivers_debug_test.go` for the pattern).

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement** `api_myuplink_oauth.go` (`handleMyUplinkOAuthStart`, `handleMyUplinkOAuthCallback`, the in-memory `oauthStateStore`, `buildAuthorizeURL`, `exchangeCode`). Scope string: `"READSYSTEM offline_access"`. Validate `state` exists + not expired + matches driver; delete on use (single-use). On exchange failure, render an error page with the MyUplink error body (do not leak secrets).

- [ ] **Step 4: Register routes** in `api.go` `routes()`:

```go
s.handle("GET /api/oauth/myuplink/start", s.handleMyUplinkOAuthStart)
s.handle("GET /api/oauth/myuplink/callback", s.handleMyUplinkOAuthCallback)
```

- [ ] **Step 5: Run, expect PASS.** `cd go && go test ./internal/api/ -run OAuth`.

- [ ] **Step 6: Commit.**

```bash
git add go/internal/api/api_myuplink_oauth.go go/internal/api/api_myuplink_oauth_test.go go/internal/api/api.go
git commit -m "feat(api): MyUplink OAuth authorize/callback bootstrap"
```

---

## Task 4: Settings UI — Connect flow

**Files:**
- Modify: `web/settings/tabs/devices.js`

- [ ] **Step 1:** In the `isApiCredsDriver` fieldset, fix the stale `client_credentials` comment/copy → "OAuth2 authorization-code". Add, after the Client ID input: a read-only Callback URL line computed as `location.origin + '/api/oauth/myuplink/callback'` with a copy hint ("Register this exact URL as the Callback Url in the MyUplink developer portal"); a "Connect to MyUplink" button; and a connection badge driven by whether `refresh_token` is saved (use the existing masked-secret "saved/not saved" signal — MyUplink declares `refresh_token` in `config_secrets`, so the catalog/secret-status path already reports it; reuse the `creds-badge` styling).

- [ ] **Step 2:** Wire the button: `GET /api/oauth/myuplink/start?driver=<name>` then `window.open(authorize_url, '_blank')`. After the popup, show "Finish the consent in the new tab, then reload this page." (No SSE; the callback persists server-side and the badge flips on next config load.) Save client_id + secret first if dirty (reuse the existing save path; prompt if unsaved).

- [ ] **Step 3:** Manual check via `make dev` — render the MyUplink device card, confirm the callback URL, button, and badge appear and match DESIGN.md tokens (no hard-coded hex; use `creds-badge`, `btn-add` classes already present).

- [ ] **Step 4: Commit.**

```bash
git add web/settings/tabs/devices.js
git commit -m "feat(ui): MyUplink OAuth connect flow in Settings → Devices"
```

---

## Task 5: Heating dashboard view

**Files:**
- Create: `web/heating.js`
- Modify: `web/index.html`

**Interfaces:**
- Consumes: `GET /api/drivers/{name}` (live metrics array; `m.name`/`m.value`) for current values; `GET /api/series?driver=<name>&metric=hp_power_w&range=24h&points=300` for the chart; `GET /api/drivers` to discover any driver whose live metrics include `hp_*` (so it works for any MyUplink-class driver, not a hard-coded name).

- [ ] **Step 1:** Add a `<section class="heating-row" id="heating-section" hidden>` to `index.html` (after the energy/history rows) with a `<div id="heating-grid">` host, and include `<script src="heating.js" defer></script>`.

- [ ] **Step 2:** Implement `web/heating.js` (IIFE, matching `twins.js` conventions): on load, `GET /api/drivers`; for each driver whose metrics include `hp_power_w`, unhide the section and render a card: compressor power (W), hot-water top (°C), indoor (°C), outdoor (°C) as labelled rows (mono numerics), plus a small inline SVG sparkline of `hp_power_w` over 24h from `/api/series`. Poll every 30s while visible. All colours via theme tokens (`var(--fg)`, `var(--accent-e)`, `var(--line)`); eyebrow label in mono uppercase per DESIGN.md. If no heat-pump driver is present, keep the section hidden.

- [ ] **Step 3:** Manual check via `make dev` with a stubbed MyUplink (or seed `hp_*` via the dev backfill) — section appears only when hp_* metrics exist; values + sparkline render; light theme flips cleanly.

- [ ] **Step 4: Commit.**

```bash
git add web/heating.js web/index.html
git commit -m "feat(ui): heat-pump telemetry dashboard view"
```

---

## Task 6: Docs + changeset + driver notes

**Files:**
- Create: `docs/myuplink-oauth.md`
- Modify: `docs/writing-a-driver.md` (document `host.persist_secret`)
- Modify: `drivers/myuplink.lua` header + (if present) driver `CLAUDE.md` notes
- Create: `.changeset/<name>.md`

- [ ] **Step 1:** Write `docs/myuplink-oauth.md`: register an app at dev.myuplink.com → Applications; paste the exact Callback URL 42W shows; copy Client Identifier → Client ID and Client Secret → secret; click Connect to MyUplink; complete consent; verify the badge + the heating card. Note the HTTPS-callback caveat (if the portal rejects an http LAN origin, use the relay/https origin or `http://localhost`). Explicitly state there is **no `mode` field** (corrects the earlier misinformation) and that the driver is read-only telemetry.

- [ ] **Step 2:** Add `host.persist_secret(key, value) -> ok, err` to the host-API section of `docs/writing-a-driver.md` and the lua.go top-of-file capability list.

- [ ] **Step 3:** `npx changeset` → **minor**, summary: "MyUplink heat-pump driver: authorization-code OAuth with in-app consent, rotated-token persistence, and a heat-pump telemetry dashboard view."

- [ ] **Step 4:** `cd go && make verify` (vet + test + build). Commit.

```bash
git add docs/ drivers/myuplink.lua .changeset/
git commit -m "docs(myuplink): OAuth setup guide + host.persist_secret + changeset"
```

---

## Task 7: Correct issue #496 + draft Discord reply

**Files:** none (GitHub + a drafted message for Fredrik to post)

- [ ] **Step 1:** Comment on issue #496 (English): root cause confirmed (portal = authorization-code, `client_credentials` unsupported → `invalid_client`); the fix (in-app consent + refresh-token); and the correction that the driver has **no `mode` field** — the earlier `hotwater`/`heating` guidance was wrong. Link the PR.
- [ ] **Step 2:** Draft (do not auto-post) a short English Discord reply for the #troubleshooting thread: the real cause, that a fix is shipping, the corrected setup (Callback URL + Connect button, no `mode`), for Fredrik to send when the release is out.

---

## Self-Review notes

- Spec coverage: #496 root cause → Tasks 2–3; `mode` misinformation → Tasks 2 (driver comment), 6 (docs), 7 (issue/Discord); heating UI gap → Task 5; rotation/persistence → Task 1. ✔
- Rotation across restart handled by Task 1 + Task 2 step 3 (`persist_secret` on rotation). ✔
- HTTPS-callback risk is documented (Task 6 step 1), not silently assumed. ✔
- Reuses `/api/series` + live-metrics rather than adding a redundant endpoint. ✔
