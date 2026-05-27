-- Solis Hybrid Inverter Driver
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP (typical port 502)
--
-- Ported from sourceful-hugin/device-support/drivers/lua/solis.lua.
-- Rewritten to forty-two-watts v2.1 host idiom:
--   * DRIVER metadata table for the catalog
--   * host.log(level, msg)         (was host.log(msg))
--   * host.decode_u32_be / i32_be  (was host.decode_u32 / i32)
--   * host.emit_metric(...) diagnostics → long-format TS DB
--   * driver_default_mode + driver_cleanup watchdog fallbacks
--
-- Sign convention (site boundary — positive W = into site):
--   PV w:      always negative (generation)
--   Battery w: positive = charging, negative = discharging
--   Meter w:   positive = import, negative = export
--
-- Register map (INPUT registers unless noted — Big-Endian word order):
--   33049-33050  MPPT1 V / A       U16 × 0.1
--   33051-33052  MPPT2 V / A       U16 × 0.1
--   33029-33030  PV lifetime gen   U32 BE kWh
--   33057-33058  PV DC power       U32 BE W
--   33093        Inverter temp     I16 × 0.1 C
--   33096        Battery temp      I16 × 0.1 C
--   33133        Battery V         U16 × 0.1 V
--   33134        Battery A         I16 × 0.1 A
--   33135        Battery direction U16 (0=charge, 1=discharge)
--   33139        Battery SoC       U16 percent
--   33149-33150  Battery W         I32 BE W (unsigned magnitude, dir from 33135)
--   33161-33162  Battery charge    U32 BE kWh
--   33165-33166  Battery discharge U32 BE kWh
--   33251-33256  Per-phase V / A   U16 pairs (V×0.1, A×0.01)
--   33257-33262  Per-phase W       I32 BE W each (vendor sign: + = export)
--   33282        Grid frequency    U16 × 0.01 Hz
--   33283-33284  Import energy     U32 BE × 0.01 kWh
--   33285-33286  Export energy     U32 BE × 0.01 kWh
--   33004-33019  Inverter SN       ASCII, 2 chars per register (16 regs = 32 chars)
--
-- Control registers (HOLDING — Solis Appendix 7 "Storage Control"):
--   43110  Mode bit-field     0x60 (96) = forced charge/discharge enable (bits 5+6)
--                             0x21 (33) = self-consumption + time-of-use
--   43129  Discharge power    U16, 10 W units
--   43130  Charge limit       U16, W (set at init to rated power)
--   43131  Discharge limit    U16, W (set at init to rated power)
--   43135  Mode select        0 = off / 1 = charge / 2 = discharge
--   43136  Charge power       U16, 10 W units
--
-- Solis firmware NACKs back-to-back holding writes; space them ~100 ms.

