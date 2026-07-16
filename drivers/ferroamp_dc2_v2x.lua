-- ferroamp_dc2_v2x.lua
-- Ferroamp DC2 V2X 20 kW charger over MQTT.
-- Emits: v2x_charger
--
-- DC2 V2X charger exposes a charger-local MQTT broker. Operator
-- commands are site-convention watts at the API boundary, translated
-- here to the charger API's percent setpoint.
--
-- Site convention:
--   v2x_charger.w > 0 = vehicle charging
--   v2x_charger.w < 0 = vehicle discharging into site/grid

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id = "ferroamp-dc2-v2x",
  name = "Ferroamp DC2 V2X",
  manufacturer = "Ferroamp",
  version = "1.1.0",
  protocols = { "mqtt" },
  capabilities = { "v2x_charger" },
  description = "Ferroamp DC2 V2X 20 kW bidirectional CCS2 charger via local MQTT.",
  homepage = "https://ferroamp.com",
  verification_status = "experimental",
  verification_notes = "Implements DC2 V2X integration manual 2026-04-22. Hardware sign and droop limits still need live verification before automatic V2G dispatch.",
  connection_defaults = {
    port = 1883,
    username = "dc2",
    password = "dc2mqtt!",
  },
}

PROTOCOL = "mqtt"

local state = {}
local seen_ms = {}
local dirty = {}
local has_data = false
local rated_power_w = 20000
local max_current_a = 50
local controller_topic = "dc2/ui/control/controller"
local power_topic = "dc2/ui/control/power"
local controller_value = "MQTT"
local connector_index = 0 -- 0 = unscoped (accept every connector); set via config to scope a multi-connector DC2
local telemetry_max_age_ms = 15000
local serial_set = false

local function num(v)
  return tonumber(v) or 0
end

local function present(key)
  return state[key] ~= nil
end

local function fresh(key, now)
  return present(key) and seen_ms[key] ~= nil and (now - seen_ms[key]) <= telemetry_max_age_ms
end

local function set_value(key, value, now)
  state[key] = value
  seen_ms[key] = now
  dirty[key] = true
  has_data = true
end

local function clear_value(key)
  state[key] = nil
  seen_ms[key] = nil
  dirty[key] = true
end

local function clamp(v, lo, hi)
  if v < lo then return lo end
  if v > hi then return hi end
  return v
end

local function abs_positive(v)
  v = math.abs(num(v))
  if v > 0 then return v end
  return nil
end

local function positive_min(a, b)
  a = abs_positive(a)
  b = abs_positive(b)
  if a and b then return math.min(a, b) end
  return a or b
end

local function parse_kwh(v)
  if v == nil then return nil end
  local n = tonumber(tostring(v):match("[-+]?%d+%.?%d*"))
  if not n then return nil end
  return n * 1000
end

local function parse_json(payload)
  if payload == nil or payload == "" then return nil end
  local value, err = host.json_decode(payload)
  if err then
    host.log("warn", "dc2 json decode failed: " .. tostring(err))
    return nil
  end
  return value
end

local function topic_to_legacy_key(topic)
  local idx, key = string.match(topic, "^dc2/connector/(%d+)/(.*)")
  if not idx then return nil end
  -- Scope ingestion to the configured connector on multi-connector units.
  -- connector_index <= 0 means "unscoped": accept every connector (legacy
  -- single-connector behaviour).
  if connector_index > 0 and tonumber(idx) ~= connector_index then
    return nil
  end
  return key
end

local function ingest_legacy(topic, payload, now)
  local key = topic_to_legacy_key(topic)
  if not key then return false end
  if payload == "" then
    clear_value("legacy/" .. key)
    return true
  end
  local nval = tonumber(payload)
  set_value("legacy/" .. key, nval or payload, now)
  return true
end

local function ingest_system(payload, now)
  if payload == "" then
    clear_value("system")
    return
  end
  local obj = parse_json(payload)
  if type(obj) ~= "table" then return end
  set_value("system", obj, now)
  if obj["Host"] and not serial_set then
    host.set_sn(tostring(obj["Host"]))
    serial_set = true
  end
end

local function ingest_pecc(payload, now)
  if payload == "" then
    clear_value("pecc")
    return
  end
  local obj = parse_json(payload)
  if type(obj) ~= "table" then return end
  set_value("pecc", obj, now)
end

local function ingest_secc(payload, now)
  if payload == "" then
    clear_value("secc")
    return
  end
  local obj = parse_json(payload)
  if type(obj) ~= "table" then return end
  set_value("secc", obj, now)
