-- ferroamp_dc2_v2x.lua
-- Ferroamp DC2 V2X 20 kW charger over MQTT.
-- Emits: v2x_charger
--
-- The DC2 V2X charger exposes a charger-local MQTT broker. External
-- control requires Manual mode on the charger.
--
-- Site convention:
--   v2x_charger.w > 0 = vehicle charging
--   v2x_charger.w < 0 = vehicle discharging into the site/grid

DRIVER = {
  id           = "ferroamp-dc2-v2x",
  name         = "Ferroamp DC2 V2X",
  manufacturer = "Ferroamp",
  version      = "1.0.0",
  protocols    = { "mqtt" },
  capabilities = { "v2x_charger" },
  description  = "Ferroamp DC2 V2X 20 kW bidirectional CCS2 charger via local MQTT.",
  homepage     = "https://ferroamp.com",
  verification_status = "experimental",
  verification_notes = "Ported from Sourceful device-support; sign of pe/measured_current must be verified on live hardware before automatic V2G dispatch.",
  connection_defaults = {
    port = 1883,
    username = "dc2",
    password = "dc2mqtt",
  },
}

PROTOCOL = "mqtt"

local state = {}
local seen_ms = {}
local dirty = {}
local has_data = false
local rated_power_w = 20000
local control_topic = "dc2/ui/control"
local connector_index = 1
local telemetry_max_age_ms = 15000

local function num(v)
    return tonumber(v) or 0
end

local function snum(key)
    return num(state[key])
end

local function present(key)
    return state[key] ~= nil
end

local function fresh(key, now)
    return present(key) and seen_ms[key] ~= nil and (now - seen_ms[key]) <= telemetry_max_age_ms
end

local function topic_to_key(topic)
    local key = string.match(topic, "^dc2/connector/%d+/(.*)")
    if key then return key end
    return string.match(topic, "^dc2/(.*)")
end

local function has_any(t)
    for _ in pairs(t) do return true end
    return false
end

local function clamp(v, lo, hi)
    if v < lo then return lo end
    if v > hi then return hi end
    return v
end

local function id_state_connected(id)
    return id == "mated" or id == "mated_ev_aux" or id == "mated_evse_aux"
end

function driver_init(config)
    host.set_make("Ferroamp")
    if config then
        if config.serial then host.set_sn(tostring(config.serial)) end
        if config.rated_power_w then rated_power_w = num(config.rated_power_w) end
        if config.control_topic then control_topic = tostring(config.control_topic) end
        if config.connector_index then connector_index = math.floor(num(config.connector_index)) end
        if config.telemetry_max_age_ms then telemetry_max_age_ms = num(config.telemetry_max_age_ms) end
    end

    host.mqtt_subscribe("dc2/connector/" .. tostring(connector_index) .. "/#")
    host.mqtt_subscribe("dc2/ui/#")
end

function driver_poll()
    local now = host.millis()
    local messages = host.mqtt_messages()
    if messages then
        for _, msg in ipairs(messages) do
            local key = topic_to_key(msg.topic)
            if key and msg.topic ~= control_topic then
                if msg.payload == "" then
                    state[key] = nil
                    seen_ms[key] = nil
                else
                    local nval = tonumber(msg.payload)
                    if nval then
                        state[key] = nval
                    else
                        state[key] = msg.payload
                    end
                    seen_ms[key] = now
                    has_data = true
                end
                dirty[key] = true
            end
        end
    end

    if not has_data then return 1000 end
    if next(dirty) == nil then return 1000 end
    if not fresh("pe/measured_voltage", now) or not fresh("pe/measured_current", now) then
        for k in pairs(dirty) do dirty[k] = nil end
        return 1000
    end

    local charger = {}

    local dc_v = snum("pe/measured_voltage")
    local dc_a = snum("pe/measured_current")
    local dc_w = dc_v * dc_a
    charger.dc_v = dc_v
    charger.dc_a = dc_a
    charger.dc_w = dc_w
    charger.w = dc_w

    if fresh("ev/soc", now) then
        charger.vehicle_soc = num(state["ev/soc"]) / 100
    end

    if fresh("em/transferred_energy", now) then
        local wh = num(state["em/transferred_energy"]) * 1000
        if dc_w >= 0 then
            charger.session_charge_wh = wh
        else
            charger.session_discharge_wh = wh
        end
    end

    if fresh("ev/limits/max_power", now) then
        charger.charge_power_max_w = snum("ev/limits/max_power")
    end
    if fresh("ev/limits/min_power", now) then
        charger.charge_power_min_w = snum("ev/limits/min_power")
    end
    if fresh("ev/limits/max_discharge_power", now) then
        charger.discharge_power_max_w = snum("ev/limits/max_discharge_power")
    end

    charger.rated_power_w = rated_power_w
    charger.protocol = "mqtt"

    if fresh("ev/id_state", now) then
        charger.connected = id_state_connected(state["ev/id_state"])
    end
    if fresh("ui/mode", now) then
        charger.control_mode = tostring(state["ui/mode"])
    elseif fresh("ui/control_mode", now) then
        charger.control_mode = tostring(state["ui/control_mode"])
    else
        charger.control_mode = "manual_required"
    end

    if fresh("ev/state", now) then
        charger.status = tostring(state["ev/state"])
    elseif charger.connected then
        if dc_w > 50 then
            charger.status = "charging"
        elseif dc_w < -50 then
            charger.status = "discharging"
        else
            charger.status = "connected"
        end
    end

    if has_any(charger) then host.emit("v2x_charger", charger) end

    for k in pairs(dirty) do dirty[k] = nil end
    return 1000
end

local function publish_setpoint(power_w)
    local limit = rated_power_w
    if state["ev/limits/max_power"] and power_w > 0 then
        limit = math.min(limit, math.abs(snum("ev/limits/max_power")))
    elseif state["ev/limits/max_discharge_power"] and power_w < 0 then
        limit = math.min(limit, math.abs(snum("ev/limits/max_discharge_power")))
    end
    local clamped_w = clamp(power_w, -limit, limit)
    return host.mqtt_publish(control_topic, string.format("%.2f", clamped_w / 1000))
end

function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "v2x_set_power" or action == "battery" then
        return publish_setpoint(power_w)
    elseif action == "v2x_stop" or action == "deinit" then
        return publish_setpoint(0)
    elseif action == "curtail" then
        return publish_setpoint(math.abs(power_w))
    elseif action == "curtail_disable" then
        return publish_setpoint(rated_power_w)
    end
    return false
end

function driver_default_mode()
    host.mqtt_publish(control_topic, "0.00")
end

function driver_cleanup()
    state = {}
    seen_ms = {}
    dirty = {}
    has_data = false
end
