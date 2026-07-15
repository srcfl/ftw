# MyUplink Telemetry (Step 1, read-only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scale PR #484 down to a read-only MyUplink heat-pump telemetry driver — emit compressor power + temperatures into the TS DB, with zero control/dispatch/MPC code — and deliver it on hannesb90's branch.

**Architecture:** Reuse hannesb90's OAuth client_credentials + REST v2 polling, strip the fake-battery/control/MPC layers. The driver only calls `host.emit_metric`. All his Go-side changes (lua.go `http_patch`, main.go `driverLimitsFrom`, dispatch.go `BlockCharge`) are reverted to base. `client_secret` is handled by the existing `config_secrets` masking path; UI keeps a minimal `client_id` field.

**Tech Stack:** Lua 5.1 (gopher-lua), Go 1.22+, `host.http_get`/`http_post`/`emit_metric`, Changesets.

**Spec:** `docs/superpowers/specs/2026-06-15-myuplink-telemetry-design.md`

---

## File Structure

| File | Responsibility | Action |
|---|---|---|
| `drivers/myuplink.lua` | Read-only telemetry driver | Rewrite (strip control/battery) |
| `go/internal/drivers/lua.go` | Lua host capabilities | Revert `http_patch` to base |
| `go/internal/drivers/lua_http_test.go` | HTTP capability tests | Revert (drop patch tests) |
| `go/cmd/forty-two-watts/main.go` | `driverLimitsFrom` | Revert to base |
| `go/internal/control/dispatch.go` | `PowerLimits` / dispatch | Revert to base |
| `web/settings/tabs/devices.js` | Settings UI | Trim to `client_id`-only fieldset |
| `go/internal/drivers/myuplink_test.go` | Driver telemetry test | Create |
| `.changeset/*.md` | Release note | Replace with read-only minor |

---

## Task 0: Git setup — isolated worktree on hannesb90's PR branch

**Files:** none (git mechanics)

- [ ] **Step 1: Confirm clean base refs**

Run:
```bash
cd /Users/fredde/repositories/forty-two-watts
git fetch origin
gh pr view 484 --repo frahlg/forty-two-watts --json headRefName,maintainerCanModify,baseRefName
```
Expected: `headRefName=feat/myuplink-driver`, `maintainerCanModify=true`, `baseRefName=master`.

- [ ] **Step 2: Create an isolated worktree and check out the PR branch into it**

The main working dir is dirty with unrelated v2x work — do NOT switch branches there.
```bash
git worktree add --detach /Users/fredde/repositories/ftw-myuplink
cd /Users/fredde/repositories/ftw-myuplink
gh pr checkout 484 --repo frahlg/forty-two-watts
```
Expected: HEAD now on `feat/myuplink-driver` tracking hannesb90's fork. `git log --oneline -1` shows his latest commit (`829a1942 fix(myuplink): address upstream review feedback`).

- [ ] **Step 3: Record the merge-base for clean reverts**

```bash
git merge-base HEAD origin/master
```
Expected: a SHA. Save it as `$BASE` for Task 2:
```bash
BASE=$(git merge-base HEAD origin/master)
echo "$BASE"
```

All remaining tasks run inside `/Users/fredde/repositories/ftw-myuplink`.

---

## Task 1: Rewrite `drivers/myuplink.lua` to read-only telemetry

**Files:**
- Modify: `drivers/myuplink.lua` (full rewrite)

- [ ] **Step 1: Replace the entire file with the read-only driver**

