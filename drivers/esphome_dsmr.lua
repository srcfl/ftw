-- esphome_dsmr.lua
-- ESPHome-flashed P1 / DSMR smart-meter reader (e.g. Sourceful Zap on
-- the open-source firmware at github.com/erikarenhill/sourceful-zap-esphome).
-- Emits: Meter (read-only)
-- Protocol: HTTP — ESPHome `web_server: version: 3` REST endpoints.
--
-- Why a separate driver from `zap.lua`:
--   `zap.lua` talks to the *closed-source Sourceful Zap gateway* via its
--   /api/devices JSON tree. This driver targets the *ESPHome firmware*
--   you flash onto the same hardware (or any DIY ESP32 + DSMR meter
--   running ESPHome's `dsmr` component). The two firmwares expose
--   completely different HTTP shapes; one driver can't cover both.
--
-- Endpoint contract (ESPHome web_server v3):
--   GET /sensor/<object_id>        → {"id":"sensor-<…>","value":<num>,"state":"<num+unit>"}
--   GET /text_sensor/<object_id>   → {"id":"text_sensor-<…>","value":"<str>","state":"<str>"}
-- Object IDs vary by firmware YAML. ESPHome derives them from each
-- entity's `name:` (lower-cased, spaces → underscores) unless an
-- explicit `object_id:` is set. The driver probes aliases in order
-- and uses the first hit:
--
--   Totals (kW): power_consumed|power_delivered,
--                power_produced|power_returned
--   Per-phase kW: power_{consumed|delivered}_lN or _phase_N
--   Voltage (V): voltage_lN or voltage_phase_N
--   Current (A): current_lN or current_phase_N
--   Energy (kWh): energy_consumed|energy_delivered,
--                 energy_produced|energy_returned
--
-- Sourceful Zap pins explicit object_ids (*_consumed, current_l1, …).
-- Default ESPHome DSMR YAMLs (community P1 readers, DIY ESP32, etc.)
-- usually slug each entity's `name:` instead (current_phase_1, …) and
-- expose import/export totals as power_delivered / power_returned unless
-- the operator renamed them.
-- Serial: electric_meter_equipment_id, meter_identification,
--          dsmr_identification
--
-- Sign convention (SITE = positive W flows INTO the site):
--   meter.w = (power_consumed - power_produced) * 1000
--   so positive = importing from grid, negative = exporting. Per-phase
--   `lN_w` is built the same way (consumed_lN − produced_lN).
--
-- Config example (config.yaml):
--   drivers:
--     - name: zap-p1
--       lua: drivers/esphome_dsmr.lua
--       is_site_meter: true
--       capabilities:
--         http:
--           allowed_hosts: ["192.168.1.147"]   # or "srcful-zap-p1.local"
--       config:
--         host: "192.168.1.147"                # required; mDNS name or IP
--         # make: "Sourceful"                  # optional, default "ESPHome"
--         # poll_ms: 5000                      # optional, default 5000
--
-- ESPHome's `dsmr` updates roughly once per telegram (~1 s on most Swedish
-- meters, longer on slower ones). Polling faster than the telegram rate
-- just re-reads the same numbers — default 5 s keeps the meter fresh
-- without hammering the ESP32's tiny TCP stack.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "esphome-dsmr",
  name         = "ESPHome DSMR (P1)",
  manufacturer = "ESPHome",
  version      = "1.0.1",
  protocols    = { "http" },
  capabilities = { "meter" },
  description  = "Smart meter via ESPHome web_server v3 + dsmr component (Sourceful Zap on open firmware, DIY ESP32+P1, etc.).",
  homepage     = "https://github.com/erikarenhill/sourceful-zap-esphome",
  authors      = { "FTW contributors" },
  tested_models = {
    "Sourceful Zap P1 (sourceful-zap-esphome firmware)",
    "ESPHome DSMR P1 readers using name-derived object_ids (SlimmeLezer+)",
  },
  verification_status = "experimental",
  verification_notes = "Validated on Sourceful Zap open firmware and a live SlimmeLezer+ running ESPHome DSMR YAML with name-derived phase object_ids (current_phase_N, power_consumed_phase_N).",
  connection_defaults = {
    -- No host default — the operator must point us at their device's
    -- IP or mDNS hostname. We deliberately don't guess: ESPHome devices
    -- pick their hostname from the YAML `name:` field, which is unique
    -- per build, so any hard-coded default would be wrong on most sites.
  },
}

