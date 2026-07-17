-- zap.lua
-- Sourceful Zap local API integration.
--
-- The Zap is a multi-device gateway. FTW reads its official local REST API,
-- discovers every attached DER, and publishes one site-level reading per DER
-- kind. Multiple PV or battery resources are aggregated; the P1 meter is
-- preferred as the site meter. The driver is deliberately read-only for now.
-- Current Zap firmware has semantic local control routes, but they only
-- acknowledge that a command was queued; unlike the MQTT control path they do
-- not attach the command to a duration/heartbeat watchdog or expose the final
-- hardware result. FTW must not leave a sticky setpoint behind if it dies.
--
-- Emits: meter, pv, battery, v2x_charger
-- Protocol: HTTP
-- API contract: https://developer.sourceful.energy/docs/api/zap-local-api
--
-- Config example:
--   - name: sourceful-zap
--     lua: drivers/zap.lua
--     is_site_meter: true
--     battery_telemetry_only: true
--     capabilities:
--       http:
--         allowed_hosts: ["zap.local"]
--     config:
--       host: zap.local
--       # meter_serial: p1m-...       # optional; P1 is auto-selected
--       # disable_pv: true            # avoid overlap with a native driver
--       # disable_battery: true
--       # disable_v2x: true
--       # discovery_interval_ms: 60000
--
-- Site convention (the official Zap model already uses the same signs):
--   meter:  +W import, -W export
--   pv:     -W generation
--   battery:+W charge, -W discharge
--   V2X:    +W vehicle charge, -W vehicle discharge

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "sourceful-zap",
  name         = "Sourceful Zap",
  manufacturer = "Sourceful",
  version      = "2.0.0",
  protocols    = { "http" },
  capabilities = { "meter", "pv", "battery", "v2x_charger" },
  read_only    = true,
  description  = "Official Sourceful Zap local-API integration for meter, PV, battery and V2X telemetry; safe leased control is pending firmware support.",
  homepage     = "https://developer.sourceful.energy/docs/api/zap-local-api",
  authors      = { "Sourceful Energy", "FTW contributors" },
  http_hosts   = { "zap.local" },
  verification_status = "production",
  verified_by = { "erikarenhill@fortytwo:3d" },
  verified_at = "2026-07-17",
  verification_notes = "Maintained against the official Zap Local API and audited against srcful-zap-x-firmware 96e0258. P1 and PV are live-hardware verified; battery and V2X payloads are contract-tested against the firmware serializers.",
  tested_models = { "Sourceful Zap (ESP32-C3 controller firmware)" },
  connection_defaults = {
    host = "zap.local",
    port = 80,
  },
}

PROTOCOL = "http"

local zap_host = "zap.local"
local gateway_serial = nil
local pinned_meter_serial = nil
local meter_serial = nil
local tracked = {}
local discovered = false

local disable_pv = false
local disable_battery = false
local disable_v2x = false
local discovery_interval_ms = 60000

local discovery_last_attempt = 0
local discovery_last_success = 0
local discovery_backoff_ms = 0
local DISCOVERY_BACKOFF_MIN = 2000
local DISCOVERY_BACKOFF_MAX = 60000

local identity_last_attempt = 0
local IDENTITY_RETRY_MS = 60000

local meter_fail_count = 0
local METER_FAIL_REDISCOVER = 10

local function base_url()
    if string.sub(zap_host, 1, 7) == "http://" or string.sub(zap_host, 1, 8) == "https://" then
        return zap_host
    end
    return "http://" .. zap_host
end

local function number(v)
    local n = tonumber(v)
    if n == nil or n ~= n or n == math.huge or n == -math.huge then return nil end
    return n
end

local function bounded(v, min_value, max_value)
    local n = number(v)
    if n == nil then return nil end
    if min_value ~= nil and n < min_value then return nil end
    if max_value ~= nil and n > max_value then return nil end
    return n
end

-- Zap firmware can surface downstream integer overflow sentinels when an
-- inverter is offline. A known nameplate rating gives a quantifiable guard:
-- anything above 10x nameplate cannot be a real operating point, while the
-- generous factor cannot trim a legitimate transient. Without a rating we do
-- not guess a residential ceiling; validation is limited to finite numbers.
local function sane_power(v, rated_power_w)
    local n = number(v)
    if n == nil then return nil end
    local rated = number(rated_power_w) or 0
    if rated > 0 and math.abs(n) > rated * 10 then return nil end
    return n