```lua
-- MyUplink Heat Pump Driver — READ-ONLY telemetry (heating workstream, Step 1)
-- Emits: metrics only (compressor power + temperatures) into the long-format
--        TS DB via host.emit_metric. NO control, NO battery emit, NO MPC.
-- Protocol: HTTPS (MyUplink Cloud REST API v2)
--
-- Observe-only by design: the EMS reads heat-pump telemetry so a proper
-- thermal-store model + control primitive can be grounded in a later step.
-- It cannot actuate the pump, so it cannot cause harm. The OAuth scope is
-- READSYSTEM only (least privilege).
--
-- Config example (config.yaml):
--   drivers:
--     - name: myuplink
--       lua: drivers/myuplink.lua
--       config:
--         client_id: "..."
--         client_secret: "..."     # masked via config_secrets
--         # device_id: "..."       # optional; auto-detected if omitted
--       capabilities:
--         http:
--           allowed_hosts: ["api.myuplink.com"]
--
-- Find your parameter IDs via GET /v2/devices/{deviceId}/points if the NIBE
-- defaults below don't match your model. Each can be overridden in config
-- (param_power_id, param_hw_temp_id, param_indoor_temp_id, param_outdoor_temp_id).

DRIVER = {
  id           = "myuplink",
  name         = "MyUplink Heat Pump (telemetry)",
  manufacturer = "MyUplink (NIBE, Bosch, Atlantic, Daikin, ...)",
  version      = "1.0.0",
  protocols    = { "http" },
  capabilities = { "apicreds" },
  description  = "Read-only heat-pump telemetry via MyUplink Cloud REST API v2: compressor power + hot-water/indoor/outdoor temperatures. Observe-only — no control.",
  homepage     = "https://dev.myuplink.com",
  http_hosts   = { "api.myuplink.com" },
  authors      = { "hannesb90", "forty-two-watts contributors" },
  tested_models = { "NIBE F1145", "NIBE S1255", "NIBE F730" },
  verification_status = "experimental",
  config_secrets = { "client_secret" },
}

PROTOCOL = "http"

local BASE_URL = "https://api.myuplink.com"

local access_token     = nil
local token_expires_at = 0

local client_id     = nil
local client_secret = nil
local device_id     = nil

-- Parameter IDs (NIBE defaults, overridable via config)
local PARAM_POWER        = "10012"  -- compressor power (W)
local PARAM_HW_TEMP      = "40013"  -- BT6 hot water top temp
local PARAM_INDOOR_TEMP  = "40033"  -- BT50 room temperature
local PARAM_OUTDOOR_TEMP = "40004"  -- BT1 outdoor temperature

-- ---- Helpers -------------------------------------------------------------

local function url_encode(s)
    return (s:gsub("[^%w%-%.%_%~]", function(c)
        return string.format("%%%02X", string.byte(c))
    end))
end

-- ---- Auth ----------------------------------------------------------------

local function fetch_token()
    local body = "grant_type=client_credentials"
        .. "&client_id=" .. url_encode(client_id)
        .. "&client_secret=" .. url_encode(client_secret)
        .. "&scope=READSYSTEM"
    local resp, err = host.http_post(
        BASE_URL .. "/oauth/token", body,
        { ["Content-Type"] = "application/x-www-form-urlencoded" })
    if err then
        host.log("error", "MyUplink: token request failed: " .. tostring(err))
        return false
    end
    local data = host.json_decode(resp)
    if not data or not data.access_token then
        host.log("error", "MyUplink: no access_token in response")
        return false
    end
    access_token = data.access_token
    local expires_in = tonumber(data.expires_in) or 3600
    token_expires_at = host.millis() + (expires_in * 1000) - 60000
    return true
end

local function ensure_auth()
    if access_token and host.millis() < token_expires_at then return true end
    return fetch_token()
end

local function auth_headers()
    return { Authorization = "Bearer " .. (access_token or "") }
end

-- ---- API helpers ---------------------------------------------------------

local function api_get(path)
    local resp, err = host.http_get(BASE_URL .. path, auth_headers())
    if err then return nil, tostring(err) end
    local data, derr = host.json_decode(resp)
    if not data then return nil, tostring(derr) end
    return data, nil
end

local function detect_device_id()
    local systems, err = api_get("/v2/systems/me")
    if err then
        host.log("error", "MyUplink: /v2/systems/me failed: " .. err)
        return nil
    end
    for _, system in ipairs(systems.objects or {}) do
        local devices = system.devices or {}
        if #devices > 0 then
            local did = devices[1].id
            host.log("info", "MyUplink: auto-detected device " .. tostring(did))
            return did
        end
    end
    host.log("error", "MyUplink: no devices found")
    return nil
end

local function fetch_points(param_ids)
    local qs = table.concat(param_ids, ",")
    local data, err = api_get("/v2/devices/" .. device_id .. "/points?parameters=" .. qs)
    if err then return nil, err end
    local pts = {}
    for _, pt in ipairs(data) do
        if pt.parameterId then pts[tostring(pt.parameterId)] = pt end
    end
    return pts, nil
end

local function decode_temp(pt)
    if not pt then return nil end
    local raw = tonumber(pt.value)
    if not raw then return nil end
    if math.abs(raw) > 100 then return raw / 10 end  -- NIBE °C×10 encoding
    return raw
end

-- ---- Lifecycle -----------------------------------------------------------

function driver_init(config)
    host.set_make("MyUplink")

    client_id     = config and config.client_id
    client_secret = config and config.client_secret
    device_id     = config and config.device_id
    if client_id     == "" then client_id     = nil end
    if client_secret == "" then client_secret = nil end
    if device_id     == "" then device_id     = nil end

    if config then
        local function ov(k, d) return (config[k] and config[k] ~= "") and config[k] or d end
        PARAM_POWER        = ov("param_power_id",        PARAM_POWER)
        PARAM_HW_TEMP      = ov("param_hw_temp_id",      PARAM_HW_TEMP)
        PARAM_INDOOR_TEMP  = ov("param_indoor_temp_id",  PARAM_INDOOR_TEMP)
        PARAM_OUTDOOR_TEMP = ov("param_outdoor_temp_id", PARAM_OUTDOOR_TEMP)
        -- base_url override exists for tests; production uses api.myuplink.com.
        if config.base_url and config.base_url ~= "" then BASE_URL = config.base_url end
    end

    if not client_id or not client_secret then
        host.log("error", "MyUplink: client_id and client_secret required")
        return
    end
    if not ensure_auth() then
        host.log("error", "MyUplink: initial auth failed")
        return
    end
    if not device_id then
        device_id = detect_device_id()
        if not device_id then return end
    end

    host.set_sn(device_id)
    host.log("info", "MyUplink: ready (read-only) device=" .. device_id)
end

function driver_poll()
    if not device_id or not client_id then return 30000 end
    if not ensure_auth() then return 30000 end

    local pts, err = fetch_points({ PARAM_POWER, PARAM_HW_TEMP, PARAM_INDOOR_TEMP, PARAM_OUTDOOR_TEMP })
    if err then
        host.log("warn", "MyUplink: poll failed: " .. err)
        return 30000
    end

    if pts[PARAM_POWER] then
        local raw = tonumber(pts[PARAM_POWER].value) or 0
        local power_w = (pts[PARAM_POWER].unit == "kW") and raw * 1000 or raw
        host.emit_metric("hp_power_w", power_w)
    end
    if pts[PARAM_HW_TEMP]      then host.emit_metric("hp_hw_top_temp_c",  decode_temp(pts[PARAM_HW_TEMP])      or 0) end
    if pts[PARAM_INDOOR_TEMP]  then host.emit_metric("hp_indoor_temp_c",  decode_temp(pts[PARAM_INDOOR_TEMP])  or 0) end
    if pts[PARAM_OUTDOOR_TEMP] then host.emit_metric("hp_outdoor_temp_c", decode_temp(pts[PARAM_OUTDOOR_TEMP]) or 0) end

    return 60000
end

function driver_command(_action, _power_w, _cmd)
    -- Read-only: no actuation in Step 1.
    return false
end

function driver_default_mode()
    -- Read-only: nothing to release.
end

function driver_cleanup()
    access_token     = nil
    token_expires_at = 0
end
```