PROTOCOL = "http"

----------------------------------------------------------------------------
-- State
----------------------------------------------------------------------------

local esp_host  = nil    -- "192.168.1.147" or "srcful-zap-p1.local" (no scheme)
local make_name = "ESPHome"
local poll_ms   = 5000
local sn_set    = false  -- one-shot: only call host.set_sn once we've read it
local serial_attempts = 0
local SERIAL_MAX_ATTEMPTS = 12

-- Capability flags filled in once on the first successful poll. The
-- ESPHome firmware varies in which entities it exposes (the
-- reference sourceful-zap-p1.yaml exposes reactive power per phase
-- + totals, but a stripped-down DIY firmware might not). Probing
-- once at startup avoids burning HTTP round-trips on perpetual 404s
-- — eight reactive channels at ~10 ms each adds 80 ms to every
-- poll otherwise.
local caps_probed       = false
local has_reactive      = false

-- Exponential backoff on poll failures. Without this, a meter that
-- drops off the LAN gets hammered every poll_ms (default 5 s) until
-- it comes back — rude to the network and to the ESP32's tiny TCP
-- stack, especially since dropping the meter is exactly the kind of
-- thing that happens during DHCP renewals + flaky wifi. We grow the
-- effective interval after each failure and reset on first success.
local consecutive_failures = 0
local BACKOFF_MAX_MS       = 60000
local function backoff_ms()
    if consecutive_failures <= 0 then return poll_ms end
    -- 2^N grows fast; cap at MAX. The first failure already doubles
    -- the interval, which is what we want — most transient blips
    -- recover within one poll cycle and don't deserve a third try
    -- 5 s later.
    local mult = 2 ^ math.min(consecutive_failures, 8)
    local interval = poll_ms * mult
    if interval > BACKOFF_MAX_MS then return BACKOFF_MAX_MS end
    return interval
end

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

local function base_url()
    if string.sub(esp_host, 1, 7) == "http://" or string.sub(esp_host, 1, 8) == "https://" then
        return esp_host
    end
    return "http://" .. esp_host
end

-- GET one ESPHome entity, return its decoded `.value` field or (nil, err).
-- domain: "sensor" or "text_sensor". object_id: lowercased name w/ underscores.
local function fetch_entity(domain, object_id)
    local url = base_url() .. "/" .. domain .. "/" .. object_id
    local body, err = host.http_get(url)
    if err then return nil, err end
    local data = host.json_decode(body)
    if not data then return nil, "json decode failed: " .. tostring(body):sub(1, 80) end
    return data.value, nil
end

-- Numeric sensor read; returns (n, nil) or (nil, err). Treats explicit
-- JSON null / non-number as missing — ESPHome publishes null when the
-- DSMR parser hasn't yet produced a fresh telegram for that field
-- (gas-meter ID on an electric-only feed is the typical case).
local function fetch_num(object_id)
    local v, err = fetch_entity("sensor", object_id)
    if err then return nil, err end
    local n = tonumber(v)
    if n == nil then return nil, "sensor/" .. object_id .. " value is not numeric (" .. tostring(v) .. ")" end
    return n, nil
end

-- Try numeric sensor ids in order; first success wins. Returns (n, nil)
-- or (nil, last_err). 404s on earlier aliases are expected — callers
-- only log when every alias misses.
local function fetch_num_first(ids)
    local last_err = nil
    for _, id in ipairs(ids) do
        local n, err = fetch_num(id)
        if n ~= nil then return n, nil end
        last_err = err
    end
    return nil, last_err
end

