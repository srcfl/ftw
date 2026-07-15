-- Tesla Vehicle Driver (read-only, via TeslaBLEProxy on local LAN)
-- Emits: Vehicle (DerVehicle)
-- Protocol: HTTP (Tesla Owner API shape — /api/1/vehicles/{VIN}/vehicle_data)
--
-- Fetches the vehicle's own SoC and charge_limit so FTW can
-- show the real "24 / 50 %" in the EV bubble and let the loadpoint
-- manager prefer the truth over its delivered-Wh inference. Designed
-- to talk to TeslaBLEProxy running on the same LAN
-- (https://github.com/wimaha/TeslaBleHttpProxy), which translates
-- HTTP/JSON to BLE under the hood. No Tesla cloud credentials, no
-- OAuth token, no internet round-trip — the proxy IS the key to the
-- vehicle.
--
-- Config: two fields, that's it.
--
--   drivers:
--     - name: tesla-garage
--       lua: drivers/tesla_vehicle.lua
--       capabilities:
--         http:
--           allowed_hosts: ["192.168.1.50"]   # IP of the proxy
--       config:
--         ip:  "192.168.1.50"
--         vin: "5YJ3E1EA1KF000000"

DRIVER = {
  id           = "tesla-vehicle",
  name         = "Tesla Vehicle (BLE Proxy)",
  manufacturer = "Tesla",
  version      = "0.1.0",
  protocols    = { "http" },
  capabilities = { "vehicle" },
  description  = "Read-only vehicle SoC + charge limit via Tesla API-compatible HTTP endpoint (e.g. TeslaBLEProxy).",
  homepage     = "https://github.com/wimaha/TeslaBleHttpProxy",
  authors      = { "FTW contributors" },
  tested_models = { "Model Y", "Model 3" },
  verification_status = "beta",
}

PROTOCOL = "http"

-- Runtime state. base_url is built from the `ip` config field.
-- TeslaBLEProxy listens on :8080 by default — hardcoded since that's
-- what the project ships. Poll + staleness are tuned in-driver;
-- operators touch only `ip` + `vin` in YAML.
local PROXY_PORT        = 8080
local POLL_INTERVAL_MS  = 60000
-- Faster cadence while the car is actively charging — the loadpoint
-- controller needs sub-watchdog-timeout resolution on amps + state to
-- avoid spurious "EV stopped" cascades. At the steady 60 s poll cadence
-- + HTTP RTT, every poll lands just past the 60 s site watchdog and
-- the driver gets repeatedly flagged stale, which spams wallbox-cycle
-- pause/resume and wake-kicks on the loadpoint side.
local POLL_INTERVAL_CHARGING_MS = 30000

-- Per-driver watchdog tolerance, registered with the host at init via
-- host.set_watchdog_timeout_s. Vehicle telemetry runs on a slower
-- natural cadence than mains drivers (BLE wake + cloud RTT) so 60 s
-- (the site default) is too tight and produces frequent stale flaps
-- that cascade into loadpoint wake-kick / wallbox-cycle spam. 5 min
-- gives plenty of headroom for occasional BLE wake-up retries while
-- still alerting on a truly-hung proxy.
local WATCHDOG_TIMEOUT_S = 300

-- While Charging, if no fresh telemetry has flowed for this long the
-- driver forces a BLE wake on the next poll regardless of the normal
-- 30/60 min wake cadence. Catches the case where the proxy returned
-- cached "Charging" once and then the car went silent — without the
-- forced wake we'd happily keep emitting that stale Charging reading
-- and the loadpoint controller would never see the real state. Only
-- fires while last.charging_state == "Charging"; parked cars don't
-- need active recovery (and shouldn't have their 12 V battery
-- drained by speculative wakes).
local CHARGING_FORCE_WAKE_AFTER_MS = 150000  -- 2.5 min
local STALE_AFTER_MS    = 900000
-- Every WAKEUP_INTERVAL_MS we attach `wakeup=true` to ONE poll so the
-- proxy forces a BLE wake. Without this the proxy serves cached data
-- indefinitely while the car sleeps and our SoC slowly drifts from
-- reality.
--
-- Two cadences: the conservative IDLE one (30 min) protects the 12 V
-- battery from constant BLE wakes when the car is parked + asleep;
-- the tighter CHARGING one (15 min) keeps SoC and charge_limit_pct
-- fresh while the car is on the wall so the MPC's planning targets
-- track the operator's in-app settings without 30-minute lag. The
-- car is already drawing AC power during a session, so the BLE-wake
-- battery-drain concern doesn't apply.
local WAKEUP_INTERVAL_MS          = 1800000  -- 30 min, idle
local WAKEUP_INTERVAL_CHARGING_MS = 900000   -- 15 min, while charging

-- When a non-wake poll fails (timeout, connection refused, anything
-- that isn't the proxy explicitly saying "BLE busy"), the most likely
-- cause is "car asleep and proxy gave up". Don't wait for the 30-min
-- periodic wake — arm a wake-retry on the very next poll. The retry
-- only fires if we haven't already attempted a wake within the recent
-- gap, so a flapping proxy can't trigger a wake storm.
local FAILURE_RETRY_INTERVAL_MS = 5000   -- schedule the retry this soon
local WAKE_RETRY_MIN_GAP_MS     = 10000  -- don't wake more than once per ~10 s

local base_url = nil
local vin = nil
local last_wakeup_ms = 0
local last_wake_attempt_ms = 0   -- includes both periodic + retry wakes
local pending_wake_retry = false -- set by failed poll, consumed by next poll

-- Cached last-known reading so we can keep publishing a value while
-- the vehicle is asleep. Tesla returns 408 "vehicle unavailable" when
-- the car is in deep sleep; we treat that as "use last-known" until
-- STALE_AFTER_MS elapses.
local last = {
  ts_ms                   = 0,
  soc                     = nil,
  charge_limit            = nil,
  charging_state          = nil,
  time_to_full            = nil,
  charge_amps             = nil,
  charger_actual_current  = nil,
}

local function auth_headers()
  -- TeslaBLEProxy on the LAN doesn't require a bearer token; it
  -- authenticates against the car via BLE itself. Plain JSON Accept
  -- header is all we need.
  return { ["Accept"] = "application/json" }
end

local function safe_http_err(err)
  if err == nil then return "ok" end
  return tostring(err):match("^(HTTP %d+)") or "request failed"
end

function driver_init(config)
  if not config then
    host.log("error", "tesla: config required (ip + vin)")
    return
  end
  local ip = config.ip
  vin = config.vin

  if not ip or ip == "" then
    host.log("error", "tesla: `ip` required (LAN address of TeslaBLEProxy)")
    return
  end
  if not vin or vin == "" then
    host.log("error", "tesla: `vin` required (vehicle VIN the proxy is paired to)")
    return
  end

  -- Accept bare IP (uses PROXY_PORT default) or host:port. We split
  -- on the LAST colon so IPv6-in-brackets-plus-port works too, though
  -- the typical config is "192.168.1.50" or "192.168.1.50:1234".
  local host_part, port_part = ip:match("^(.*):(%d+)$")
  if host_part and port_part then
    base_url = "http://" .. host_part .. ":" .. port_part
  else
    base_url = "http://" .. ip .. ":" .. tostring(PROXY_PORT)
  end

  host.set_make("Tesla")
  host.set_sn(tostring(vin))
  -- Loosen the watchdog so the loadpoint controller doesn't see brief
  -- BLE-wake stalls as "driver offline → revert to autonomous".
  if host.set_watchdog_timeout_s then
    host.set_watchdog_timeout_s(WATCHDOG_TIMEOUT_S)
  end
  -- Two-phase poll cadence:
  --  1. Init pumps the interval down to 500 ms so the registry's
  --     initial timer fires almost immediately. The first poll
  --     thus runs ≤ 1 s after startup / restart / hot-reload —
  --     no 60-second blank window where the dashboard says
  --     "no vehicle data".
  --  2. driver_poll bumps the interval back up to POLL_INTERVAL_MS
  --     (60 s) before returning, so steady-state polling is
  --     conservative on BLE wake-ups + Tesla cloud rate.
  -- The registry re-reads PollInterval() on every iteration, so the
  -- mid-flight change takes effect immediately.
  host.set_poll_interval(500)
  host.log("info", "tesla: driver initialized vin=" .. tostring(vin) ..
                   " proxy=" .. base_url ..
                   " poll_s=" .. tostring(POLL_INTERVAL_MS / 1000) ..
                   " (first poll within 1s)")
end

-- emit_last sends the cached reading with a stale flag computed from
-- the cached timestamp. Called when the HTTP poll fails or the
-- vehicle is asleep, so the UI keeps showing a number instead of a
-- blank.
local function emit_last()
  if last.soc == nil then
    return
  end
  local age = host.millis() - last.ts_ms
  local stale = age > STALE_AFTER_MS
  host.emit("vehicle", {
    soc                    = last.soc,
    charge_limit_pct       = last.charge_limit,
    charging_state         = last.charging_state,
    time_to_full_min       = last.time_to_full,
    charge_amps            = last.charge_amps,
    charger_actual_current = last.charger_actual_current,
    stale                  = stale,
  })
end

function driver_poll()
  if not base_url or not vin then
    return 10000
  end

  -- Bump the steady-state interval back up after the (deliberately
  -- short) init interval that gets us our first reading inside a
  -- second of startup. Idempotent — running set_poll_interval with the
  -- same value every poll is a no-op cost-wise.
  --
  -- Cadence depends on whether the car was Charging at the last
  -- successful poll: 30 s while charging (keeps us under the 60 s
  -- watchdog so the loadpoint controller doesn't see spurious stale
  -- flags), 60 s otherwise (parked / disconnected — no urgency).
  local steady_ms = POLL_INTERVAL_MS
  if last.charging_state == "Charging" then
    steady_ms = POLL_INTERVAL_CHARGING_MS
  end
  host.set_poll_interval(steady_ms)

  -- endpoints=charge_state narrows the response to just what we
  -- care about (SoC + limit + charging_state + time_to_full).
  --
  -- wakeup=true is OFF on the steady-state poll. The proxy returns
  -- cached data from its own background sync (typically ≤ 5 s
  -- fresh, fine for our 60 s poll cadence + pokes). Forcing a BLE
  -- wake on every poll caused HTTP 503 "Command Disallowed" storms
  -- when the driver was poked alongside regular polls.
  --
  -- BUT once every WAKEUP_INTERVAL_MS (30 min) we DO request a
  -- wakeup so cached data can't drift forever while the car sleeps.
  -- The cadence is anchored to driver-process uptime, so the FIRST
  -- poll after startup intentionally does NOT force a wakeup. Reason:
  -- a crash-loop (the host wedges, restarts every few seconds) would
  -- otherwise hammer Tesla's BLE radio at multiple wakeups per minute,
  -- draining the 12 V battery and likely hitting Tesla's "Command
  -- Disallowed" rate limit. The tradeoff is that immediately after a
  -- restart the dashboard may show up to 30 min of stale SoC; the
  -- operator can press "Verify connection" in settings to force a
  -- one-shot wake, or wait for the next scheduled 30-min wake.
  local now = host.millis()
  -- Cadence depends on whether the last-known state was Charging — see
  -- WAKEUP_INTERVAL_CHARGING_MS comment above. "Charging" comes from
  -- the Tesla payload's charge_state.charging_state field; anything
  -- else (Stopped / Complete / Disconnected / unknown) uses the
  -- conservative 30-min cadence.
  local wake_cadence_ms = (last.charging_state == "Charging")
    and WAKEUP_INTERVAL_CHARGING_MS or WAKEUP_INTERVAL_MS
  local do_wakeup = false
  if pending_wake_retry and ((now - last_wake_attempt_ms) >= WAKE_RETRY_MIN_GAP_MS) then
    -- Previous poll failed; we armed a retry. Consume it.
    do_wakeup = true
    pending_wake_retry = false
    host.log("info", "tesla: wake-retry firing after previous poll failure")
  elseif (last_wakeup_ms > 0) and ((now - last_wakeup_ms) >= wake_cadence_ms) then
    do_wakeup = true
  elseif last.charging_state == "Charging" and last.ts_ms > 0 and
         ((now - last.ts_ms) >= CHARGING_FORCE_WAKE_AFTER_MS) and
         ((now - last_wake_attempt_ms) >= WAKE_RETRY_MIN_GAP_MS) then
    -- Stale-while-charging recovery. The car was last seen Charging
    -- but no fresh telemetry has flowed for >2.5 min — likely the
    -- proxy returned cached data and the car went BLE-silent. Force
    -- a wake on this poll rather than continuing to emit_last() the
    -- stale "Charging" reading. Gated to Charging only so a parked
    -- car doesn't get its 12 V drained by speculative wakes.
    do_wakeup = true
    host.log("info", "tesla: stale-while-charging force-wake (" ..
                     tostring(math.floor((now - last.ts_ms) / 1000)) ..
                     "s since last emit)")
  end
  if last_wakeup_ms == 0 then
    -- Anchor the periodic cadence at "now" so the first FORCED wake
    -- fires exactly wake_cadence_ms after startup, not on first poll.
    last_wakeup_ms = now
  end
  local url = base_url .. "/api/1/vehicles/" .. vin ..
              "/vehicle_data?endpoints=charge_state"
  if do_wakeup then
    url = url .. "&wakeup=true"
    last_wakeup_ms = now
    last_wake_attempt_ms = now
    host.log("info", "tesla: forcing BLE wakeup on this poll" ..
                     " (cadence=" .. tostring(wake_cadence_ms / 60000) .. "min" ..
                     " charging=" .. tostring(last.charging_state == "Charging") .. ")")
  end
  -- host.http_get returns (body_string, nil) or (nil, error_string) —
  -- first return is the body directly, NOT a table with .body. The
  -- earlier tesla_vehicle iterations treated it as a table and got
  -- length 0 on every poll, which silently emit_last()'d the driver.
  local body, err = host.http_get(url, auth_headers())
  if err ~= nil then
    -- 503 "Command Disallowed" or 408 timeouts mean the proxy's
    -- BLE radio is busy (usually because we just poked or the car
    -- just started charging and the radio negotiation hasn't
    -- cleared). Back off to a longer interval rather than
    -- continuing to hammer at the steady-state rate — every poll
    -- in this state piles onto the rate-limit and prolongs it.
    -- Recovery is automatic when the next emit succeeds:
    -- driver_poll resets to POLL_INTERVAL_MS at the top of the
    -- next call. Don't arm a wake-retry for these — the radio is
    -- already busy; adding another wake won't help.
    local es = tostring(err)
    if es:match("HTTP 503") or es:match("HTTP 408") or es:match("Command Disallowed") then
      host.log("debug", "tesla: proxy busy (BLE busy) — backing off 3 min")
      emit_last()
      return 180000  -- 3 min
    end
    -- Any other error (most commonly the host's 15 s HTTP timeout
    -- when the car is asleep and the proxy's read of charge_state
    -- never returns) — the car is probably asleep and the next
    -- ordinary poll would just time out again. Arm a wake-retry
    -- so the next poll attaches `&wakeup=true` and forces the
    -- proxy to BLE-wake before reading. We don't arm if THIS poll
    -- already woke (no point retrying with another wake — that's
    -- the case the proxy is genuinely failing) — fall back to the
    -- normal interval and let the periodic cadence catch up.
    host.log("warn", "tesla: poll HTTP error: " .. es)
    emit_last()
    if not do_wakeup then
      pending_wake_retry = true
      host.log("info", "tesla: wake-retry armed (next poll will force BLE wake)")
      return FAILURE_RETRY_INTERVAL_MS
    end
    return steady_ms
  end
  if not body or body == "" then
    host.log("warn", "tesla: empty body from proxy")
    emit_last()
    return steady_ms
  end

  local decoded, derr = host.json_decode(body)
  if derr or not decoded then
    host.log("warn", "tesla: json decode failed: " .. tostring(derr))
    emit_last()
    return steady_ms
  end

  -- Response shapes (in the wild):
  --   1. TeslaBLEProxy double-wrapped:
  --        { response = { response = { charge_state = {...} } } }
  --   2. Tesla Owner API envelope:
  --        { response = { charge_state = {...} } }
  --   3. Bare: { charge_state = {...} }
  local charge_state = nil
  if type(decoded) == "table" then
    if type(decoded.response) == "table" then
      if type(decoded.response.response) == "table" and
         decoded.response.response.charge_state then
        charge_state = decoded.response.response.charge_state
      elseif decoded.response.charge_state then
        charge_state = decoded.response.charge_state
      end
    end
    if not charge_state and decoded.charge_state then
      charge_state = decoded.charge_state
    end
  end
  if not charge_state then
    host.log("debug", "tesla: no charge_state in response")
    emit_last()
    return steady_ms
  end

  local soc = tonumber(charge_state.battery_level)
  local limit = tonumber(charge_state.charge_limit_soc)
  -- Per-vehicle in-app current limit. The car negotiates DOWN to
  -- this amperage regardless of what the wallbox offers; surface it
  -- so operators can see "wallbox commands 16A but car capped to 5A"
  -- mismatches without digging into the raw proxy response.
  local charge_amps = tonumber(charge_state.charge_amps)
  local charger_actual_current = tonumber(charge_state.charger_actual_current)
  -- Field name depends on source:
  --   - TeslaBLEProxy emits `minutes_to_full_charge` (integer minutes).
  --   - Tesla Owner API emits `time_to_full_charge` (fractional hours).
  local ttf_min = tonumber(charge_state.minutes_to_full_charge)
  if ttf_min == nil then
    local ttf_h = tonumber(charge_state.time_to_full_charge)
    if ttf_h ~= nil then ttf_min = math.floor(ttf_h * 60 + 0.5) end
  end
  local cs = charge_state.charging_state
  if type(cs) ~= "string" then cs = nil end

  if soc ~= nil then
    last.soc                    = soc
    last.charge_limit           = limit
    last.charging_state         = cs
    last.time_to_full           = ttf_min
    last.charge_amps            = charge_amps
    last.charger_actual_current = charger_actual_current
    last.ts_ms                  = host.millis()

    host.log("info", "tesla: emit soc=" .. tostring(soc) ..
                     " limit=" .. tostring(limit) ..
                     " state=" .. tostring(cs) ..
                     " amps=" .. tostring(charge_amps) ..
                     "/" .. tostring(charger_actual_current))
    local emit_err = host.emit("vehicle", {
      soc                     = soc,
      charge_limit_pct        = limit,
      charging_state          = cs,
      time_to_full_min        = ttf_min,
      charge_amps             = charge_amps,
      charger_actual_current  = charger_actual_current,
      stale                   = false,
    })
    if emit_err then
      host.log("warn", "tesla: emit returned error: " .. tostring(emit_err))
    end
  else
    -- Malformed response (no battery_level) → keep last-known.
    emit_last()
  end

  return steady_ms
end

function driver_command(action, _, _)
  -- Generic vehicle wake. The Go side fires `wake_up` whenever it
  -- wants fresh telemetry without side effects — schedule edits,
  -- the rising edge of wallbox-delivering-power, operator clicks
  -- "Refresh". Any vehicle driver can implement this against its
  -- own back-end; nothing here is Tesla-specific at the protocol
  -- level. We hit the proxy's dedicated wake endpoint (rather than
  -- the GET-with-wake the poll uses) so the response confirms
  -- "wake initiated" instead of returning vehicle data — cleaner
  -- separation between "wake" and "read".
  --
  -- Reset the periodic-wake anchor so we don't immediately re-fire
  -- a second wake on the next 30/15-min cadence right after the
  -- caller already woke the car. Also clear any pending failure
  -- retry — if a previous poll failed and armed a retry, the
  -- caller's wake just supersedes it.
  if action == "wake_up" or action == "ev_wake" then
    if not base_url or not vin then
      host.log("warn", "tesla: wake_up before init")
      return false
    end
    local url = base_url .. "/api/1/vehicles/" .. vin .. "/command/wake_up"
    local body, err = host.http_post(url, "{}", auth_headers())
    if err then
      local es = tostring(err)
      if es:match("HTTP 503") or es:match("HTTP 408") then
        host.log("debug", "tesla: wake_up busy, will retry on next caller: " .. es)
        return false
      end
      host.log("warn", "tesla: wake_up failed: " .. es)
      return false
    end
    last_wakeup_ms = host.millis()
    last_wake_attempt_ms = last_wakeup_ms
    pending_wake_retry = false
    local snippet = (body and #body > 0) and body:sub(1, 200) or "(empty body)"
    host.log("info", "tesla: wake_up sent: " .. snippet)
    return true
  end
  -- Wake-and-start support. The loadpoint controller fires the
  -- generic `charge_start` action (defined as a cross-driver
  -- protocol — any vehicle driver can implement it) when the
  -- matched vehicle detached mid-session and won't accept current
  -- from the wallbox. We translate that to the Tesla BLE command;
  -- other vehicle drivers (BMW, Audi, Polestar via their own
  -- proxies) implement the same action against their own back-end
  -- — the Go side has no Tesla-specific knowledge.
  if action == "charge_start" or action == "ev_start" then
    if not base_url or not vin then
      host.log("warn", "tesla: charge_start before init")
      return false
    end
    local url = base_url .. "/api/1/vehicles/" .. vin .. "/command/charge_start"
    -- Empty JSON object body — TeslaBLEProxy's command endpoints
    -- accept GET-ish POSTs; some Tesla SDKs send `{}` for parity
    -- with the cloud API. Either form works on the proxy.
    local body, err = host.http_post(url, "{}", auth_headers())
    if err then
      local es = tostring(err)
      -- 503 / "Command Disallowed" means the proxy's BLE radio is
      -- busy or rate-limited. Not an error from our perspective —
      -- the controller's cooldown will retry on the next window.
      if es:match("HTTP 503") or es:match("HTTP 408") then
        host.log("debug", "tesla: charge_start busy/asleep, will retry: " .. es)
        return false
      end
      host.log("warn", "tesla: charge_start failed: " .. es)
      return false
    end
    -- Surface the proxy's response body so we can see WHY a
    -- nominally-successful POST didn't wake the car. Common cases:
    -- Tesla returned `not_charging` (already at limit), `is_charging`
    -- (idempotent / no-op), or a vehicle-side rejection (e.g. user
    -- has charge-on-schedule enabled).
    local snippet = (body and #body > 0) and body:sub(1, 200) or "(empty body)"
    host.log("info", "tesla: charge_start response: " .. snippet)
    return true
  end
  host.log("debug", "tesla: command ignored: " .. tostring(action))
  return false
end

function driver_default_mode()
  -- Nothing to do — there's no output state to reset.
end

function driver_cleanup()
  -- Reset every persistent piece of state so a hot-reload looks
  -- identical to a fresh process start (the `last_wakeup_ms` reset
  -- in particular re-anchors the periodic cadence so we don't fire
  -- a wakeup the moment the reloaded driver runs its first poll).
  last.soc                    = nil
  last.charge_limit           = nil
  last.charging_state         = nil
  last.time_to_full           = nil
  last.charge_amps            = nil
  last.charger_actual_current = nil
  last.ts_ms                  = 0
  last_wakeup_ms              = 0
  last_wake_attempt_ms        = 0
  pending_wake_retry          = false
end
