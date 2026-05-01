-- MyUplink Heat Pump Driver
-- Emits: battery (thermal storage faked as a battery so the MPC can block
--         consumption at expensive prices)
-- Protocol: HTTPS (MyUplink Cloud REST API v2)
--
-- DESIGN — "thermal battery" trick
-- =================================
-- The MPC only dispatches to drivers it believes hold battery capacity.
-- A heat pump has no battery_capacity_wh, so the MPC ignores it.
--
-- Trick: pretend each thermal store (hot water tank + heating buffer) IS
-- a battery. The MPC is then free to "discharge" it — which the driver
-- translates to "block the pump / lower the setpoint" so the compressor
-- doesn't run during expensive hours.
--
-- Key constraints that make this safe:
--
--   max_charge_w = 0    → MPC never asks the pump to run EXTRA to "charge"
--                         the thermal store. It only runs on its own schedule.
--
--   max_discharge_w = X → MPC can "discharge" up to X W.
--                         Driver maps discharge to: lower setpoint or block.
--
--   SoC = 1.0 always    → MPC always believes there is stored heat to shed,
--                         so it never refuses to discharge (i.e. block).
--                         Stays 1.0 because real thermal state is opaque —
--                         we don't know how many kWh remain in the tank.
--
-- In driver_command:
--   action="battery", power_w < 0  → MPC wants to discharge → block pump
--   action="battery", power_w = 0  → MPC releases block → pump runs freely
--   action="init" / "deinit"       → release any block
--
-- Two instances in config.yaml — one for hot water, one for heating:
--
--   drivers:
--     - name: myuplink-hw          # hot water tank
--       lua: drivers/myuplink.lua
--       battery_capacity_wh: 5000  # ~thermal capacity of a 200 L tank
--       config:
--         client_id: "..."
--         client_secret: "..."
--         mode: "hotwater"
--       capabilities:
--         http:
--           allowed_hosts: ["api.myuplink.com"]
--
--     - name: myuplink-heat        # space heating buffer
--       lua: drivers/myuplink.lua
--       battery_capacity_wh: 10000
--       config:
--         client_id: "..."
--         client_secret: "..."
--         mode: "heating"
--       capabilities:
--         http:
--           allowed_hosts: ["api.myuplink.com"]
--
-- IMPORTANT: set max_charge_w: 0 per driver in config.yaml so the MPC
-- never tries to pre-heat on cheap electricity (not implemented here).
-- For NIBE: find your exact parameter IDs via
--   GET https://api.myuplink.com/v2/devices/{deviceId}/points

DRIVER = {
  id           = "myuplink",
  name         = "MyUplink Heat Pump",
  manufacturer = "MyUplink (NIBE, Bosch, Atlantic, Daikin, ...)",
  version      = "2.0.0",
  protocols    = { "http" },
  capabilities = { "battery", "apicreds" },
  description  = "Heat pump via MyUplink Cloud REST API v2. Fakes thermal stores as batteries so the MPC can block consumption during expensive price hours. Never charges — only blocks.",
  homepage     = "https://dev.myuplink.com",
  http_hosts   = { "api.myuplink.com" },
  authors      = { "forty-two-watts contributors" },
  tested_models = { "NIBE F1145", "NIBE S1255", "NIBE F730" },
  verification_status = "experimental",
}

PROTOCOL = "http"

local BASE_URL = "https://api.myuplink.com"

local access_token     = nil
local token_expires_at = 0

local client_id     = nil
local client_secret = nil
local device_id     = nil
local mode          = "hotwater"   -- "hotwater" | "heating"

local block_active = false

-- Parameter IDs (NIBE defaults, overridable via config)
local PARAM_POWER        = "10012"  -- compressor power (W)
local PARAM_HW_TEMP      = "40013"  -- BT6 hot water top temp (read)
local PARAM_HW_STOP      = "47044"  -- HW stop comfort temperature (write)
local PARAM_INDOOR_TEMP  = "40033"  -- BT50 room temperature (read)
local PARAM_HEAT_OFFSET  = "47398"  -- heating curve offset °C (write)
local PARAM_OUTDOOR_TEMP = "40004"  -- BT1 outdoor temperature (read)

