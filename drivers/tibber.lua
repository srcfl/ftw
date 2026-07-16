-- tibber.lua
-- Tibber Pulse driver — streams the home's live grid meter via Tibber's
-- GraphQL-transport-ws subscription (liveMeasurement). Read-only.
--
-- Configuration:
--   capabilities:
--     http:
--       allowed_hosts: ["api.tibber.com"]
--     websocket:
--       allowed_hosts: ["websocket-api.tibber.com"]
--   config:
--     api_key: <Tibber personal access token>
--     home_id: <optional — auto-resolved from /viewer/homes on first poll>
--
-- Getting an API key:
--   Generate a personal access token at
--     https://developer.tibber.com/settings/access-token
--   Grant at least the `homes` and `liveMeasurement` scopes. The token is
--   tied to your Tibber account; revoke it from the same page to rotate.
--
-- Protocol references:
--   https://developer.tibber.com/docs/guides/calling-api
--   https://github.com/enisdenjo/graphql-ws/blob/master/PROTOCOL.md
--
-- Sign convention:
--   Tibber `power` = consumption W (≥0 when importing).
--   Tibber `powerProduction` = production W (≥0 when exporting).
--   Site convention `meter.w` = positive on import, so we publish
--   `power - powerProduction` (giving negative numbers on export).

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "tibber",
  name         = "Tibber Pulse",
  manufacturer = "Tibber",
  version      = "1.0.0",
  protocols    = { "websocket", "http" },
  capabilities = { "meter" },
  description  = "Tibber Pulse grid meter via GraphQL-transport-ws liveMeasurement stream.",
  homepage     = "https://tibber.com",
  authors      = { "FTW contributors" },
  tested_models = { "Pulse IR", "Pulse HAN", "Pulse P1" },
  verification_status = "experimental",
  -- Settings UI: the api_key field gets rendered as a password input in the
  -- per-driver "secrets" slot. home_id is left out of config_secrets because
  -- it's optional and auto-resolved from /viewer/homes on first poll.
  config_secrets = { "api_key" },
  config_schema = {
    api_key = {
      label = "Tibber API key",
      type  = "string",
      secret = true,
      required = true,
      help = "Personal access token from https://developer.tibber.com/settings/access-token. Needs at least read access to home + liveMeasurement.",
    },
    home_id = {
      label = "Home ID (optional)",
      type  = "string",
      help  = "UUID of the home to subscribe to. Auto-resolved from /viewer/homes if omitted.",
    },
  },
}

PROTOCOL = "websocket"

local TIBBER_HTTP_URL = "https://api.tibber.com/v1-beta/gql"
local TIBBER_WS_URL   = "wss://websocket-api.tibber.com/v1-beta/gql/subscriptions"

-- Module state. The reconnect cooldown protects against hammering the
-- Tibber WS endpoint on auth failures or scheduled maintenance — five
-- seconds for a healthy reconnect, escalating to 60 s after repeated
-- failures so a wrong api_key doesn't burn the operator's rate budget.
local S = {
  api_key       = nil,
  home_id       = nil,
  ws_active     = false,  -- a connection was opened and connection_ack received
  subscribed    = false,
  init_sent     = false,
  last_emit_ms  = 0,
  reconnect_at  = 0,
  consec_fail   = 0,
  last_ping_ms  = 0,
}

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

