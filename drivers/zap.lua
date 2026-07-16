-- zap.lua
-- Sourceful Zap — multi-device energy gateway with P1 meter, Modbus TCP
-- inverters/batteries and local JSON API. This driver reads the P1 grid
-- meter exposed over /api/devices/<p1-sn>/data/json and emits it as the
-- site meter, and aggregates PV generation from any inverter the Zap is
-- talking to (device_type=="inverter" with an enabled pv DER).
-- Emits: Meter, PV (read-only)
-- Protocol: HTTP (local JSON API on the Zap gateway)
--
-- Discovery:
--   The Zap advertises itself via mDNS as `zap.local`. On Linux the OS
--   resolver handles that transparently as long as nss-mdns / avahi is
--   installed (it is on RPi OS). If zap.local doesn't resolve on the
--   operator's network — router blocks mDNS, DNS rebinding filter, or
--   the EMS is on a different VLAN — set `host` to the device's LAN IP.
--
--   On the first poll the driver walks GET /api/devices once and picks:
--     - the first `p1_uart` entry as the P1 meter (site meter)
--     - every `device_type=="inverter"` entry with an enabled `pv` DER
--       as a PV source; W values across them are summed.
--   Override the P1 via config.serial if you have several; the PV set is
--   always auto-detected (add/remove an inverter → restart the driver).
--
-- Config example (config.yaml):
--   drivers:
--     - name: zap
--       lua: drivers/zap.lua
--       is_site_meter: true
--       capabilities:
--         http:
--           allowed_hosts: ["zap.local"]  # or the LAN IP
--       config:
--         host: "zap.local"          # default; override with IP if mDNS fails
--         # serial: "p1m-xxxxxxxx"   # optional; auto-detected when omitted
--
-- Sign convention (SITE = positive W flows INTO the site):
--   meter.w: positive = importing from grid, negative = exporting
--   pv.w   : negative = generating (source).
-- The Zap already reports both in site convention directly:
--   P1 meter.W is positive when importing (verified against a house
--   drawing +208 W on L1, -62 W on L2, -179 W on L3 ⇒ net -33 W export).
--   Inverter pv.W is negative when generating (SolarEdge SE at 2745 W
--   of generation reports pv.W = -2745). So we pass both through
--   unchanged — no boundary sign flip.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "sourceful-zap",
  name         = "Sourceful Zap",
  manufacturer = "Sourceful",
  version      = "1.1.0",
  protocols    = { "http" },
  capabilities = { "meter", "pv" },
  description  = "Sourceful Zap local-JSON gateway: P1 grid meter + PV from any attached inverter.",
  homepage     = "https://sourceful.energy",
  authors      = { "FTW contributors" },
  http_hosts   = { "zap.local" },
  verification_status = "beta",
  verified_by = { "erikarenhill@fortytwo:3d" },
  verified_at = "2026-04-17",
  verification_notes = "P1 + PV aggregation verified against a live Sourceful Zap. Awaiting a second site to promote to production.",
  connection_defaults = {
    host = "zap.local",
  },
}

PROTOCOL = "http"

local zap_host = "zap.local"
local p1_serial = nil
local pv_serials = {}     -- list of inverter serials that carry an enabled pv DER
local discovered   = false  -- true once discover_devices has succeeded
                             -- (even with zero PV). Prevents re-hitting
                             -- /api/devices every poll on meter-only sites.
local disable_pv   = false  -- operator opt-out: when a site has a second
                             -- Zap used for metering only, set `disable_pv:
                             -- true` in config to skip PV discovery + emit.
                             -- The primary Zap keeps owning PV aggregation.

-- Backoff state for *discovery* (/api/devices) failures. Separate from
-- data-fetch failures: discovery backoff must NOT gate the site meter
-- data poll, or a transient /api/devices glitch silently stalls the
-- site meter for up to 60 s and trips the watchdog on every network
-- hiccup.
local discovery_last_attempt = 0
local discovery_backoff_ms   = 0
local DISCOVERY_BACKOFF_MIN  = 2000
local DISCOVERY_BACKOFF_MAX  = 60000

-- Rediscover after this many consecutive P1 data-fetch failures. The
-- P1 serial could rotate on a dev gateway reboot, so we fall back to a
-- fresh discovery if data keeps failing.
local p1_fail_count       = 0
local P1_FAIL_REDISCOVER  = 10