-- How aggressively to block each mode
local HW_BLOCK_DROP_C   = 10   -- lower HW stop temp by this many °C to block
local HEAT_BLOCK_DROP_C = -5   -- add this to heating curve offset to block

-- Saved original values so we can restore exactly
local original_hw_stop     = nil
local original_heat_offset = nil

-- ---- Auth ----------------------------------------------------------------

local function fetch_token()
    local body = "grant_type=client_credentials"
        .. "&client_id=" .. client_id
        .. "&client_secret=" .. client_secret
        .. "&scope=READSYSTEM%20WRITESYSTEM"
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

local function write_point(param_id, value)
    if not ensure_auth() then return false end
    local body = host.json_encode({ { parameterId = param_id, value = tostring(value) } })
    local _, err = host.http_patch(
        BASE_URL .. "/v2/devices/" .. device_id .. "/points",
        body, auth_headers())
    if err then
        host.log("warn", "MyUplink: write " .. param_id .. "=" .. tostring(value)
            .. " failed: " .. tostring(err))
        return false
    end
    return true
end

local function decode_temp(pt)
    if not pt then return nil end
    local raw = tonumber(pt.value)
    if not raw then return nil end
    if math.abs(raw) > 100 then return raw / 10 end  -- NIBE °C×10 encoding
    return raw
end

-- ---- Block / release -----------------------------------------------------

local function block_hotwater()
    if original_hw_stop == nil then
        local pts, err = fetch_points({ PARAM_HW_STOP })
        if err or not pts[PARAM_HW_STOP] then
            host.log("warn", "MyUplink: could not read HW stop temp before blocking")
            return false
        end
        original_hw_stop = tonumber(pts[PARAM_HW_STOP].value)
    end
    local blocked = (original_hw_stop or 55) - HW_BLOCK_DROP_C
    host.log("info", "MyUplink: blocking HW — stop temp "
        .. tostring(original_hw_stop) .. "→" .. tostring(blocked) .. "°C")
    return write_point(PARAM_HW_STOP, blocked)
end

local function release_hotwater()
    if original_hw_stop == nil then return true end
    host.log("info", "MyUplink: releasing HW block — restoring stop temp to "
        .. tostring(original_hw_stop) .. "°C")
    local ok = write_point(PARAM_HW_STOP, original_hw_stop)
    if ok then original_hw_stop = nil end
    return ok
end

local function block_heating()
    if original_heat_offset == nil then
        local pts, err = fetch_points({ PARAM_HEAT_OFFSET })
        if err or not pts[PARAM_HEAT_OFFSET] then
            host.log("warn", "MyUplink: could not read heat offset before blocking")
            return false
        end
        original_heat_offset = tonumber(pts[PARAM_HEAT_OFFSET].value) or 0
    end
    local blocked = original_heat_offset + HEAT_BLOCK_DROP_C
    host.log("info", "MyUplink: blocking heating — curve offset "
        .. tostring(original_heat_offset) .. "→" .. tostring(blocked) .. "°C")
    return write_point(PARAM_HEAT_OFFSET, blocked)
end

local function release_heating()
    if original_heat_offset == nil then return true end
    host.log("info", "MyUplink: releasing heating block — restoring offset to "
        .. tostring(original_heat_offset) .. "°C")
    local ok = write_point(PARAM_HEAT_OFFSET, original_heat_offset)
    if ok then original_heat_offset = nil end
    return ok
end

local function apply_block()
    if mode == "hotwater" then return block_hotwater() else return block_heating() end
end