end

local function ingest_psu(payload, now)
  if payload == "" then
    clear_value("psu")
    return
  end
  local obj = parse_json(payload)
  if type(obj) ~= "table" then return end
  set_value("psu", obj, now)
end

local function system_state()
  local system = state["system"]
  if type(system) ~= "table" then return nil end
  return tostring(system["State"] or "")
end

local function connected_from_state(s)
  if s == nil or s == "" then return nil end
  local lower = string.lower(s)
  if lower:find("discharging", 1, true) or lower:find("charging", 1, true) or lower:find("connected", 1, true) then
    return true
  end
  if lower:find("idle", 1, true) or lower:find("available", 1, true) or lower:find("disconnected", 1, true) then
    return false
  end
  return nil
end

local function id_state_connected(id)
  return id == "mated" or id == "mated_ev_aux" or id == "mated_evse_aux"
end

local function status2()
  local pecc = state["pecc"]
  if type(pecc) ~= "table" then return nil end
  return pecc["PECCStatus2"]
end

local function limits1()
  local pecc = state["pecc"]
  if type(pecc) ~= "table" then return nil end
  return pecc["PECCLimits1"]
end

local function limits3()
  local pecc = state["pecc"]
  if type(pecc) ~= "table" then return nil end
  return pecc["PECCLimits3"]
end

local function charge_limit_w()
  local l1 = limits1()
  local from_pecc = l1 and l1["limitPowerMax"]
  local from_legacy = state["legacy/ev/limits/max_power"]
  return positive_min(rated_power_w, from_pecc or from_legacy) or rated_power_w
end

local function discharge_limit_w()
  local l3 = limits3()
  local from_pecc = l3 and l3["limitDischargePowerMax"]
  local from_legacy = state["legacy/ev/limits/max_discharge_power"]
  return positive_min(rated_power_w, from_pecc or from_legacy) or rated_power_w
end

local function current_dc_v(now)
  if fresh("pecc", now) then
    local st2 = status2()
    if type(st2) == "table" and st2["measuredVoltage"] ~= nil then
      return num(st2["measuredVoltage"])
    end
  end
  if fresh("psu", now) then
    local psu = state["psu"]
    local measured = type(psu) == "table" and psu["Measured values"] or nil
    if type(measured) == "table" and measured["Battery Voltage"] ~= nil then
      return num(measured["Battery Voltage"])
    end
  end
  if fresh("legacy/pe/measured_voltage", now) then
    return num(state["legacy/pe/measured_voltage"])
  end
  return 0
end

local function hardware_max_power_w(now)
  local dc_v = current_dc_v(now)
  if dc_v > 0 and max_current_a > 0 then
    return math.min(rated_power_w, dc_v * max_current_a)
  end
  return rated_power_w
end

local function publish_json(topic, value)
  local payload, err = host.json_encode({
    timestamp = os.time(),
    value = value,
  })
  if err then return err end
  return host.mqtt_publish(topic, payload)
end

local function publish_setpoint(power_w)
  local now = host.millis()
  local hardware_limit = hardware_max_power_w(now)
  local limit = hardware_limit
  if power_w > 0 then
    limit = math.min(hardware_limit, charge_limit_w())
  elseif power_w < 0 then
    limit = math.min(hardware_limit, discharge_limit_w())
  end
  local clamped_w = clamp(power_w, -limit, limit)
  local pct = 0
  if hardware_limit > 0 then
    pct = clamp((clamped_w / hardware_limit) * 100, -100, 100)
  end

  local err = publish_json(controller_topic, controller_value)
  if err then return err end
  return publish_json(power_topic, pct)
end

function driver_init(config)
  host.set_make("Ferroamp")
  if config then
    if config.serial then host.set_sn(tostring(config.serial)); serial_set = true end
    if config.rated_power_w then rated_power_w = num(config.rated_power_w) end
    if config.max_current_a then max_current_a = num(config.max_current_a) end
    if config.controller_topic then controller_topic = tostring(config.controller_topic) end
    if config.power_topic then power_topic = tostring(config.power_topic) end
    if config.controller_value then controller_value = tostring(config.controller_value) end
    if config.connector_index then connector_index = math.floor(num(config.connector_index)) end
    if config.telemetry_max_age_ms then telemetry_max_age_ms = num(config.telemetry_max_age_ms) end
  end

  host.mqtt_subscribe("dc2/#")
end

