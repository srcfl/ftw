-- Tesla Wall Connector Gen 3 (local HTTP).
--
-- Protocol: unauthenticated GET on the LAN.
--   /api/1/vitals   live state, currents, voltages, session energy
--   /api/1/version  serial / part / firmware (once)
-- Canonical copy belongs in srcfl/device-drivers as tesla_wall_connector.lua;
-- this testdata file is the in-tree source until that pin lands. Operators
-- can drop the same file in the user-drivers directory in the meantime.
--
-- The wall connector has no local current setpoint. ev_set_current,
-- ev_pause and ev_resume are accepted so the planner does not mark the
-- driver failed; they do not write hardware. Steer a Tesla through
-- tesla_vehicle. Sign convention: positive W is charging.
--
-- Config:
--   host       LAN hostname or IP (no scheme)
--   base_url   optional origin override (tests)
--   split_phase  true for North-American split-phase power (grid_v × vehicle_a)
--
-- verification_status is experimental until a live Gen 3 has been
-- exercised end-to-end.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "tesla-wall-connector",
  name         = "Tesla Wall Connector",
  manufacturer = "Tesla",
  version      = "0.1.0",
  protocols    = { "http" },
  capabilities = { "ev" },
  description  = "Tesla Wall Connector Gen 3 on the LAN. Observation only; no cloud account.",
  homepage     = "https://www.tesla.com/support/charging/wall-connector",
  authors      = { "FTW contributors" },
  tested_models = { "Wall Connector Gen 3" },
  verification_status = "experimental",
}

PROTOCOL = "http"

-- EVSE state codes documented by the ioBroker tesla-wallconnector3 adapter
-- (public field notes on /api/1/vitals). 3, 5, 7, 12 are unused.
local EVSE_IDLE         = 1
local EVSE_CONNECTED    = 2
local EVSE_READY        = 4
local EVSE_HANDSHAKE    = 6
local EVSE_FINISHED     = 8
local EVSE_WAIT_CAR     = 9
local EVSE_CHARGE_LOW   = 10
local EVSE_CHARGE_FULL  = 11

local BASE_URL = nil
local split_phase = false
local warned_readonly = false

local function as_number(v)
  if type(v) == "number" then return v end
  if v == nil then return nil end
  return tonumber(v)
end

local function pick(t, ...)
  if type(t) ~= "table" then return nil end
  for i = 1, select("#", ...) do
    local v = t[select(i, ...)]
    if v ~= nil then return v end
  end
  return nil
end

-- Some Tesla firmware emits bare `nan` or drops the closing brace.
-- Repair before json_decode so a single bad sample does not stall poll.
local function repair_json(raw)
  if raw == nil then return "" end
  raw = tostring(raw)
  raw = raw:gsub("[Nn][Aa][Nn]", "null")
  local opens = 0
  local closes = 0
  for i = 1, #raw do
    local c = raw:sub(i, i)
    if c == "{" then opens = opens + 1
    elseif c == "}" then closes = closes + 1
    end
  end
  while closes < opens do
    raw = raw .. "}"
    closes = closes + 1
  end
  return raw
end

local function decode_json(raw)
  if raw == nil or raw == "" then return nil, "empty body" end
  local data, err = host.json_decode(repair_json(raw))
  if data == nil then return nil, tostring(err or "decode failed") end
  return data, nil
end

local function origin_from_config(config)
  if config.base_url and tostring(config.base_url) ~= "" then
    return tostring(config.base_url):gsub("/$", "")
  end
  local host = config.host or config.ip
  if not host or host == "" then return nil end
  host = tostring(host)
  if host:find("://", 1, true) then
    return host:gsub("/$", "")
  end
  return "http://" .. host
end

local function phase_power(v, a)
  v = as_number(v) or 0
  a = as_number(a) or 0
  -- Ghost voltages (~1–3 V) appear on unused legs; ignore them.
  if v < 50 then return 0 end
  if a < 0 then a = 0 end
  return v * a
end