local function release_block_fn()
    if mode == "hotwater" then return release_hotwater() else return release_heating() end
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
    if config and config.mode and config.mode ~= "" then mode = config.mode end

    if config then
        local function ov(k, d) return (config[k] and config[k] ~= "") and config[k] or d end
        PARAM_POWER        = ov("param_power_id",        PARAM_POWER)
        PARAM_HW_TEMP      = ov("param_hw_temp_id",      PARAM_HW_TEMP)
        PARAM_HW_STOP      = ov("param_hw_stop_id",      PARAM_HW_STOP)
        PARAM_INDOOR_TEMP  = ov("param_indoor_temp_id",  PARAM_INDOOR_TEMP)
        PARAM_HEAT_OFFSET  = ov("param_heat_offset_id",  PARAM_HEAT_OFFSET)
        PARAM_OUTDOOR_TEMP = ov("param_outdoor_temp_id", PARAM_OUTDOOR_TEMP)
        if tonumber(config.hw_block_drop_c)   then HW_BLOCK_DROP_C   = tonumber(config.hw_block_drop_c)   end
        if tonumber(config.heat_block_drop_c) then HEAT_BLOCK_DROP_C = tonumber(config.heat_block_drop_c) end
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

    host.set_sn(device_id .. "-" .. mode)
    host.log("info", "MyUplink: ready — mode=" .. mode .. " device=" .. device_id)
end

function driver_poll()
    if not device_id or not client_id then return 30000 end
    if not ensure_auth() then return 30000 end

    local pts, err = fetch_points({ PARAM_POWER, PARAM_HW_TEMP, PARAM_INDOOR_TEMP, PARAM_OUTDOOR_TEMP })
    if err then
        host.log("warn", "MyUplink: poll failed: " .. err)
        return 30000
    end

    local power_w = 0
    if pts[PARAM_POWER] then
        local raw = tonumber(pts[PARAM_POWER].value) or 0
        power_w = (pts[PARAM_POWER].unit == "kW") and raw * 1000 or raw
    end

    -- Emit as battery:
    --   w   = compressor power right now (positive = consuming = "charging")
    --   soc = always 1.0 so MPC always considers discharge (blocking) available
    --
    -- The MPC will never send charge commands because max_charge_w = 0
    -- in config.yaml. It will only send discharge (block) or idle (release).
    host.emit("battery", {
        w   = power_w,
        soc = 1.0,
    })

    host.emit_metric("hp_" .. mode .. "_power_w",  power_w)
    host.emit_metric("hp_" .. mode .. "_blocked",  block_active and 1 or 0)
    if pts[PARAM_HW_TEMP]     then host.emit_metric("hp_hw_top_temp_c",  decode_temp(pts[PARAM_HW_TEMP])     or 0) end
    if pts[PARAM_INDOOR_TEMP] then host.emit_metric("hp_indoor_temp_c",  decode_temp(pts[PARAM_INDOOR_TEMP]) or 0) end
    if pts[PARAM_OUTDOOR_TEMP]then host.emit_metric("hp_outdoor_temp_c", decode_temp(pts[PARAM_OUTDOOR_TEMP])or 0) end

    host.log("debug", string.format("MyUplink [%s]: %.0f W  blocked=%s",
        mode, power_w, tostring(block_active)))

    return 60000
end

function driver_command(action, power_w, _cmd)
    if not device_id or not ensure_auth() then return false end

    if action == "init" or action == "deinit" then
        if block_active then
            local ok = release_block_fn()
            if ok then block_active = false end
        end
        return true
    end

    if action == "battery" then
        -- power_w < 0 = MPC wants to "discharge" = block the pump
        -- power_w = 0 = MPC releases = let pump run on its own schedule
        -- power_w > 0 = MPC wants to "charge" = should never happen
        --               (max_charge_w=0 in config), but we guard it anyway
        local want_block = (power_w or 0) < 0

        if want_block and not block_active then
            block_active = apply_block()
            return block_active
        elseif not want_block and block_active then
            local ok = release_block_fn()
            if ok then block_active = false end
            return ok
        end
        return true  -- already in the right state
    end

    return false
end

function driver_default_mode()
    -- Watchdog: EMS offline — always release block.
    -- Never leave a building without heat because the EMS crashed.
    if block_active and device_id and client_id then
        if ensure_auth() then
            release_block_fn()
            block_active = false
        end
    end
end

function driver_cleanup()
    driver_default_mode()
    access_token     = nil
    token_expires_at = 0
end