- [ ] **Step 2: Sanity-check the catalog parses it**

Run:
```bash
cd /Users/fredde/repositories/ftw-myuplink
go test ./go/internal/drivers/ -run TestCatalog -count=1 2>&1 | tail -5
```
Expected: PASS (or no catalog test fails). If no `TestCatalog*` exists, skip — Task 4 exercises the driver directly.

- [ ] **Step 3: Commit**

```bash
git add drivers/myuplink.lua
git commit -m "refactor(myuplink): scale driver down to read-only telemetry

Drop the fake-battery emit, block/release control, and READSYSTEM+WRITESYSTEM
scope. Heating goes telemetry-first: observe real heat-pump data before
modelling a thermal store. Control returns in a later step as a proper
thermal-load primitive, not a battery."
```

---

## Task 2: Revert hannesb90's Go-side changes to base

**Files:**
- Modify: `go/internal/drivers/lua.go` (revert `http_patch`)
- Modify: `go/internal/drivers/lua_http_test.go` (revert patch tests)
- Modify: `go/cmd/forty-two-watts/main.go` (revert `driverLimitsFrom`)
- Modify: `go/internal/control/dispatch.go` (revert `BlockCharge`)

- [ ] **Step 1: Restore the four files from the merge-base**

```bash
cd /Users/fredde/repositories/ftw-myuplink
BASE=$(git merge-base HEAD origin/master)
git checkout "$BASE" -- \
  go/internal/drivers/lua.go \
  go/internal/drivers/lua_http_test.go \
  go/cmd/forty-two-watts/main.go \
  go/internal/control/dispatch.go
```