local function charge_power(vitals)
  if split_phase then
    local w = (as_number(vitals.grid_v) or 0) * (as_number(vitals.vehicle_current_a) or 0)
    if w < 0 then w = 0 end
    return w
  end
  local w = phase_power(vitals.voltageA_v, vitals.currentA_a)
    + phase_power(vitals.voltageB_v, vitals.currentB_a)
    + phase_power(vitals.voltageC_v, vitals.currentC_a)
  if w <= 0 then
    local fallback = (as_number(vitals.grid_v) or 0) * (as_number(vitals.vehicle_current_a) or 0)
    if fallback > 0 then w = fallback end
  end
  return w
end

local function active_phases(vitals)
  local n = 0
  if phase_power(vitals.voltageA_v, 1) > 0 then n = n + 1 end
  if phase_power(vitals.voltageB_v, 1) > 0 then n = n + 1 end
  if phase_power(vitals.voltageC_v, 1) > 0 then n = n + 1 end
  if n < 1 then n = 1 end
  return n
end

local function is_charging(state, vitals)
  if state == EVSE_CHARGE_LOW or state == EVSE_CHARGE_FULL then return true end
  return vitals.contactor_closed == true
end

local function is_connected(state, vitals)
  if vitals.vehicle_connected == true then return true end
  return state == EVSE_CONNECTED or state == EVSE_READY
    or state == EVSE_HANDSHAKE or state == EVSE_FINISHED
    or state == EVSE_WAIT_CAR or is_charging(state, vitals)
end

function driver_init(config)
  host.set_make("Tesla")
  host.set_model("Wall Connector Gen 3")
  config = config or {}
  BASE_URL = origin_from_config(config)
  if not BASE_URL then
    error("Tesla Wall Connector: host or base_url required")
  end
  if config.split_phase == true or config.split_phase == "true" then
    split_phase = true
  end

  local raw, err = host.http_get(BASE_URL .. "/api/1/version")
  if not err then
    local ver = decode_json(raw)
    if ver then
      local sn = pick(ver, "serial_number", "serialNumber")
      if sn and sn ~= "" then host.set_sn(tostring(sn)) end
      local part = pick(ver, "part_number", "partNumber")
      if part and part ~= "" then
        host.set_model("Wall Connector " .. tostring(part))
      end
    end
  end
  host.log("info", "Tesla Wall Connector: polling " .. BASE_URL)
end

function driver_poll()
  if not BASE_URL then return 10000 end
  local raw, err = host.http_get(BASE_URL .. "/api/1/vitals")
  if err then
    host.log("warn", "Tesla Wall Connector: vitals failed")
    return 10000
  end
  local vitals, derr = decode_json(raw)
  if derr or type(vitals) ~= "table" then
    host.log("warn", "Tesla Wall Connector: vitals decode failed")
    return 10000
  end

  local state = as_number(vitals.evse_state) or EVSE_IDLE
  local power_w = charge_power(vitals)
  local session_wh = as_number(vitals.session_energy_wh) or 0
  local connected = is_connected(state, vitals)
  local charging = is_charging(state, vitals)
  local phases = active_phases(vitals)

  host.emit("ev", {
    w          = power_w,
    connected  = connected,
    charging   = charging,
    session_wh = session_wh,
    phases     = phases,
    evse_state = state,
  })

  local i1 = as_number(vitals.currentA_a)
  if i1 then host.emit_metric("ev_l1_a", i1) end
  local v1 = as_number(vitals.voltageA_v)
  if v1 then host.emit_metric("ev_l1_v", v1) end
  host.emit_metric("ev_power_w", power_w)
  host.emit_metric("evse_state", state)

  return 5000
end

function driver_command(action, power_w, cmd)
  if action == "init" or action == "deinit" then
    return true
  end
  if action == "ev_set_current" or action == "ev_pause"
      or action == "ev_resume" or action == "ev_start" then
    if not warned_readonly then
      host.log("warn",
        "Tesla Wall Connector has no local current setpoint; " ..
        "steer a Tesla through tesla_vehicle")
      warned_readonly = true
    end
    return true
  end
  host.log("warn", "Tesla Wall Connector: unknown action " .. tostring(action))
  return false
end

function driver_default_mode()
  -- Observation-only: the box keeps charging at the car's last request.
end

function driver_cleanup()
  BASE_URL = nil
end
