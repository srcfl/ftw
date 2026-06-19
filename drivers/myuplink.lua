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
