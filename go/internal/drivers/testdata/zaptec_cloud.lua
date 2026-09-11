-- Zaptec Cloud EV charger driver (Go / Go 2 / Pro).
--
-- Protocol: HTTPS against the Zaptec Cloud REST API (api.zaptec.com).
-- Canonical copy belongs in srcfl/device-drivers as zaptec_cloud.lua;
-- this testdata file is the in-tree source until that pin lands. Operators
-- can drop the same file in the user-drivers directory in the meantime.
--
-- Auth:      POST /oauth/token  (OAuth2 password grant, form-encoded)
-- Telemetry: GET  /api/chargers/{id}/state
-- Control:   POST /api/chargers/{id}/update          (current / phases)
--            POST /api/chargers/{id}/sendCommand/506 (pause)
--            POST /api/chargers/{id}/sendCommand/507 (resume; 528 = already running)
--
-- Sign convention: positive W is charging (power into the vehicle / site load).
-- Minimum offered current is 6 A (IEC 61851); below that we pause.
--
-- Config:
--   email / username  Zaptec account email
--   password          Zaptec account password (secret)
--   serial            optional charger Id (UUID) or SerialNo
--   phases            1 or 3 (default 3)
--   min_a             minimum current, default 6
--   max_a             maximum current, default 32
--   base_url          optional origin override (tests / staging)
--
-- verification_status is experimental until a live charger has been
-- exercised end-to-end.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "zaptec-cloud",
  name         = "Zaptec Cloud",
  manufacturer = "Zaptec",
  version      = "0.1.0",
  protocols    = { "http" },
  capabilities = { "ev" },
  description  = "Zaptec Go / Go 2 / Pro via Zaptec Cloud. Email + password; optional charger serial.",
  homepage     = "https://zaptec.com",
  http_hosts   = { "api.zaptec.com" },
  authors      = { "FTW contributors" },
  tested_models = { "Go", "Go 2", "Pro" },
  verification_status = "experimental",
  config_secrets = { "password" },
}

PROTOCOL = "http"

local BASE_URL = "https://api.zaptec.com"

-- Observation IDs from the Zaptec Cloud charger state document.
local OBS_VOLTAGE_L1     = 501
local OBS_CURRENT_L1     = 507
local OBS_MAX_CURRENT    = 510
local OBS_ACTIVE_PHASES  = 512
local OBS_CHARGE_POWER   = 513
local OBS_SESSION_ENERGY = 553
local OBS_OP_MODE        = 710

-- Operating modes (observation 710).
local OP_DISCONNECTED = 1
local OP_CONNECTED    = 2
local OP_CHARGING     = 3
local OP_FINISHED     = 5

-- sendCommand codes.
local CMD_STOP   = 506
local CMD_RESUME = 507

local email         = nil
local password      = nil
local serial_want   = nil
local charger_id    = nil
local phases        = 3
local min_a         = 6
local max_a         = 32
local access_token  = nil
local refresh_token = nil
local token_expiry  = 0
local paused_state  = false

local function pick(t, ...)
  if type(t) ~= "table" then return nil end
  for i = 1, select("#", ...) do
    local v = t[select(i, ...)]
    if v ~= nil then return v end
  end
  return nil
end

local function as_number(v)
  if type(v) == "number" then return v end
  if v == nil then return nil end
  return tonumber(v)
end

-- Keep HTTP error strings to a status prefix so a 4xx body that echoes
-- credentials or a bearer token never lands in the driver log.
local function redact_http_err(err)
  if err == nil then return "request failed" end
  local s = tostring(err)
  return s:match("^(HTTP %d+)") or "request failed"
end

local function url_encode(s)
  s = tostring(s or "")
  return (s:gsub("[^%w%-%.%_%~]", function(c)
    return string.format("%%%02X", string.byte(c))
  end))
end

local function auth_headers()
  return { Authorization = "Bearer " .. (access_token or "") }
end

local function decode_json(raw)
  if raw == nil or raw == "" then return nil, "empty body" end
  local data, err = host.json_decode(raw)
  if data == nil then return nil, tostring(err or "decode failed") end
  return data, nil
end