local function base_url()
    -- Accept both "zap.local" and "http://zap.local" styles.
    if string.sub(zap_host, 1, 7) == "http://" or string.sub(zap_host, 1, 8) == "https://" then
        return zap_host
    end
    return "http://" .. zap_host
end

-- Clamp obviously-bogus readings. The Zap passes through raw overflow
-- sentinels when a downstream device is offline (u16=65535, i16=32768/
-- -32768, u32=4294967295, and a couple of *100-scaled lookalikes for
-- currents). Treat any |v| above `max_abs` as "not available" → nil.
local function sane(v, max_abs)
    local n = tonumber(v)
    if n == nil then return nil end
    if max_abs and math.abs(n) > max_abs then return nil end
    return n
end

-- Walk GET /api/devices and return:
--   (p1_serial, pv_serials[], err)
-- p1_serial is the first `p1_uart` device; pv_serials is every inverter
-- with at least one enabled `pv` DER, in the order the Zap returns them.
-- Does not log — the caller owns that so success and failure messages
-- stay co-located with the backoff / retry logic.
local function discover_devices()
    local url = base_url() .. "/api/devices"
    local body, err = host.http_get(url)
    if err then
        return nil, nil, err
    end
    local data = host.json_decode(body)
    if not data or not data.devices then
        return nil, nil, "unexpected payload (no `devices` array)"
    end
    local p1 = nil
    local pvs = {}
    for _, dev in ipairs(data.devices) do
        if not dev.sn then
            -- nothing to key on; skip
        elseif dev.type == "p1_uart" and not p1 then
            p1 = dev.sn
        elseif dev.device_type == "inverter" and type(dev.ders) == "table" then
            for _, der in ipairs(dev.ders) do
                if der.type == "pv" and der.enabled then
                    pvs[#pvs + 1] = dev.sn
                    break
                end
            end
        end
    end
    if not p1 then
        return nil, nil, "no p1_uart device found"
    end
    return p1, pvs, nil
end

-- Bump the discovery backoff exponentially (cap at MAX). Called whenever
-- discovery or the site-meter data poll fails so we don't spam the network.
local function bump_backoff()
    if discovery_backoff_ms == 0 then
        discovery_backoff_ms = DISCOVERY_BACKOFF_MIN
    else
        discovery_backoff_ms = math.min(discovery_backoff_ms * 2, DISCOVERY_BACKOFF_MAX)
    end
    discovery_last_attempt = host.millis()
end

local function clear_backoff()
    discovery_backoff_ms = 0
    discovery_last_attempt = 0
end

local function in_backoff()
    if discovery_backoff_ms == 0 then return false end
    return (host.millis() - discovery_last_attempt) < discovery_backoff_ms
end

-- Fetch /api/devices/<sn>/data/json, return the decoded table or (nil, err).
local function fetch_device(sn)
    local url = base_url() .. "/api/devices/" .. sn .. "/data/json"
    local body, err = host.http_get(url)
    if err then return nil, err end
    local data = host.json_decode(body)
    if not data then return nil, "json decode failed" end
    return data, nil
end

----------------------------------------------------------------------------
-- Fingerprint
----------------------------------------------------------------------------

-- driver_fingerprint(target) — passive HTTP probe for
-- /api/drivers/fingerprint and /api/scan?fingerprint=1. GETs the Zap's
-- local JSON device list and matches on its signature shape. Read-only;
-- uses target.base_url (the scanned endpoint) rather than the configured
-- zap_host, since driver_init has not run. Tri-state:
--   true  → GET <base>/api/devices returned the Zap's {devices=[…]} shape
--   false → :80 answered HTTP, but it isn't the Zap device API
--   nil    → no usable HTTP response (port closed, timeout) → inconclusive
function driver_fingerprint(target)
    local base = target and target.base_url
    if not base or base == "" then
        return nil
    end
    local body, err = host.http_get(base .. "/api/devices")
    if err or not body then
        return nil
    end
    local data = host.json_decode(body)
    if type(data) ~= "table" or type(data.devices) ~= "table" then
        return false -- answered HTTP, but not the Zap device API
    end
    -- A Zap device entry carries `sn` plus a `type` / `device_type`. Latch
    -- the P1 serial as the gateway identity when present.
    local serial = nil
    local recognised = false
    for _, dev in ipairs(data.devices) do
        if type(dev) == "table" and dev.sn and (dev.type or dev.device_type) then
            recognised = true
            if dev.type == "p1_uart" then
                serial = dev.sn
                break
            end
        end
    end
    if not recognised then
        return false -- generic/empty devices JSON is not a Zap signature
    end
    return true, { make = "Sourceful", model = "Zap", serial = serial or "", confidence = 0.95 }
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Sourceful")

    if config and type(config.host) == "string" and config.host ~= "" then
        zap_host = config.host
    end
    if config and type(config.serial) == "string" and config.serial ~= "" then
        p1_serial = config.serial
        host.set_sn(p1_serial)
        host.log("info", "Zap: using pinned P1 serial " .. p1_serial)
    end
    if config and config.disable_pv then
        disable_pv = true
        host.log("info", "Zap: PV aggregation disabled via config")
    end

    host.log("info", "Zap: driver initialized (host=" .. zap_host .. ")")
end

function driver_poll()
    -- Phase 1: discover P1 + PV inverters once. Re-run only if a flag
    -- explicitly invalidates the cached set (e.g. persistent P1-fetch
    -- failures re-trigger it below). For meter-only Zap deploys, this
    -- means we don't re-hit /api/devices every poll logging "no PV
    -- inverters found" — discovery latches after the first success.
    if not discovered then
        if in_backoff() then
            return 1000
        end
        local p1, pvs, err = discover_devices()
        if err then
            bump_backoff()
            host.log("warn", "Zap: discovery failed: " .. tostring(err)
                .. " (retry in " .. discovery_backoff_ms .. "ms)")
            return 1000
        end
        if not p1_serial then
            p1_serial = p1
            host.set_sn(p1_serial)
            host.log("info", "Zap: discovered P1 meter " .. p1_serial)
        end
        if disable_pv then
            pv_serials = {}
            host.log("info", "Zap: PV inverters ignored (disable_pv=true)")
        else
            pv_serials = pvs
            if #pv_serials > 0 then
                host.log("info", "Zap: discovered " .. #pv_serials
                    .. " PV inverter(s): " .. table.concat(pv_serials, ","))
            else
                host.log("info", "Zap: no PV inverters found")
            end
        end
        discovered = true
        clear_backoff()
    end

    -- Phase 2: poll the P1 meter.
    -- P1 fetch failures get their OWN counter — they must NOT gate the
    -- discovery backoff (which applies to /api/devices only). A transient
    -- HTTP glitch should just log and let the next 1 s tick retry; after
    -- N consecutive failures we invalidate the cached discovery and
    -- fall back to /api/devices in case the Zap rebooted / the P1 serial
    -- rotated.
    local data, err = fetch_device(p1_serial)
    if err then
        p1_fail_count = p1_fail_count + 1
        host.log("warn", "Zap: P1 data fetch failed (" .. p1_fail_count
            .. "/" .. P1_FAIL_REDISCOVER .. "): " .. tostring(err))
        if p1_fail_count >= P1_FAIL_REDISCOVER then
            host.log("warn", "Zap: repeated P1 failures — will re-discover devices")
            p1_serial = nil
            pv_serials = {}
            discovered = false
            p1_fail_count = 0
        end
        return 1000
    end
    if not data.meter then
        p1_fail_count = p1_fail_count + 1
        host.log("warn", "Zap: P1 payload missing `meter` block")
        return 1000
    end
    p1_fail_count = 0

    local m = data.meter
    local meter = {
        w         = tonumber(m.W)    or 0,
        l1_w      = tonumber(m.L1_W) or 0,
        l2_w      = tonumber(m.L2_W) or 0,
        l3_w      = tonumber(m.L3_W) or 0,
        l1_v      = tonumber(m.L1_V) or 0,
        l2_v      = tonumber(m.L2_V) or 0,
        l3_v      = tonumber(m.L3_V) or 0,
        l1_a      = tonumber(m.L1_A) or 0,
        l2_a      = tonumber(m.L2_A) or 0,
        l3_a      = tonumber(m.L3_A) or 0,
        import_wh = tonumber(m.total_import_Wh) or 0,
        export_wh = tonumber(m.total_export_Wh) or 0,
    }

    host.emit("meter", meter)
    host.emit_metric("meter_l1_w", meter.l1_w)
    host.emit_metric("meter_l2_w", meter.l2_w)
    host.emit_metric("meter_l3_w", meter.l3_w)
    host.emit_metric("meter_l1_v", meter.l1_v)
    host.emit_metric("meter_l2_v", meter.l2_v)
    host.emit_metric("meter_l3_v", meter.l3_v)
    host.emit_metric("meter_l1_a", meter.l1_a)
    host.emit_metric("meter_l2_a", meter.l2_a)
    host.emit_metric("meter_l3_a", meter.l3_a)

    -- Phase 3: aggregate PV across every inverter the Zap exposes.
    -- Each inverter has an independent data endpoint; we sum W and
    -- generation energy, and emit diagnostics per inverter (the structured
    -- `pv` reading on the telemetry store is a single slot, so combining
    -- them is the only way both inverters show up). PV fetch failures
    -- don't reset the meter's backoff — the site meter is load-bearing,
    -- PV is nice-to-have; we just log debug and skip that inverter.
    if #pv_serials > 0 then
        local total_w = 0
        local total_gen_wh = 0
        local any = false
        for _, sn in ipairs(pv_serials) do
            local pvdata, perr = fetch_device(sn)
            if perr or not pvdata or not pvdata.pv then
                host.log("debug", "Zap: PV fetch failed for "
                    .. sn .. ": " .. tostring(perr or "missing pv block"))
            else
                any = true
                local pv = pvdata.pv
                -- Rated cap gives us a sanity bound for the W reading.
                -- The Zap emits overflow sentinels when the inverter is
                -- offline (i16 32768, u32 huge values); anything above
                -- 10× rated or otherwise clearly bogus → treat as 0.
                local rated = tonumber(pv.rated_power_W) or 0
                -- When the inverter doesn't report a rated power, fall back
                -- to 16 A × 3 φ × 230 V ≈ 11 kW — a residential upper bound.
                -- A looser cap (e.g. 1 MW) would let a single bogus reading
                -- from an offline inverter propagate into site PV and swing
                -- the control loop into aggressive charge.
                local cap = rated > 0 and (rated * 10) or 11040
                local w = sane(pv.W, cap) or 0
                total_w = total_w + w
                local gen = sane(pv.total_generation_Wh, 1e12) or 0
                total_gen_wh = total_gen_wh + gen

                -- Per-inverter diagnostics into the TS DB. For multi-inverter
                -- setups tag the serial onto each metric name so they don't
                -- collide; for single-inverter sites the outer aggregate
                -- emission below covers `pv_w` and we only need the extra
                -- MPPT / heatsink detail here.
                local multi = (#pv_serials > 1)
                local tag = multi and ("_" .. sn) or ""
                if multi then host.emit_metric("pv_w" .. tag, w) end
                local hs = sane(pv.heatsink_C, 150)
                if hs then host.emit_metric("pv_heatsink_c" .. tag, hs) end
                local m1v = sane(pv.mppt1_V, 1500)
                if m1v then host.emit_metric("pv_mppt1_v" .. tag, m1v) end
                local m1a = sane(pv.mppt1_A, 50)
                if m1a then host.emit_metric("pv_mppt1_a" .. tag, m1a) end
                local m2v = sane(pv.mppt2_V, 1500)
                if m2v then host.emit_metric("pv_mppt2_v" .. tag, m2v) end
                local m2a = sane(pv.mppt2_A, 50)
                if m2a then host.emit_metric("pv_mppt2_a" .. tag, m2a) end
            end
        end
        if any then
            -- The Zap already reports pv.W as negative when the inverter
            -- is generating — matches site convention. Pass through as-is.
            host.emit("pv", { w = total_w, lifetime_wh = total_gen_wh })
            host.emit_metric("pv_w", total_w)
        end
    end

    return 1000
end

----------------------------------------------------------------------------
-- Control (READ-ONLY — Zap exposes no writable endpoint)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end
    host.log("warn", "Zap: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only: nothing to revert.
end

function driver_cleanup()
    p1_serial = nil
    pv_serials = {}
    discovered = false
    p1_fail_count = 0
    clear_backoff()
end
