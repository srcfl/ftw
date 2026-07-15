-- Huawei SUN2000 Hybrid Inverter Driver
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP, ALL HOLDING registers, Big-Endian word order
--
-- Ported from sourceful-hugin/device-support/drivers/lua/huawei.lua
-- for FTW Lua host v2.1.
--
-- Reference: Huawei SUN2000 Modbus Interface Definition
-- (inverter + LUNA2000 battery module via SDongle / embedded Modbus TCP).
--
-- Sign convention (site convention — positive W = INTO the site):
--   pv.w:       always negative (generation)
--   battery.w:  positive = charging, negative = discharging
--   meter.w:    positive = import from grid, negative = export
--
-- Huawei's meter register reports power with the OPPOSITE sign from our
-- convention (positive = export on Huawei), so meter power + per-phase
-- power + per-phase current are all negated at the boundary.

DRIVER = {
  id           = "huawei-sun2000",
  name         = "Huawei SUN2000 Hybrid Inverter",
  manufacturer = "Huawei",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Huawei SUN2000 hybrid inverters with LUNA2000 battery via Modbus TCP.",
  homepage     = "https://solar.huawei.com",
  authors      = { "FTW contributors" },
  tested_models = { "SUN2000L1", "SUN2000-LUNA2000" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

local sn_read = false

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- Write a U32 value split across two consecutive holding registers (BE).
-- Used for forcible charge/discharge power targets (watts).
local function write_u32(addr, val)
    val = math.floor(math.abs(val))
    local hi = math.floor(val / 65536)
    local lo = val % 65536
    host.modbus_write_multi(addr, { hi, lo })
end

-- Decode a 10-register ASCII string (20 bytes, big-endian packing).
-- Skips bytes outside printable ASCII.
local function decode_ascii(regs, n)
    local s = ""
    for i = 1, n do
        local hi = math.floor(regs[i] / 256)
        local lo = regs[i] % 256
        if hi > 32 and hi < 127 then s = s .. string.char(hi) end
        if lo > 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
end

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Huawei")
    host.log("info", "Huawei SUN2000: driver_init")
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Read serial number once (register 30015, 10 registers, ASCII).
    if not sn_read then
        local ok, sn_regs = pcall(host.modbus_read, 30015, 10, "holding")
        if ok and sn_regs then
            local sn = decode_ascii(sn_regs, 10)
            if string.len(sn) > 0 then
                host.set_sn(sn)
                sn_read = true
                host.log("info", "Huawei SUN2000: SN=" .. sn)
            end
        end
    end

    -- ---- PV ----

    -- PV1 V/A: 32016-32017, I16 × 0.1 V, I16 × 0.01 A
    local ok_pv1, pv1_regs = pcall(host.modbus_read, 32016, 2, "holding")
    local pv1_v, pv1_a = 0, 0
    if ok_pv1 and pv1_regs then
        pv1_v = host.decode_i16(pv1_regs[1]) * 0.1
        pv1_a = host.decode_i16(pv1_regs[2]) * 0.01
    end

    -- PV2 V/A: 32018-32019, I16 × 0.1 V, I16 × 0.01 A
    local ok_pv2, pv2_regs = pcall(host.modbus_read, 32018, 2, "holding")
    local pv2_v, pv2_a = 0, 0
    if ok_pv2 and pv2_regs then
        pv2_v = host.decode_i16(pv2_regs[1]) * 0.1
        pv2_a = host.decode_i16(pv2_regs[2]) * 0.01
    end

    -- Input power (PV total): 32064-32065, I32 BE × 0.001 kW → × 1000 for W
    local ok_pvw, pvw_regs = pcall(host.modbus_read, 32064, 2, "holding")
    local pv_w = 0
    if ok_pvw and pvw_regs then
        pv_w = host.decode_i32_be(pvw_regs[1], pvw_regs[2]) * 0.001 * 1000
    end

    -- Inverter temperature: 32087, I16 × 0.1 °C
    local ok_itemp, itemp_regs = pcall(host.modbus_read, 32087, 1, "holding")
    local inv_temp = 0
    if ok_itemp and itemp_regs then
        inv_temp = host.decode_i16(itemp_regs[1]) * 0.1
    end

    -- PV lifetime yield: 32106-32107, U32 BE × 0.01 kWh → × 1000 for Wh
    local ok_yield, yield_regs = pcall(host.modbus_read, 32106, 2, "holding")
    local pv_gen_wh = 0
    if ok_yield and yield_regs then
        pv_gen_wh = host.decode_u32_be(yield_regs[1], yield_regs[2]) * 0.01 * 1000
    end

    -- ---- Battery ----

    -- Battery power: 37001-37002, I32 BE, watts
    -- Huawei sign: positive = charging, negative = discharging (matches site convention).
    local ok_bw, bw_regs = pcall(host.modbus_read, 37001, 2, "holding")
    local bat_w = 0
    if ok_bw and bw_regs then
        bat_w = host.decode_i32_be(bw_regs[1], bw_regs[2])
    end

    -- Battery bus voltage: 37003, U16 × 0.1 V
    local ok_bv, bv_regs = pcall(host.modbus_read, 37003, 1, "holding")
    local bat_v = 0
    if ok_bv and bv_regs then
        bat_v = bv_regs[1] * 0.1
    end

    -- Battery SoC: 37004, U16 × 0.1 percent (convert to 0-1 fraction).
    local ok_bsoc, bsoc_regs = pcall(host.modbus_read, 37004, 1, "holding")
    local bat_soc = 0
    if ok_bsoc and bsoc_regs then
        bat_soc = bsoc_regs[1] * 0.1 / 100
    end

    -- Battery current: 37021, I16 × 0.1 A
    local ok_ba, ba_regs = pcall(host.modbus_read, 37021, 1, "holding")
    local bat_a = 0
    if ok_ba and ba_regs then
        bat_a = host.decode_i16(ba_regs[1]) * 0.1
    end

    -- Battery temperature: 37022, I16 × 0.1 °C
    local ok_btemp, btemp_regs = pcall(host.modbus_read, 37022, 1, "holding")
    local bat_temp = 0
    if ok_btemp and btemp_regs then
        bat_temp = host.decode_i16(btemp_regs[1]) * 0.1
    end

    -- Battery charge/discharge lifetime energy: 37066-37069, U32 BE × 0.01 kWh pairs.
    local ok_benergy, benergy_regs = pcall(host.modbus_read, 37066, 4, "holding")
    local bat_charge_wh, bat_discharge_wh = 0, 0
    if ok_benergy and benergy_regs then
        bat_charge_wh    = host.decode_u32_be(benergy_regs[1], benergy_regs[2]) * 0.01 * 1000
        bat_discharge_wh = host.decode_u32_be(benergy_regs[3], benergy_regs[4]) * 0.01 * 1000
    end

    -- ---- Meter ----

    -- Per-phase voltage: 37101-37106, I32 BE × 0.1 pairs (L1 V, L2 V, L3 V)
    local ok_lv, lv_regs = pcall(host.modbus_read, 37101, 6, "holding")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_lv and lv_regs then
        l1_v = host.decode_i32_be(lv_regs[1], lv_regs[2]) * 0.1
        l2_v = host.decode_i32_be(lv_regs[3], lv_regs[4]) * 0.1
        l3_v = host.decode_i32_be(lv_regs[5], lv_regs[6]) * 0.1
    end

    -- Per-phase current: 37107-37112, I32 BE × 0.01 pairs (L1 A, L2 A, L3 A)
    local ok_la, la_regs = pcall(host.modbus_read, 37107, 6, "holding")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_la and la_regs then
        l1_a = host.decode_i32_be(la_regs[1], la_regs[2]) * 0.01
        l2_a = host.decode_i32_be(la_regs[3], la_regs[4]) * 0.01
        l3_a = host.decode_i32_be(la_regs[5], la_regs[6]) * 0.01
    end

    -- Meter total power: 37113-37114, I32 BE, watts.
    -- Huawei sign: positive = export, negative = import. Negate for site convention.
    -- If this read fails, skip ALL emits so the watchdog catches staleness —
    -- emitting PV/battery while meter is down would keep the driver "healthy"
    -- but give the control loop a stale grid reading.
    local ok_mw, mw_regs = pcall(host.modbus_read, 37113, 2, "holding")
    if not ok_mw or not mw_regs then
        host.log("warn", "Huawei: meter power read failed, skipping all emits")
        return 5000
    end
    local meter_w = host.decode_i32_be(mw_regs[1], mw_regs[2])

    -- Grid frequency: 37118, I16 × 0.01 Hz
    local ok_hz, hz_regs = pcall(host.modbus_read, 37118, 1, "holding")
    local hz = 0
    if ok_hz and hz_regs then
        hz = host.decode_i16(hz_regs[1]) * 0.01
    end

    -- Export/Import lifetime energy: 37119-37122, I32 BE × 0.01 kWh pairs
    local ok_energy, energy_regs = pcall(host.modbus_read, 37119, 4, "holding")
    local export_wh, import_wh = 0, 0
    if ok_energy and energy_regs then
        export_wh = host.decode_i32_be(energy_regs[1], energy_regs[2]) * 0.01 * 1000
        import_wh = host.decode_i32_be(energy_regs[3], energy_regs[4]) * 0.01 * 1000
    end

    -- Per-phase power: 37132-37137, I32 BE pairs, watts (Huawei sign).
    local ok_lpw, lpw_regs = pcall(host.modbus_read, 37132, 6, "holding")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_lpw and lpw_regs then
        l1_w = host.decode_i32_be(lpw_regs[1], lpw_regs[2])
        l2_w = host.decode_i32_be(lpw_regs[3], lpw_regs[4])
        l3_w = host.decode_i32_be(lpw_regs[5], lpw_regs[6])
    end

    -- ---- All reads succeeded — emit everything ----

    -- PV telemetry (site convention: generation is negative).
    host.emit("pv", {
        w           = -pv_w,
        mppt1_v     = pv1_v,
        mppt1_a     = pv1_a,
        mppt2_v     = pv2_v,
        mppt2_a     = pv2_a,
        lifetime_wh = pv_gen_wh,
        temp_c      = inv_temp,
    })
    host.emit_metric("pv_mppt1_v",      pv1_v)
    host.emit_metric("pv_mppt1_a",      pv1_a)
    host.emit_metric("pv_mppt2_v",      pv2_v)
    host.emit_metric("pv_mppt2_a",      pv2_a)
    host.emit_metric("inverter_temp_c", inv_temp)

    -- Battery telemetry.
    host.emit("battery", {
        w            = bat_w,
        v            = bat_v,
        a            = bat_a,
        soc          = bat_soc,
        temp_c       = bat_temp,
        charge_wh    = bat_charge_wh,
        discharge_wh = bat_discharge_wh,
    })
    host.emit_metric("battery_dc_v",    bat_v)
    host.emit_metric("battery_dc_a",    bat_a)
    host.emit_metric("battery_temp_c",  bat_temp)

    -- Negate meter power + per-phase power + per-phase current to site convention
    -- (positive = import from grid).
    host.emit("meter", {
        w         = -meter_w,
        l1_w      = -l1_w,
        l2_w      = -l2_w,
        l3_w      = -l3_w,
        l1_v      = l1_v,
        l2_v      = l2_v,
        l3_v      = l3_v,
        l1_a      = -l1_a,
        l2_a      = -l2_a,
        l3_a      = -l3_a,
        hz        = hz,
        import_wh = import_wh,
        export_wh = export_wh,
    })
    host.emit_metric("meter_l1_w", -l1_w)
    host.emit_metric("meter_l2_w", -l2_w)
    host.emit_metric("meter_l3_w", -l3_w)
    host.emit_metric("meter_l1_v", l1_v)
    host.emit_metric("meter_l2_v", l2_v)
    host.emit_metric("meter_l3_v", l3_v)
    host.emit_metric("meter_l1_a", -l1_a)
    host.emit_metric("meter_l2_a", -l2_a)
    host.emit_metric("meter_l3_a", -l3_a)
    host.emit_metric("grid_hz",    hz)

    return 5000
end

----------------------------------------------------------------------------
-- Battery control
----------------------------------------------------------------------------

-- Battery control command handler.
-- Site convention: positive power_w = charge, negative = discharge, 0 = idle.
-- Huawei forcible-mode registers (all holding):
--   47086: working mode (2 = maximise self-consumption)
--   47100: forcible charge/discharge command (0=stop, 1=charge, 2=discharge)
--   47083: forcible charge/discharge period (minutes; must be re-sent each command)
--   47246: forcible-mode kind (0 = duration-based)
--   47247-47248: forcible charge power (U32, watts)
--   47249-47250: forcible discharge power (U32, watts)
function driver_command(action, power_w, cmd)
    if action == "init" then
        -- Duration-based forcible mode, 24 h cap so we don't need to re-arm often.
        host.modbus_write(47246, 0)
        host.modbus_write(47083, 1440)
        return true
    elseif action == "battery" then
        return set_battery_power(power_w)
    elseif action == "curtail" then
        -- Force charge to absorb excess PV.
        write_u32(47247, math.abs(power_w))
        host.modbus_write(47083, 1440)
        host.modbus_write(47100, 1)
        host.log("debug", "Huawei: curtail (force charge) " .. tostring(math.abs(power_w)) .. "W")
        return true
    elseif action == "curtail_disable" then
        host.modbus_write(47100, 0)
        host.log("debug", "Huawei: curtail_disable")
        return true
    elseif action == "deinit" then
        return set_self_consumption()
    end
    return false
end

-- Drive the battery to a specific setpoint.
-- power_w > 0: forcible charge at power_w W
-- power_w < 0: forcible discharge at |power_w| W
-- power_w = 0: stop forcible mode (device resumes self-consumption).
function set_battery_power(power_w)
    if power_w > 0 then
        write_u32(47247, power_w)
        host.modbus_write(47083, 1440)
        host.modbus_write(47100, 1)
        host.log("debug", "Huawei: force charge " .. tostring(power_w) .. "W")
    elseif power_w < 0 then
        write_u32(47249, math.abs(power_w))
        host.modbus_write(47083, 1440)
        host.modbus_write(47100, 2)
        host.log("debug", "Huawei: force discharge " .. tostring(math.abs(power_w)) .. "W")
    else
        host.modbus_write(47100, 0)
        host.log("debug", "Huawei: stop forcible mode")
    end
    return true
end

-- Revert to autonomous self-consumption (safe default).
function set_self_consumption()
    host.modbus_write(47100, 0)  -- stop forcible mode
    host.modbus_write(47086, 2)  -- maximise self-consumption
    host.log("debug", "Huawei: self-consumption mode")
    return true
end

-- Watchdog fallback: always revert to autonomous self-consumption.
function driver_default_mode()
    host.log("info", "Huawei: watchdog -> reverting to self-consumption")
    set_self_consumption()
end

function driver_cleanup()
    set_self_consumption()
    sn_read = false
end