-- Best-effort serial discovery. Tries the dedicated DSMR equipment-id
-- text_sensor first (newer meters populate it), falls back to the OBIS
-- meter-identification line (e.g. LGF5E360 on the LG meters Sourceful
-- typically ships against), then the common ESPHome dsmr
-- text_sensor `identification` slug (dsmr_identification).
-- Returns "" on any failure — the host falls back to MAC ARP / endpoint
-- hashing for `device_id`, so a missing SN is a soft failure, not a fatal one.
local function fetch_serial()
    local ids = {
        "electric_meter_equipment_id",
        "meter_identification",
        "dsmr_identification",
    }
    for _, id in ipairs(ids) do
        local v, err = fetch_entity("text_sensor", id)
        if not err and type(v) == "string" and v ~= "" then return v end
    end
    return ""
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    if not config or type(config.host) ~= "string" or config.host == "" then
        host.log("error", "esphome_dsmr: config.host is required (e.g. \"192.168.1.147\" or \"srcful-zap-p1.local\")")
        return
    end
    esp_host = config.host

    if type(config.make) == "string" and config.make ~= "" then
        make_name = config.make
    end
    local configured_poll_ms = tonumber(config.poll_ms)
    if configured_poll_ms and configured_poll_ms >= 1000 then
        poll_ms = configured_poll_ms
    end

    host.set_make(make_name)
    host.set_poll_interval(poll_ms)
    host.log("info", "esphome_dsmr: initialized (host=" .. esp_host .. ", make=" .. make_name .. ", poll=" .. poll_ms .. "ms)")
end