function driver_poll()
  local now = host.millis()
  local messages = host.mqtt_messages()
  if messages then
    for _, msg in ipairs(messages) do
      if msg.topic ~= controller_topic and msg.topic ~= power_topic then
        if msg.topic == "dc2/system" or msg.topic == "dc2/system/" or msg.topic == "dc2/v2x/system" then
          ingest_system(msg.payload, now)
        elseif msg.topic == "dc2/pecc" or msg.topic == "dc2/pecc/" or msg.topic == "dc2/v2x/pecc" then
          ingest_pecc(msg.payload, now)
        elseif msg.topic == "dc2/v2x/secc" then
          ingest_secc(msg.payload, now)
        elseif msg.topic == "dc2/v2x/psu" then
          ingest_psu(msg.payload, now)
        else
          ingest_legacy(msg.topic, msg.payload, now)
        end
      end
    end
  end

  if not has_data then return 1000 end
  if next(dirty) == nil then return 1000 end

  local st2 = nil
  if fresh("pecc", now) then st2 = status2() end
  local have_new_power = type(st2) == "table" and st2["measuredVoltage"] ~= nil and st2["measuredCurrent"] ~= nil
  local psu_measured = nil
  if fresh("psu", now) and type(state["psu"]) == "table" then
    psu_measured = state["psu"]["Measured values"]
  end
  local have_psu_power = type(psu_measured) == "table" and psu_measured["Battery Voltage"] ~= nil and psu_measured["Battery Current"] ~= nil
  local have_legacy_power = fresh("legacy/pe/measured_voltage", now) and fresh("legacy/pe/measured_current", now)
  if not have_new_power and not have_psu_power and not have_legacy_power then
    for k in pairs(dirty) do dirty[k] = nil end
    return 1000
  end

  local dc_v = 0
  local dc_a = 0
  if have_new_power then
    dc_v = num(st2["measuredVoltage"])
    dc_a = num(st2["measuredCurrent"])
  elseif have_psu_power then
    dc_v = num(psu_measured["Battery Voltage"])
    dc_a = num(psu_measured["Battery Current"])
  else
    dc_v = num(state["legacy/pe/measured_voltage"])
    dc_a = num(state["legacy/pe/measured_current"])
  end
  local dc_w = dc_v * dc_a

  local charger = {
    dc_v = dc_v,
    dc_a = dc_a,
    dc_w = dc_w,
    w = dc_w,
    rated_power_w = rated_power_w,
    charge_power_max_w = charge_limit_w(),
    discharge_power_max_w = discharge_limit_w(),
    protocol = "mqtt",
  }

  local soc = state["legacy/ev/soc"]
  if soc ~= nil then
    charger.vehicle_soc = num(soc) / 100
  end

  local secc = state["secc"]
  if type(secc) == "table" then
    local vehicle_status = secc["VehicleStatus"]
    if type(vehicle_status) == "table" then
      if vehicle_status["batteryStateOfCharge"] ~= nil then
        charger.vehicle_soc = num(vehicle_status["batteryStateOfCharge"]) / 100
      end
      if vehicle_status["evConnectionState"] ~= nil then
        local conn_state = tostring(vehicle_status["evConnectionState"])
        charger.connected = conn_state == "energyTransferAllowed" or conn_state == "connected"
      end
    end
  end

  local system = state["system"]
  if type(system) == "table" then
    charger.status = system_state()
    charger.control_mode = tostring(system["Control"] or "")
    local connected = connected_from_state(charger.status)
    if connected ~= nil then charger.connected = connected end
    local charge_wh = parse_kwh(system["Charged energy"])
    local discharge_wh = parse_kwh(system["Discharged energy"])
    if charge_wh then charger.session_charge_wh = charge_wh end
    if discharge_wh then charger.session_discharge_wh = discharge_wh end
  end

  if charger.connected == nil and fresh("legacy/ev/id_state", now) then
    charger.connected = id_state_connected(state["legacy/ev/id_state"])
  end
  if charger.status == nil or charger.status == "" then
    if dc_w > 50 then
      charger.status = "charging"
    elseif dc_w < -50 then
      charger.status = "discharging"
    elseif charger.connected then
      charger.status = "connected"
    else
      charger.status = "idle"
    end
  end
  if charger.control_mode == nil or charger.control_mode == "" then
    charger.control_mode = "manual_required"
  end

  host.emit("v2x_charger", charger)

  for k in pairs(dirty) do dirty[k] = nil end
  return 1000
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
  return publish_setpoint(0)
end

function driver_cleanup()
  state = {}
  seen_ms = {}
  dirty = {}
  has_data = false
  serial_set = false
end