end

local function metric_tag(sn)
    local tag = string.lower(tostring(sn or "device"))
    tag = string.gsub(tag, "[^%w]+", "_")
    tag = string.gsub(tag, "^_+", "")
    tag = string.gsub(tag, "_+$", "")
    if tag == "" then return "device" end
    return tag
end

local function fetch_json(path)
    local body, err = host.http_get(base_url() .. path)
    if err then return nil, err end
    local data, decode_err = host.json_decode(body)
    if not data then return nil, decode_err or "json decode failed" end
    return data, nil
end

local function fetch_device(sn)
    return fetch_json("/api/devices/" .. tostring(sn) .. "/data/json")
end

local function resolve_gateway_identity()
    local now = host.millis()
    if gateway_serial or (identity_last_attempt > 0 and now - identity_last_attempt < IDENTITY_RETRY_MS) then
        return
    end
    identity_last_attempt = now
    local data, err = fetch_json("/api/crypto")
    if err then
        host.log("debug", "Zap: identity endpoint unavailable: " .. tostring(err))
        return
    end
    local serial = data.serialNumber or data.serial_number
    if type(serial) == "string" and serial ~= "" then
        gateway_serial = serial
        host.set_sn(serial)
        host.log("info", "Zap: gateway identity " .. serial)
    end
end

local function bump_discovery_backoff()
    if discovery_backoff_ms == 0 then
        discovery_backoff_ms = DISCOVERY_BACKOFF_MIN
    else
        discovery_backoff_ms = math.min(discovery_backoff_ms * 2, DISCOVERY_BACKOFF_MAX)
    end
    discovery_last_attempt = host.millis()
end

local function clear_discovery_backoff()
    discovery_backoff_ms = 0
    discovery_last_attempt = 0
end

local function discovery_in_backoff()
    if discovery_backoff_ms == 0 then return false end
    return host.millis() - discovery_last_attempt < discovery_backoff_ms
end

