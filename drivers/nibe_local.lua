-- NIBE S-series Heat Pump — LOCAL REST API driver (READ-ONLY telemetry)
-- Emits: metrics only (compressor power, energy meters, temperatures, …)
--        into the long-format TS DB via host.emit_metric. NO control.
-- Protocol: HTTPS (NIBE "Local REST API", self-described at https://<ip>:8443/)
--
-- This is the local-network twin of drivers/myuplink.lua. Instead of the
-- MyUplink cloud (OAuth + internet round-trip), it reads the pump directly
-- over the LAN. The local API is RICHER than the cloud one: every point
-- ships its own metadata (modbus register, unit, exact divisor, writable
-- flag), so scaling is exact — no °C×10 heuristic. ~980 points come back in
-- one bulk GET. Headline metrics land every minute; the bulk map records
-- changes plus an hourly full snapshot so the TS DB stays bounded on a Pi.
--
-- Observe-only by design: the pump is left in read-only mode (aidMode=off),
-- so this driver cannot actuate anything and cannot cause harm.
--
-- Site sign convention: a heat pump is a LOAD. Its electrical draw would be
-- positive W flowing into the site at the grid boundary — but this driver
-- emits diagnostics via host.emit_metric only (never host.emit("meter"|…)),
-- so it performs NO sign conversion and never double-counts against the real
-- grid meter. The thermal/load models consume hp_power_w etc. as twins.
--
-- AUTH + TRANSPORT:
--   The local API uses HTTP Basic auth over HTTPS with a SELF-SIGNED
--   certificate. The system trust store can't validate it, so the driver
--   relies on certificate PINNING in the host: grant
--   capabilities.http.tls_pin_sha256 with the pump's cert fingerprint
--   (the "fingeravtryck" shown in the myUplink app, or from
--   `openssl s_client -connect <ip>:8443 | openssl x509 -fingerprint -sha256`).
--   That pins exactly one leaf cert — a swapped cert (MITM on the LAN, which
--   would otherwise capture the Basic-auth password) is rejected at the
--   handshake. Do NOT fall back to blanket insecure-skip-verify.
--
-- Config example (config.yaml):
--   drivers:
--     - name: nibe
--       lua: drivers/nibe_local.lua
--       config:
--         host: "192.168.1.180"
--         port: 8443
--         username: "<local-api-username>"
--         password: "<local-api-password>"   # masked via config_secrets
--         # device_id: "..."        # optional; auto-detected if omitted
--       capabilities:
--         http:
--           allowed_hosts: ["192.168.1.180:8443"]
--           tls_pin_sha256: "<64-hex-char certificate fingerprint>"
--
-- The four heating-UI headline metrics map to NIBE S735 variable ids by
-- default; override per model via param_power_id / param_hw_temp_id /
-- param_indoor_temp_id / param_outdoor_temp_id if yours differs (find them
-- in the bulk GET /api/v1/devices/<serial>/points).

DRIVER = {
  id           = "nibe-local",
  name         = "NIBE REST API S-series",
  manufacturer = "NIBE",
  version      = "1.0.0",
  protocols    = { "http" },
  capabilities = { "apicreds" },
  description  = "Read-only NIBE S-series heat-pump telemetry over the on-prem Local REST API (HTTPS + Basic auth, self-signed cert pinned via tls_pin_sha256). Emits compressor/used power, lifetime energy meters, and the full ~980-point register map. Observe-only — no control.",
  homepage     = "https://www.nibe.eu",
  authors      = { "HuggeK", "forty-two-watts contributors" },
  tested_models = { "NIBE S735" },
  verification_status = "beta",
  config_secrets = { "password" },
  connection_defaults = { port = 8443 },
}

PROTOCOL = "http"

-- ---- Runtime state -------------------------------------------------------

local base_url      = nil    -- https://<host>:<port>
local auth_value    = nil    -- "Basic <base64(user:pass)>"
local serial        = nil    -- device id (NIBE serial number) used in the path

-- Self-heal: the pump can be briefly unreachable at boot / after a network
-- blip. Rather than wedge on a nil serial (which needed a manual restart),
-- driver_poll retries device detection on this backoff.
local SETUP_RETRY_MS = 30000
local last_setup_ms  = nil
local POLL_INTERVAL_MS = 60000
-- Headline metrics are emitted every poll. The remaining ~980-point map is
-- change-only, with an hourly full refresh so latest-value timestamps stay
-- useful without writing ~1.4 million mostly-duplicate TS rows per day.
local FULL_REFRESH_MS = 3600000
local last_full_emit_ms = nil
local last_emitted = {}

-- ---- Headline metrics + per-model profiles -------------------------------
-- The BULK of telemetry is metadata-driven (every point self-describes its
-- unit + divisor), so reading any S-series pump needs NO per-model code. The
-- only model-specific knobs are the handful of STABLE headline aliases
-- (hp_power_w, hp_outdoor_temp_c, …) that web/heating.js + the thermal twin
-- read by fixed name. Each maps to a local-API variableId, resolved per pump
-- in priority order: explicit config override > model profile > generic
-- S-series default.

-- Logical headline -> { config override key, emitted metric name, watts? }.
local HEADLINES = {
    { key = "power",   cfg = "param_power_id",           name = "hp_power_w",            watts = true },
    { key = "used",    cfg = "param_used_id",            name = "hp_used_power_w",       watts = true },
    { key = "hw",      cfg = "param_hw_temp_id",         name = "hp_hw_top_temp_c" },
    { key = "indoor",  cfg = "param_indoor_temp_id",     name = "hp_indoor_temp_c" },
    { key = "outdoor", cfg = "param_outdoor_temp_id",    name = "hp_outdoor_temp_c" },
    { key = "econs",   cfg = "param_energy_consumed_id", name = "hp_energy_consumed_kwh" },
    { key = "eprod",   cfg = "param_energy_produced_id", name = "hp_energy_produced_kwh" },
    { key = "dm",      cfg = "param_degree_minutes_id",  name = "hp_degree_minutes" },
}

-- Per-model headline variable-id profiles, auto-selected from the pump's
-- product.name / firmwareId (GET /api/v1/devices), matched case-insensitively
-- as a substring. The S-series shares the core register ids, so `default`
-- covers the whole family (verified on an S735); add a model entry ONLY when
-- a specific model is confirmed to renumber a headline. A profile may set
-- just the keys that differ — the rest fall back to default.
local PROFILES = {
    default = {  -- generic NIBE S-series (verified: S735)
        power = "1801", used = "22130", hw = "11", indoor = "158",
        outdoor = "4",  econs = "28393", eprod = "28392", dm = "781",
    },
    -- Example — uncomment + verify the ids against GET …/points before use:
    -- ["s320"] = { power = "1801", hw = "11" },
}

local CANON           = {}   -- id(string) -> { name = "...", watts = bool }
local driver_config   = nil  -- kept so CANON can be rebuilt once the model is known
local device_model    = nil  -- product.name reported by the pump (may be "" / nil)
local device_firmware = nil  -- product.firmwareId (e.g. "nibe-n")

-- ---- Helpers -------------------------------------------------------------

-- Pure-Lua base64 (no host builtin). Used once per init to build the
-- Basic-auth header value.
local b64chars = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'
local function base64_encode(data)
    return ((data:gsub('.', function(x)
        local r, b = '', x:byte()
        for i = 8, 1, -1 do r = r .. (b % 2 ^ i - b % 2 ^ (i - 1) > 0 and '1' or '0') end
        return r
    end) .. '0000'):gsub('%d%d%d?%d?%d?%d?', function(x)
        if #x < 6 then return '' end
        local c = 0
        for i = 1, 6 do c = c + (x:sub(i, i) == '1' and 2 ^ (6 - i) or 0) end
        return b64chars:sub(c + 1, c + 1)
    end) .. ({ '', '==', '=' })[#data % 3 + 1])
end

-- The "not connected" sentinel a NIBE variable reports per size. An
-- unconnected sensor returns this (e.g. an absent BT50 room sensor is
-- -32768 for s16) — and the API marks it isOk=true anyway, so we filter
-- by size, not by isOk.
local function size_sentinel(size)
    if size == "s8"  then return -128 end
    if size == "s16" then return -32768 end
    if size == "s32" then return -2147483648 end
    if size == "u8"  then return 255 end
    if size == "u16" then return 65535 end
    if size == "u32" then return 4294967295 end
    return nil
end

-- Turn a point title into a stable hp_ snake_case metric name. NIBE titles
-- embed soft hyphens (U+00AD = bytes 0xC2 0xAD) inside long words
-- ("Compres­sor", "Instant­aneous"); strip them first so the name reads
-- "compressor", not "compres_sor". Remaining non-ASCII / punctuation
-- collapses to single underscores. Empty falls back to the id.
local function sanitize_metric_name(title, id)
    local s = title or ""
    s = string.gsub(s, "\194\173", "")        -- soft hyphen
    s = string.lower(s)
    s = string.gsub(s, "[^a-z0-9]+", "_")
    s = string.gsub(s, "^_+", "")
    s = string.gsub(s, "_+$", "")
    if s == "" then s = "p" .. tostring(id) end
    return "hp_" .. s
end

-- Watts normalisation for the power headline metrics: some models report
-- compressor power in kW, others in W. Emit W either way.
local function to_watts(value, unit)
    if unit == "kW" then return value * 1000.0, "W" end
    return value, (unit ~= "" and unit or "W")
end

-- The NIBE Modbus register id for a point (metadata.modbusRegisterID), formatted
-- as a string for host.emit_metric's optional 4th (register) arg. Surfaced in
-- the per-driver "all signals" detail view so each signal shows its source
-- register. 0 / absent means the point has no Modbus mapping (menu-only) — emit
-- "" so the column stays blank rather than showing a misleading "0".
local function register_str(m)
    local r = tonumber(m.modbusRegisterID)
    if r and r ~= 0 then return string.format("%d", r) end
    return ""
end

-- Validate and scale one API point. Returns nil for malformed values and the
-- per-size "not connected" sentinel.
local function scaled_point(pt)
    local m = pt and pt.metadata
    local v = pt and pt.value
    if type(m) ~= "table" or type(v) ~= "table" or type(v.integerValue) ~= "number" then
        return nil
    end
    local raw = v.integerValue
    local sentinel = size_sentinel(m.variableSize)
    if sentinel and raw == sentinel then return nil end
    local div = tonumber(m.divisor) or 1
    if div == 0 then div = 1 end
    return raw / div, m.unit or "", register_str(m)
end

-- Build the id -> canonical-metric lookup. Each headline's id is resolved
-- explicit config override > model profile > generic default.
local function build_canon(profile, config)
    config = config or {}
    profile = profile or PROFILES.default
    local function s(v) return (v ~= nil and v ~= "") and tostring(v) or nil end
    CANON = {}
    for _, h in ipairs(HEADLINES) do
        local id = s(config[h.cfg]) or s(profile[h.key]) or s(PROFILES.default[h.key])
        if id then CANON[id] = { name = h.name, watts = h.watts } end
    end
end

-- Pick the model profile whose key appears (case-insensitively, as a
-- substring) in the pump's product.name or firmwareId. Falls back to the
-- generic default, which covers the whole S-series.
local function select_profile(model_name, firmware_id)
    local n = string.lower(model_name or "")
    local f = string.lower(firmware_id or "")
    for k, prof in pairs(PROFILES) do
        if k ~= "default" then
            if (n ~= "" and string.find(n, k, 1, true)) or
               (f ~= "" and string.find(f, k, 1, true)) then
                return k, prof
            end
        end
    end
    return "default", PROFILES.default
end

local function auth_headers()
    return { Authorization = auth_value, Accept = "application/json" }
end

local function api_get(path)
    local resp, err = host.http_get(base_url .. path, auth_headers())
    if err then return nil, tostring(err) end
    local data = host.json_decode(resp)
    if not data then return nil, "json decode failed" end
    return data, nil
end

-- ---- Setup ---------------------------------------------------------------

local function detect_serial()
    local data, err = api_get("/api/v1/devices")
    if err then
        host.log("warn", "NIBE: /api/v1/devices failed: " .. err)
        return nil
    end
    local devs = data.devices
    if type(devs) == "table" and devs[1] and devs[1].product then
        local p = devs[1].product
        device_model    = p.name
        device_firmware = p.firmwareId
        if p.serialNumber and p.serialNumber ~= "" then
            host.log("info", "NIBE: detected " .. tostring(p.manufacturer) ..
                " '" .. tostring(p.name) .. "' " .. tostring(p.serialNumber) ..
                " (fw " .. tostring(p.firmwareId) .. ")")
            return p.serialNumber
        end
    end
    host.log("error", "NIBE: no device serial in /api/v1/devices response")
    return nil
end

-- Bring the driver to "ready" (serial known). Safe to call repeatedly;
-- rate-limited by SETUP_RETRY_MS. Returns true once serial is established.
local function try_setup()
    if serial then return true end
    local now = host.millis()
    if last_setup_ms ~= nil and (now - last_setup_ms) < SETUP_RETRY_MS then
        return false
    end
    last_setup_ms = now
    serial = detect_serial()
    if not serial then return false end
    host.set_sn(serial)
    -- Now that the model is known, refine the headline ids (config overrides
    -- still win inside build_canon).
    local pkey, prof = select_profile(device_model, device_firmware)
    build_canon(prof, driver_config)
    host.log("info", "NIBE: ready (read-only) serial=" .. serial .. " profile=" .. pkey)
    return true
end

-- ---- Lifecycle -----------------------------------------------------------

function driver_init(config)
    host.set_make("NIBE")
    config = config or {}

    local function s(v) return (v ~= nil and v ~= "") and tostring(v) or nil end
    local username = s(config.username) or ""
    local password = s(config.password) or ""
    serial         = s(config.device_id)

    -- base_url override exists for tests; production builds it from host:port.
    base_url = s(config.base_url)
    if not base_url then
        local host_ip = s(config.host)
        local port    = s(config.port) or "8443"
        if host_ip then base_url = "https://" .. host_ip .. ":" .. port end
    end
    auth_value = "Basic " .. base64_encode(username .. ":" .. password)

    if config.poll_interval_ms ~= nil then
        POLL_INTERVAL_MS = tonumber(config.poll_interval_ms) or POLL_INTERVAL_MS
    end
    if config.setup_retry_ms ~= nil then
        SETUP_RETRY_MS = tonumber(config.setup_retry_ms) or SETUP_RETRY_MS
    end
    if config.full_refresh_ms ~= nil then
        FULL_REFRESH_MS = tonumber(config.full_refresh_ms) or FULL_REFRESH_MS
    end

    -- Build the headline lookup with the generic profile now; try_setup
    -- refines it to the detected model's profile once the pump answers.
    -- Config overrides (param_*_id) win in build_canon either way.
    driver_config = config
    build_canon(PROFILES.default, config)

    if not base_url then
        host.log("error", "NIBE: 'host' (pump IP) is required")
        return
    end
    if username == "" or password == "" then
        host.log("error", "NIBE: username and password are required")
        return
    end

    host.set_poll_interval(POLL_INTERVAL_MS)
    -- Best-effort initial detection; driver_poll self-heals if it fails.
    if not try_setup() then
        host.log("warn", "NIBE: initial setup did not complete — will retry automatically")
    end
end

function driver_poll()
    if not base_url then return SETUP_RETRY_MS end
    if not serial then
        if not try_setup() then return SETUP_RETRY_MS end
    end

    local data, err = api_get("/api/v1/devices/" .. serial .. "/points")
    if err then
        host.log("warn", "NIBE: points poll failed: " .. err)
        return POLL_INTERVAL_MS
    end

    local now = host.millis()
    local full_refresh = last_full_emit_ms == nil or (now - last_full_emit_ms) >= FULL_REFRESH_MS

    -- Titles are not guaranteed unique. Count sanitized names first so every
    -- collision gets an id suffix deterministically; the old one-pass `seen`
    -- approach made whichever point happened to appear first change names
    -- across polls because Lua table iteration order is unspecified.
    local name_counts = {}
    for id, pt in pairs(data) do
        if not CANON[tostring(id)] then
            local scaled = scaled_point(pt)
            if scaled ~= nil then
                local base = sanitize_metric_name(pt.title, id)
                name_counts[base] = (name_counts[base] or 0) + 1
            end
        end
    end

    for id, pt in pairs(data) do
        local scaled, unit, reg = scaled_point(pt)
        if scaled ~= nil then
            local canon = CANON[tostring(id)]
            local name = canon and canon.name or sanitize_metric_name(pt.title, id)
            if not canon and (name_counts[name] or 0) > 1 then
                name = name .. "_" .. tostring(id)
            end
            local value = scaled
            if canon and canon.watts then value, unit = to_watts(scaled, unit) end

            -- Stable headline series retain one-minute resolution. The bulk
            -- map records transitions plus an hourly complete snapshot.
            if canon or full_refresh or last_emitted[name] ~= value then
                host.emit_metric(name, value, unit, reg, pt.title)
                last_emitted[name] = value
            end
        end
    end
    if full_refresh then last_full_emit_ms = now end
    -- Guarantees watchdog freshness even on a model whose configured headline
    -- ids are absent and whose non-headline values remain unchanged.
    host.emit_metric("hp_poll_ok", 1, "")

    return POLL_INTERVAL_MS
end

function driver_command(_action, _power_w, _cmd)
    -- Read-only: no actuation. The pump stays in aidMode=off.
    return false
end

function driver_default_mode()
    -- Read-only: nothing to release.
end

function driver_cleanup()
    serial       = nil
    last_setup_ms = nil
    last_full_emit_ms = nil
    last_emitted = {}
end
