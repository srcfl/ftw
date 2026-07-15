-- CTEK Chargestorm EV Charger Driver — Hybrid (Modbus control + MQTT telemetry/state)
-- Emits: EV
-- Protocols: Modbus/TCP (control + fallback telemetry), MQTT (state + live telemetry)
--
-- Why hybrid:
--   The CSOS Modbus Automation API (v1, base 0x1xxx) gives us full telemetry
--   (per-phase V/I, total W, lifetime Wh) and current-limit control, but the
--   clean connector state code — "CHRG" (charging), "PAUS" (paused), "EVRD"
--   (EV ready / connected, awaiting authorisation or schedule), "IDLE", etc.
--   — is published ONLY over the CCU's Local MQTT publisher. With Modbus
--   alone we have to *guess* "connected" from "limit > 0 and max_assign > 0",
--   which mis-reports connected when the cable is unplugged but a charging
--   window is queued.
--
--   The CCU's Local MQTT publisher also produces a higher-quality telemetry
--   stream (decimal voltages, grid frequency, real timestamps) on the `em`
--   topic. This driver prefers MQTT-sourced telemetry when fresh and falls
--   back to Modbus heuristics on every cycle — so a misconfigured or
--   unreachable broker never takes the loadpoint offline.
--
-- CSOS setup (web UI, https://<ccu-ip>/):
--   1) Configuration → MQTT → enable (the CCU itself runs the broker on
--      port 1883 by default — no auth on stock CSOS 4.9.x).
--   2) Automation → ModbusTCPEnable = true,
--                   modbus_tcp_automation_api_version = 1.
--
-- Observed CTEK CSOS 4.9.x topic shape:
--   CTEK/<station-id>/evse<c>/status     {"assigned":0,"state":"PAUS","timestamp":...}
--   CTEK/<station-id>/evse<c>/em         {"current":[a,a,a],"energy":Wh,"frequency":Hz,
--                                         "power":W,"voltage":[v,v,v],"timestamp":...}
--   CTEK/<station-id>/evse<c>/meterinfo  {"serialno":"272274U"}
--
--   <station-id> is a CSOS-internal identifier (e.g. "91728M03W4010406"),
--   *not* the EVSE serial — so we use a `+` wildcard for it by default.
--   Modbus is point-to-point per driver instance, so the wildcard only ever
--   matches our own CCU's traffic.
--
-- Config example (config.yaml):
--   drivers:
--     - name: ctek-chargestorm
--       lua: drivers/ctek_hybrid.lua
--       capabilities:
--         modbus:
--           host: 192.168.1.190
--           port: 502
--           unit_id: 1          # 1 = EVSE1, 2 = EVSE2
--         mqtt:
--           host: 192.168.1.190 # CCU's own broker, OR a relay broker
--           port: 1883
--           username: ""        # optional
--           password: ""        # optional
--       config:
--         phases:    3
--         min_a:     6
--         max_a:     16
--         voltage_v: 230
--         # MQTT topic config — defaults match stock CSOS 4.9.x. Substitute
--         # <c> for the connector id; <station> for an explicit station ID
--         # if you want to pin it instead of the default `+` wildcard.
--         mqtt_connector:        1
--         mqtt_discover_topic:   "CTEK/#"                       # one-shot per-topic logging
--         mqtt_state_topic:      "CTEK/+/evse<c>/status"        # JSON; state field
--         mqtt_state_json_field: "state"                        # JSON key for the code
--         mqtt_em_topic:         "CTEK/+/evse<c>/em"            # JSON; live telemetry
--         mqtt_max_stale_ms:     30000                          # after this, fall back to Modbus

