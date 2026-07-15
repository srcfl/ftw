-- pixii_pv.lua
-- Pixii PowerShaper — read-only PV + grid meter driver
-- Emits:
--   PV    from pixii/status/<sn>/meter_ext6 (meter_source=PV)
--   Meter from pixii/status/<sn>/meter
--
-- SoC / battery data is intentionally NOT handled here — that comes in
-- over Modbus via a separate driver. This driver is PV + grid-meter
-- only, and does not declare a battery capability.

DRIVER = {
  id           = "pixii-pv",
  name         = "Pixii PowerShaper (PV + meter)",
  manufacturer = "Pixii",
  version      = "0.2.0",
  protocols    = { "mqtt" },
  capabilities = { "pv", "meter" },
  description  = "Read-only Pixii telemetry: PV (external CTs) + grid meter (meter_w). Battery / SoC comes from the Modbus driver, not this one.",
  homepage     = "https://pixii.com",
  authors      = { "FTW contributors" },
  verification_status = "experimental",
  connection_defaults = {
    port = 1883,
  },
}

PROTOCOL = "mqtt"

local TOPIC_PV    = "pixii/status/+/meter_ext6"
local TOPIC_METER = "pixii/status/+/meter"

-- Optional filter: only accept messages from this SN. nil = accept any.
local SN_FILTER = nil

-- Remember the SN we latched onto so device_id stays stable even if a
-- second Pixii ever appears on the bus by mistake.
local LATCHED_SN = nil

local function sn_from(topic, suffix)
    return topic:match("^pixii/status/([^/]+)/" .. suffix .. "$")
end

local function sn_ok(sn)
    if sn == nil then return false end
    if SN_FILTER ~= nil and sn ~= SN_FILTER then return false end
    -- Once we've latched onto an SN, reject traffic from any other Pixii
    -- that might show up on the bus — otherwise we'd mix telemetry while
    -- still reporting the first-seen SN as the device identity.
    if LATCHED_SN ~= nil and sn ~= LATCHED_SN then return false end
    return true
end

local function latch_sn(sn)
    if LATCHED_SN == nil and sn then
        LATCHED_SN = sn
        host.set_sn(sn)
        host.log("info", "Pixii: latched SN " .. sn)
    end
end

function driver_init(config)
    host.set_make("Pixii")

    -- Explicit reset so hot-reload in the same Lua VM doesn't inherit
    -- state from a previous incarnation.
    SN_FILTER  = nil
    LATCHED_SN = nil

    if config and config.sn and config.sn ~= "" then
        SN_FILTER = tostring(config.sn)
        host.log("info", "Pixii: pinned to SN " .. SN_FILTER)
    end

    host.mqtt_subscribe(TOPIC_PV)
    host.mqtt_subscribe(TOPIC_METER)
    host.log("info", "Pixii: subscribed to meter_ext6 + meter")
end

function driver_poll()
    local messages = host.mqtt_messages()
    if not messages then return 1000 end

    local pv_payload, meter_payload = nil, nil

    for _, msg in ipairs(messages) do
        local ok, data = pcall(host.json_decode, msg.payload)
        if ok and type(data) == "table" then
            local sn = sn_from(msg.topic, "meter_ext6")
            if sn and sn_ok(sn) then
                -- The same topic schema is reused for grid / building
                -- meters on other deployments — gate on meter_source.
                if data.meter_source == "PV" then
                    latch_sn(sn)
                    pv_payload = data
                end
            else
                sn = sn_from(msg.topic, "meter")
                if sn and sn_ok(sn) then
                    latch_sn(sn)
                    meter_payload = data
                end
            end
        end
    end

    --------------------------------------------------------------------------
    -- PV (external-CT meter on the PV feed).
    -- Pixii reports PV as positive generation; site convention requires
    -- negative (power leaving the grid boundary into the loads), so negate.
    --------------------------------------------------------------------------
    if pv_payload then
        local w1 = tonumber(pv_payload.meter_w1) or 0
        local w2 = tonumber(pv_payload.meter_w2) or 0
        local w3 = tonumber(pv_payload.meter_w3) or 0
        local pv_w = -(w1 + w2 + w3)
        local pv = { w = pv_w }
        local kwh_imp = tonumber(pv_payload.meter_kwh_imp)
        if kwh_imp then pv.generation_wh = kwh_imp * 1000.0 end
        host.emit("pv", pv)

        host.emit_metric("pv_w_total", pv_w)
        host.emit_metric("pv_l1_w", -w1)
        host.emit_metric("pv_l2_w", -w2)
        host.emit_metric("pv_l3_w", -w3)
        if pv_payload.meter_v1 then host.emit_metric("pv_l1_v", tonumber(pv_payload.meter_v1)) end
        if pv_payload.meter_v2 then host.emit_metric("pv_l2_v", tonumber(pv_payload.meter_v2)) end
        if pv_payload.meter_v3 then host.emit_metric("pv_l3_v", tonumber(pv_payload.meter_v3)) end
        if pv_payload.meter_a1 then host.emit_metric("pv_l1_a", tonumber(pv_payload.meter_a1)) end
        if pv_payload.meter_a2 then host.emit_metric("pv_l2_a", tonumber(pv_payload.meter_a2)) end
        if pv_payload.meter_a3 then host.emit_metric("pv_l3_a", tonumber(pv_payload.meter_a3)) end
    end

    --------------------------------------------------------------------------
    -- Meter (grid connection point).
    -- Pixii `meter_w`: negative = export, positive = import — matches site
    -- convention directly, no negation.
    --------------------------------------------------------------------------
    if meter_payload then
        local meter = {}
        local w = tonumber(meter_payload.meter_w)
        if w then meter.w = w end

        local hz = tonumber(meter_payload.freq)
        if hz then meter.hz = hz end

        if meter_payload.ac_v1 then meter.l1_v = tonumber(meter_payload.ac_v1) end
        if meter_payload.ac_v2 then meter.l2_v = tonumber(meter_payload.ac_v2) end
        if meter_payload.ac_v3 then meter.l3_v = tonumber(meter_payload.ac_v3) end

        host.emit("meter", meter)

        if meter.w    then host.emit_metric("meter_w",    meter.w)    end
        if meter.hz   then host.emit_metric("grid_hz",    meter.hz)   end
        if meter.l1_v then host.emit_metric("meter_l1_v", meter.l1_v) end
        if meter.l2_v then host.emit_metric("meter_l2_v", meter.l2_v) end
        if meter.l3_v then host.emit_metric("meter_l3_v", meter.l3_v) end

        local building_w = tonumber(meter_payload.building_ac_w)
        if building_w then host.emit_metric("building_ac_w", building_w) end
    end

    return 1000
end

function driver_command(action)
    -- Read-only driver, nothing to control. Accept init/deinit silently.
    if action == "init" or action == "deinit" then return true end
    return false
end

function driver_default_mode()
    -- No-op: nothing to revert.
end

function driver_cleanup()
    LATCHED_SN = nil
    SN_FILTER  = nil
end
