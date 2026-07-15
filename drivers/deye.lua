-- Deye Hybrid Inverter Driver
-- Ported from sourceful-hugin/device-support/drivers/lua/deye.lua
-- Emits: PV, Battery, Meter telemetry + battery control
-- Protocol: Modbus TCP (holding registers throughout)
-- Byte order: Little-Endian for multi-register U32 values

DRIVER = {
  id           = "deye",
  name         = "Deye hybrid inverter",
  manufacturer = "Deye",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Deye SUN-SG series hybrid inverters via Modbus. Auto-detects LV vs HV battery at init.",
  homepage     = "https://www.deyeinverter.com",
  authors      = { "FTW contributors" },
  tested_models = { "SUN-SG03LP1", "SUN-SG04LP3" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Register conventions (all holding, FC 0x03/0x06):
--   0        : device type code. High byte == 6 → HV battery variant.
--   3-7      : serial number, packed 2 bytes/register, big-endian within each word.
--   20-21    : rated power, U32 LE × 0.1 kW
--   141      : energy management mode (0=Selling First, 1=Zero Export To Load,
--              2=Zero Export To CT, 3=External EMS / forced)
--   143      : grid-charge enable (bit 0)
--   144      : power limit for forced charge/discharge (W)
--   217      : battery temperature, U16, actual C = (val - 1000) / 10
--   516-519  : battery charge/discharge energy counters, U32 LE × 0.1 kWh
--   522-525  : grid import/export energy, U32 LE × 0.1 kWh
--   534-535  : total PV generation, U32 LE × 0.1 kWh
--   541      : heatsink temperature, U16 × 0.1 C
--   587-591  : battery V/SoC/power/current
--   598-612  : per-phase V/current, grid frequency
--   619, 622 : total + per-phase meter power (I16, W)
--   672-679  : PV power + MPPT V/A
--
-- Sign convention (site view, see docs/site-convention.md):
--   pv.w      : always negative (generation flowing into the site)
--   battery.w : positive = charging, negative = discharging
--   meter.w   : positive = importing, negative = exporting

PROTOCOL = "modbus"

-- Cached across polls
local is_hv = false
local sn_read = false
local control_initialized = false
local rated_power_w = 0
-- Grid → battery charge current cap (reg 128). Sized to the install's main
-- fuse / grid subscription, not the battery's max charge rate. Overridable
-- via config.max_grid_charge_a; default 31 A matches Zap's init profile.
local grid_charge_current_a = 31

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Deye")

    -- Strict HV detect (reg 0 raw value == 6 is the HV variant per Deye
    -- protocol docs). Earlier ports also accepted the value in the high
    -- byte, but the Zap reference on real hardware uses strict equality
    -- and we match that to avoid false HV positives on LV units.
    local ok, mode_regs = pcall(host.modbus_read, 0, 1, "holding")
    if ok and mode_regs then
        local val = mode_regs[1]
        is_hv = (val == 6)
        host.log("info", string.format("Deye: device type 0x%04x (%s battery)",
            val, is_hv and "HV" or "LV"))
    else
        host.log("warn", "Deye: device type read failed; assuming LV")
    end

    -- Rated power drives curtailment ceiling and the init profile's max-sell
    -- register. Prefer an operator-supplied value from YAML, else pull the
    -- device-reported rating from regs 20-21 (U32 LE × 0.1 kW), else default.
    if config and type(config) == "table" and tonumber(config.rated_w) then
        rated_power_w = math.floor(tonumber(config.rated_w))
    else
        local ok_r, rated_regs = pcall(host.modbus_read, 20, 2, "holding")
        if ok_r and rated_regs then
            rated_power_w = math.floor(
                host.decode_u32_le(rated_regs[1], rated_regs[2]) * 0.1 * 1000)
        end
    end
    if rated_power_w <= 0 then rated_power_w = 5000 end

    if config and type(config) == "table" and tonumber(config.max_grid_charge_a) then
        local a = math.floor(tonumber(config.max_grid_charge_a))
        if a < 0 then a = 0 end
        if a > 185 then a = 185 end  -- register ceiling per Deye V105.3 spec
        grid_charge_current_a = a
    end

    local function clamp_soc(v)
        v = math.floor(v)
        if v < 0   then v = 0   end
        if v > 100 then v = 100 end
        return v
    end
    if config and type(config) == "table" then
        if tonumber(config.soc_max) then soc_max = clamp_soc(tonumber(config.soc_max)) end
        if tonumber(config.soc_min) then soc_min = clamp_soc(tonumber(config.soc_min)) end
        if soc_min > soc_max then
            host.log("warn", string.format(
                "Deye: soc_min (%d) > soc_max (%d); swapping", soc_min, soc_max))
            soc_min, soc_max = soc_max, soc_min
        end
    end

    host.log("info", string.format(
        "Deye: driver_init (rated=%dW, max_grid_charge=%dA, soc=%d..%d)",
        rated_power_w, grid_charge_current_a, soc_min, soc_max))
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Read serial number once.  Registers 3..7 hold 10 ASCII bytes
    -- (2 chars per register, big-endian within each word).
    if not sn_read then
        local ok, sn_regs = pcall(host.modbus_read, 3, 5, "holding")
        if ok and sn_regs then
            local sn = ""
            for i = 1, 5 do
                local hi = math.floor(sn_regs[i] / 256)
                local lo = sn_regs[i] % 256
                if hi > 32 and hi < 127 then sn = sn .. string.char(hi) end
                if lo > 32 and lo < 127 then sn = sn .. string.char(lo) end
            end
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---- PV ----

    -- PV1-PV4 power: 672-675, U16 each (×1 LV, ×10 HV)
    local ok_pvw, pvw_regs = pcall(host.modbus_read, 672, 4, "holding")
    local pv_total_w = 0
    if ok_pvw and pvw_regs then
        local pv_scale = is_hv and 10 or 1
        for i = 1, 4 do
            pv_total_w = pv_total_w + pvw_regs[i] * pv_scale
        end
    end

    -- MPPT1 V/A: 676-677, U16 × 0.1 each
    local ok_m1, m1_regs = pcall(host.modbus_read, 676, 2, "holding")
    local mppt1_v, mppt1_a = 0, 0
    if ok_m1 and m1_regs then
        mppt1_v = m1_regs[1] * 0.1
        mppt1_a = m1_regs[2] * 0.1
    end

    -- MPPT2 V/A: 678-679, U16 × 0.1 each
    local ok_m2, m2_regs = pcall(host.modbus_read, 678, 2, "holding")
    local mppt2_v, mppt2_a = 0, 0
    if ok_m2 and m2_regs then
        mppt2_v = m2_regs[1] * 0.1
        mppt2_a = m2_regs[2] * 0.1
    end

    -- Total generation: 534-535, U32 LE × 0.1 kWh
    local ok_gen, gen_regs = pcall(host.modbus_read, 534, 2, "holding")
    local pv_gen_wh = 0
    if ok_gen and gen_regs then
        pv_gen_wh = host.decode_u32_le(gen_regs[1], gen_regs[2]) * 0.1 * 1000
    end

    -- Rated power: 20-21, U32 LE × 0.1 kW
    local ok_rated, rated_regs = pcall(host.modbus_read, 20, 2, "holding")
    local rated_w = 0
    if ok_rated and rated_regs then
        rated_w = host.decode_u32_le(rated_regs[1], rated_regs[2]) * 0.1 * 1000
    end

    -- Heatsink temperature: 541, U16 × 0.1 C
    local ok_temp, temp_regs = pcall(host.modbus_read, 541, 1, "holding")
    local heatsink_c = 0
    if ok_temp and temp_regs then
        heatsink_c = temp_regs[1] * 0.1
    end

    host.emit("pv", {
        w           = -pv_total_w,  -- negate: PV generation is negative in site convention
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = pv_gen_wh,
        rated_w     = rated_w,
        temp_c      = heatsink_c,
    })
    host.emit_metric("pv_mppt1_v",      mppt1_v)
    host.emit_metric("pv_mppt1_a",      mppt1_a)
    host.emit_metric("pv_mppt2_v",      mppt2_v)
    host.emit_metric("pv_mppt2_a",      mppt2_a)
    host.emit_metric("inverter_temp_c", heatsink_c)

    -- ---- Battery ----

    -- Battery voltage: 587, U16 (×0.01 LV, ×0.1 HV)
    local ok_bv, bv_regs = pcall(host.modbus_read, 587, 1, "holding")
    local bat_v = 0
    if ok_bv and bv_regs then
        bat_v = bv_regs[1] * (is_hv and 0.1 or 0.01)
    end

    -- Battery SoC: 588, U16 percent → 0..1 fraction
    local ok_bsoc, bsoc_regs = pcall(host.modbus_read, 588, 1, "holding")
    local bat_soc = 0
    if ok_bsoc and bsoc_regs then
        bat_soc = bsoc_regs[1] / 100
    end

    -- Battery power: 590, I16 (×1 LV, ×10 HV).  Deye native: positive = charging.
    local ok_bw, bw_regs = pcall(host.modbus_read, 590, 1, "holding")
    local bat_w = 0
    if ok_bw and bw_regs then
        local bat_scale = is_hv and 10 or 1
        bat_w = host.decode_i16(bw_regs[1]) * bat_scale
    end

    -- Battery current: 591, I16 × 0.01 A
    local ok_ba, ba_regs = pcall(host.modbus_read, 591, 1, "holding")
    local bat_a = 0
    if ok_ba and ba_regs then
        bat_a = host.decode_i16(ba_regs[1]) * 0.01
    end

    -- Battery temperature: 217, U16 offset-encoded (actual = (val-1000)/10)
    local ok_btemp, btemp_regs = pcall(host.modbus_read, 217, 1, "holding")
    local bat_temp = 0
    if ok_btemp and btemp_regs then
        bat_temp = (btemp_regs[1] - 1000) / 10
    end

    -- Battery charge energy: 516-517, U32 LE × 0.1 kWh
    local ok_bchg, bchg_regs = pcall(host.modbus_read, 516, 2, "holding")
    local bat_charge_wh = 0
    if ok_bchg and bchg_regs then
        bat_charge_wh = host.decode_u32_le(bchg_regs[1], bchg_regs[2]) * 0.1 * 1000
    end

    -- Battery discharge energy: 518-519, U32 LE × 0.1 kWh
    local ok_bdis, bdis_regs = pcall(host.modbus_read, 518, 2, "holding")
    local bat_discharge_wh = 0
    if ok_bdis and bdis_regs then
        bat_discharge_wh = host.decode_u32_le(bdis_regs[1], bdis_regs[2]) * 0.1 * 1000
    end

    -- Deye reports positive = charging already, which matches site convention.
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

    -- ---- Meter ----

    -- Per-phase voltage: 598-600, U16 × 0.1 V
    local ok_lv, lv_regs = pcall(host.modbus_read, 598, 3, "holding")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_lv and lv_regs then
        l1_v = lv_regs[1] * 0.1
        l2_v = lv_regs[2] * 0.1
        l3_v = lv_regs[3] * 0.1
    end

    -- Grid frequency: 609, U16 × 0.01 Hz
    local ok_hz, hz_regs = pcall(host.modbus_read, 609, 1, "holding")
    local hz = 0
    if ok_hz and hz_regs then
        hz = hz_regs[1] * 0.01
    end

    -- Per-phase current: 610-612, I16 × 0.01 A
    local ok_la, la_regs = pcall(host.modbus_read, 610, 3, "holding")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_la and la_regs then
        l1_a = host.decode_i16(la_regs[1]) * 0.01
        l2_a = host.decode_i16(la_regs[2]) * 0.01
        l3_a = host.decode_i16(la_regs[3]) * 0.01
    end

    -- Total meter power: 619, I16 W (positive = import, negative = export)
    local ok_tw, tw_regs = pcall(host.modbus_read, 619, 1, "holding")
    local meter_w = 0
    if ok_tw and tw_regs then
        meter_w = host.decode_i16(tw_regs[1])
    end

    -- Per-phase power: 622-624, I16 W
    local ok_lw, lw_regs = pcall(host.modbus_read, 622, 3, "holding")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_lw and lw_regs then
        l1_w = host.decode_i16(lw_regs[1])
        l2_w = host.decode_i16(lw_regs[2])
        l3_w = host.decode_i16(lw_regs[3])
    end

    -- Import energy: 522-523, U32 LE × 0.1 kWh
    local ok_imp, imp_regs = pcall(host.modbus_read, 522, 2, "holding")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = host.decode_u32_le(imp_regs[1], imp_regs[2]) * 0.1 * 1000
    end

    -- Export energy: 524-525, U32 LE × 0.1 kWh
    local ok_exp, exp_regs = pcall(host.modbus_read, 524, 2, "holding")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = host.decode_u32_le(exp_regs[1], exp_regs[2]) * 0.1 * 1000
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
-- Battery control
----------------------------------------------------------------------------
--
-- Register map (holding, all FC 0x06):
--   108 : Max battery charge current           (A)
--   109 : Max battery discharge current        (A)
--   128 : Charging current                     (A)
--   129 : Generator charge enable              (0/1)
--   130 : Utility (grid) charge enable         (0/1)
--   141 : Energy management mode
--           0 = Selling First (self-consume)
--           1 = Load First    (self-consume, battery tops up after load)
--           2 = Zero Export To CT
--           3 = External EMS / Forced
--   142 : Limit control
--           0 = enable discharge
--           1 = no CT clamps
--           2 = extraposition enable (forced-setpoint gate)
--   143 : Max sell power                       (W, scaled HV/LV) — also curtail setpoint
--   145 : PV sell enable                       (0/1)
--   146 : Advanced peak-shaving enable
--   148 : TOU timestamp 1
--   149 : TOU timestamp 2
--   154 : Battery output (discharge) power     (W, scaled HV/LV)
--   166 : Battery SoC target                   (percent, 20=min / 100=max)
--   172 : Charge enable                        (3 = grid + PV)
--   178 : Control-board special function       (vendor magic 11816)
--   587 : Battery voltage                      (HV × 0.1 V, LV × 0.01 V)
--
-- HV models encode 10 W per register unit on 143/154; LV models 1 W.
-- Charging is expressed as current on reg 108 (A = W / V).
-- Deye firmware tolerates ≥50 ms between consecutive holding writes.

local REG_MAX_CHARGE_A       = 108
local REG_MAX_DISCHARGE_A    = 109
local REG_CHARGE_CURRENT     = 128
local REG_GEN_CHARGE_ENABLE  = 129
local REG_GRID_CHARGE_ENABLE = 130
local REG_EMS_MODE           = 141
local REG_LIMIT_CONTROL      = 142
local REG_MAX_SELL_POWER     = 143
local REG_PV_SELL_ENABLE     = 145
local REG_PEAK_SHAVING       = 146
local REG_TOU_TS1            = 148
local REG_TOU_TS2            = 149
local REG_DISCHARGE_POWER    = 154
local REG_SOC_TARGET         = 166
local REG_CHARGE_ENABLE      = 172
local REG_SPECIAL_FUNC       = 178
local REG_BATTERY_VOLTAGE    = 587

local EMS_LOAD_FIRST  = 1  -- native self-consumption, battery tops up load
local EMS_EXTERNAL    = 3  -- forced setpoints via reg 108/154

-- SoC targets written to reg 166. Charge commands aim for soc_max,
-- discharge commands floor at soc_min. Defaults match Zap; overridable
-- via config.soc_max / config.soc_min.
local soc_max = 100
local soc_min = 20

local CHARGE_CURRENT_DEFAULT_A = 31  -- matches Zap init profile

local WRITE_DELAY_MS = 50

local function write_reg(addr, val)
    local err = host.modbus_write(addr, val)
    host.sleep(WRITE_DELAY_MS)
    if err ~= nil and err ~= "" then
        host.log("warn", string.format("Deye: write %d=%d failed: %s",
            addr, val, tostring(err)))
        return false
    end
    return true
end

local function write_sequence(writes)
    for i = 1, #writes do
        if not write_reg(writes[i][1], writes[i][2]) then return false end
    end
    return true
end

local function clamp_u16(v)
    if v < 0 then return 0 end
    if v > 65535 then return 65535 end
    return math.floor(v)
end

-- HV registers are in 10 W units; LV in 1 W. Returns the register value.
local function scale_power(watts)
    local abs_w = math.abs(watts)
    if is_hv then abs_w = abs_w / 10 end
    return clamp_u16(abs_w)
end

local function read_battery_voltage()
    local ok, regs = pcall(host.modbus_read, REG_BATTERY_VOLTAGE, 1, "holding")
    if not (ok and regs) then return nil end
    return regs[1] * (is_hv and 0.1 or 0.01)
end

-- Full init profile (13 writes). Reapplied whenever we re-enter forced
-- mode after a cleanup/default-mode revert so the inverter has a known
-- baseline for charge-current limits, peak-shaving and TOU boundaries.
local function initialize_control()
    local max_sell = scale_power(rated_power_w)
    local ok = write_sequence({
        { REG_SPECIAL_FUNC,       11816 },
        { REG_MAX_CHARGE_A,       CHARGE_CURRENT_DEFAULT_A },
        { REG_MAX_DISCHARGE_A,    CHARGE_CURRENT_DEFAULT_A },
        { REG_CHARGE_CURRENT,     grid_charge_current_a },
        { REG_GEN_CHARGE_ENABLE,  1 },
        { REG_GRID_CHARGE_ENABLE, 1 },
        { REG_EMS_MODE,           EMS_EXTERNAL },
        { REG_LIMIT_CONTROL,      2 },
        { REG_MAX_SELL_POWER,     max_sell },
        { REG_PV_SELL_ENABLE,     1 },
        { REG_PEAK_SHAVING,       255 },
        { REG_TOU_TS1,            0 },
        { REG_TOU_TS2,            2355 },
    })
    control_initialized = ok
    if ok then
        host.log("info", string.format(
            "Deye: control initialized (%s, rated=%dW, max_sell_reg=%d)",
            is_hv and "HV" or "LV", rated_power_w, max_sell))
    else
        host.log("error", "Deye: control init failed")
    end
    return ok
end

local function ensure_initialized()
    return control_initialized or initialize_control()
end

-- Site convention: power_w > 0 = charge, < 0 = discharge, 0 = stop both.
-- Charging path converts watts to amps using the live battery voltage —
-- the Deye charge register is current, not power.
local function set_battery_power(power_w)
    if not ensure_initialized() then return false end

    if power_w > 0 then
        local voltage = read_battery_voltage()
        if not voltage or voltage <= 0.1 then
            host.log("error", string.format(
                "Deye: charge rejected, invalid battery voltage %s",
                tostring(voltage)))
            return false
        end
        local current = math.floor(power_w / voltage + 0.5)
        if current < 1 then current = 1 end
        current = clamp_u16(current)

        host.log("debug", string.format("Deye: charge %dW @ %.2fV → %dA",
            power_w, voltage, current))

        return write_sequence({
            { REG_SOC_TARGET,    soc_max },
            { REG_MAX_CHARGE_A,  current },
            { REG_CHARGE_ENABLE, 3 },  -- 3 = grid + PV
        })
    end

    if power_w < 0 then
        local power_val = scale_power(power_w)
        host.log("debug", string.format("Deye: discharge %dW → reg=%d",
            math.abs(power_w), power_val))
        return write_sequence({
            { REG_DISCHARGE_POWER, power_val },
            { REG_SOC_TARGET,      soc_min },
            { REG_LIMIT_CONTROL,   0 },  -- enable discharge
        })
    end

    -- Zero setpoint: hold forced mode but drive both directions to 0 so
    -- the inverter idles without leaving external-EMS control.
    host.log("debug", "Deye: setpoint 0W (hold)")
    return write_sequence({
        { REG_MAX_CHARGE_A,    0 },
        { REG_DISCHARGE_POWER, 0 },
    })
end

local function enable_curtailment(power_w)
    if not ensure_initialized() then return false end
    if power_w < 0 then power_w = 0 end
    local limit = scale_power(power_w)
    host.log("debug", string.format("Deye: curtail → %dW (reg=%d)",
        power_w, limit))
    return write_sequence({
        { REG_PV_SELL_ENABLE, 1 },
        { REG_MAX_SELL_POWER, limit },
    })
end

local function disable_curtailment()
    if not ensure_initialized() then return false end
    local limit = scale_power(rated_power_w)
    host.log("debug", string.format("Deye: curtail disabled → %dW", rated_power_w))
    return write_sequence({
        { REG_PV_SELL_ENABLE, 1 },
        { REG_MAX_SELL_POWER, limit },
    })
end

-- Drop the inverter back to native self-consumption. Mirrors Zap's
-- applyDefaultSelfConsumptionMode(): Load First + PV sell + TOU on,
-- grid-charging off, CT-clamp extraposition left enabled.
local function set_self_consumption()
    local ok = write_sequence({
        { REG_LIMIT_CONTROL,      2 },
        { REG_EMS_MODE,           EMS_LOAD_FIRST },
        { REG_PV_SELL_ENABLE,     1 },
        { REG_PEAK_SHAVING,       1 },
        { REG_GRID_CHARGE_ENABLE, 0 },
    })
    if ok then control_initialized = false end
    return ok
end

function driver_command(action, power_w, cmd)
    if action == "init" then
        return initialize_control()
    elseif action == "battery" then
        return set_battery_power(power_w or 0)
    elseif action == "curtail" then
        return enable_curtailment(power_w or 0)
    elseif action == "curtail_disable" then
        return disable_curtailment()
    elseif action == "deinit" then
        return set_self_consumption()
    end
    host.log("debug", "Deye: unsupported action: " .. tostring(action))
    return false
end

-- Watchdog fallback: always revert to autonomous self-consumption so the
-- device doesn't get stuck in a forced mode when the EMS goes offline.
function driver_default_mode()
    host.log("info", "Deye: watchdog → reverting to self-consumption")
    set_self_consumption()
end

function driver_cleanup()
    pcall(set_self_consumption)
    is_hv = false
    sn_read = false
    control_initialized = false
    rated_power_w = 0
    grid_charge_current_a = 31
    soc_max = 100
    soc_min = 20
end