DRIVER = {
  id           = "ctek-chargestorm-hybrid",
  name         = "CTEK Chargestorm (Modbus + MQTT)",
  manufacturer = "CTEK",
  version      = "0.2.0",
  protocols    = { "modbus", "mqtt" },
  capabilities = { "ev" },
  description  = "CTEK Chargestorm Connected 2/3 — MQTT for state + live telemetry (preferred), Modbus/TCP for control + telemetry fallback.",
  homepage     = "https://www.ctek.com",
  authors      = { "FTW contributors" },
  tested_models = { "Chargestorm Connected 2", "Chargestorm Connected 3" },
  verification_status = "alpha",
  verification_notes = "Observed against CSOS 4.9.x on station 91728M03W4010406 (EVSE serial 272274U). MQTT broker is the CCU itself; defaults assume the stock topic layout `CTEK/<station>/evse<n>/...`. Falls back to Modbus telemetry whenever MQTT is stale (>mqtt_max_stale_ms).",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Modbus register map (CSOS Automation interface v1.0)
----------------------------------------------------------------------------
local REG_API_VERSION   = 0x1000
local REG_API_STATUS    = 0x1001
local REG_METER_TYPE    = 0x1002
local REG_SERIAL_BASE   = 0x1003   -- 6 regs → 12 ASCII chars
local REG_TELEMETRY     = 0x1100   -- 9 regs: energy(2) + I(3) + V(3) + W(1)
local REG_CHARGE_LIMIT  = 0x1200   -- r/w
local REG_MAX_ASSIGN    = 0x1201   -- r/o

----------------------------------------------------------------------------
-- Runtime config (overridden from config.yaml in driver_init)
----------------------------------------------------------------------------
local phases    = 3
local min_a     = 6
local max_a     = 16
local voltage_v = 230

local mqtt_connector       = 1
local mqtt_discover_topic  = "CTEK/#"
local mqtt_state_topic_cfg = "CTEK/+/evse<c>/status"
local mqtt_state_field     = "state"
local mqtt_em_topic_cfg    = "CTEK/+/evse<c>/em"
local mqtt_max_stale_ms    = 30000

-- Last setpoint we successfully wrote.
local last_set_a = 0
local sn_read    = false
local serial     = ""

-- MQTT-derived state. `mqtt_state_str` is the raw text we received last
-- (uppercased); `mqtt_state_ts` is host.millis() at receipt. We compute
-- charging/connected from `mqtt_state_str` when fresh.
local mqtt_state_str = ""
local mqtt_state_ts  = 0
local mqtt_state_topic_resolved = ""
local mqtt_subscribed_state     = false

-- MQTT-derived live telemetry from the `em` topic. Preferred over Modbus
-- when fresh (lower CCU poll load, decimal V/I/Hz). `mqtt_em_ts` is the
-- host-side receipt time stamp.
local mqtt_em_power_w   = 0
local mqtt_em_energy_wh = 0
local mqtt_em_l1_a, mqtt_em_l2_a, mqtt_em_l3_a = 0, 0, 0
local mqtt_em_l1_v, mqtt_em_l2_v, mqtt_em_l3_v = 0, 0, 0
local mqtt_em_hz        = 0
local mqtt_em_ts        = 0
local mqtt_em_topic_resolved = ""
local mqtt_subscribed_em     = false

local seen_topics = {}                 -- for one-shot discovery logging

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

local function clamp_amps(a)
    if a == nil then return 0 end
    a = math.floor(a + 0.5)
    if a <= 0 then return 0 end
    if a < min_a then return 0 end
    if a > max_a then return max_a end
    return a
end

local function watts_to_amps(power_w)
    if not power_w or power_w <= 0 then return 0 end
    return math.floor((power_w / voltage_v / phases) + 0.5)
end

local function decode_ascii(regs, n)
    local s = ""
    for i = 1, n do
        local r = regs[i] or 0
        local hi = math.floor(r / 256)
        local lo = r % 256
        if hi == 0 then break end
        if hi >= 32 and hi < 127 then s = s .. string.char(hi) end
        if lo == 0 then break end
        if lo >= 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
end

local function write_setpoint(amps)
    local err = host.modbus_write(REG_CHARGE_LIMIT, amps)
    if err ~= nil and err ~= "" then
        host.log("warn", "CTEK-hybrid: write charging limit failed: " .. tostring(err))
        return false
    end
    last_set_a = amps
    return true
end

local function read_setpoint()
    local ok, regs = pcall(host.modbus_read, REG_CHARGE_LIMIT, 1, "holding")
    if not ok or not regs or not regs[1] then return nil end
    return regs[1]
end

-- Substitute <sn> and <c> placeholders in a topic template.
local function expand_topic(tpl)
    if not tpl or tpl == "" then return "" end
    local t = tpl
    t = string.gsub(t, "<sn>", serial)
    t = string.gsub(t, "<c>", tostring(mqtt_connector))
    return t
end

-- Match an inbound topic against an MQTT subscription pattern.
-- `+` matches exactly one level; `#` matches the remainder (must be the
-- final segment). Needed because the broker delivers the wildcard-expanded
-- topic (e.g. CTEK/91728M03W4010406/evse1/status) while we hold the
-- pattern (CTEK/+/evse1/status).
local function topic_match(pattern, topic)
    if pattern == "" or topic == "" then return false end
    if pattern == topic then return true end
    -- Split on "/" cheaply.
    local function split(s)
        local out = {}
        for seg in string.gmatch(s, "([^/]+)") do out[#out+1] = seg end
        return out
    end
    local p = split(pattern)
    local t = split(topic)
    local pi = 1
    while pi <= #p do
        local seg = p[pi]
        if seg == "#" then
            return pi == #p   -- # only legal as final segment
        end
        local ts = t[pi]
        if ts == nil then return false end
        if seg ~= "+" and seg ~= ts then return false end
        pi = pi + 1
    end
    return pi - 1 == #t
end

-- Returns true if the topic template can be expanded right now (no
-- unresolved <sn> waiting for the Modbus serial read). <c> is always
-- known at init.
local function topic_ready(tpl)
    if tpl == "" then return false end
    if string.find(tpl, "<sn>", 1, true) and serial == "" then
        return false
    end
    return true
end

-- Late-bind topic subscriptions. Called both from driver_init (so topics
-- without <sn> subscribe immediately) and from driver_poll on the first
-- successful serial read.
local function maybe_subscribe_state()
    if mqtt_subscribed_state then return false end
    if not topic_ready(mqtt_state_topic_cfg) then return false end
    mqtt_state_topic_resolved = expand_topic(mqtt_state_topic_cfg)
    host.mqtt_subscribe(mqtt_state_topic_resolved)
    host.log("info", "CTEK-hybrid: subscribed to state topic " .. mqtt_state_topic_resolved)
    mqtt_subscribed_state = true
    return true
end

local function maybe_subscribe_em()
    if mqtt_subscribed_em then return false end
    if not topic_ready(mqtt_em_topic_cfg) then return false end
    mqtt_em_topic_resolved = expand_topic(mqtt_em_topic_cfg)
    host.mqtt_subscribe(mqtt_em_topic_resolved)
    host.log("info", "CTEK-hybrid: subscribed to em topic " .. mqtt_em_topic_resolved)
    mqtt_subscribed_em = true
    return true
end

-- Extract a state code from a payload that may be raw text or JSON.
local function parse_state_payload(payload)
    if not payload or payload == "" then return "" end
    -- Strip surrounding whitespace + quotes for the trivial-text case.
    local trimmed = payload:gsub("^%s+", ""):gsub("%s+$", "")
    if mqtt_state_field == "" then
        -- Raw-text mode: take the whole payload as the state code, but
        -- strip JSON quoting in case the broker wraps it.
        trimmed = trimmed:gsub('^"', ""):gsub('"$', "")
        return string.upper(trimmed)
    end
    local ok, data = pcall(host.json_decode, payload)
    if not ok or type(data) ~= "table" then
        -- JSON decode failed — fall back to raw text so we still get a
        -- usable code if the broker switches modes mid-flight.
        return string.upper(trimmed)
    end
    local v = data[mqtt_state_field]
    if v == nil then return "" end
    return string.upper(tostring(v))
end

-- Parse an `em` topic payload. CTEK CSOS publishes JSON of the form
--   {"current":[a,a,a], "energy":Wh, "frequency":Hz, "power":W,
--    "voltage":[v,v,v], "timestamp":"..."}
-- Returns true on a clean parse so the caller can timestamp it.
local function parse_em_payload(payload)
    if not payload or payload == "" then return false end
    local ok, data = pcall(host.json_decode, payload)
    if not ok or type(data) ~= "table" then return false end

    if tonumber(data.power) then mqtt_em_power_w = tonumber(data.power) end
    if tonumber(data.energy) then mqtt_em_energy_wh = tonumber(data.energy) end
    if tonumber(data.frequency) then mqtt_em_hz = tonumber(data.frequency) end

    local cur = data.current
    if type(cur) == "table" then
        mqtt_em_l1_a = tonumber(cur[1]) or mqtt_em_l1_a
        mqtt_em_l2_a = tonumber(cur[2]) or mqtt_em_l2_a
        mqtt_em_l3_a = tonumber(cur[3]) or mqtt_em_l3_a
    end
    local volt = data.voltage
    if type(volt) == "table" then
        mqtt_em_l1_v = tonumber(volt[1]) or mqtt_em_l1_v
        mqtt_em_l2_v = tonumber(volt[2]) or mqtt_em_l2_v
        mqtt_em_l3_v = tonumber(volt[3]) or mqtt_em_l3_v
    end
    return true
end

-- Map a CTEK/CSOS state code to (charging, connected, request_active). The
-- state strings here are CTEK's internal codes as observed in CSOS firmware
-- 4.9.x; if a future firmware adds new ones, the function returns nils →
-- the caller falls back to Modbus heuristics for that frame only.
--
--   IDLE / A / "DISC"        — cable not present
--   EVRD / B / "CONN"        — vehicle connected, not charging yet
--   CHRG / C                 — actively charging
--   PAUS                     — vehicle connected, paused (we, EVSE, or grid)
--   NCRQ                     — vehicle connected but NOT requesting current
--                              (car hit its own SoC target or its onboard
--                              schedule ended). Distinct from PAUS because
--                              the vehicle, not the EVSE, owns the refusal —
--                              waking it back into CHRG requires user action
--                              at the car. Loadpoint controller uses this
--                              to stop allocating PV surplus to a phantom
--                              sink.
--   FAULT / ERR / "ERR_*"    — fault → conservative: connected=true so the
--                              loadpoint UI shows the device, charging=false
--
-- request_active is the third return: true when the vehicle is (or could
-- imminently be) drawing current, false when the vehicle has explicitly
-- refused. nil for plug-out / unknown states — caller defaults to true so
-- non-NCRQ-aware drivers don't accidentally trip completion.
local function classify_state(code)
    if code == nil or code == "" then return nil, nil, nil end
    if code == "IDLE" or code == "A" or code == "DISC" or code == "DISCONNECTED" then
        return false, false, nil
    end
    if code == "NCRQ" then
        -- Connected, not charging, vehicle refusing — explicit signal.
        return false, true, false
    end
    if code == "EVRD" or code == "B" or code == "CONN" or code == "CONNECTED" or code == "PAUS" or code == "PAUSED" then
        return false, true, true
    end
    if code == "CHRG" or code == "C" or code == "CHARGING" then
        return true, true, true
    end
    if code:find("^FAULT") or code:find("^ERR") or code == "F" then
        return false, true, nil
    end
    -- Unknown code — caller falls back.
    return nil, nil, nil
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("CTEK")

    if config then
        if tonumber(config.phases) then
            local p = math.floor(tonumber(config.phases))
            if p == 1 or p == 3 then phases = p end
        end
        if tonumber(config.min_a)     then min_a     = math.floor(tonumber(config.min_a))     end
        if tonumber(config.max_a)     then max_a     = math.floor(tonumber(config.max_a))     end
        if tonumber(config.voltage_v) then voltage_v = tonumber(config.voltage_v)             end

        if tonumber(config.mqtt_connector) then
            mqtt_connector = math.floor(tonumber(config.mqtt_connector))
        end
        if type(config.mqtt_discover_topic) == "string" then
            mqtt_discover_topic = config.mqtt_discover_topic
        end
        if type(config.mqtt_state_topic) == "string" then
            mqtt_state_topic_cfg = config.mqtt_state_topic
        end
        if type(config.mqtt_state_json_field) == "string" then
            mqtt_state_field = config.mqtt_state_json_field
        end
        if type(config.mqtt_em_topic) == "string" then
            mqtt_em_topic_cfg = config.mqtt_em_topic
        end
        if tonumber(config.mqtt_max_stale_ms) then
            mqtt_max_stale_ms = math.floor(tonumber(config.mqtt_max_stale_ms))
        end
    end

    if min_a < 6 then min_a = 6 end
    if max_a < min_a then max_a = min_a end

    -- Sanity-check API version. Same logic as drivers/ctek.lua.
    local ok, api_regs = pcall(host.modbus_read, REG_API_VERSION, 2, "holding")
    if ok and api_regs and api_regs[1] then
        host.log("info", string.format(
            "CTEK-hybrid: API v%d, status %d (expected v1)",
            api_regs[1], api_regs[2] or 0))
    end

    host.log("info", string.format(
        "CTEK-hybrid: init phases=%d min=%dA max=%dA V=%.0f connector=%d stale=%dms",
        phases, min_a, max_a, voltage_v, mqtt_connector, mqtt_max_stale_ms))

    local cur = read_setpoint()
    if cur then
        last_set_a = cur
        host.log("info", "CTEK-hybrid: charge limit readback = " .. tostring(cur) .. "A")
    end

    -- Discovery subscription — fires immediately so we capture topics even
    -- before the serial is known. If the operator configured an explicit
    -- state topic with no <sn>, we can also subscribe right now; the
    -- serial-dependent case is deferred to driver_poll.
    if mqtt_discover_topic ~= "" then
        host.mqtt_subscribe(mqtt_discover_topic)
        host.log("info", "CTEK-hybrid: discovery subscribed to " .. mqtt_discover_topic)
    end
    maybe_subscribe_state()
    maybe_subscribe_em()
end

function driver_poll()
    -- One-shot serial read, anchors device identity to the EVSE serial.
    if not sn_read then
        local ok_sn, sn_regs = pcall(host.modbus_read, REG_SERIAL_BASE, 6, "holding")
        if ok_sn and sn_regs then
            local sn = decode_ascii(sn_regs, 6)
            if #sn > 0 then
                serial = sn
                host.set_sn(sn)
                sn_read = true
                -- Complete any subscriptions that needed the serial.
                maybe_subscribe_state()
                maybe_subscribe_em()
            end
        end
    end

    -- Drain MQTT messages. We do this every poll regardless of whether
    -- the state topic is resolved — messages on the discovery topic
    -- still need to be drained or the host's buffer fills up.
    local now = host.millis()
    local messages = host.mqtt_messages()
    if messages then
        for _, msg in ipairs(messages) do
            -- One-shot discovery logging — first time we see a topic,
            -- print it with a preview of the payload. Helps the operator
            -- pin down the right `mqtt_state_topic` for their broker.
            if not seen_topics[msg.topic] then
                seen_topics[msg.topic] = true
                local preview = msg.payload or ""
                if #preview > 80 then preview = preview:sub(1, 80) .. "…" end
                host.log("info", "CTEK-hybrid: mqtt topic seen "
                    .. msg.topic .. " = " .. preview)
            end
            -- State topic (pattern may contain MQTT wildcards).
            if topic_match(mqtt_state_topic_resolved, msg.topic) then
                local code = parse_state_payload(msg.payload)
                if code ~= "" then
                    if code ~= mqtt_state_str then
                        host.log("info", "CTEK-hybrid: state " .. mqtt_state_str .. " → " .. code)
                    end
                    mqtt_state_str = code
                    mqtt_state_ts  = now
                end
            end
            -- em topic (pattern may contain MQTT wildcards).
            if topic_match(mqtt_em_topic_resolved, msg.topic) then
                if parse_em_payload(msg.payload) then
                    mqtt_em_ts = now
                end
            end
        end
    end

    -- Control-block readback is cheap (2 regs) and required regardless of
    -- telemetry source: we need the live charging-limit + max-assignment
    -- for both the EV emit and the loadpoint clamp.
    local limit, max_assign = last_set_a, max_a
    local ok_ctl, ctl = pcall(host.modbus_read, REG_CHARGE_LIMIT, 2, "holding")
    if ok_ctl and ctl then
        limit       = ctl[1] or last_set_a
        max_assign  = ctl[2] or max_a
        last_set_a  = limit
    end

    -- Telemetry source selection. MQTT `em` is preferred — decimal V/I, real
    -- frequency, lower load on the CCU. Modbus is the fallback when MQTT is
    -- stale or never arrived (operator pointed `mqtt:` at the wrong broker,
    -- CSOS local-MQTT disabled, etc.) — so a misconfigured broker never
    -- takes telemetry offline.
    local em_age_ms = (mqtt_em_ts == 0) and -1 or (now - mqtt_em_ts)
    local em_fresh  = (mqtt_em_ts ~= 0) and (em_age_ms <= mqtt_max_stale_ms)
    local ev_w
    local i_l1, i_l2, i_l3
    local v_l1, v_l2, v_l3
    local lifetime_wh
    local hz = 0
    if em_fresh then
        ev_w        = mqtt_em_power_w
        i_l1, i_l2, i_l3 = mqtt_em_l1_a, mqtt_em_l2_a, mqtt_em_l3_a
        v_l1, v_l2, v_l3 = mqtt_em_l1_v, mqtt_em_l2_v, mqtt_em_l3_v
        lifetime_wh = mqtt_em_energy_wh
        hz          = mqtt_em_hz
    else
        -- Modbus fallback: 9-reg telemetry block in a single transaction.
        local ok_tel, tel = pcall(host.modbus_read, REG_TELEMETRY, 9, "holding")
        if ok_tel and tel then
            lifetime_wh = host.decode_u32_be(tel[1], tel[2])
            i_l1 = (tel[3] or 0) / 1000
            i_l2 = (tel[4] or 0) / 1000
            i_l3 = (tel[5] or 0) / 1000
            v_l1 = (tel[6] or 0) / 10
            v_l2 = (tel[7] or 0) / 10
            v_l3 = (tel[8] or 0) / 10
            ev_w = tel[9] or 0
        else
            host.log("warn", "CTEK-hybrid: Modbus telemetry fallback read failed")
            ev_w = 0
            i_l1, i_l2, i_l3 = 0, 0, 0
            v_l1, v_l2, v_l3 = 0, 0, 0
            lifetime_wh = 0
        end
    end

    -- Authoritative state: prefer MQTT when fresh.
    local state_age_ms = (mqtt_state_ts == 0) and -1 or (now - mqtt_state_ts)
    local state_fresh  = (mqtt_state_ts ~= 0) and (state_age_ms <= mqtt_max_stale_ms)
    local charging, connected, request_active
    if state_fresh then
        local c, k, r = classify_state(mqtt_state_str)
        if c ~= nil and k ~= nil then
            charging, connected, request_active = c, k, r
        end
    end
    if charging == nil or connected == nil then
        -- Modbus / em-derived heuristic. With em-fresh telemetry the
        -- voltage + current numbers are accurate enough to trust the
        -- current-based plug detection.
        local max_phase_a = math.max(i_l1, i_l2, i_l3)
        charging  = (ev_w > 100) or (max_phase_a > 1.0)
        connected = charging or (limit >= min_a and max_assign > 0)
        -- Modbus can't distinguish "throttled to 0" from "car refusing";
        -- leave request_active unset so the Go side defaults to true and
        -- keeps the legacy behaviour for non-NCRQ-aware code paths.
    end
    if request_active == nil then
        request_active = true
    end

    host.emit("ev", {
        w               = ev_w,
        connected       = connected,
        charging        = charging,
        request_active  = request_active,
        max_a           = limit,
        phases          = phases,
        l1_v            = v_l1, l2_v = v_l2, l3_v = v_l3,
        l1_a            = i_l1, l2_a = i_l2, l3_a = i_l3,
        lifetime_wh     = lifetime_wh,
        state_code      = mqtt_state_str,
    })

    host.emit_metric("ev_set_current_a",  limit)
    host.emit_metric("ev_max_assign_a",   max_assign)
    host.emit_metric("ev_l1_a",           i_l1)
    host.emit_metric("ev_l2_a",           i_l2)
    host.emit_metric("ev_l3_a",           i_l3)
    host.emit_metric("ev_l1_v",           v_l1)
    host.emit_metric("ev_l2_v",           v_l2)
    host.emit_metric("ev_l3_v",           v_l3)
    host.emit_metric("ev_power_w",        ev_w)
    host.emit_metric("ev_lifetime_wh",    lifetime_wh)
    if hz > 0 then host.emit_metric("grid_hz", hz) end
    host.emit_metric("ev_mqtt_state_age_ms", state_age_ms < 0 and 0 or state_age_ms)
    host.emit_metric("ev_mqtt_state_fresh",  state_fresh and 1 or 0)
    host.emit_metric("ev_mqtt_em_age_ms",    em_age_ms < 0 and 0 or em_age_ms)
    host.emit_metric("ev_mqtt_em_fresh",     em_fresh and 1 or 0)

    return 5000
end

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end

    if action == "ev_set_current" then
        local amps = clamp_amps(watts_to_amps(power_w))
        host.log("debug", "CTEK-hybrid: ev_set_current "
            .. tostring(power_w) .. "W → " .. tostring(amps) .. "A")
        return write_setpoint(amps)
    end

    if action == "ev_pause" then
        return write_setpoint(0)
    end

    if action == "ev_start" or action == "ev_resume" then
        local amps = (last_set_a and last_set_a >= min_a) and last_set_a or max_a
        return write_setpoint(amps)
    end

    host.log("warn", "CTEK-hybrid: unknown action " .. tostring(action))
    return false
end

function driver_default_mode()
    -- Same stance as drivers/ctek.lua: leave the current limit alone so a
    -- watchdog trip doesn't interrupt a charging session.
end

function driver_cleanup()
end
