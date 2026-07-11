-- heishamon.lua
-- Panasonic Aquarea heat pump driver via Heishamon (MQTT).
-- Emits: heat pump telemetry via metrics. Supports heat curve offset control.
--
-- Heishamon communicates over MQTT using topic prefix panasonic_heat_pump/
-- Read topics:  panasonic_heat_pump/main/TOP<n>
-- Write topics: panasonic_heat_pump/set/<name>
--
-- This driver controls Zone 1 heat curve offset (Z1_Heat_Request_Temp)
-- in the range -3..+3 °C relative to the configured heat curve.
--
-- Config example (config.yaml):
--   drivers:
--     - name: heishamon
--       lua: /data/heishamon.lua
--       capabilities:
--         mqtt:
--           host: core-mosquitto
--           port: 1883
--           username: mqtt-user
--           password: 42wenkel
--       config:
--         base_topic: panasonic_heat_pump
--         min_offset: -3
--         max_offset: 3
--         safe_offset: 0

DRIVER = {
  id           = "heishamon",
  name         = "Panasonic Aquarea (Heishamon)",
  manufacturer = "Panasonic",
  version      = "0.4.0",
  protocols    = { "mqtt" },
  capabilities = { "heatpump" },
  description  = "Panasonic Aquarea H/J/K/L/M-series heat pump via Heishamon MQTT bridge. Controls Zone 1 heat curve offset (Z1_Heat_Request_Temp) in range -3..+3 °C.",
  homepage     = "https://github.com/Egyjs/HeishaMon",
  authors      = { "Rolf (Runneval)", "Claude (Anthropic)", "forty-two-watts contributors" },
  tested_models = { "WH-SXC09H3E8" },
  verification_status = "experimental",
  verified_by = { "Rolf (Runneval)" },
  verified_at = "2026-06-21",
  verification_notes = "Tested on WH-SXC09H3E8 (H-series) with Heishamon Large v4.1.6 on ESP32. MQTT via core-mosquitto on HA Green. Live metrics confirmed. Offset control verified via Z1_Heat_Request_Temp.",
}

PROTOCOL = "mqtt"

-- State
local outside_temp   = nil
local outlet_temp    = nil
local inlet_temp     = nil
local target_temp    = nil
local z1_offset      = nil
local last_msg_ts    = 0
local STALE_AFTER_MS = 60000

-- Config (overridable via config.yaml)
local base_topic   = "panasonic_heat_pump"
local min_offset   = -3
local max_offset   = 3
local safe_offset  = 0

----------------------------------------------------------------------------
-- Lifecycle
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Panasonic")

    if config then
        if config.base_topic  then base_topic  = config.base_topic                    end
        if config.min_offset  then min_offset  = tonumber(config.min_offset)  or -3   end
        if config.max_offset  then max_offset  = tonumber(config.max_offset)  or  3   end
        if config.safe_offset then safe_offset = tonumber(config.safe_offset) or  0   end
    end

    -- Subscribe broadly to all Heishamon topics
    local err = host.mqtt_subscribe(base_topic .. "/#")
    if err then
        host.log("error", "Heishamon: subscribe failed: " .. tostring(err))
    else
        host.log("info", "Heishamon: subscribed to " .. base_topic .. "/#")
    end

    host.log("info", "Heishamon: initialized, base_topic=" .. base_topic
        .. " offset_range=[" .. min_offset .. ".." .. max_offset .. "]"
        .. " safe_offset=" .. safe_offset)
end

function driver_poll()
    local now = host.millis()
    local messages = host.mqtt_messages()
    if not messages then messages = {} end

    for _, msg in ipairs(messages) do
        local val = tonumber(msg.payload)
        if val ~= nil then
            if msg.topic == base_topic .. "/main/Outside_Temp" then
                outside_temp = val
                last_msg_ts  = now
            elseif msg.topic == base_topic .. "/main/Main_Inlet_Temp" then
                inlet_temp  = val
                last_msg_ts = now
            elseif msg.topic == base_topic .. "/main/Main_Outlet_Temp" then
                outlet_temp = val
                last_msg_ts = now
            elseif msg.topic == base_topic .. "/main/Main_Target_Temp" then
                target_temp = val
                last_msg_ts = now
            elseif msg.topic == base_topic .. "/main/Z1_Heat_Request_Temp" then
                z1_offset   = val
                last_msg_ts = now
            end
        end
    end

    -- Drop stale data
    if last_msg_ts > 0 and (now - last_msg_ts) > STALE_AFTER_MS then
        host.log("warn", "Heishamon: no MQTT messages for "
            .. tostring(STALE_AFTER_MS) .. " ms — data stale")
        outside_temp = nil
        outlet_temp  = nil
        inlet_temp   = nil
        target_temp  = nil
        z1_offset    = nil
    end

    -- Emit metrics
    if outside_temp ~= nil then host.emit_metric("hp_outside_temp_c", outside_temp, "°C") end
    if outlet_temp  ~= nil then host.emit_metric("hp_outlet_temp_c",  outlet_temp,  "°C") end
    if inlet_temp   ~= nil then host.emit_metric("hp_inlet_temp_c",   inlet_temp,   "°C") end
    if target_temp  ~= nil then host.emit_metric("hp_target_temp_c",  target_temp,  "°C") end
    if z1_offset    ~= nil then host.emit_metric("hp_z1_heat_offset", z1_offset,    "°C") end

    return 5000
end

----------------------------------------------------------------------------
-- Control
----------------------------------------------------------------------------

local function apply_offset(offset)
    -- Clamp to valid range
    if offset < min_offset then offset = min_offset end
    if offset > max_offset then offset = max_offset end
    offset = math.floor(offset + 0.5)  -- round to nearest integer

    local topic   = base_topic .. "/set/Z1_Heat_Request_Temp"
    local payload = tostring(offset)
    local err = host.mqtt_publish(topic, payload)
    if err then
        host.log("warn", "Heishamon: publish to " .. topic .. " failed: " .. tostring(err))
        return false
    end
    host.log("info", "Heishamon: Z1_Heat_Request_Temp → " .. payload)
    return true
end

function driver_command(action, _power_w, cmd)
    if action == "set_heat_curve_offset" then
        local offset = tonumber(cmd and (cmd.offset or cmd.value))
        if offset == nil then
            host.log("warn", "Heishamon: set_heat_curve_offset missing value in cmd")
            return false
        end
        return apply_offset(offset)
    end

    host.log("debug", "Heishamon: ignoring unsupported action=" .. tostring(action))
    return false
end

-- Called by 42W watchdog when control is released or EMS stops.
-- Return to safe/neutral offset so the pump runs on its own heat curve.
function driver_default_mode()
    host.log("info", "Heishamon: default_mode — resetting offset to " .. tostring(safe_offset))
    apply_offset(safe_offset)
end

function driver_cleanup()
    host.log("info", "Heishamon: cleanup — resetting offset to " .. tostring(safe_offset))
    apply_offset(safe_offset)
    outside_temp = nil
    outlet_temp  = nil
    inlet_temp   = nil
    target_temp  = nil
    z1_offset    = nil
end