- [ ] **Step 2: Verify the control/HTTP changes are gone**

Run:
```bash
grep -n "http_patch\|BlockCharge\|blockCharge" \
  go/internal/drivers/lua.go go/cmd/forty-two-watts/main.go go/internal/control/dispatch.go
```
Expected: no matches.

- [ ] **Step 3: Verify the build + existing tests still pass**

Run:
```bash
go build ./... && go test ./go/internal/control/ ./go/internal/drivers/ -count=1 2>&1 | tail -15
```
Expected: build OK, tests PASS (the reverted `lua_http_test.go` is back to its base form).

- [ ] **Step 4: Commit**

```bash
git add go/internal/drivers/lua.go go/internal/drivers/lua_http_test.go \
        go/cmd/forty-two-watts/main.go go/internal/control/dispatch.go
git commit -m "revert(myuplink): drop control/dispatch/MPC + http_patch changes

Step 1 is read-only telemetry. The block_charge dispatch wiring overloaded
max_charge_w==0 (breaking charging for every battery on the MaxCommandW
default) and never reached the MPC fleet builder. Control returns later with
an explicit primitive + regression tests. http_patch is unused by a read-only
driver."
```

---

## Task 3: Trim the Settings UI to a minimal `client_id` field

**Files:**
- Modify: `web/settings/tabs/devices.js`

Background: the `config_secrets = { "client_secret" }` declared in Task 1 makes
the existing `.drv-secrets-slot` render a masked `client_secret` input
automatically. So the API-credentials fieldset only needs `client_id`, and the
block-charge checkbox must go (no control in Step 1).

- [ ] **Step 1: Replace the API-credentials fieldset body (drop secret input + checkbox)**

Find the `if (isApiCredsDriver) {` block (~line 153) and replace its `html += ...`
assignment so it renders ONLY the `client_id` field:

```javascript
        if (isApiCredsDriver) {
          // OAuth2 client_credentials drivers (e.g. MyUplink).
          // User registers an app at the provider's developer portal and
          // pastes the Client ID here. The Client Secret is rendered by the
          // config_secrets slot below (masked, never echoed into the DOM).
          var acfg = d.config || {};
          html += '<fieldset><legend>API credentials</legend>' +
            '<p style="color:var(--text-dim);font-size:0.75rem;margin:0 0 8px">Register an application at the provider\'s developer portal to get a Client ID and Client Secret. Paste the secret in the Secrets section below.</p>' +
            '<label>Client ID ' + help('Application identifier from the developer portal.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.client_id" value="' + escHtml(acfg.client_id || '') + '" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx">' +
            '</fieldset>';
        }
```

- [ ] **Step 2: Remove the block-charge checkbox event handler**

Delete the entire `bodyEl.querySelectorAll(".apicreds-block-charge")...` listener
block (~line 613, the one whose comment says `API creds drivers: "blockera extra
produktion" checkbox`). It references a checkbox that no longer exists.

- [ ] **Step 3: Verify no dangling references remain**

Run:
```bash
grep -n "apicreds-block-charge\|blockCharge\|block-charge\|max_charge_w" web/settings/tabs/devices.js
```
Expected: no matches.

- [ ] **Step 4: Confirm the add-device default still seeds creds**