local function login()
  local body = "grant_type=password"
    .. "&username=" .. url_encode(email)
    .. "&password=" .. url_encode(password)
  local resp, err = host.http_post(
    BASE_URL .. "/oauth/token",
    body,
    { ["Content-Type"] = "application/x-www-form-urlencoded" })
  if err then
    return false, err
  end
  local data, derr = decode_json(resp)
  if derr or not data then
    return false, derr or "invalid JSON"
  end
  local token = pick(data, "access_token", "accessToken")
  if not token or token == "" then
    return false, "no access_token"
  end
  access_token = token
  refresh_token = pick(data, "refresh_token", "refreshToken") or refresh_token
  local expires_in = as_number(pick(data, "expires_in", "expiresIn")) or 3600
  token_expiry = host.millis() + (expires_in * 1000) - 60000
  host.log("info", "Zaptec: logged in")
  return true
end

local function refresh()
  if not refresh_token or refresh_token == "" then return false end
  local body = "grant_type=refresh_token"
    .. "&refresh_token=" .. url_encode(refresh_token)
  local resp, err = host.http_post(
    BASE_URL .. "/oauth/token",
    body,
    { ["Content-Type"] = "application/x-www-form-urlencoded" })
  if err then
    host.log("warn", "Zaptec token refresh failed: " .. redact_http_err(err))
    return false
  end
  local data, derr = decode_json(resp)
  if derr or not data then return false end
  local token = pick(data, "access_token", "accessToken")
  if not token or token == "" then return false end
  access_token = token
  local rotated = pick(data, "refresh_token", "refreshToken")
  if rotated and rotated ~= "" then refresh_token = rotated end
  local expires_in = as_number(pick(data, "expires_in", "expiresIn")) or 3600
  token_expiry = host.millis() + (expires_in * 1000) - 60000
  return true
end

local function ensure_auth()
  if access_token and host.millis() < token_expiry then return true end
  if refresh() then return true end
  local ok = login()
  return ok
end

local function charger_rows(data)
  if type(data) ~= "table" then return nil end
  local rows = pick(data, "Data", "data")
  if type(rows) == "table" then return rows end
  if data[1] ~= nil then return data end
  return nil
end

local function charger_matches(ch, want)
  if not want or want == "" then return true end
  local id = tostring(pick(ch, "Id", "id") or "")
  local serial = tostring(pick(ch, "SerialNo", "serialNo", "Serial") or "")
  local device = tostring(pick(ch, "DeviceId", "deviceId") or "")
  return id == want or serial == want or device == want
end

local function resolve_charger()
  local resp, err = host.http_get(BASE_URL .. "/api/chargers", auth_headers())
  if err then return nil, err end
  local data, derr = decode_json(resp)
  if derr then return nil, derr end
  local rows = charger_rows(data)
  if not rows or #rows == 0 then
    return nil, "no chargers on account"
  end
  if serial_want and serial_want ~= "" then
    for i = 1, #rows do
      if charger_matches(rows[i], serial_want) then
        return tostring(pick(rows[i], "Id", "id")), nil
      end
    end
    return nil, "charger not found"
  end
  return tostring(pick(rows[1], "Id", "id")), nil
end

local function observation_map(raw)
  local data, err = decode_json(raw)
  if err then return nil, err end
  local rows = data
  if type(data) == "table" and data[1] == nil then
    rows = pick(data, "Data", "data") or data
  end
  local obs = {}
  if type(rows) ~= "table" then return obs, nil end
  for i = 1, #rows do
    local item = rows[i]
    local id = as_number(pick(item, "StateId", "stateId", "ObservationId", "observationId"))
    if id then
      obs[id] = pick(item, "ValueAsString", "valueAsString", "Value", "value")
    end
  end
  return obs, nil
end

local function send_command(code)
  local url = BASE_URL .. "/api/chargers/" .. charger_id .. "/sendCommand/" .. tostring(code)
  local _, err = host.http_post(url, "{}", auth_headers())
  if err then
    -- 528: not paused, cannot resume — treat as success so a resume
    -- issued while already running does not fail the command.
    if code == CMD_RESUME and tostring(err):match("528") then
      return true
    end
    host.log("warn", "Zaptec sendCommand " .. tostring(code) .. " failed: " .. redact_http_err(err))
    return false
  end
  return true
end

local function update_charger(fields)
  local body = host.json_encode(fields)
  local _, err = host.http_post(
    BASE_URL .. "/api/chargers/" .. charger_id .. "/update",
    body,
    auth_headers())
  if err then
    host.log("warn", "Zaptec update failed: " .. redact_http_err(err))
    return false
  end
  return true
end

local function watts_to_amps(power_w)
  local p = phases
  if p < 1 then p = 1 end
  local amps = math.floor((tonumber(power_w) or 0) / (230 * p) + 0.5)
  if amps < 0 then amps = 0 end
  if amps > max_a then amps = max_a end
  return amps
end

