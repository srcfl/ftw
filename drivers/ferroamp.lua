-- ferroamp.lua
-- Ferroamp EnergyHub MQTT driver
-- Emits: PV, Battery, Meter telemetry

DRIVER = {
  id           = "ferroamp",
  name         = "Ferroamp EnergyHub",
  manufacturer = "Ferroamp",
  version      = "1.0.0",
  protocols    = { "mqtt" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Ferroamp EnergyHub with ESO battery + SSO solar strings (3-phase).",
  homepage     = "https://ferroamp.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "EnergyHub XL" },
  verification_status = "production",
  verified_by = { "frahlg@homelab-rpi:14d" },
  verified_at = "2026-04-18",
  verification_notes = "In continuous use on 3-phase 16A SE site, MPC + dispatch control loop exercised daily.",
  connection_defaults = {
    port     = 1883,
    username = "extapi",
    password = "ferroampExtApi",
  },
}
--
-- Subscribes to:
--   extapi/data/ehub  - main hub data (grid, frequency, energy counters, PV summary)
--   extapi/data/eso   - battery storage object (SoC, battery power, voltage, current)
--   extapi/data/sso   - solar string object (per-string PV power)
--
-- Ferroamp payload format: {"key": {"val": value}} or {"key": {"L1": v1, "L2": v2, "L3": v3}}
-- Energy counters are in mJ (millijoules); convert to Wh: mJ / 3,600,000
--
-- Sign convention:
--   PV w:      always negative (generation)
--   Battery w: positive = charging, negative = discharging
--   Meter w:   positive = import, negative = export

PROTOCOL = "mqtt"

-- Cached state from each topic
local ehub_data = nil
local eso_data = nil
local sso_data = nil

-- Last-arrival timestamp per topic (host.millis()). The EnergyHub
-- normally publishes ehub at ~1 Hz; if it goes silent (power off,
-- fuse blow, broker partition) the cached tables above stay
-- populated. Without per-topic age checks the driver would re-emit
-- last-known values on every poll, host.emit would re-stamp
-- LastSuccess, and the watchdog could not flip the driver offline.
-- Real incident: 2026-05-02 fuse blow left ferroamp emitting
-- pv_w=-3996.7040 / meter_w=-7294.0490 identical to four decimals
-- for 30+ minutes while the EnergyHub itself was unpowered.
local ehub_ts = 0
local eso_ts  = 0
local sso_ts  = 0

-- Treat cached topic data as stale beyond this age. EnergyHub
-- publishes ehub at ~1 Hz and eso/sso slightly slower; 30 s gives
-- generous slack for a WiFi blip or broker reconnect without
-- flipping the driver offline.
local STALE_AFTER_MS = 30000

-- Optional config knob: when `skip_battery` is true the driver will
-- NOT emit battery telemetry even when the ESO/pbat fields are
-- present on the wire. Useful for dev setups that want a PV-only
-- dashboard fed by the otherwise full-featured Ferroamp sim.
local SKIP_BATTERY = false
local last_control_mode = nil

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- Extract a value from Ferroamp's {"key": {"val": v}} structure.
-- Returns the raw val (string/number), or the field table if no "val" key.
local function extract_val(obj, key)
    if not obj then return nil end
    local field = obj[key]
    if not field then return nil end
    if type(field) == "table" and field.val ~= nil then
        return field.val
    end
    return field
end

-- Sum L1+L2+L3 from a phase table {"L1":..,"L2":..,"L3":..}, or return scalar.
-- Also handles numeric arrays for backwards compatibility.
local function sum_phases(val)
    if val == nil then return 0 end
    if type(val) == "number" then return val end
    if type(val) == "string" then return tonumber(val) or 0 end
    if type(val) == "table" then
        -- Try named keys first (current Ferroamp format)
        if val.L1 or val.L2 or val.L3 then
            return (tonumber(val.L1) or 0) + (tonumber(val.L2) or 0) + (tonumber(val.L3) or 0)
        end
        -- Fall back to numeric array
        local s = 0
        for _, v in ipairs(val) do
            s = s + (tonumber(v) or 0)
        end
        return s
    end
    return 0
end

-- Get a specific phase value from {"L1":..,"L2":..,"L3":..} or array [1,2,3].
local function phase_val(val, phase)
    if val == nil then return 0 end
    if type(val) ~= "table" then return 0 end
    -- Named key (e.g. "L1")
    if val[phase] then return tonumber(val[phase]) or 0 end
    -- Numeric index fallback (L1=1, L2=2, L3=3)
    local idx = ({L1=1, L2=2, L3=3})[phase]
    if idx and val[idx] then return tonumber(val[idx]) or 0 end
    return 0
end

-- Convert Ferroamp mJ counter to Wh
local function mj_to_wh(mj_val)
    local mj = tonumber(mj_val) or 0
    return mj / 3600000
end

-- Prefer the primary topic value, but fall back to the role-specific topic
-- when the primary is missing or reports zero while the fallback has a real
-- magnitude. Some EnergyHub payloads keep pbat/ppv useful on ESO/SSO even
-- when ehub's summary field is zeroed.
local function choose_power(primary, fallback)
    local p = tonumber(primary)
    local f = tonumber(fallback)
    if p ~= nil and math.abs(p) > 0.5 then return p end
    if f ~= nil and math.abs(f) > 0.5 then return f end
    if p ~= nil then return p end
    return f
end

local function eso_battery_power(data)
    if not data then return nil end
    local ubat = tonumber(extract_val(data, "ubat"))
    local ibat = tonumber(extract_val(data, "ibat"))
    if ubat == nil or ibat == nil then return nil end
    return ubat * ibat
end

local function sso_power(data)
    if not data then return nil end
    local ppv = extract_val(data, "ppv")
    if ppv ~= nil then return tonumber(ppv) end

    -- The SSO topic does not publish ppv in all External API versions.
    -- It does publish string voltage/current; use their product as a
    -- measured-string fallback. Some firmware reports PV voltage in kV
    -- below 10, so normalize that shape back to volts.
    local upv = tonumber(extract_val(data, "upv"))
    local ipv = tonumber(extract_val(data, "ipv"))
    if upv == nil or ipv == nil then return nil end
    local scale = math.abs(upv) < 10 and 1000 or 1
    return upv * scale * ipv
end

local function publish_auto(trans_id)
    local err = host.mqtt_publish("extapi/control/request",
        string.format('{"transId":"%s","cmd":{"name":"auto"}}', trans_id))
    if not err then last_control_mode = "auto" end
    return err
end

-- Force the EnergyHub to hold the battery at 0 W instead of handing
-- control back to autonomous self-consumption. `auto` reads the
-- house load and discharges to cover it, which silently overrides
-- any planner slot that wants the battery idle so PV surplus can
-- export. `discharge` with arg=0 keeps the inverter in forced mode
-- but locked at zero — equivalent to Sungrow's forced-idle path.
local function publish_idle(trans_id)
    local err = host.mqtt_publish("extapi/control/request",
        string.format('{"transId":"%s","cmd":{"name":"discharge","arg":0}}', trans_id))
    if not err then last_control_mode = "idle" end
    return err
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Ferroamp")

    -- Honour the `skip_battery` config knob if set — the driver stays
    -- otherwise unchanged, but host.emit("battery", …) is skipped so
    -- the rest of the stack (dashboard, models, planner) sees no
    -- battery capability from this instance.
    if config and config.skip_battery then
        SKIP_BATTERY = true
        host.log("info", "Ferroamp: skip_battery=true — battery emission disabled")
    end

    -- Subscribe to telemetry topics
    host.mqtt_subscribe("extapi/data/ehub")
    host.mqtt_subscribe("extapi/data/eso")
    host.mqtt_subscribe("extapi/data/sso")

    -- Subscribe to control response topic to verify commands are received
    host.mqtt_subscribe("extapi/result")

    -- Query API version to verify connectivity and external API access
    local version_cmd = '{"transId":"init","cmd":{"name":"extapiversion"}}'
    host.mqtt_publish("extapi/control/request", version_cmd)
    host.log("info", "Ferroamp: sent extapiversion query")

    -- Ensure we start in auto mode (clean state)
    publish_auto("init")
    host.log("info", "Ferroamp: set auto mode on init")
end

function driver_poll()
    local now = host.millis()
    local messages = host.mqtt_messages()
    if not messages then messages = {} end

    -- Process incoming messages and stamp arrival time per topic
    for _, msg in ipairs(messages) do
        local ok, data = pcall(host.json_decode, msg.payload)
        if ok and data then
            if msg.topic == "extapi/data/ehub" then
                ehub_data = data; ehub_ts = now
            elseif msg.topic == "extapi/data/eso" then
                eso_data = data; eso_ts = now
            elseif msg.topic == "extapi/data/sso" then
                sso_data = data; sso_ts = now
            end
        end
    end

    -- Drop stale caches so the rest of the poll falls through and
    -- the watchdog catches us when the EnergyHub stops publishing.
    -- Per-topic so a partial outage (e.g. eso lags but ehub flows)
    -- still lets the live channels through.
    if ehub_data and (now - ehub_ts) > STALE_AFTER_MS then
        host.log("warn", "Ferroamp: ehub stale (" .. (now - ehub_ts) .. " ms) — dropping cache")
        ehub_data = nil
    end
    if eso_data and (now - eso_ts) > STALE_AFTER_MS then
        eso_data = nil
    end
    if sso_data and (now - sso_ts) > STALE_AFTER_MS then
        sso_data = nil
    end

    -- Diagnostics: per-topic age into the long-format TS DB so
    -- operators can see partial outages directly in the metric
    -- browser. Reported as "0" when never seen yet (ts = 0).
    host.emit_metric("ehub_age_ms", ehub_ts == 0 and 0 or (now - ehub_ts))
    host.emit_metric("eso_age_ms",  eso_ts  == 0 and 0 or (now - eso_ts))
    host.emit_metric("sso_age_ms",  sso_ts  == 0 and 0 or (now - sso_ts))

    --------------------------------------------------------------------------
    -- Meter (grid connection point)
    --------------------------------------------------------------------------
    if ehub_data then
        local pext     = extract_val(ehub_data, "pext")     -- per-phase grid power (W)
        local gridfreq = extract_val(ehub_data, "gridfreq") -- grid frequency (Hz)
        local ul       = extract_val(ehub_data, "ul")       -- per-phase voltage (V)
        -- iext = per-phase GRID current at the service-entrance CTs, the
        -- same source pext is derived from. NOT il (which is inverter AC
        -- current and misses any load not routed through the Ferroamp
        -- inverter, e.g. an EV charger on a separate breaker — that mix
        -- made the fuse bars under-read by the EV share of total import).
        local iext     = extract_val(ehub_data, "iext")     -- per-phase grid current (A)
        -- 3-phase energy totals in mJ
        local wextconsq3p = extract_val(ehub_data, "wextconsq3p") -- total import mJ
        local wextprodq3p = extract_val(ehub_data, "wextprodq3p") -- total export mJ

        local meter = {}

        -- Grid power: negative = exporting, positive = importing
        meter.w    = sum_phases(pext)
        meter.l1_w = phase_val(pext, "L1")
        meter.l2_w = phase_val(pext, "L2")
        meter.l3_w = phase_val(pext, "L3")

        -- Grid frequency
        if gridfreq then
            meter.hz = tonumber(gridfreq) or 0
        end

        -- Per-phase voltage
        meter.l1_v = phase_val(ul, "L1")
        meter.l2_v = phase_val(ul, "L2")
        meter.l3_v = phase_val(ul, "L3")

        -- Per-phase grid current (from service-entrance CTs, consistent
        -- with pext above — previously read il by mistake, which is
        -- inverter AC current).
        meter.l1_a = phase_val(iext, "L1")
        meter.l2_a = phase_val(iext, "L2")
        meter.l3_a = phase_val(iext, "L3")

        -- Energy counters (mJ → Wh)
        if wextconsq3p then
            meter.import_wh = mj_to_wh(wextconsq3p)
        end
        if wextprodq3p then
            meter.export_wh = mj_to_wh(wextprodq3p)
        end

        host.emit("meter", meter)
        -- Diagnostics: long-format TS DB
        if meter.l1_w then host.emit_metric("meter_l1_w", meter.l1_w) end
        if meter.l2_w then host.emit_metric("meter_l2_w", meter.l2_w) end
        if meter.l3_w then host.emit_metric("meter_l3_w", meter.l3_w) end
        if meter.l1_v then host.emit_metric("meter_l1_v", meter.l1_v) end
        if meter.l2_v then host.emit_metric("meter_l2_v", meter.l2_v) end
        if meter.l3_v then host.emit_metric("meter_l3_v", meter.l3_v) end
        if meter.l1_a then host.emit_metric("meter_l1_a", meter.l1_a) end
        if meter.l2_a then host.emit_metric("meter_l2_a", meter.l2_a) end
        if meter.l3_a then host.emit_metric("meter_l3_a", meter.l3_a) end
        if meter.hz   then host.emit_metric("grid_hz",    meter.hz)   end

        local state = extract_val(ehub_data, "state")
        if state then host.emit_metric("ehub_state", tonumber(state) or 0) end
    end

    --------------------------------------------------------------------------
    -- PV (solar generation)
    --------------------------------------------------------------------------
    if ehub_data or sso_data then
        local ppv = choose_power(extract_val(ehub_data, "ppv"), sso_power(sso_data))
        if ppv then
            -- Negate: Ferroamp reports PV as positive, convention requires negative
            host.emit("pv", { w = -ppv })
        end
    end

    --------------------------------------------------------------------------
    -- Battery
    --------------------------------------------------------------------------
    if (ehub_data or eso_data) and not SKIP_BATTERY then
        local pbat = eso_battery_power(eso_data)
        if pbat == nil then
            pbat = choose_power(extract_val(ehub_data, "pbat"), extract_val(eso_data, "pbat"))
        end
        if pbat then
            local battery = {}
            -- Ferroamp: positive pbat = discharging, negate for convention
            -- Convention: positive = charging, negative = discharging
            battery.w = -pbat

            -- Enrich with ESO data (battery-specific telemetry)
            if eso_data then
                local soc = extract_val(eso_data, "soc")
                if soc then
                    local soc_val = tonumber(soc) or 0
                    -- Ferroamp reports SoC as 0-100%, convert to 0.0-1.0 fraction
                    if soc_val > 1 then soc_val = soc_val / 100 end
                    battery.soc = soc_val
                end

                local ubat = extract_val(eso_data, "ubat")
                if ubat then battery.v = tonumber(ubat) or 0 end

                local ibat = extract_val(eso_data, "ibat")
                if ibat then battery.a = tonumber(ibat) or 0 end

                local eso_udc = extract_val(eso_data, "udc")
                if eso_udc then host.emit_metric("eso_dc_link_v", tonumber(eso_udc) or 0) end
                local eso_relay = extract_val(eso_data, "relaystatus")
                if eso_relay then host.emit_metric("eso_relaystatus", tonumber(eso_relay) or 0) end
                local eso_fault = extract_val(eso_data, "faultcode")
                if eso_fault then host.emit_metric("eso_faultcode", tonumber(eso_fault) or 0) end

                -- Battery energy counters (mJ → Wh)
                local wbatprod = extract_val(eso_data, "wbatprod")
                local wbatcons = extract_val(eso_data, "wbatcons")
                if wbatprod then battery.discharge_wh = mj_to_wh(wbatprod) end
                if wbatcons then battery.charge_wh    = mj_to_wh(wbatcons) end
            end

            host.emit("battery", battery)
            if battery.v then host.emit_metric("battery_dc_v", battery.v) end
            if battery.a then host.emit_metric("battery_dc_a", battery.a) end
        end
    end

    if sso_data then
        local sso_udc = extract_val(sso_data, "udc")
        if sso_udc then host.emit_metric("sso_dc_link_v", tonumber(sso_udc) or 0) end
        local sso_upv = extract_val(sso_data, "upv")
        if sso_upv then host.emit_metric("sso_pv_v", tonumber(sso_upv) or 0) end
        local sso_ipv = extract_val(sso_data, "ipv")
        if sso_ipv then host.emit_metric("sso_pv_a", tonumber(sso_ipv) or 0) end
        local sso_relay = extract_val(sso_data, "relaystatus")
        if sso_relay then host.emit_metric("sso_relaystatus", tonumber(sso_relay) or 0) end
        local sso_fault = extract_val(sso_data, "faultcode")
        if sso_fault then host.emit_metric("sso_faultcode", tonumber(sso_fault) or 0) end
    end

    return 1000
end

----------------------------------------------------------------------------
-- Control
----------------------------------------------------------------------------

-- Control: Ferroamp External API
-- Reference: https://github.com/henricm/ha-ferroamp
-- Topic: extapi/control/request
-- Commands:
--   {"transId":"...","cmd":{"name":"charge","arg":<watts>}}    — force charge (arg always positive)
--   {"transId":"...","cmd":{"name":"discharge","arg":<watts>}} — force discharge (arg always positive)
--   {"transId":"...","cmd":{"name":"auto"}}                    — return to auto mode
-- EMS convention: positive power_w = charge, negative = discharge
function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        local tid = "ems-" .. tostring(host.millis())
        if power_w > 0 then
            -- Charge: use "charge" command with positive watts
            local payload = string.format(
                '{"transId":"%s","cmd":{"name":"charge","arg":%d}}',
                tid, math.floor(power_w)
            )
            local err = host.mqtt_publish("extapi/control/request", payload)
            if not err then last_control_mode = "charge" end
            return err
        elseif power_w < 0 then
            -- Discharge: use "discharge" command with positive watts
            local payload = string.format(
                '{"transId":"%s","cmd":{"name":"discharge","arg":%d}}',
                tid, math.floor(math.abs(power_w))
            )
            local err = host.mqtt_publish("extapi/control/request", payload)
            if not err then last_control_mode = "discharge" end
            return err
        else
            -- Zero: force idle at 0 W. Do NOT fall back to autonomous
            -- self-consumption — that would let the EnergyHub discharge
            -- to cover load and silently override the planner.
            if last_control_mode == "idle" then
                return true
            end
            return publish_idle(tid)
        end
    elseif action == "curtail" then
        local payload = string.format(
            '{"transId":"ems","cmd":{"name":"pplim","arg":%d}}',
            math.floor(math.abs(power_w))
        )
        return host.mqtt_publish("extapi/control/request", payload)
    elseif action == "curtail_disable" then
        return host.mqtt_publish("extapi/control/request",
            '{"transId":"ems","cmd":{"name":"pplim","arg":0}}')
    elseif action == "deinit" then
        return publish_auto("ems")
    end
    return false
end

function driver_default_mode()
    publish_auto("watchdog")
end

function driver_cleanup()
    -- Leave the EnergyHub in autonomous self-consumption when the EMS
    -- stops or the driver hot-reloads. Otherwise the last forced
    -- charge/discharge reference can remain visible in the Ferroamp app.
    pcall(publish_auto, "cleanup")
    ehub_data = nil
    eso_data = nil
    sso_data = nil
end