The `driver.config = { client_id: "", client_secret: "" };` branch (~line 421)
for `entryCaps.indexOf("apicreds") >= 0` is correct and stays. Verify it's present:
```bash
grep -n 'client_id: ""' web/settings/tabs/devices.js
```
Expected: one match.

- [ ] **Step 5: Commit**

```bash
git add web/settings/tabs/devices.js
git commit -m "fix(settings): myuplink UI is client_id + config_secrets only

Drop the block-charge checkbox (no control in Step 1) and the duplicate
client_secret input — the secret is rendered by the config_secrets slot
(masked, never echoed into the DOM)."
```

---

## Task 4: Read-only telemetry driver test

**Files:**
- Create: `go/internal/drivers/myuplink_test.go`

- [ ] **Step 1: Write the failing test**

```go
package drivers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"

	"forty-two-watts/go/internal/telemetry"
)

// driverPath resolves drivers/myuplink.lua from the repo root regardless of
// the test's working directory (tests run in go/internal/drivers/).
func myuplinkDriverPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = <repo>/go/internal/drivers/myuplink_test.go → up 3 to <repo>.
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	return filepath.Join(repo, "drivers", "myuplink.lua")
}

// TestMyUplinkEmitsTelemetry loads the real driver against a fake MyUplink
// server and asserts the compressor-power metric lands in the telemetry store.
func TestMyUplinkEmitsTelemetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-token", "expires_in": 3600,
			})
		default:
			// /v2/devices/{id}/points
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"parameterId": "10012", "value": "1500", "unit": "W"},
				{"parameterId": "40013", "value": "520"}, // 52.0 °C (×10 encoding)
			})
		}
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("myuplink", tel).WithHTTP()

	d, err := NewLuaDriver(myuplinkDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	cfg := map[string]any{
		"client_id":     "cid",
		"client_secret": "csecret",
		"device_id":     "DEV1",
		"base_url":      srv.URL,
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if v, _, ok := tel.LatestMetric("myuplink", "hp_power_w"); !ok || v != 1500 {
		t.Errorf("hp_power_w = %v (ok=%v), want 1500", v, ok)
	}
	if v, _, ok := tel.LatestMetric("myuplink", "hp_hw_top_temp_c"); !ok || v != 52 {
		t.Errorf("hp_hw_top_temp_c = %v (ok=%v), want 52", v, ok)
	}
}
```

- [ ] **Step 2: Run it to confirm it passes (driver already written in Task 1)**

Run:
```bash
cd /Users/fredde/repositories/ftw-myuplink
go test ./go/internal/drivers/ -run TestMyUplinkEmitsTelemetry -v -count=1 2>&1 | tail -20
```
Expected: PASS. If it fails on `Init` signature or `cfg` type, check the real
`Driver.Init` signature in `lua.go` and adapt the `cfg` argument (it accepts a
`map[string]any` config table); the TDD loop catches this. If `LatestMetric`
has a different signature, match the one used in `lua_http_test.go`
(`tel.LatestMetric(driver, name) (value, ts, ok)`).

- [ ] **Step 3: Commit**

```bash
git add go/internal/drivers/myuplink_test.go
git commit -m "test(myuplink): assert read-only driver emits hp_* telemetry"
```

---

## Task 5: Replace the changeset

**Files:**
- Modify/Create: `.changeset/*.md`

- [ ] **Step 1: Remove any stale myuplink changeset on the branch**

```bash
cd /Users/fredde/repositories/ftw-myuplink
ls .changeset/*.md
grep -rl "myuplink\|MyUplink\|block.charge\|http_patch" .changeset/ 2>/dev/null
```
Delete any changeset file that describes the old control/fake-battery feature:
```bash
git rm .changeset/<stale-myuplink-file>.md   # only if one exists
```

- [ ] **Step 2: Write the read-only changeset**

Create `.changeset/myuplink-telemetry.md`:
```markdown
---
"forty-two-watts": minor
---

Add a read-only MyUplink heat-pump telemetry driver (`drivers/myuplink.lua`).
Authenticates to the MyUplink Cloud REST API v2 (OAuth2 client_credentials,
READSYSTEM scope) and emits compressor power and hot-water / indoor / outdoor
temperatures into the time-series DB. Observe-only — no control. Configure the
Client ID in Settings → Devices; the Client Secret is stored as a masked
config secret.
```