function driver_poll()
    if not esp_host then
        return poll_ms  -- init failed; idle
    end

    -- One-shot serial discovery (cheap text_sensor reads, runs once per
    -- driver lifetime — re-flashing the meter restarts the driver anyway).
    if not sn_set and serial_attempts < SERIAL_MAX_ATTEMPTS then
        serial_attempts = serial_attempts + 1
        local sn = fetch_serial()
        if sn ~= "" then
            host.set_sn(sn)
            host.log("info", "esphome_dsmr: serial = " .. sn)
            sn_set = true
        elseif serial_attempts == SERIAL_MAX_ATTEMPTS then
            host.log("info", "esphome_dsmr: no DSMR equipment id available; falling back to MAC/endpoint identity")
        end
    end

    -- Site-meter totals are the only mandatory reads. If either fails the
    -- whole emit is meaningless — bail and let the watchdog flip us
    -- offline if it persists. Failures bump `consecutive_failures` so
    -- subsequent polls back off exponentially instead of hammering a
    -- meter that's gone offline.
    local pc_kw, err = fetch_num_first({ "power_consumed", "power_delivered" })
    if err then
        consecutive_failures = consecutive_failures + 1
        host.log("warn", "esphome_dsmr: site import power read failed (backoff " .. backoff_ms() .. "ms): " .. err)
        return backoff_ms()
    end
    local pp_kw, err2 = fetch_num_first({ "power_produced", "power_returned" })
    if err2 then
        consecutive_failures = consecutive_failures + 1
        host.log("warn", "esphome_dsmr: site export power read failed (backoff " .. backoff_ms() .. "ms): " .. err2)
        return backoff_ms()
    end
    -- Both totals succeeded — clear any prior backoff streak.
    if consecutive_failures > 0 then
        host.log("info", "esphome_dsmr: poll recovered after " .. consecutive_failures .. " failure(s)")
        consecutive_failures = 0
    end

    -- Convert kW → W and combine into site-convention net power.
    local total_w = (pc_kw - pp_kw) * 1000.0

    -- Per-phase power: ESPHome publishes consumed_lN and produced_lN
    -- separately; site convention is the net. Missing phase reads are
    -- non-fatal — fall back to 0 for that phase, log debug, keep going.
    -- (Single-phase meters won't expose L2/L3 entities at all.)
    local phase_w = {}
    for i = 1, 3 do
        local c, ec = fetch_num_first({
            "power_consumed_l" .. i,
            "power_consumed_phase_" .. i,
            "power_delivered_l" .. i,
        })
        local p, ep = fetch_num_first({
            "power_produced_l" .. i,
            "power_produced_phase_" .. i,
            "power_returned_l" .. i,
        })
        if ec then
            host.log("debug", "esphome_dsmr: L" .. i .. " import power unavailable: " .. ec)
        end
        if ep then
            host.log("debug", "esphome_dsmr: L" .. i .. " export power unavailable: " .. ep)
        end
        if c ~= nil and p ~= nil then
            phase_w[i] = (c - p) * 1000.0
        end
    end

    -- Per-phase voltage / current (V, A — already in base units).
    local v = {}
    local a = {}
    for i = 1, 3 do
        v[i] = fetch_num_first({ "voltage_l" .. i, "voltage_phase_" .. i })
        a[i] = fetch_num_first({ "current_l" .. i, "current_phase_" .. i })
    end

    -- Lifetime energy counters: ESPHome serves these in kWh, we emit in Wh.
    local imp_kwh = fetch_num_first({ "energy_consumed", "energy_delivered" })
    local exp_kwh = fetch_num_first({ "energy_produced", "energy_returned" })

    -- Optional phase/counter values are omitted when their HTTP read fails.
    -- Publishing a synthetic 0 A would disable the per-phase fuse guard, and
    -- publishing a synthetic 0 Wh would look like a lifetime-counter reset.
    local meter = { w = total_w }
    for i = 1, 3 do
        if phase_w[i] ~= nil then meter["l" .. i .. "_w"] = phase_w[i] end
        if v[i] ~= nil then meter["l" .. i .. "_v"] = v[i] end
        if a[i] ~= nil then meter["l" .. i .. "_a"] = a[i] end
    end
    if imp_kwh ~= nil then meter.import_wh = imp_kwh * 1000.0 end
    if exp_kwh ~= nil then meter.export_wh = exp_kwh * 1000.0 end
    host.emit("meter", meter)

    -- Long-format diagnostics into the TS DB.
    for i = 1, 3 do
        if phase_w[i] ~= nil then host.emit_metric("meter_l" .. i .. "_w", phase_w[i], "W") end
        if v[i] ~= nil then host.emit_metric("meter_l" .. i .. "_v", v[i], "V") end
        if a[i] ~= nil then host.emit_metric("meter_l" .. i .. "_a", a[i], "A") end
    end

    -- Reactive power (kvar → var). DSMR meters publish reactive both
    -- as a total and per phase, in each direction. The FTW
    -- Meter struct doesn't carry kvar (load-flow control is active-only
    -- for now), but the values are useful for grid-quality dashboards
    -- and PF inference, so we drop them into the long-format TS DB
    -- as diagnostic metrics. The capability is probed ONCE on first
    -- successful poll — subsequent polls skip these requests on
    -- firmwares that don't expose them, saving the 80-ish ms of
    -- 404 round-trips per cycle.
    if not caps_probed then
        local probe, _ = fetch_num("reactive_power_imported")
        has_reactive = (probe ~= nil)
        caps_probed = true
        if has_reactive then
            host.log("info", "esphome_dsmr: reactive-power channels detected")
        else
            host.log("info", "esphome_dsmr: no reactive-power channels (firmware doesn't expose them)")
        end
    end
    if has_reactive then
        local function emit_var(name, kvar)
            if kvar ~= nil then host.emit_metric(name, kvar * 1000.0) end
        end
        emit_var("meter_q_imp_var", fetch_num("reactive_power_imported"))
        emit_var("meter_q_exp_var", fetch_num("reactive_power_exported"))
        for i = 1, 3 do
            emit_var("meter_q_imp_l" .. i .. "_var", fetch_num("reactive_power_imported_l" .. i))
            emit_var("meter_q_exp_l" .. i .. "_var", fetch_num("reactive_power_exported_l" .. i))
        end
    end

    return poll_ms
end

----------------------------------------------------------------------------
-- Control (READ-ONLY — meter exposes no writable endpoint)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end
    host.log("warn", "esphome_dsmr: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only: nothing to revert.
end

function driver_cleanup()
    sn_set = false
    serial_attempts = 0
    caps_probed = false
    has_reactive = false
    consecutive_failures = 0
end