function driver_init(config)
  host.set_make("Zaptec")
  config = config or {}
  email = config.email or config.username
  password = config.password
  serial_want = config.serial
  if serial_want == "" then serial_want = nil end
  if email == "" then email = nil end
  if password == "" then password = nil end
  if config.base_url and tostring(config.base_url) ~= "" then
    BASE_URL = tostring(config.base_url):gsub("/$", "")
  end
  local p = as_number(config.phases)
  if p == 1 or p == 3 then phases = p end
  local mn = as_number(config.min_a)
  if mn and mn > 0 then min_a = mn end
  local mx = as_number(config.max_a)
  if mx and mx > 0 then max_a = mx end

  if not email or not password then
    error("Zaptec: email/username and password required")
  end
  local ok, lerr = login()
  if not ok then
    error("Zaptec: initial login failed: " .. redact_http_err(lerr))
  end
  local id, rerr = resolve_charger()
  if not id then
    error("Zaptec: could not list chargers: " .. redact_http_err(rerr))
  end
  charger_id = id
  host.set_sn(charger_id)
  host.log("info", "Zaptec: driver initialized for " .. charger_id)
end

function driver_poll()
  if not charger_id or not email then
    return 10000
  end
  if not ensure_auth() then
    host.log("warn", "Zaptec: auth failed, skipping poll")
    return 10000
  end
  local resp, err = host.http_get(
    BASE_URL .. "/api/chargers/" .. charger_id .. "/state",
    auth_headers())
  if err then
    host.log("warn", "Zaptec: state poll failed: " .. redact_http_err(err))
    return 10000
  end
  local obs, oerr = observation_map(resp)
  if oerr or not obs then
    host.log("warn", "Zaptec: state decode failed")
    return 10000
  end

  local op_mode = as_number(obs[OBS_OP_MODE]) or OP_DISCONNECTED
  local power_w = as_number(obs[OBS_CHARGE_POWER]) or 0
  if power_w < 0 then power_w = 0 end
  local session_kwh = as_number(obs[OBS_SESSION_ENERGY]) or 0
  local session_wh = session_kwh * 1000
  local connected = (op_mode == OP_CONNECTED or op_mode == OP_CHARGING or op_mode == OP_FINISHED)
  local charging = (op_mode == OP_CHARGING)
  local max_current = as_number(obs[OBS_MAX_CURRENT])
  local active_phases = as_number(obs[OBS_ACTIVE_PHASES]) or phases

  host.emit("ev", {
    w          = power_w,
    connected  = connected,
    charging   = charging,
    session_wh = session_wh,
    max_a      = max_current,
    phases     = active_phases,
    op_mode    = op_mode,
  })

  local i1 = as_number(obs[OBS_CURRENT_L1])
  if i1 then host.emit_metric("ev_l1_a", i1) end
  local v1 = as_number(obs[OBS_VOLTAGE_L1])
  if v1 then host.emit_metric("ev_l1_v", v1) end
  host.emit_metric("ev_power_w", power_w)

  return 5000
end

function driver_command(action, power_w, cmd)
  if action == "init" or action == "deinit" then
    return true
  end
  if not charger_id then
    host.log("warn", "Zaptec: command before charger resolved")
    return false
  end
  if not ensure_auth() then return false end

  if action == "ev_pause" then
    local ok = send_command(CMD_STOP)
    if ok then paused_state = true end
    return ok
  end

  if action == "ev_start" or action == "ev_resume" then
    local ok = send_command(CMD_RESUME)
    if ok then paused_state = false end
    return ok
  end

  if action == "ev_set_current" then
    local amps = watts_to_amps(power_w)
    if cmd and type(cmd) == "table" then
      local req_phases = as_number(cmd.phases)
      if req_phases == 1 or req_phases == 3 then
        phases = req_phases
        if not update_charger({ maxChargePhases = phases }) then
          return false
        end
      end
    end
    if amps > 0 and amps < min_a then
      amps = 0
    end
    if amps <= 0 then
      local ok = send_command(CMD_STOP)
      if ok then paused_state = true end
      return ok
    end
    if not update_charger({ maxChargeCurrent = amps }) then
      return false
    end
    if paused_state then
      if send_command(CMD_RESUME) then
        paused_state = false
      end
    end
    return true
  end

  host.log("warn", "Zaptec: unknown action " .. tostring(action))
  return false
end

function driver_default_mode()
  -- Cloud charger keeps its last setpoint when FTW goes away.
end

function driver_cleanup()
  access_token = nil
  refresh_token = nil
end