- [ ] **Step 3: Commit**

```bash
git add .changeset/
git commit -m "chore(changeset): read-only myuplink telemetry driver (minor)"
```

---

## Task 6: Full verification

**Files:** none

- [ ] **Step 1: Run the project's pre-commit verification**

Run:
```bash
cd /Users/fredde/repositories/ftw-myuplink
make verify 2>&1 | tail -30
```
Expected: vet + test + build all pass. If `make verify` is unavailable, run:
```bash
go vet ./... && go test ./... -count=1 2>&1 | tail -30
```

- [ ] **Step 2: Confirm the diff vs base is read-only-shaped**

Run:
```bash
git diff --stat origin/master...HEAD
```
Expected: changes limited to `drivers/myuplink.lua`, `web/settings/tabs/devices.js`,
`go/internal/drivers/myuplink_test.go`, `.changeset/*`. No diff in
`lua.go`, `dispatch.go`, `main.go`, `lua_http_test.go`.

---

## Task 7: Push to hannesb90's branch + PR comment

**Files:** none

- [ ] **Step 1: Push the rewritten branch to his fork**

```bash
cd /Users/fredde/repositories/ftw-myuplink
git push
```
Expected: pushes to `hannesb90:feat/myuplink-driver` (gh configured the remote
on checkout; `maintainer_can_modify=true` allows it). If it rejects as non-fast-
forward because we reverted files, STOP and report — do not force-push without
confirming with Fredrik.

- [ ] **Step 2: Post the explanatory PR comment (English)**

```bash
gh pr comment 484 --repo frahlg/forty-two-watts --body "$(cat <<'EOF'
Thanks @hannesb90 — really solid OAuth/REST work here, and the fake-battery idea is clever. We want to take heating on as its own holistic workstream, and the call is to sequence it **telemetry-first**: get real heat-pump data flowing and observable before we model a thermal store or add any control.

So I've scaled this PR down (on your branch) to a **read-only telemetry driver**: it keeps your OAuth client_credentials flow + REST v2 polling, and emits compressor power + hot-water/indoor/outdoor temps into the time-series DB via `emit_metric`. Everything else is deferred to a later step:

- removed the `emit("battery")` fake-battery + `soc=1.0`,
- removed the block/release control and `driver_command`,
- reverted the Go changes (`http_patch`, `driverLimitsFrom`, `dispatch.go`),
- the secret now goes through the existing `config_secrets` masking path (so it's never echoed into the DOM), and the UI keeps a Client ID field.

Two reasons the control path is parked rather than merged as-is:
1. `blockCharge := MaxChargeW == 0 && BatteryCapacityWh > 0` in `driverLimitsFrom` overloads an absent `max_charge_w`, which would stop **every** existing battery (ferroamp/sungrow) that relies on the `MaxCommandW` default from ever charging — it contradicts the #145 contract.
2. The MPC fleet builder (`mpcBatteryFleetFromConfig`) only reads the `batteries:` section, never the driver-level `max_charge_w`, so the planner would still schedule pre-heating.

Both are very fixable — they come back when we add a proper thermal-load control primitive (with an explicit `block_charge`, propagated to both dispatch and the MPC, plus regression tests). Your control design is recorded in the roadmap, not rejected. Once this read-only step lands and we see real telemetry, the model + control step is next.
EOF
)"
```
Expected: comment URL printed.

- [ ] **Step 3: Report back to Fredrik**

Summarize: branch pushed, comment posted, link to the PR, and that the worktree
at `/Users/fredde/repositories/ftw-myuplink` can be removed with
`git worktree remove` once he's happy.

---

## Self-Review Notes

- **Spec coverage:** read-only driver (Task 1), strip control/Go (Task 2), secrets via config_secrets + minimal UI (Tasks 1+3), test (Task 4), changeset minor (Task 5), delivery on his branch + credit (Task 7), roadmap recorded (spec). ✓
- **No new control/dispatch tests** — correct, there is no control code in Step 1.
- **Risk flagged:** `Driver.Init` config-arg type and `LatestMetric` signature are verified against `lua_http_test.go` during Task 4's TDD loop; force-push is explicitly gated in Task 7.