local function record_for(records_by_sn, records, sn)
    local rec = records_by_sn[sn]
    if rec then return rec end
    rec = {
        sn = sn,
        meter = false,
        is_p1 = false,
        pv = false,
        pv_rated_w = 0,
        battery = false,
        battery_rated_w = 0,
        battery_capacity_wh = 0,
        v2x = false,
        v2x_capacity_wh = 0,
    }
    records_by_sn[sn] = rec
    records[#records + 1] = rec
    return rec
end

-- GET /api/devices describes connection points and DERs. The `enabled` flag
-- controls publishing from Zap to Novacore; it does not control local data
-- availability, so FTW intentionally discovers DER types regardless of that
-- flag. This lets local-first FTW work even when Zap cloud publishing is off.
local function discover_devices()
    local data, err = fetch_json("/api/devices")
    if err then return nil, nil, err end
    if type(data.devices) ~= "table" then
        return nil, nil, "unexpected payload (no devices array)"
    end

    local records = {}
    local records_by_sn = {}
    local first_p1 = nil
    local first_meter = nil
    local recognised = 0

    for _, dev in ipairs(data.devices) do
        if type(dev) == "table" and dev.sn then
            local sn = tostring(dev.sn)
            local rec = record_for(records_by_sn, records, sn)
            recognised = recognised + 1

            if dev.type == "p1_uart" then
                rec.meter = true
                rec.is_p1 = true
                if not first_p1 then first_p1 = sn end
                if not first_meter then first_meter = sn end
            end

            if type(dev.ders) == "table" then
                for _, der in ipairs(dev.ders) do
                    if type(der) == "table" then
                        if der.type == "meter" then
                            rec.meter = true
                            if not first_meter then first_meter = sn end
                        elseif der.type == "pv" and not disable_pv then
                            rec.pv = true
                            rec.pv_rated_w = number(der.rated_power) or number(der.installed_power) or rec.pv_rated_w
                        elseif der.type == "battery" and not disable_battery then
                            rec.battery = true
                            rec.battery_rated_w = number(der.rated_power) or rec.battery_rated_w
                            rec.battery_capacity_wh = number(der.capacity) or rec.battery_capacity_wh
                        elseif der.type == "v2x_charger" and not disable_v2x then
                            rec.v2x = true
                            rec.v2x_capacity_wh = number(der.capacity) or rec.v2x_capacity_wh
                        end
                    end
                end
            end

            -- Some firmware builds identify the device category even before
            -- their `ders` metadata has been persisted.
            if (dev.device_type == "energy_meter" or dev.device_type == "meter") and not rec.meter then
                rec.meter = true
                if not first_meter then first_meter = sn end
            elseif dev.device_type == "v2x_charger" and not disable_v2x then
                rec.v2x = true
            end
        end
    end

    if recognised == 0 then
        return nil, nil, "no recognised Zap devices found"
    end

    local selected_meter = pinned_meter_serial or first_p1 or first_meter
    if pinned_meter_serial then
        local pinned = record_for(records_by_sn, records, pinned_meter_serial)
        pinned.meter = true
    end
    return records, selected_meter, nil
end

local function count_kind(kind)
    local n = 0
    for _, rec in ipairs(tracked) do
        if rec[kind] then n = n + 1 end
    end
    return n
end

local function apply_discovery(records, selected_meter)
    tracked = records
    meter_serial = selected_meter
    discovered = true
    discovery_last_success = host.millis()
    meter_fail_count = 0
    clear_discovery_backoff()

    -- Legacy fallback for older Zap firmware without /api/crypto. Current
    -- firmware overwrites this with the gateway's own zap-* serial.
    if not gateway_serial and meter_serial then host.set_sn(meter_serial) end

    host.log("info", "Zap: discovered " .. #tracked .. " device(s); meter="
        .. tostring(meter_serial or "none") .. ", pv=" .. count_kind("pv")
        .. ", battery=" .. count_kind("battery") .. ", v2x=" .. count_kind("v2x"))
end

local function maybe_discover()
    local now = host.millis()
    local due = not discovered or discovery_last_success == 0
        or now - discovery_last_success >= discovery_interval_ms
    if not due or discovery_in_backoff() then return discovered end

    discovery_last_attempt = now
    local records, selected_meter, err = discover_devices()
    if err then
        bump_discovery_backoff()
        host.log("warn", "Zap: discovery failed: " .. tostring(err)
            .. " (retry in " .. discovery_backoff_ms .. "ms)")
        return discovered
    end
    apply_discovery(records, selected_meter)
    return true
end

local function emit_optional_metric(name, value, unit)
    local n = number(value)
    if n ~= nil then host.emit_metric(name, n, unit) end
end

local meter_fields = {
    { "l1_w", "L1_W", "W" }, { "l2_w", "L2_W", "W" }, { "l3_w", "L3_W", "W" },
    { "l1_v", "L1_V", "V" }, { "l2_v", "L2_V", "V" }, { "l3_v", "L3_V", "V" },
    { "l1_a", "L1_A", "A" }, { "l2_a", "L2_A", "A" }, { "l3_a", "L3_A", "A" },
    { "freq_hz", "Hz", "Hz" },
    { "total_import_wh", "total_import_Wh", "Wh" },
    { "total_export_wh", "total_export_Wh", "Wh" },
}

local function emit_meter(data)
    if type(data) ~= "table" or type(data.meter) ~= "table" then return false end
    local raw = data.meter
    local w = number(raw.W)
    if w == nil then return false end

    local reading = { w = w }
    for _, mapping in ipairs(meter_fields) do
        local value = number(raw[mapping[2]])
        if value ~= nil then
            reading[mapping[1]] = value
            host.emit_metric("meter_" .. mapping[1], value, mapping[3])
        end
    end
    -- Keep the established local FTW aliases while also emitting the clean
    -- Sourceful federation names consumed by internal/nova.
    reading.import_wh = reading.total_import_wh
    reading.export_wh = reading.total_export_wh
    host.emit("meter", reading)
    return true
end

local function snapshot_map()
    local out = {}
    for _, rec in ipairs(tracked) do
        local data, err = fetch_device(rec.sn)
        if data then
            out[rec.sn] = data
        elseif rec.sn == meter_serial then
            host.log("warn", "Zap: site-meter fetch failed: " .. tostring(err))
        else
            host.log("debug", "Zap: device fetch failed for " .. rec.sn .. ": " .. tostring(err))
        end
    end
    return out
end

local function emit_pv(snapshots)
    local source_count = count_kind("pv")
    if source_count == 0 then return end

    local total_w = 0
    local total_generation_wh = 0
    local any_power = false
    local any_generation = false
    local total_rated_w = 0
    local any_rating = false

    for _, rec in ipairs(tracked) do
        local data = snapshots[rec.sn]
        local pv = data and data.pv
        if rec.pv and type(pv) == "table" then
            local rated = number(pv.rated_power_W) or rec.pv_rated_w
            if rated and rated > 0 then
                total_rated_w = total_rated_w + rated
                any_rating = true
            end
            local w = sane_power(pv.W, rated)
            if w ~= nil and w <= 0 then
                total_w = total_w + w
                any_power = true
                if source_count > 1 then
                    host.emit_metric("pv_w_" .. metric_tag(rec.sn), w, "W")
                end
            elseif w ~= nil then
                host.log("warn", "Zap: rejected positive PV power for " .. rec.sn .. ": " .. w .. "W")
            end

            local generation = bounded(pv.total_generation_Wh, 0, nil)
            if generation ~= nil then
                total_generation_wh = total_generation_wh + generation
                any_generation = true
            end

            local tag = source_count > 1 and ("_" .. metric_tag(rec.sn)) or ""
            emit_optional_metric("pv_heatsink_c" .. tag, number(pv.heatsink_C), "°C")
            emit_optional_metric("pv_mppt1_v" .. tag, number(pv.mppt1_V), "V")
            emit_optional_metric("pv_mppt1_a" .. tag, number(pv.mppt1_A), "A")
            emit_optional_metric("pv_mppt2_v" .. tag, number(pv.mppt2_V), "V")
            emit_optional_metric("pv_mppt2_a" .. tag, number(pv.mppt2_A), "A")
        end
    end

    if any_power then
        local reading = { w = total_w }
        if any_generation then
            reading.lifetime_wh = total_generation_wh
            reading.total_generation_wh = total_generation_wh
        end
        if any_rating then reading.rated_power_w = total_rated_w end
        host.emit("pv", reading)
        host.emit_metric("pv_w", total_w, "W")
    end
end

local function emit_battery(snapshots)
    local source_count = count_kind("battery")
    if source_count == 0 then return end

    local total_w = 0
    local any_power = false
    local soc_sum = 0
    local soc_count = 0
    local weighted_soc_sum = 0
    local weight_sum = 0
    local weighted_count = 0
    local total_charge_wh = 0
    local total_discharge_wh = 0
    local any_charge_energy = false
    local any_discharge_energy = false
    local total_capacity_wh = 0
    local any_capacity = false
    local total_rated_w = 0
    local any_rating = false
    local single = nil

    for _, rec in ipairs(tracked) do
        local data = snapshots[rec.sn]
        local battery = data and data.battery
        if rec.battery and type(battery) == "table" then
            single = battery
            local rated = number(battery.rated_power_W) or rec.battery_rated_w
            if rated and rated > 0 then
                total_rated_w = total_rated_w + rated
                any_rating = true
            end
            local w = sane_power(battery.W, rated)
            if w ~= nil then
                total_w = total_w + w
                any_power = true
            end

            local soc = bounded(battery.SoC_nom_fract, 0, 1)
            if soc ~= nil then
                soc_sum = soc_sum + soc
                soc_count = soc_count + 1
                local capacity = bounded(battery.capacity_Wh, 0, nil) or bounded(rec.battery_capacity_wh, 0, nil)
                if capacity and capacity > 0 then
                    weighted_soc_sum = weighted_soc_sum + soc * capacity
                    weight_sum = weight_sum + capacity
                    weighted_count = weighted_count + 1
                end
            end

            local capacity = bounded(battery.capacity_Wh, 0, nil) or bounded(rec.battery_capacity_wh, 0, nil)
            if capacity and capacity > 0 then
                total_capacity_wh = total_capacity_wh + capacity
                any_capacity = true
            end

            local charge = bounded(battery.total_charge_Wh, 0, nil)
            if charge ~= nil then total_charge_wh = total_charge_wh + charge; any_charge_energy = true end
            local discharge = bounded(battery.total_discharge_Wh, 0, nil)
            if discharge ~= nil then total_discharge_wh = total_discharge_wh + discharge; any_discharge_energy = true end

            local tag = source_count > 1 and ("_" .. metric_tag(rec.sn)) or ""
            if source_count > 1 and w ~= nil then host.emit_metric("battery_w" .. tag, w, "W") end
            if source_count > 1 and soc ~= nil then host.emit_metric("battery_soc" .. tag, soc, "fraction") end
            emit_optional_metric("battery_dc_v" .. tag, number(battery.V), "V")
            emit_optional_metric("battery_dc_a" .. tag, number(battery.A), "A")
            emit_optional_metric("battery_temp_c" .. tag, number(battery.heatsink_C), "°C")
        end
    end

    if not any_power then return end
    local reading = { w = total_w }
    if soc_count > 0 then
        if weighted_count == soc_count and weight_sum > 0 then
            reading.soc = weighted_soc_sum / weight_sum
        else
            reading.soc = soc_sum / soc_count
        end
    end
    if any_charge_energy then
        reading.charge_wh = total_charge_wh
        reading.total_charge_wh = total_charge_wh
    end
    if any_discharge_energy then
        reading.discharge_wh = total_discharge_wh
        reading.total_discharge_wh = total_discharge_wh
    end
    if any_capacity then reading.capacity_wh = total_capacity_wh end
    if any_rating then reading.rated_power_w = total_rated_w end

    if source_count == 1 and single then
        reading.dc_v = number(single.V)
        reading.dc_a = number(single.A)
        reading.temp_c = number(single.heatsink_C)
        local lower = number(single.lower_limit_W)
        local upper = number(single.upper_limit_W)
        if lower ~= nil then reading.discharge_capable = lower < 0 end
        if upper ~= nil then reading.charge_capable = upper > 0 end
    end

    host.emit("battery", reading)
    host.emit_metric("battery_w", total_w, "W")
    if reading.soc ~= nil then host.emit_metric("battery_soc", reading.soc, "fraction") end
end

local function status_connected(status)
    if type(status) ~= "string" then return nil end
    local s = string.lower(status)
    if s == "available" or s == "disconnected" or s == "offline" or s == "unavailable" then
        return false
    end
    if s == "preparing" or s == "suspended" or s == "connected" or s == "idle"
        or s == "ready" or s == "charging" or s == "discharging" then
        return true
    end
    return nil
end

local function emit_v2x(snapshots)
    local source_count = count_kind("v2x")
    if source_count == 0 then return end

    local total_w = 0
    local any_power = false
    local soc_sum = 0
    local soc_count = 0
    local connected = false
    local known_connected = false
    local single = nil
    local single_capacity_wh = nil

    for _, rec in ipairs(tracked) do
        local data = snapshots[rec.sn]
        local v2x = data and data.v2x_charger
        if rec.v2x and type(v2x) == "table" then
            single = v2x
            single_capacity_wh = bounded(v2x.capacity_Wh, 0, nil)
                or bounded(rec.v2x_capacity_wh, 0, nil)
            local rated = number(v2x.rated_power_W)
            local w = sane_power(v2x.W, rated)
            if w ~= nil then
                total_w = total_w + w
                any_power = true
            end
            local soc = bounded(v2x.vehicle_soc_fract or v2x.vehicle_soc, 0, 1)
            if soc ~= nil then soc_sum = soc_sum + soc; soc_count = soc_count + 1 end
            local is_connected = v2x.plug_connected
            if type(is_connected) ~= "boolean" then is_connected = v2x.connected end
            if type(is_connected) ~= "boolean" then is_connected = status_connected(v2x.status) end
            if type(is_connected) == "boolean" then
                known_connected = true
                connected = connected or is_connected
            end

            if source_count > 1 then
                local tag = "_" .. metric_tag(rec.sn)
                if w ~= nil then host.emit_metric("v2x_w" .. tag, w, "W") end
                if soc ~= nil then host.emit_metric("v2x_soc" .. tag, soc, "fraction") end
            end

            local tag = source_count > 1 and ("_" .. metric_tag(rec.sn)) or ""
            emit_optional_metric("v2x_ac_v" .. tag, number(v2x.V), "V")
            emit_optional_metric("v2x_ac_a" .. tag, number(v2x.A), "A")
            emit_optional_metric("v2x_ac_w" .. tag, number(v2x.ac_W), "W")
            emit_optional_metric("v2x_freq_hz" .. tag, number(v2x.Hz), "Hz")
            emit_optional_metric("v2x_l1_v" .. tag, number(v2x.L1_V), "V")
            emit_optional_metric("v2x_l1_a" .. tag, number(v2x.L1_A), "A")
            emit_optional_metric("v2x_l1_w" .. tag, number(v2x.L1_W), "W")
            emit_optional_metric("v2x_l2_v" .. tag, number(v2x.L2_V), "V")
            emit_optional_metric("v2x_l2_a" .. tag, number(v2x.L2_A), "A")
            emit_optional_metric("v2x_l2_w" .. tag, number(v2x.L2_W), "W")
            emit_optional_metric("v2x_l3_v" .. tag, number(v2x.L3_V), "V")
            emit_optional_metric("v2x_l3_a" .. tag, number(v2x.L3_A), "A")
            emit_optional_metric("v2x_l3_w" .. tag, number(v2x.L3_W), "W")
            emit_optional_metric("v2x_dc_w" .. tag, number(v2x.dc_W), "W")
            emit_optional_metric("v2x_dc_v" .. tag, number(v2x.dc_V), "V")
            emit_optional_metric("v2x_dc_a" .. tag, number(v2x.dc_A), "A")
            emit_optional_metric("v2x_ev_target_energy_req_wh" .. tag, number(v2x.ev_target_energy_req_Wh), "Wh")
            emit_optional_metric("v2x_ev_min_energy_req_wh" .. tag, number(v2x.ev_min_energy_req_Wh), "Wh")
            emit_optional_metric("v2x_ev_max_energy_req_wh" .. tag, number(v2x.ev_max_energy_req_Wh), "Wh")
            emit_optional_metric("v2x_session_charge_wh" .. tag, bounded(v2x.session_charge_Wh, 0, nil), "Wh")
            emit_optional_metric("v2x_session_discharge_wh" .. tag, bounded(v2x.session_discharge_Wh, 0, nil), "Wh")
            emit_optional_metric("v2x_total_charge_wh" .. tag, bounded(v2x.total_charge_Wh, 0, nil), "Wh")
            emit_optional_metric("v2x_total_discharge_wh" .. tag, bounded(v2x.total_discharge_Wh, 0, nil), "Wh")
            emit_optional_metric("v2x_capacity_wh" .. tag, single_capacity_wh, "Wh")
            emit_optional_metric("v2x_rated_power_w" .. tag, bounded(v2x.rated_power_W, 0, nil), "W")
        end
    end

    if not any_power then return end
    local reading = { w = total_w }
    if soc_count > 0 then reading.vehicle_soc = soc_sum / soc_count end
    if known_connected then reading.connected = connected end

    if source_count == 1 and single then
        reading.status = single.status
        reading.protocol = single.protocol
        reading.control_mode = single.control_mode
        reading.connector_status = single.connector_status
        reading.charging_state = single.charging_state
        reading.ac_w = number(single.ac_W)
        reading.ac_v = number(single.V)
        reading.ac_a = number(single.A)
        reading.dc_w = number(single.dc_W)
        reading.dc_v = number(single.dc_V)
        reading.dc_a = number(single.dc_A)
        reading.freq_hz = number(single.Hz)
        reading.l1_v = number(single.L1_V)
        reading.l1_a = number(single.L1_A)
        reading.l1_w = number(single.L1_W)
        reading.l2_v = number(single.L2_V)
        reading.l2_a = number(single.L2_A)
        reading.l2_w = number(single.L2_W)
        reading.l3_v = number(single.L3_V)
        reading.l3_a = number(single.L3_A)
        reading.l3_w = number(single.L3_W)
        reading.session_charge_wh = bounded(single.session_charge_Wh, 0, nil)
        reading.session_discharge_wh = bounded(single.session_discharge_Wh, 0, nil)
        reading.total_charge_wh = bounded(single.total_charge_Wh, 0, nil)
        reading.total_discharge_wh = bounded(single.total_discharge_Wh, 0, nil)
        reading.ev_target_energy_req_wh = number(single.ev_target_energy_req_Wh)
        reading.ev_min_energy_req_wh = number(single.ev_min_energy_req_Wh)
        reading.ev_max_energy_req_wh = number(single.ev_max_energy_req_Wh)
        reading.capacity_wh = single_capacity_wh
        reading.rated_power_w = bounded(single.rated_power_W, 0, nil)
        -- Preserve the firmware's limit curves verbatim for diagnostics and
        -- the future semantic control adapter.
        reading.lower_limit_w = single.lower_limit_W
        reading.upper_limit_w = single.upper_limit_W
    end

    host.emit("v2x_charger", reading)
    host.emit_metric("v2x_w", total_w, "W")
end

----------------------------------------------------------------------------
-- Fingerprint
----------------------------------------------------------------------------

function driver_fingerprint(target)
    local base = target and target.base_url
    if not base or base == "" then return nil end

    -- Current firmware has a strong identity signature.
    local crypto_body = host.http_get(base .. "/api/crypto")
    if crypto_body then
        local crypto = host.json_decode(crypto_body)
        if type(crypto) == "table" and crypto.publicKey and crypto.serialNumber
            and (crypto.deviceName == "software_zap" or string.sub(tostring(crypto.serialNumber), 1, 4) == "zap-") then
            return true, {
                make = "Sourceful", model = "Zap", serial = tostring(crypto.serialNumber), confidence = 1.0,
            }
        end
    end

    -- Legacy fallback: older field units lacked /api/crypto but exposed the
    -- characteristic devices list. Keep them discoverable at lower confidence.
    local body, err = host.http_get(base .. "/api/devices")
    if err or not body then return nil end
    local data = host.json_decode(body)
    if type(data) ~= "table" or type(data.devices) ~= "table" then return false end
    local serial = nil
    local recognised = false
    for _, dev in ipairs(data.devices) do
        if type(dev) == "table" and dev.sn and (dev.type or dev.device_type or dev.ders) then
            recognised = true
            if dev.type == "p1_uart" then serial = tostring(dev.sn) end
        end
    end
    if recognised then
        return true, { make = "Sourceful", model = "Zap", serial = serial or "", confidence = 0.85 }
    end
    return false
end

----------------------------------------------------------------------------
-- Driver lifecycle
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Sourceful")

    if config and type(config.host) == "string" and config.host ~= "" then
        zap_host = config.host
    end
    if config then
        local pinned = config.meter_serial or config.serial
        if type(pinned) == "string" and pinned ~= "" then
            pinned_meter_serial = pinned
            host.log("info", "Zap: using pinned meter serial " .. pinned)
        end
        disable_pv = config.disable_pv == true
        disable_battery = config.disable_battery == true
        disable_v2x = config.disable_v2x == true
        local interval = number(config.discovery_interval_ms)
        if interval and interval >= 5000 then discovery_interval_ms = interval end
    end

    host.log("info", "Zap: driver initialized (host=" .. zap_host .. ", read-only=true)")
end

function driver_poll()
    resolve_gateway_identity()
    if not maybe_discover() then return 1000 end

    local snapshots = snapshot_map()
    if meter_serial then
        local ok = emit_meter(snapshots[meter_serial])
        if ok then
            meter_fail_count = 0
        else
            meter_fail_count = meter_fail_count + 1
            host.log("warn", "Zap: site-meter payload unavailable (" .. meter_fail_count
                .. "/" .. METER_FAIL_REDISCOVER .. ")")
            if meter_fail_count >= METER_FAIL_REDISCOVER then
                discovered = false
                discovery_last_success = 0
                meter_fail_count = 0
                host.log("warn", "Zap: repeated meter failures; device discovery invalidated")
            end
        end
    end

    emit_pv(snapshots)
    emit_battery(snapshots)
    emit_v2x(snapshots)
    return 1000
end

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then return true end
    host.log("warn", "Zap: safe leased local control is unavailable; ignored action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only: Zap and each downstream DER retain their autonomous modes.
end

function driver_cleanup()
    gateway_serial = nil
    meter_serial = nil
    tracked = {}
    discovered = false
    discovery_last_success = 0
    identity_last_attempt = 0
    meter_fail_count = 0
    clear_discovery_backoff()
end