local function backoff_ms()
  -- 5s, 10s, 20s, 30s, 60s … then cap.
  local steps = {5000, 10000, 20000, 30000, 60000}
  local i = math.min(S.consec_fail + 1, #steps)
  return steps[i]
end

-- HTTP query Tibber for the user's homes; cache the first one as the
-- subscription target. Called when home_id is not provided in config.
local function resolve_home_id()
  if S.home_id ~= nil and S.home_id ~= "" then return true end
  local body, err = host.http_post(
    TIBBER_HTTP_URL,
    '{"query":"{ viewer { homes { id } } }"}',
    {
      ["Authorization"] = "Bearer " .. (S.api_key or ""),
      ["Content-Type"]  = "application/json",
    })
  if err or not body then
    host.log("warn", "Tibber: home query failed: " .. tostring(err))
    return false
  end
  local ok, parsed = pcall(host.json_decode, body)
  if not ok or not parsed then
    host.log("warn", "Tibber: home query returned non-JSON: " .. tostring(body):sub(1, 200))
    return false
  end
  local data = parsed.data or {}
  local viewer = data.viewer or {}
  local homes = viewer.homes or {}
  if #homes == 0 then
    host.log("error", "Tibber: api_key resolves to no homes (check token permissions)")
    return false
  end
  if #homes > 1 then
    -- Multi-home Tibber accounts (e.g. house + cabin) must not be
    -- silently bound to the first home — picking the wrong one would
    -- feed unrelated grid power into control. Require explicit choice.
    local ids = {}
    for i, h in ipairs(homes) do ids[i] = tostring(h.id) end
    host.log("error",
      "Tibber: api_key resolves to " .. tostring(#homes) ..
      " homes (" .. table.concat(ids, ", ") ..
      "); set `home_id` explicitly in the driver config to pick one")
    return false
  end
  S.home_id = homes[1].id
  host.set_sn(S.home_id)
  host.log("info", "Tibber: home_id = " .. tostring(S.home_id))
  return true
end

local function ws_connect()
  if host.millis() < S.reconnect_at then return false end
  local ok, err = host.ws_open(TIBBER_WS_URL, {
    ["Sec-WebSocket-Protocol"] = "graphql-transport-ws",
    ["User-Agent"]             = "FTW/tibber",
  })
  if not ok then
    host.log("warn", "Tibber WS dial failed: " .. tostring(err))
    S.consec_fail = S.consec_fail + 1
    S.reconnect_at = host.millis() + backoff_ms()
    return false
  end
  S.ws_active   = false  -- not active until connection_ack arrives
  S.subscribed  = false
  S.init_sent   = false
  S.last_ping_ms = host.millis()
  -- Send connection_init with the bearer token in the payload — Tibber
  -- expects auth here, not in the HTTP upgrade headers.
  local init = host.json_encode({
    type    = "connection_init",
    payload = { token = S.api_key },
  })
  local _, err2 = host.ws_send(init)
  if err2 then
    host.log("warn", "Tibber WS connection_init send failed: " .. tostring(err2))
    host.ws_close()
    S.consec_fail = S.consec_fail + 1
    S.reconnect_at = host.millis() + backoff_ms()
    return false
  end
  S.init_sent = true
  host.log("info", "Tibber WS: connected, connection_init sent")
  return true
end

local function ws_subscribe()
  if S.subscribed then return end
  if not S.home_id then return end
  -- Pass home_id via GraphQL variables, never interpolated into the
  -- query string — keeps a malformed or hostile config value from
  -- breaking parse or injecting fragments.
  local q =
    "subscription($homeId: ID!){liveMeasurement(homeId: $homeId){" ..
    "power powerProduction " ..
    "accumulatedConsumption accumulatedProduction " ..
    "voltagePhase1 voltagePhase2 voltagePhase3 " ..
    "currentL1 currentL2 currentL3 " ..
    "signalStrength" ..
    "}}"
  local payload = host.json_encode({
    id      = "lm",
    type    = "subscribe",
    payload = {
      query     = q,
      variables = { homeId = S.home_id },
    },
  })
  local _, err = host.ws_send(payload)
  if err then
    host.log("warn", "Tibber WS subscribe send failed: " .. tostring(err))
    return
  end
  S.subscribed = true
  host.log("info", "Tibber WS: subscribed to liveMeasurement")
end

-- liveMeasurement payload → site-convention meter emit. Tibber publishes
-- ~once per second per Pulse generation; the host throttles `emit` itself
-- via the telemetry store, so we don't need to dedupe here.
local function emit_live(lm)
  if type(lm) ~= "table" then return end
  local p_import = tonumber(lm.power) or 0
  local p_export = tonumber(lm.powerProduction) or 0
  local meter = {
    w = p_import - p_export,
  }
  if lm.voltagePhase1 then meter.l1_v = tonumber(lm.voltagePhase1) end
  if lm.voltagePhase2 then meter.l2_v = tonumber(lm.voltagePhase2) end
  if lm.voltagePhase3 then meter.l3_v = tonumber(lm.voltagePhase3) end
  if lm.currentL1     then meter.l1_a = tonumber(lm.currentL1)     end
  if lm.currentL2     then meter.l2_a = tonumber(lm.currentL2)     end
  if lm.currentL3     then meter.l3_a = tonumber(lm.currentL3)     end
  -- Tibber publishes accumulatedConsumption / Production in kWh; the
  -- meter contract expects Wh.
  if lm.accumulatedConsumption then
    meter.import_wh = tonumber(lm.accumulatedConsumption) * 1000
  end
  if lm.accumulatedProduction then
    meter.export_wh = tonumber(lm.accumulatedProduction) * 1000
  end
  host.emit("meter", meter)

  -- Diagnostics: per-phase voltage/current + Pulse radio link.
  if meter.l1_v then host.emit_metric("meter_l1_v", meter.l1_v) end
  if meter.l2_v then host.emit_metric("meter_l2_v", meter.l2_v) end
  if meter.l3_v then host.emit_metric("meter_l3_v", meter.l3_v) end
  if meter.l1_a then host.emit_metric("meter_l1_a", meter.l1_a) end
  if meter.l2_a then host.emit_metric("meter_l2_a", meter.l2_a) end
  if meter.l3_a then host.emit_metric("meter_l3_a", meter.l3_a) end
  if lm.signalStrength then
    host.emit_metric("tibber_signal_dbm", tonumber(lm.signalStrength) or 0)
  end

  S.last_emit_ms = host.millis()
  S.consec_fail  = 0
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
  host.set_make("Tibber")
  config = config or {}
  if not config.api_key or config.api_key == "" then
    host.log("error", "Tibber: api_key missing from driver config — driver will idle")
    return
  end
  S.api_key = config.api_key
  if config.home_id and config.home_id ~= "" then
    S.home_id = config.home_id
    host.set_sn(S.home_id)
  end
  -- 2-second poll is enough — the WS read pump is async; the poll only
  -- drains the queue, sends keepalives, and handles reconnect.
  host.set_poll_interval(2000)
  host.log("info", "Tibber: init complete")
end

function driver_poll()
  if S.api_key == nil then return end

  -- Step 1: make sure we know the home_id.
  if not S.home_id then
    if not resolve_home_id() then
      -- backoff via HTTP retry — try again in 30s
      return 30000
    end
  end

  -- Step 2: open WS if not open.
  if not host.ws_is_open() then
    S.ws_active   = false
    S.subscribed  = false
    if not ws_connect() then return end
  end

  -- Step 3: drain inbound frames.
  local msgs = host.ws_messages() or {}
  for _, raw in ipairs(msgs) do
    if raw == "" then
      -- EOF sentinel: read pump exited. Close + schedule reconnect.
      host.log("warn", "Tibber WS: stream closed, will reconnect")
      host.ws_close()
      S.ws_active  = false
      S.subscribed = false
      S.consec_fail = S.consec_fail + 1
      S.reconnect_at = host.millis() + backoff_ms()
      break
    end
    local ok, msg = pcall(host.json_decode, raw)
    if ok and type(msg) == "table" then
      local t = msg.type
      if t == "connection_ack" then
        S.ws_active = true
        host.log("info", "Tibber WS: connection_ack")
        ws_subscribe()
      elseif t == "next" then
        local data = (msg.payload or {}).data or {}
        emit_live(data.liveMeasurement)
      elseif t == "ping" then
        -- Server keepalive — respond per graphql-transport-ws spec.
        host.ws_send('{"type":"pong"}')
      elseif t == "pong" then
        -- Our ping ack — nothing to do.
      elseif t == "error" then
        host.log("warn", "Tibber WS error: " .. tostring(raw):sub(1, 200))
      elseif t == "complete" then
        host.log("info", "Tibber WS: subscription complete, re-subscribing")
        S.subscribed = false
        ws_subscribe()
      else
        host.log("debug", "Tibber WS unknown type: " .. tostring(t))
      end
    end
  end

  -- Step 4: keepalive ping every ~30 s once the connection is acked.
  -- Tibber doesn't strictly require client pings, but it shortens
  -- detection of half-open TCP states from minutes to seconds.
  if S.ws_active and (host.millis() - S.last_ping_ms) > 30000 then
    host.ws_send('{"type":"ping"}')
    S.last_ping_ms = host.millis()
  end
end

function driver_command(action, power_w, cmd)
  -- Tibber Pulse is a read-only meter; no control surface.
  return false
end

function driver_default_mode()
  -- No actuation to revert.
end

function driver_cleanup()
  pcall(host.ws_close)
end