DRIVER = {
  id           = "solis",
  name         = "Solis hybrid inverter",
  manufacturer = "Ginlong Solis",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Solis S5/S6 hybrid inverters via Modbus TCP.",
  homepage     = "https://www.ginlong.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "S6-EH", "S5-GR", "S6-GR" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a 42W site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

-- Cached state
local sn_read = false
local control_initialized = false
local rated_power_w = 5000  -- default; operator can override via config.rated_w

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Ginlong Solis")
    if config and type(config) == "table" and tonumber(config.rated_w) then
        rated_power_w = math.floor(tonumber(config.rated_w))
    end
    host.log("info", string.format("Solis: driver_init (rated=%dW)", rated_power_w))
end

----------------------------------------------------------------------------
-- Serial number (best-effort, 33004-33019 ASCII)
----------------------------------------------------------------------------

local function try_read_sn()
    if sn_read then return end
    local ok, sn_regs = pcall(host.modbus_read, 33004, 16, "input")
    if not (ok and sn_regs) then return end
    local sn = ""
    for i = 1, 16 do
        local hi = math.floor(sn_regs[i] / 256)
        local lo = sn_regs[i] % 256
        if hi > 32 and hi < 127 then sn = sn .. string.char(hi) end
        if lo > 32 and lo < 127 then sn = sn .. string.char(lo) end
    end
    -- Trim whitespace
    sn = sn:gsub("^%s+", ""):gsub("%s+$", "")
    if #sn >= 4 then
        host.set_sn(sn)
        host.log("info", "Solis: SN=" .. sn)
        sn_read = true
    end
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    try_read_sn()

    ------------------------------------------------------------------------
    -- PV
    ------------------------------------------------------------------------

    -- PV DC power: 33057-33058, U32 BE, W
    local ok_pvw, pvw_regs = pcall(host.modbus_read, 33057, 2, "input")
    local pv_w = 0
    if ok_pvw and pvw_regs then
        pv_w = host.decode_u32_be(pvw_regs[1], pvw_regs[2])
    end

    -- MPPT1 V/A: 33049-33050
    local ok_mppt1, mppt1_regs = pcall(host.modbus_read, 33049, 2, "input")
    local mppt1_v, mppt1_a = 0, 0
    if ok_mppt1 and mppt1_regs then
        mppt1_v = mppt1_regs[1] * 0.1
        mppt1_a = mppt1_regs[2] * 0.1
    end

    -- MPPT2 V/A: 33051-33052
    local ok_mppt2, mppt2_regs = pcall(host.modbus_read, 33051, 2, "input")
    local mppt2_v, mppt2_a = 0, 0
    if ok_mppt2 and mppt2_regs then
        mppt2_v = mppt2_regs[1] * 0.1
        mppt2_a = mppt2_regs[2] * 0.1
    end

    -- PV lifetime energy: 33029-33030, U32 BE × 1 kWh
    local ok_pvgen, pvgen_regs = pcall(host.modbus_read, 33029, 2, "input")
    local pv_gen_wh = 0
    if ok_pvgen and pvgen_regs then
        pv_gen_wh = host.decode_u32_be(pvgen_regs[1], pvgen_regs[2]) * 1000
    end

    -- Inverter temperature: 33093, I16 × 0.1 C
    local ok_itemp, itemp_regs = pcall(host.modbus_read, 33093, 1, "input")
    local inv_temp = 0
    if ok_itemp and itemp_regs then
        inv_temp = host.decode_i16(itemp_regs[1]) * 0.1
    end

    host.emit("pv", {
        w           = -pv_w,  -- negative = generation (site convention)
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = pv_gen_wh,
        temp_c      = inv_temp,
    })
    host.emit_metric("pv_mppt1_v",      mppt1_v)
    host.emit_metric("pv_mppt1_a",      mppt1_a)
    host.emit_metric("pv_mppt2_v",      mppt2_v)
    host.emit_metric("pv_mppt2_a",      mppt2_a)
    host.emit_metric("inverter_temp_c", inv_temp)

    ------------------------------------------------------------------------
    -- Battery
    ------------------------------------------------------------------------

    -- Voltage: 33133, U16 × 0.1 V
    local ok_bv, bv_regs = pcall(host.modbus_read, 33133, 1, "input")
    local bat_v = 0
    if ok_bv and bv_regs then
        bat_v = bv_regs[1] * 0.1
    end

    -- Current: 33134, I16 × 0.1 A
    local ok_ba, ba_regs = pcall(host.modbus_read, 33134, 1, "input")
    local bat_a = 0
    if ok_ba and ba_regs then
        bat_a = host.decode_i16(ba_regs[1]) * 0.1
    end

    -- Direction: 33135, U16 (0=charge, 1=discharge)
    local ok_bdir, bdir_regs = pcall(host.modbus_read, 33135, 1, "input")
    local bat_direction = 0
    if ok_bdir and bdir_regs then
        bat_direction = bdir_regs[1]
    end

    -- SoC: 33139, U16 percent (0-100) → fraction (0-1)
    local ok_bsoc, bsoc_regs = pcall(host.modbus_read, 33139, 1, "input")
    local bat_soc = 0
    if ok_bsoc and bsoc_regs then
        bat_soc = bsoc_regs[1] / 100
    end

    -- Power magnitude: 33149-33150, I32 BE, W
    local ok_bw, bw_regs = pcall(host.modbus_read, 33149, 2, "input")
    local bat_w = 0
    if ok_bw and bw_regs then
        bat_w = host.decode_i32_be(bw_regs[1], bw_regs[2])
    end
    -- Apply direction: 1 = discharge (negative in site convention)
    if bat_direction == 1 then
        bat_w = -bat_w
    end

    -- Charge energy: 33161-33162, U32 BE × 1 kWh
    local ok_bchg, bchg_regs = pcall(host.modbus_read, 33161, 2, "input")
    local bat_charge_wh = 0
    if ok_bchg and bchg_regs then
        bat_charge_wh = host.decode_u32_be(bchg_regs[1], bchg_regs[2]) * 1000
    end

    -- Discharge energy: 33165-33166, U32 BE × 1 kWh
    local ok_bdis, bdis_regs = pcall(host.modbus_read, 33165, 2, "input")
    local bat_discharge_wh = 0
    if ok_bdis and bdis_regs then
        bat_discharge_wh = host.decode_u32_be(bdis_regs[1], bdis_regs[2]) * 1000
    end

    -- Battery temperature: 33096, I16 × 0.1 C
    local ok_btemp, btemp_regs = pcall(host.modbus_read, 33096, 1, "input")
    local bat_temp = 0
    if ok_btemp and btemp_regs then
        bat_temp = host.decode_i16(btemp_regs[1]) * 0.1
    end

    host.emit("battery", {
        w            = bat_w,
        v            = bat_v,
        a            = bat_a,
        soc          = bat_soc,
        temp_c       = bat_temp,
        charge_wh    = bat_charge_wh,
        discharge_wh = bat_discharge_wh,
    })
    host.emit_metric("battery_dc_v",   bat_v)
    host.emit_metric("battery_dc_a",   bat_a)
    host.emit_metric("battery_temp_c", bat_temp)

    ------------------------------------------------------------------------
    -- Meter (grid connection point)
    ------------------------------------------------------------------------

    -- Per-phase V/A: 33251-33256 (V×0.1, A×0.01 interleaved)
    local ok_mva, mva_regs = pcall(host.modbus_read, 33251, 6, "input")
    local l1_v, l1_a, l2_v, l2_a, l3_v, l3_a = 0, 0, 0, 0, 0, 0
    if ok_mva and mva_regs then
        l1_v = mva_regs[1] * 0.1
        l1_a = mva_regs[2] * 0.01
        l2_v = mva_regs[3] * 0.1
        l2_a = mva_regs[4] * 0.01
        l3_v = mva_regs[5] * 0.1
        l3_a = mva_regs[6] * 0.01
    end

    -- Per-phase power: 33257-33262, I32 BE each pair
    -- Solis vendor sign: positive = export (out of grid meter).
    -- Site convention: positive = import. Flip sign at the boundary.
    local ok_mpw, mpw_regs = pcall(host.modbus_read, 33257, 6, "input")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_mpw and mpw_regs then
        l1_w = -host.decode_i32_be(mpw_regs[1], mpw_regs[2])
        l2_w = -host.decode_i32_be(mpw_regs[3], mpw_regs[4])
        l3_w = -host.decode_i32_be(mpw_regs[5], mpw_regs[6])
    end
    local meter_w = l1_w + l2_w + l3_w

    -- Frequency: 33282, U16 × 0.01 Hz
    local ok_hz, hz_regs = pcall(host.modbus_read, 33282, 1, "input")
    local hz = 0
    if ok_hz and hz_regs then
        hz = hz_regs[1] * 0.01
    end

    -- Import: 33283-33284, U32 BE × 0.01 kWh
    local ok_imp, imp_regs = pcall(host.modbus_read, 33283, 2, "input")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = host.decode_u32_be(imp_regs[1], imp_regs[2]) * 0.01 * 1000
    end

    -- Export: 33285-33286, U32 BE × 0.01 kWh
    local ok_exp, exp_regs = pcall(host.modbus_read, 33285, 2, "input")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = host.decode_u32_be(exp_regs[1], exp_regs[2]) * 0.01 * 1000
    end

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
        hz        = hz,
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
    host.emit_metric("grid_hz",    hz)

    return 5000
end

----------------------------------------------------------------------------
-- Control
----------------------------------------------------------------------------

local REG_MODE_BITS       = 43110
local REG_DISCHARGE_POWER = 43129
local REG_CHARGE_LIMIT    = 43130
local REG_DISCHARGE_LIMIT = 43131
local REG_MODE            = 43135
local REG_CHARGE_POWER    = 43136

local MODE_BITS_FORCED       = 96  -- 0b0110_0000 — bits 5+6: forced charge/discharge
local MODE_BITS_SELF_CONSUME = 33  -- 0b0010_0001 — self-consumption + TOU

local MODE_OFF       = 0
local MODE_CHARGE    = 1
local MODE_DISCHARGE = 2

local WRITE_DELAY_MS = 100

-- Write a single holding register and pace the bus so the next write
-- doesn't land inside Solis's NACK window.
local function write_reg(addr, val)
    local err = host.modbus_write(addr, val)
    host.sleep(WRITE_DELAY_MS)
    if err ~= nil and err ~= "" then
        host.log("warn", string.format("Solis: write %d=%d failed: %s",
            addr, val, tostring(err)))
        return false
    end
    return true
end

-- Lazy init: load per-direction power limits (rated power) the first
-- time we actually need to force the inverter. Cheap to retry after
-- failure because the flag stays false until all writes succeed.
local function initialize_control()
    if not write_reg(REG_CHARGE_LIMIT,    rated_power_w) then return false end
    if not write_reg(REG_DISCHARGE_LIMIT, rated_power_w) then return false end
    control_initialized = true
    host.log("info", string.format("Solis: control initialized (rated=%dW)",
        rated_power_w))
    return true
end

local function clamp_magnitude(power_w)
    local m = math.abs(power_w)
    if m > rated_power_w then m = rated_power_w end
    return m
end

-- Positive W = charge, negative W = discharge (site convention).
-- Magnitude is clamped to rated power and encoded in 10 W units for
-- both the charge and discharge power registers; the mode register
-- (43135) picks which one the inverter acts on.
local function set_battery_power(power_w)
    if not control_initialized and not initialize_control() then
        host.log("error", "Solis: cannot set power, init failed")
        return false
    end

    local mode = MODE_OFF
    if power_w > 0 then
        mode = MODE_CHARGE
    elseif power_w < 0 then
        mode = MODE_DISCHARGE
    end

    local magnitude_10w = math.floor(clamp_magnitude(power_w) / 10 + 0.5)

    if not write_reg(REG_MODE_BITS,       MODE_BITS_FORCED) then return false end
    if not write_reg(REG_DISCHARGE_POWER, magnitude_10w)    then return false end
    if not write_reg(REG_CHARGE_POWER,    magnitude_10w)    then return false end

    -- For 0 W leave the mode register alone so the inverter stays under
    -- forced control with both limits at 0, instead of drifting back to
    -- self-consumption.
    if mode ~= MODE_OFF then
        if not write_reg(REG_MODE, mode) then return false end
    end

    host.log("debug", string.format("Solis: setpoint %dW (mode=%d, mag10W=%d)",
        power_w, mode, magnitude_10w))
    return true
end

-- Return the inverter to its native self-consumption behaviour.
-- Clears control_initialized so the next forced setpoint re-writes the
-- per-direction limits.
local function set_self_consumption()
    if not write_reg(REG_MODE_BITS, MODE_BITS_SELF_CONSUME) then
        return false
    end
    control_initialized = false
    host.log("debug", "Solis: self-consumption mode")
    return true
end

function driver_command(action, power_w, cmd)
    if action == "init" then
        return initialize_control()
    elseif action == "battery" then
        return set_battery_power(power_w or 0)
    elseif action == "curtail" or action == "curtail_disable" then
        -- Solar curtailment is not implemented for Solis (no reliable
        -- export-limit register in Appendix 7); matches Zap reference.
        host.log("warn", "Solis: curtailment not implemented")
        return false
    elseif action == "deinit" then
        return set_self_consumption()
    end
    return false
end

-- Watchdog fallback: on EMS timeout, go back to self-consumption.
function driver_default_mode()
    host.log("info", "Solis: watchdog → self-consumption")
    set_self_consumption()
end

function driver_cleanup()
    -- Best-effort: leave the inverter in autonomous mode on shutdown/reload.
    pcall(set_self_consumption)
    sn_read = false
    control_initialized = false
end
