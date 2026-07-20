-- Pixii PowerShaper Driver
-- Bundled offline recovery snapshot for Device Support package pixii 1.2.0.
-- Canonical shared releases live in srcfl/srcful-device-support.
-- Emits: Battery, Meter
-- Protocol: Modbus TCP — SunSpec-compliant commercial battery storage
-- Register type: ALL HOLDING (FC 0x03)
-- Uses SunSpec scale factors (signed i16 exponents → value * 10^sf)
--
-- Read path ported from sourceful-hugin/device-support/drivers/lua/pixii.lua.
-- Control path (active power setpoint) implemented against the Pixii
-- PowerShaper Modbus Mapping doc 13300 rev 2.0 (page 15 note about
-- control modes "simple" / "use control power activate"):
--
--   39903  Handshake counter (uint16)  — must be ticked at least once
--                                         per 60 s or the system drops
--                                         to idle.
--   39905/06  Power regulation set-point (int32 W, generator frame)
--                                         — must be written atomically
--                                         as a two-register multi-write.
--
-- "Generator reference frame" inverts the EMS sign convention: positive
-- setpoint = discharge on the Pixii side, positive power_w = charge on
-- the EMS side. The driver negates at the setpoint boundary.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "pixii",
  name         = "Pixii PowerShaper",
  manufacturer = "Pixii",
  version      = "1.2.0",
  protocols    = { "modbus" },
  capabilities = { "battery", "meter" },
  description  = "Pixii PowerShaper commercial battery storage via Modbus TCP.",
  homepage     = "https://pixii.com",
  authors      = { "Tommy Lindgren", "Sourceful contributors" },
  tested_models = { "PowerShaper" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Sign convention (SITE = positive W flows INTO the site):
--   Battery w: positive = charging  (load), negative = discharging (source)
--   Meter   w: positive = importing,        negative = exporting
--
-- On this Pixii firmware the native meter already reports positive = import,
-- matching the site convention, so W and A are passed through unchanged.

PROTOCOL = "modbus"

-- Pixii control registers (doc 13300, section "Register addresses below 40000")
local REG_HEARTBEAT   = 39903  -- uint16, 0..100, must tick >= 1/min
local REG_SETPOINT_HI = 39905  -- int32 MSB (big-endian, paired with 39906)
local REG_SETPOINT_LO = 39906  -- int32 LSB

-- SunSpec model 802 battery status points. Pixii's SoC is already read
-- from this block at 40132, so these adjacent points are cheap diagnostics.
local REG_BATTERY_SOC = 40132
local REG_BATTERY_CHARGE_STATUS = 40137 -- ChaSt: OFF/EMPTY/DISCHARGING/...
local REG_BATTERY_CONTROL_MODE = 40138 -- LocRemCtl: REMOTE/LOCAL
local REG_BATTERY_STATE = 40143 -- State: CONNECTED/STANDBY/FAULT/...
local REG_BATTERY_STATE_VENDOR = 40144 -- StateVnd: Pixii-specific
local REG_BATTERY_EVT1 = 40147 -- Evt1 bitfield32

local sn_read = false
local troubleshooting = false
local last_status_key = nil
local last_command_ems_w = nil
local last_command_pixii_w = nil
local last_command_ms = nil
local last_command_ok = nil

-- Handshake counter, bumped every poll so the Pixii never times out to idle.
-- Any change is sufficient; we just walk 0..99 and wrap.
local hb_tick = 0

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- SunSpec scale factor: value * 10^sf, where sf is a signed int16 exponent.
local function scale(v, sf)
    if sf == 0 then return v end
    return v * (10 ^ sf)
end

-- Read a single i16-typed scale factor register, returning 0 on error.
local function read_sf(addr)
    local ok, regs = pcall(host.modbus_read, addr, 1, "holding")
    if ok and regs then return host.decode_i16(regs[1]) end
    return 0
end

-- Decode a SunSpec ASCII string from a block of u16 registers. Stops at
-- the first NUL byte and strips trailing whitespace.
local function decode_ascii(regs, count)
    local s = ""
    for i = 1, count do
        local hi = math.floor(regs[i] / 256)
        local lo = regs[i] % 256
        if hi == 0 and lo == 0 then break end
        if hi > 32 and hi < 127 then s = s .. string.char(hi) end
        if lo > 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
end

local function config_bool(config, key)
    if config == nil then return false end
    local v = config[key]
    return v == true or v == 1 or v == "1" or v == "true" or v == "yes" or v == "on"
end

local function read_u16(addr)
    local ok, regs = pcall(host.modbus_read, addr, 1, "holding")
    if ok and regs and regs[1] ~= nil then return regs[1] end
    return nil
end

local function read_u32_be(addr)
    local ok, regs = pcall(host.modbus_read, addr, 2, "holding")
    if ok and regs and regs[1] ~= nil and regs[2] ~= nil then
        return host.decode_u32_be(regs[1], regs[2])
    end
    return nil
end

local function read_i32_be(addr)
    local ok, regs = pcall(host.modbus_read, addr, 2, "holding")
    if ok and regs and regs[1] ~= nil and regs[2] ~= nil then
        return host.decode_i32_be(regs[1], regs[2])
    end
    return nil
end

local charge_status_labels = {
    [1] = "off",
    [2] = "empty",
    [3] = "discharging",
    [4] = "charging",
    [5] = "full",
    [6] = "holding",
    [7] = "testing",
}

local control_mode_labels = {
    [0] = "remote",
    [1] = "local",
}

local battery_state_labels = {
    [1] = "disconnected",
    [2] = "initializing",
    [3] = "connected",
    [4] = "standby",
    [5] = "soc_protection",
    [6] = "suspending",
    [99] = "fault",
}

local function label_for(labels, value)
    if value == nil then return "unknown" end
    return labels[value] or ("unknown_" .. tostring(value))
end

local function read_battery_status()
    local charge_status = read_u16(REG_BATTERY_CHARGE_STATUS)
    local control_mode = read_u16(REG_BATTERY_CONTROL_MODE)
    local battery_state = read_u16(REG_BATTERY_STATE)
    local vendor_state = read_u16(REG_BATTERY_STATE_VENDOR)
    local event1 = read_u32_be(REG_BATTERY_EVT1)

    if charge_status ~= nil then host.emit_metric("battery_charge_status_code", charge_status) end
    if control_mode ~= nil then host.emit_metric("battery_control_mode_code", control_mode) end
    if battery_state ~= nil then host.emit_metric("battery_state_code", battery_state) end
    if vendor_state ~= nil then host.emit_metric("battery_vendor_state_code", vendor_state) end
    if event1 ~= nil then host.emit_metric("battery_event1_bits", event1) end

    local charge_label = label_for(charge_status_labels, charge_status)
    local control_label = label_for(control_mode_labels, control_mode)
    local state_label = label_for(battery_state_labels, battery_state)
    local key = charge_label .. "/" .. control_label .. "/" .. state_label .. "/" .. tostring(vendor_state) .. "/" .. tostring(event1)
    if key ~= last_status_key then
        host.log("info", "Pixii: status charge=" .. charge_label
            .. " control=" .. control_label
            .. " state=" .. state_label
            .. " vendor_state=" .. tostring(vendor_state)
            .. " event1=" .. tostring(event1))
        if charge_status == 7 then
            host.log("warn", "Pixii: charge status is TESTING; excluding from dispatch until calibration finishes")
        end
        last_status_key = key
    end

    -- SunSpec 802 ChaSt=testing (7): Pixii is calibrating and ignores
    -- external setpoints. Flag a device fault so dispatch + MPC exclude it
    -- while keeping telemetry and site-meter data live.
    if charge_status ~= nil then
        local calibrating = charge_status == 7
        host.set_device_fault(calibrating,
            calibrating and "Pixii battery calibrating/testing (SunSpec ChaSt=testing)" or "")
    end

    return {
        charge_status = charge_status,
        charge_status_label = charge_label,
        control_mode = control_mode,
        control_mode_label = control_label,
        battery_state = battery_state,
        battery_state_label = state_label,
        vendor_state = vendor_state,
        event1 = event1,
    }
end

local function emit_troubleshooting_metrics()
    host.emit_metric("pixii_heartbeat_counter", hb_tick)
    local setpoint_pixii_w = read_i32_be(REG_SETPOINT_HI)
    if setpoint_pixii_w ~= nil then
        host.emit_metric("pixii_setpoint_native_w", setpoint_pixii_w)
        host.emit_metric("pixii_setpoint_ems_w", -setpoint_pixii_w)
    end
    if last_command_ems_w ~= nil then
        host.emit_metric("pixii_last_command_ems_w", last_command_ems_w)
        host.emit_metric("pixii_last_command_native_w", last_command_pixii_w)
        host.emit_metric("pixii_last_command_ok", last_command_ok or 0)
        if last_command_ms ~= nil then
            host.emit_metric("pixii_last_command_age_s", (host.millis() - last_command_ms) / 1000)
        end
    end
end

----------------------------------------------------------------------------
-- Fingerprint
----------------------------------------------------------------------------

-- driver_fingerprint() — passive probe for /api/drivers/fingerprint. Reads
-- ONLY the SunSpec common block; never writes (no heartbeat / setpoint).
-- Pixii exposes the common block on HOLDING registers (FC 0x03), unlike
-- SolarEdge which mirrors SunSpec onto input registers. Tri-state:
--   true  → SunSpec common block reports manufacturer "Pixii"
--   false → answered Modbus but it's a non-Pixii device
--   nil    → couldn't read (wrong unit id, not Modbus, timeout)
function driver_fingerprint()
    local ok, sig = pcall(host.modbus_read, 40000, 2, "holding")
    if not ok or sig == nil or sig[1] == nil or sig[2] == nil then
        return nil
    end
    -- SunSpec identifier "SunS" = 0x5375, 0x6E53.
    if sig[1] ~= 0x5375 or sig[2] ~= 0x6E53 then
        return false
    end
    local mok, mfg_regs = pcall(host.modbus_read, 40004, 16, "holding")
    if not mok or mfg_regs == nil then
        return nil
    end
    local mfg = decode_ascii(mfg_regs, 16)
    if mfg:sub(1, 5) ~= "Pixii" then
        return false -- SunSpec, but a different vendor
    end
    local serial = ""
    local sok, sn_regs = pcall(host.modbus_read, 40052, 16, "holding")
    if sok and sn_regs ~= nil then
        serial = decode_ascii(sn_regs, 16)
    end
    return true, { make = "Pixii", serial = serial, confidence = 0.95 }
end

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Pixii")
    troubleshooting = config_bool(config, "_troubleshooting_mode")
        or config_bool(config, "troubleshooting_mode")
        or config_bool(config, "troubleshooting")
        or config_bool(config, "debug")
    if troubleshooting then
        host.log("info", "Pixii: troubleshooting mode enabled")
    end

    -- Verify SunSpec signature at 40000 ("SunS") as a sanity check.
    local ok, sig = pcall(host.modbus_read, 40000, 2, "holding")
    if ok and sig then
        local want = "SunS"
        local got = decode_ascii(sig, 2)
        if got ~= want then
            host.log("warn", "Pixii: unexpected SunSpec header '" .. got .. "' at 40000")
        end
    end
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Keep the handshake counter moving so the Pixii never times out
    -- to idle. Pixii only requires that the value changes; 0..99 wrap.
    hb_tick = (hb_tick + 1) % 100
    local hb_err = host.modbus_write(REG_HEARTBEAT, hb_tick)
    if hb_err ~= nil and hb_err ~= "" then
        host.log("warn", "Pixii: heartbeat write failed: " .. tostring(hb_err))
    end

    -- Read serial number once from SunSpec Common Model (offset 52 from
    -- the common block → absolute 40052, 16 regs ASCII).
    if not sn_read then
        local ok, sn_regs = pcall(host.modbus_read, 40052, 16, "holding")
        if ok and sn_regs then
            local sn = decode_ascii(sn_regs, 16)
            if string.len(sn) > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---- Scale Factors (all i16 exponents) ----
    local ac_w_sf        = read_sf(40084)
    local hz_sf          = read_sf(40086)
    local temp_sf        = read_sf(40106)
    local soc_sf         = read_sf(40177)
    local bat_v_sf       = read_sf(40180)
    local bat_a_sf       = read_sf(40182)
    local bat_w_sf       = read_sf(40184)
    local meter_a_sf     = read_sf(40240)
    local meter_v_sf     = read_sf(40249)
    local meter_hz_sf    = read_sf(40251)
    local meter_w_sf     = read_sf(40256)
    local meter_va_sf    = read_sf(40261) -- SunSpec model 213 offset 27
    local meter_var_sf   = read_sf(40266) -- SunSpec model 213 offset 32
    local meter_energy_sf = read_sf(40288)

    -- ---- Battery Values ----

    -- AC power (inverter): 40083, I16  (diagnostic only; bat_w below is DC)
    local ok_acw, acw_regs = pcall(host.modbus_read, 40083, 1, "holding")
    local ac_w = 0
    if ok_acw and acw_regs then
        ac_w = scale(host.decode_i16(acw_regs[1]), ac_w_sf)
    end

    -- Inverter frequency: 40085, U16
    local ok_hz, hz_regs = pcall(host.modbus_read, 40085, 1, "holding")
    local inv_hz = 0
    if ok_hz and hz_regs then
        inv_hz = scale(hz_regs[1], hz_sf)
    end

    -- Inverter temperature: 40102, I16 (°C)
    local ok_temp, temp_regs = pcall(host.modbus_read, 40102, 1, "holding")
    local temp_c = 0
    if ok_temp and temp_regs then
        temp_c = scale(host.decode_i16(temp_regs[1]), temp_sf)
    end

    -- Battery SoC: 40132, U16 (percent → fraction 0..1).
    -- If Pixii returns a sentinel or otherwise impossible value, omit SoC
    -- from the emit rather than invalidating the whole battery reading.
    local ok_soc, soc_regs = pcall(host.modbus_read, REG_BATTERY_SOC, 1, "holding")
    local bat_soc = nil
    local bat_soc_pct = nil
    if ok_soc and soc_regs and soc_regs[1] ~= nil then
        bat_soc_pct = scale(soc_regs[1], soc_sf)
        local candidate = bat_soc_pct / 100
        if candidate >= 0 and candidate <= 1 then
            bat_soc = candidate
        else
            host.log("warn", "Pixii: ignoring invalid battery SoC percent=" .. tostring(bat_soc_pct)
                .. " raw=" .. tostring(soc_regs[1]) .. " sf=" .. tostring(soc_sf))
        end
    elseif troubleshooting then
        host.log("warn", "Pixii: SoC read failed")
    end
    if bat_soc_pct ~= nil then
        host.emit_metric("battery_soc_pct_raw", bat_soc_pct)
        host.emit_metric("battery_soc_valid", bat_soc ~= nil and 1 or 0)
    end

    -- Battery voltage: 40155, I16
    local ok_bv, bv_regs = pcall(host.modbus_read, 40155, 1, "holding")
    local bat_v = 0
    if ok_bv and bv_regs then
        bat_v = scale(host.decode_i16(bv_regs[1]), bat_v_sf)
    end

    -- Battery current: 40165, I16
    local ok_ba, ba_regs = pcall(host.modbus_read, 40165, 1, "holding")
    local bat_a = 0
    if ok_ba and ba_regs then
        bat_a = scale(host.decode_i16(ba_regs[1]), bat_a_sf)
    end

    -- Battery DC power: 40168, I16  (SunSpec: positive = charge, so site-conv)
    local ok_bw, bw_regs = pcall(host.modbus_read, 40168, 1, "holding")
    local bat_w = 0
    if ok_bw and bw_regs then
        bat_w = scale(host.decode_i16(bw_regs[1]), bat_w_sf)
    end

    -- Cabinet charge/discharge energy: 39958-39961, two I32 BE pairs, kWh
    local ok_cab, cab_regs = pcall(host.modbus_read, 39958, 4, "holding")
    local bat_charge_wh, bat_discharge_wh = 0, 0
    if ok_cab and cab_regs then
        bat_charge_wh    = host.decode_i32_be(cab_regs[1], cab_regs[2]) * 1000
        bat_discharge_wh = host.decode_i32_be(cab_regs[3], cab_regs[4]) * 1000
    end

    local status = read_battery_status()
    if troubleshooting then
        emit_troubleshooting_metrics()
    end

    local battery = {
        w                    = bat_w,
        v                    = bat_v,
        a                    = bat_a,
        temp_c               = temp_c,
        charge_wh            = bat_charge_wh,
        discharge_wh         = bat_discharge_wh,
        charge_status        = status.charge_status_label,
        control_mode         = status.control_mode_label,
        battery_state        = status.battery_state_label,
        battery_vendor_state = status.vendor_state,
        battery_event1       = status.event1,
    }
    if bat_soc ~= nil then battery.soc = bat_soc end
    host.emit("battery", battery)
    -- Diagnostics: long-format TS DB
    host.emit_metric("battery_dc_v",      bat_v)
    host.emit_metric("battery_dc_a",      bat_a)
    host.emit_metric("battery_ac_w",      ac_w)
    host.emit_metric("inverter_temp_c",   temp_c)
    host.emit_metric("inverter_hz",       inv_hz)

    -- ---- Meter Values ----

    -- Per-phase current: 40237-40239, I16 each. Pixii's amperage
    -- registers are magnitude-only (the firmware reports the absolute
    -- value regardless of direction), so we derive the SIGN from the
    -- signed per-phase power read in the same atomic poll below
    -- (l1_w / l2_w / l3_w decode immediately after this block from
    -- contiguous registers — same Modbus snapshot, same instant).
    -- Final emit is `sign(l*_w) * |l*_a|`, giving the UI the
    -- direction it needs without the Pixii's own missing sign bit.
    --
    -- Fuse safety: dispatch.go:1561 takes math.Abs() before clamping,
    -- so signed amps here do NOT weaken the per-phase fuse guard —
    -- the guard fires on magnitude regardless of direction, exactly
    -- as before.
    local ok_la, la_regs = pcall(host.modbus_read, 40237, 3, "holding")
    local l1_a_mag, l2_a_mag, l3_a_mag = 0, 0, 0
    if ok_la and la_regs then
        l1_a_mag = math.abs(scale(host.decode_i16(la_regs[1]), meter_a_sf))
        l2_a_mag = math.abs(scale(host.decode_i16(la_regs[2]), meter_a_sf))
        l3_a_mag = math.abs(scale(host.decode_i16(la_regs[3]), meter_a_sf))
    end

    -- Per-phase voltage: 40242-40244, I16 each
    local ok_lv, lv_regs = pcall(host.modbus_read, 40242, 3, "holding")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_lv and lv_regs then
        l1_v = scale(host.decode_i16(lv_regs[1]), meter_v_sf)
        l2_v = scale(host.decode_i16(lv_regs[2]), meter_v_sf)
        l3_v = scale(host.decode_i16(lv_regs[3]), meter_v_sf)
    end

    -- Meter frequency: 40250, U16
    local ok_mhz, mhz_regs = pcall(host.modbus_read, 40250, 1, "holding")
    local meter_hz = 0
    if ok_mhz and mhz_regs then
        meter_hz = scale(mhz_regs[1], meter_hz_sf)
    end

    -- Total meter power: 40252, I16
    local ok_mw, mw_regs = pcall(host.modbus_read, 40252, 1, "holding")
    local meter_w = 0
    if ok_mw and mw_regs then
        meter_w = scale(host.decode_i16(mw_regs[1]), meter_w_sf)
    end

    -- Per-phase meter power: 40253-40255, I16 each
    local ok_lpw, lpw_regs = pcall(host.modbus_read, 40253, 3, "holding")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_lpw and lpw_regs then
        l1_w = scale(host.decode_i16(lpw_regs[1]), meter_w_sf)
        l2_w = scale(host.decode_i16(lpw_regs[2]), meter_w_sf)
        l3_w = scale(host.decode_i16(lpw_regs[3]), meter_w_sf)
    end

    -- Reactive-power diagnostics: total VA and total VAR (model 213
    -- offsets 23 + 28 → 40257 / 40262, both I16). Per-phase variants
    -- (offsets 24-26 / 29-31) are the SunSpec "not implemented" sentinel
    -- 0x8000 on Pixii — confirmed live 2026-05-06 — so we don't bother
    -- reading them. Total registers usually ARE populated.
    --
    -- Sentinel-aware: SunSpec uses 0x8000 (= -32768 i16) for "register
    -- not implemented". Filter before emit so the TS DB doesn't get
    -- polluted with constant `-32768 × 10^sf` rows that look like real
    -- measurements.
    local function i16_present(reg)
        return reg ~= 0x8000
    end
    local ok_va, va_regs = pcall(host.modbus_read, 40257, 1, "holding")
    local meter_va, meter_va_ok = 0, false
    if ok_va and va_regs and i16_present(va_regs[1]) then
        meter_va = scale(host.decode_i16(va_regs[1]), meter_va_sf)
        meter_va_ok = true
    end
    local ok_var, var_regs = pcall(host.modbus_read, 40262, 1, "holding")
    local meter_var, meter_var_ok = 0, false
    if ok_var and var_regs and i16_present(var_regs[1]) then
        meter_var = scale(host.decode_i16(var_regs[1]), meter_var_sf)
        meter_var_ok = true
    end

    -- Compose signed per-phase current = sign(power) × |amperage|.
    -- A small dead-band around 0 W avoids flipping the sign when a
    -- near-zero phase reads as +0.4 W vs -0.4 W between polls. With
    -- |W| < 1, treat the phase as zero-amp regardless of magnitude
    -- (consumer current at <1 W on 230 V is 4 mA — below register
    -- resolution anyway).
    local function signed_a(mag, w)
        if math.abs(w) < 1 then return 0 end
        if w < 0 then return -mag end
        return mag
    end
    local l1_a = signed_a(l1_a_mag, l1_w)
    local l2_a = signed_a(l2_a_mag, l2_w)
    local l3_a = signed_a(l3_a_mag, l3_w)

    -- Export energy: 40272-40275, U32 BE (two regs consumed for the value)
    local ok_exp, exp_regs = pcall(host.modbus_read, 40272, 4, "holding")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = scale(host.decode_u32_be(exp_regs[1], exp_regs[2]), meter_energy_sf)
    end

    -- Import energy: 40280-40283, U32 BE
    local ok_imp, imp_regs = pcall(host.modbus_read, 40280, 4, "holding")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = scale(host.decode_u32_be(imp_regs[1], imp_regs[2]), meter_energy_sf)
    end

    -- Native Pixii meter already uses site convention (+import / -export),
    -- so values are passed through without a sign flip.
    host.emit("meter", {
        w         = meter_w,
        l1_w      = l1_w,
        l2_w      = l2_w,
        l3_w      = l3_w,
        l1_v      = l1_v,
        l2_v      = l2_v,
        l3_v      = l3_v,
        l1_a      = l1_a,
        l2_a      = l2_a,
        l3_a      = l3_a,
        hz        = meter_hz,
        import_wh = import_wh,
        export_wh = export_wh,
    })
    host.emit_metric("meter_l1_w", l1_w)
    host.emit_metric("meter_l2_w", l2_w)
    host.emit_metric("meter_l3_w", l3_w)
    host.emit_metric("meter_l1_v", l1_v)
    host.emit_metric("meter_l2_v", l2_v)
    host.emit_metric("meter_l3_v", l3_v)
    host.emit_metric("meter_l1_a", l1_a)
    host.emit_metric("meter_l2_a", l2_a)
    host.emit_metric("meter_l3_a", l3_a)
    if meter_va_ok  then host.emit_metric("meter_va",  meter_va)  end
    if meter_var_ok then host.emit_metric("meter_var", meter_var) end
    host.emit_metric("grid_hz",    meter_hz)

    return 5000
end

----------------------------------------------------------------------------
-- Control: active power setpoint (demand response)
----------------------------------------------------------------------------
-- EMS convention on the FTW side: power_w > 0 = charge, < 0 = discharge.
-- Pixii 39905/06 uses generator reference frame (positive = discharge),
-- so the sign is flipped at the setpoint boundary. The two registers
-- MUST be written atomically as a single write-multiple (FC 0x10) so
-- the Pixii doesn't see a half-updated int32 — doc 13300 page 15 is
-- explicit about this.

-- Encode a signed int32 watt value into two big-endian u16 registers.
-- Lua numbers are 64-bit doubles so the two's-complement math stays
-- exact for any realistic setpoint (< ~2^53).
local function encode_i32_be(value)
    local raw = math.floor(value + 0.5)
    if raw < 0 then raw = raw + 0x100000000 end
    local hi = math.floor(raw / 0x10000) % 0x10000
    local lo = raw % 0x10000
    return hi, lo
end

local function write_setpoint_w(pixii_w)
    local hi, lo = encode_i32_be(pixii_w)
    host.log("info", "Pixii: modbus_write_multi addr=" .. REG_SETPOINT_HI
        .. " hi=" .. tostring(hi) .. " lo=" .. tostring(lo)
        .. " (pixii_w=" .. tostring(pixii_w) .. ")")
    local err = host.modbus_write_multi(REG_SETPOINT_HI, { hi, lo })
    if err ~= nil and err ~= "" then
        host.log("warn", "Pixii: setpoint write failed: " .. tostring(err))
        return false
    end
    -- Bump the heartbeat on every command too — the dispatch tick is
    -- often faster than the poll tick, and we don't want the Pixii to
    -- edge into idle right after we told it to move.
    hb_tick = (hb_tick + 1) % 100
    local hb_err = host.modbus_write(REG_HEARTBEAT, hb_tick)
    if hb_err ~= nil and hb_err ~= "" then
        host.log("warn", "Pixii: heartbeat write failed: " .. tostring(hb_err))
    end
    if troubleshooting then
        local actual_pixii_w = read_i32_be(REG_SETPOINT_HI)
        if actual_pixii_w ~= nil then
            host.emit_metric("pixii_setpoint_native_w", actual_pixii_w)
            host.emit_metric("pixii_setpoint_ems_w", -actual_pixii_w)
            host.log("info", "Pixii: setpoint readback pixii_w=" .. tostring(actual_pixii_w)
                .. " ems_w=" .. tostring(-actual_pixii_w))
        else
            host.log("warn", "Pixii: setpoint readback failed")
        end
    end
    return true
end

local function set_battery_power(power_w)
    -- Flip EMS → generator frame.
    local pixii_w = -power_w
    host.log("info", "Pixii: setpoint ems_w=" .. tostring(power_w)
        .. " pixii_w=" .. tostring(pixii_w))
    last_command_ems_w = power_w
    last_command_pixii_w = pixii_w
    last_command_ms = host.millis()
    local ok = write_setpoint_w(pixii_w)
    last_command_ok = ok and 1 or 0
    return ok
end

function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        return set_battery_power(power_w or 0)
    elseif action == "curtail_disable" or action == "deinit" then
        return set_battery_power(0)
    end
    host.log("debug", "Pixii: unsupported action=" .. tostring(action))
    return false
end

-- Watchdog fallback: site-meter stale or driver-host decided to bail.
-- Return the Pixii to idle (setpoint 0). The PI loop will re-assert
-- its desired setpoint on the next cycle once telemetry recovers.
function driver_default_mode()
    host.log("info", "Pixii: watchdog → setpoint 0")
    set_battery_power(0)
end

function driver_cleanup()
    set_battery_power(0)
end
