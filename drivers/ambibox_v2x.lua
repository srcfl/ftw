-- ambibox_v2x.lua
-- Ambibox V2X charger over MQTT.
-- Emits: v2x_charger
--
-- Site convention:
--   v2x_charger.w > 0 = vehicle charging
--   v2x_charger.w < 0 = vehicle discharging into the site/grid

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "ambibox-v2x",
  name         = "Ambibox V2X",
  manufacturer = "Ambibox",
  version      = "1.0.0",
  protocols    = { "mqtt" },
  capabilities = { "v2x_charger" },
  description  = "Ambibox bidirectional V2X charger via MQTT.",
  verification_status = "experimental",
  connection_defaults = {
    host = "sid-os.local",
    port = 1883,
  },
}

PROTOCOL = "mqtt"

local state = {}
local seen_ms = {}
local dirty = {}
local has_data = false
local rated_power_w = 22000
local telemetry_max_age_ms = 15000

local function field_from_topic(topic)
    local field = nil
    for segment in string.gmatch(topic, "[^/]+") do
        field = segment
    end
    return field
end

local function num(v)
    return tonumber(v) or 0
end

local function snum(key)
    return num(state[key])
end

local function has_any(t)
    for _ in pairs(t) do return true end
    return false
end

local function present(key)
    return state[key] ~= nil
end

local function fresh(key, now)
    return present(key) and seen_ms[key] ~= nil and (now - seen_ms[key]) <= telemetry_max_age_ms
end

local function boolish(v)
    return v == true or v == "true" or v == 1 or v == "1"
end

local function clamp(v, lo, hi)
    if v < lo then return lo end
    if v > hi then return hi end
    return v
end

function driver_init(config)
    host.set_make("Ambibox")
    if config then
        if config.serial then host.set_sn(tostring(config.serial)) end
        if config.rated_power_w then rated_power_w = num(config.rated_power_w) end
        if config.telemetry_max_age_ms then telemetry_max_age_ms = num(config.telemetry_max_age_ms) end
    end
    host.mqtt_subscribe("device/evCharger/#")
end

function driver_poll()
    local now = host.millis()
    local messages = host.mqtt_messages()
    if messages then
        for _, msg in ipairs(messages) do
            local field = field_from_topic(msg.topic)
            if field then
                if msg.payload == "" then
                    state[field] = nil
                    seen_ms[field] = nil
                else
                    local nval = tonumber(msg.payload)
                    if nval then
                        state[field] = nval
                    else
                        state[field] = msg.payload
                    end
                    seen_ms[field] = now
                    has_data = true
                end
                dirty[field] = true
            end
        end
    end

    if not has_data then return 1000 end
    if next(dirty) == nil then return 1000 end
    if not fresh("powerAc", now) then
        for k in pairs(dirty) do dirty[k] = nil end
        return 1000
    end

    local charger = {}

    -- host.emit requires w. MQTT fields arrive independently, so emit a
    -- fresh cached snapshot on every fresh field update once powerAc exists.
    if fresh("powerAc", now)   then charger.w       = snum("powerAc")   end
    if fresh("currentAc", now) then charger.a       = snum("currentAc") end
    if fresh("voltageAc", now) then charger.v       = snum("voltageAc") end
    if fresh("frequency", now) then charger.freq_hz = snum("frequency") end

    if fresh("currentAc1", now) then charger.l1_a = snum("currentAc1") end
    if fresh("currentAc2", now) then charger.l2_a = snum("currentAc2") end
    if fresh("currentAc3", now) then charger.l3_a = snum("currentAc3") end
    if fresh("voltageAc1", now) then charger.l1_v = snum("voltageAc1") end
    if fresh("voltageAc2", now) then charger.l2_v = snum("voltageAc2") end
    if fresh("voltageAc3", now) then charger.l3_v = snum("voltageAc3") end

    if fresh("voltageAc1", now) and fresh("currentAc1", now) then
        charger.l1_w = snum("voltageAc1") * snum("currentAc1")
    end
    if fresh("voltageAc2", now) and fresh("currentAc2", now) then
        charger.l2_w = snum("voltageAc2") * snum("currentAc2")
    end
    if fresh("voltageAc3", now) and fresh("currentAc3", now) then
        charger.l3_w = snum("voltageAc3") * snum("currentAc3")
    end

    if fresh("powerDc", now)   then charger.dc_w = snum("powerDc")   end
    if fresh("currentDc", now) then charger.dc_a = snum("currentDc") end
    if fresh("voltageDc", now) then charger.dc_v = snum("voltageDc") end

    if fresh("soc", now) then
        local soc = num(state.soc)
        if soc > 1 then soc = soc / 100 end
        charger.vehicle_soc = soc
    end

    if fresh("maxEnergyRequest", now) then charger.ev_max_energy_req_wh = snum("maxEnergyRequest") end
    if fresh("minEnergyRequest", now) then charger.ev_min_energy_req_wh = snum("minEnergyRequest") end

    if fresh("energyAcImportSession", now) then charger.session_charge_wh    = snum("energyAcImportSession") end
    if fresh("energyAcExportSession", now) then charger.session_discharge_wh = snum("energyAcExportSession") end
    if fresh("energyAcImport", now)        then charger.total_charge_wh      = snum("energyAcImport")        end
    if fresh("energyAcExport", now)        then charger.total_discharge_wh   = snum("energyAcExport")        end

    if fresh("chargePowerMin", now)    then charger.charge_power_min_w    = snum("chargePowerMin")    end
    if fresh("chargePowerMax", now)    then charger.charge_power_max_w    = snum("chargePowerMax")    end
    if fresh("dischargePowerMin", now) then charger.discharge_power_min_w = snum("dischargePowerMin") end
    if fresh("dischargePowerMax", now) then charger.discharge_power_max_w = snum("dischargePowerMax") end

    if fresh("evConnected", now) then charger.connected = boolish(state.evConnected) end

    charger.protocol = "mqtt"

    if has_any(charger) then host.emit("v2x_charger", charger) end

    for k in pairs(dirty) do dirty[k] = nil end
    return 1000
end

local function clamp_setpoint(power_w)
    local limit = rated_power_w
    if power_w > 0 and present("chargePowerMax") then
        limit = math.min(limit, math.abs(snum("chargePowerMax")))
    elseif power_w < 0 and present("dischargePowerMax") then
        limit = math.min(limit, math.abs(snum("dischargePowerMax")))
    end
    return clamp(power_w, -limit, limit)
end

function driver_command(action, power_w, cmd)
    if action == "init" then
        return host.mqtt_publish("device/evCharger/0/wakeUp", "true")
    elseif action == "v2x_set_power" or action == "battery" then
        return host.mqtt_publish("device/ess/0/targetPower", tostring(clamp_setpoint(power_w)))
    elseif action == "v2x_stop" or action == "deinit" then
        return host.mqtt_publish("device/ess/0/targetPower", "0")
    elseif action == "curtail" then
        local max = num(state.chargePowerMax) > 0 and snum("chargePowerMax") or rated_power_w
        local limited = clamp(math.abs(power_w), 0, max)
        return host.mqtt_publish("device/ess/0/limitChargePower", tostring(limited))
    elseif action == "curtail_disable" then
        local max = num(state.chargePowerMax) > 0 and snum("chargePowerMax") or rated_power_w
        return host.mqtt_publish("device/ess/0/limitChargePower", tostring(max))
    end
    return false
end

function driver_default_mode()
    host.mqtt_publish("device/ess/0/targetPower", "0")
end

function driver_cleanup()
    state = {}
    seen_ms = {}
    dirty = {}
    has_data = false
end
