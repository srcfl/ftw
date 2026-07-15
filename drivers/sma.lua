-- SMA Hybrid Inverter Driver
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP (all registers are INPUT, SunSpec, Big-Endian)
-- Tested: SMA Sunny Tripower, Sunny Boy Storage
--
-- Ported from sourceful-hugin/device-support/drivers/lua/sma.lua to the
-- FTW v2.1 Lua host idiom. READ-ONLY driver — SMA control is
-- not wired up here.

DRIVER = {
  id           = "sma",
  name         = "SMA hybrid inverter",
  manufacturer = "SMA",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "SMA Sunny Tripower / Sunny Boy Storage via Modbus TCP (SunSpec).",
  homepage     = "https://www.sma.de",
  authors      = { "FTW contributors" },
  tested_models = { "Sunny Tripower", "Sunny Boy Storage" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Local helpers
----------------------------------------------------------------------------

-- Combine four big-endian uint16 registers into a non-negative 64-bit
-- counter. Lua 5.1 numbers are IEEE-754 doubles, so values up to 2^53
-- are exact — energy counters in Wh fit comfortably.
local function decode_u64_be(h1, h2, h3, h4)
    h1 = h1 % 65536
    h2 = h2 % 65536
    h3 = h3 % 65536
    h4 = h4 % 65536
    return ((h1 * 65536 + h2) * 65536 + h3) * 65536 + h4
end

-- SunSpec "not implemented" sentinel values. SMA registers that are not
-- populated return these magic numbers; clamping them to 0 keeps
-- telemetry sane.
local function u32_valid(v)
    -- 0xFFFFFFFF = U32 NaN
    return v ~= 4294967295
end
local function i32_valid(v)
    -- 0x80000000 (as signed -2147483648) = I32 NaN
    return v ~= -2147483648
end
-- 0xFFFFFFFFFFFFFFFF = U64 NaN. All four words 0xFFFF means sentinel.
local function u64_valid(h1, h2, h3, h4)
    return not (h1 == 65535 and h2 == 65535 and h3 == 65535 and h4 == 65535)
end

local sn_read = false

----------------------------------------------------------------------------
-- Lifecycle
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("SMA")

    -- SunSpec common model serial number: 30057-30058 (U32 BE).
    -- Best-effort; older SMA firmware can report 0 here — we fall back
    -- to make:endpoint identity in that case.
    local ok, sn_regs = pcall(host.modbus_read, 30057, 2, "input")
    if ok and sn_regs then
        local sn = host.decode_u32_be(sn_regs[1], sn_regs[2])
        if u32_valid(sn) and sn > 0 then
            host.set_sn(tostring(sn))
            sn_read = true
        end
    end
end

function driver_poll()
    -- Opportunistically retry SN if init couldn't read it (e.g. the
    -- inverter was still booting).
    if not sn_read then
        local ok, sn_regs = pcall(host.modbus_read, 30057, 2, "input")
        if ok and sn_regs then
            local sn = host.decode_u32_be(sn_regs[1], sn_regs[2])
            if u32_valid(sn) and sn > 0 then
                host.set_sn(tostring(sn))
                sn_read = true
            end
        end
    end

    ------------------------------------------------------------------ PV --

    -- PV power: 30775-30776, I32 BE, watts
    local ok_pvw, pvw_regs = pcall(host.modbus_read, 30775, 2, "input")
    local pv_w = 0
    if ok_pvw and pvw_regs then
        local v = host.decode_i32_be(pvw_regs[1], pvw_regs[2])
        if i32_valid(v) then pv_w = v end
    end

    -- MPPT1 current: 30769-30770, I32 BE × 0.001 A
    local ok_m1a, m1a_regs = pcall(host.modbus_read, 30769, 2, "input")
    local mppt1_a = 0
    if ok_m1a and m1a_regs then
        local v = host.decode_i32_be(m1a_regs[1], m1a_regs[2])
        if i32_valid(v) then mppt1_a = v * 0.001 end
    end

    -- MPPT1 voltage: 30771-30772, I32 BE × 0.01 V
    local ok_m1v, m1v_regs = pcall(host.modbus_read, 30771, 2, "input")
    local mppt1_v = 0
    if ok_m1v and m1v_regs then
        local v = host.decode_i32_be(m1v_regs[1], m1v_regs[2])
        if i32_valid(v) then mppt1_v = v * 0.01 end
    end

    -- MPPT2 current: 30957-30958, I32 BE × 0.001 A
    local ok_m2a, m2a_regs = pcall(host.modbus_read, 30957, 2, "input")
    local mppt2_a = 0
    if ok_m2a and m2a_regs then
        local v = host.decode_i32_be(m2a_regs[1], m2a_regs[2])
        if i32_valid(v) then mppt2_a = v * 0.001 end
    end

    -- MPPT2 voltage: 30959-30960, I32 BE × 0.01 V
    local ok_m2v, m2v_regs = pcall(host.modbus_read, 30959, 2, "input")
    local mppt2_v = 0
    if ok_m2v and m2v_regs then
        local v = host.decode_i32_be(m2v_regs[1], m2v_regs[2])
        if i32_valid(v) then mppt2_v = v * 0.01 end
    end

    -- PV lifetime generation: 30513-30516, U64 BE, Wh
    local ok_pvgen, pvgen_regs = pcall(host.modbus_read, 30513, 4, "input")
    local pv_gen_wh = 0
    if ok_pvgen and pvgen_regs and u64_valid(pvgen_regs[1], pvgen_regs[2], pvgen_regs[3], pvgen_regs[4]) then
        pv_gen_wh = decode_u64_be(pvgen_regs[1], pvgen_regs[2], pvgen_regs[3], pvgen_regs[4])
    end

    -- Inverter heatsink temp: 30953-30954, I32 BE × 0.1 C
    local ok_itemp, itemp_regs = pcall(host.modbus_read, 30953, 2, "input")
    local inv_temp = 0
    if ok_itemp and itemp_regs then
        local v = host.decode_i32_be(itemp_regs[1], itemp_regs[2])
        if i32_valid(v) then inv_temp = v * 0.1 end
    end

    -- Rated power: 31085-31086, U32 BE, watts
    local ok_rated, rated_regs = pcall(host.modbus_read, 31085, 2, "input")
    local rated_w = 0
    if ok_rated and rated_regs then
        local v = host.decode_u32_be(rated_regs[1], rated_regs[2])
        if u32_valid(v) then rated_w = v end
    end

    host.emit("pv", {
        w           = -pv_w,  -- site convention: generation is negative
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = pv_gen_wh,
        temp_c      = inv_temp,
        rated_w     = rated_w,
    })
    host.emit_metric("pv_mppt1_v",      mppt1_v)
    host.emit_metric("pv_mppt1_a",      mppt1_a)
    host.emit_metric("pv_mppt2_v",      mppt2_v)
    host.emit_metric("pv_mppt2_a",      mppt2_a)
    host.emit_metric("inverter_temp_c", inv_temp)

    ------------------------------------------------------------- Battery --

    -- Battery current: 30843-30844, I32 BE × 0.001 A (+ = charging)
    local ok_ba, ba_regs = pcall(host.modbus_read, 30843, 2, "input")
    local bat_a = 0
    if ok_ba and ba_regs then
        local v = host.decode_i32_be(ba_regs[1], ba_regs[2])
        if i32_valid(v) then bat_a = v * 0.001 end
    end

    -- Battery SoC: 30845-30846, U32 BE, percent → fraction
    local ok_bsoc, bsoc_regs = pcall(host.modbus_read, 30845, 2, "input")
    local bat_soc = 0
    if ok_bsoc and bsoc_regs then
        local v = host.decode_u32_be(bsoc_regs[1], bsoc_regs[2])
        if u32_valid(v) then bat_soc = v / 100 end
    end

    -- Battery temperature: 30849-30850, I32 BE × 0.1 C
    local ok_btemp, btemp_regs = pcall(host.modbus_read, 30849, 2, "input")
    local bat_temp = 0
    if ok_btemp and btemp_regs then
        local v = host.decode_i32_be(btemp_regs[1], btemp_regs[2])
        if i32_valid(v) then bat_temp = v * 0.1 end
    end

    -- Battery voltage: 30851-30852, U32 BE × 0.01 V
    local ok_bv, bv_regs = pcall(host.modbus_read, 30851, 2, "input")
    local bat_v = 0
    if ok_bv and bv_regs then
        local v = host.decode_u32_be(bv_regs[1], bv_regs[2])
        if u32_valid(v) then bat_v = v * 0.01 end
    end

    -- Battery power: V*A. Site convention matches native here because
    -- SMA reports signed current (+ = charging = into battery).
    local bat_w = bat_v * bat_a

    -- Battery charge energy: 31397-31400, U64 BE, Wh
    local ok_bchg, bchg_regs = pcall(host.modbus_read, 31397, 4, "input")
    local bat_charge_wh = 0
    if ok_bchg and bchg_regs and u64_valid(bchg_regs[1], bchg_regs[2], bchg_regs[3], bchg_regs[4]) then
        bat_charge_wh = decode_u64_be(bchg_regs[1], bchg_regs[2], bchg_regs[3], bchg_regs[4])
    end

    -- Battery discharge energy: 31401-31404, U64 BE, Wh
    local ok_bdis, bdis_regs = pcall(host.modbus_read, 31401, 4, "input")
    local bat_discharge_wh = 0
    if ok_bdis and bdis_regs and u64_valid(bdis_regs[1], bdis_regs[2], bdis_regs[3], bdis_regs[4]) then
        bat_discharge_wh = decode_u64_be(bdis_regs[1], bdis_regs[2], bdis_regs[3], bdis_regs[4])
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

    --------------------------------------------------------------- Meter --

    -- Meter total power: 30885-30886, I32 BE, watts (signed: + = import, - = export)
    local ok_mw, mw_regs = pcall(host.modbus_read, 30885, 2, "input")
    local meter_w = 0
    if ok_mw and mw_regs then
        local v = host.decode_i32_be(mw_regs[1], mw_regs[2])
        if i32_valid(v) then meter_w = v end
    end

    -- Per-phase meter power: 30887-30892, I32 BE pairs, watts (signed)
    local ok_mpw, mpw_regs = pcall(host.modbus_read, 30887, 6, "input")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_mpw and mpw_regs then
        local v1 = host.decode_i32_be(mpw_regs[1], mpw_regs[2])
        local v2 = host.decode_i32_be(mpw_regs[3], mpw_regs[4])
        local v3 = host.decode_i32_be(mpw_regs[5], mpw_regs[6])
        if i32_valid(v1) then l1_w = v1 end
        if i32_valid(v2) then l2_w = v2 end
        if i32_valid(v3) then l3_w = v3 end
    end

    -- Grid frequency: 30901-30902, U32 BE × 0.01 Hz
    local ok_hz, hz_regs = pcall(host.modbus_read, 30901, 2, "input")
    local hz = 0
    if ok_hz and hz_regs then
        local v = host.decode_u32_be(hz_regs[1], hz_regs[2])
        if u32_valid(v) then hz = v * 0.01 end
    end

    -- Per-phase voltage: 30903-30908, U32 BE × 0.01 V pairs
    local ok_lv, lv_regs = pcall(host.modbus_read, 30903, 6, "input")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_lv and lv_regs then
        local v1 = host.decode_u32_be(lv_regs[1], lv_regs[2])
        local v2 = host.decode_u32_be(lv_regs[3], lv_regs[4])
        local v3 = host.decode_u32_be(lv_regs[5], lv_regs[6])
        if u32_valid(v1) then l1_v = v1 * 0.01 end
        if u32_valid(v2) then l2_v = v2 * 0.01 end
        if u32_valid(v3) then l3_v = v3 * 0.01 end
    end

    -- Per-phase current: 30909-30914, U32 BE × 0.001 A pairs
    local ok_la, la_regs = pcall(host.modbus_read, 30909, 6, "input")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_la and la_regs then
        local v1 = host.decode_u32_be(la_regs[1], la_regs[2])
        local v2 = host.decode_u32_be(la_regs[3], la_regs[4])
        local v3 = host.decode_u32_be(la_regs[5], la_regs[6])
        if u32_valid(v1) then l1_a = v1 * 0.001 end
        if u32_valid(v2) then l2_a = v2 * 0.001 end
        if u32_valid(v3) then l3_a = v3 * 0.001 end
    end

    -- Import energy: 30581-30582, U32 BE, Wh
    local ok_imp, imp_regs = pcall(host.modbus_read, 30581, 2, "input")
    local import_wh = 0
    if ok_imp and imp_regs then
        local v = host.decode_u32_be(imp_regs[1], imp_regs[2])
        if u32_valid(v) then import_wh = v end
    end

    -- Export energy: 30583-30584, U32 BE, Wh
    local ok_exp, exp_regs = pcall(host.modbus_read, 30583, 2, "input")
    local export_wh = 0
    if ok_exp and exp_regs then
        local v = host.decode_u32_be(exp_regs[1], exp_regs[2])
        if u32_valid(v) then export_wh = v end
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
-- Control (read-only — SMA control is not implemented)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    end
    host.log("warn", "SMA: control not implemented (action=" .. tostring(action) .. ")")
    return false
end

function driver_default_mode()
    -- Read-only driver: nothing to revert.
end

function driver_cleanup()
    -- Read-only driver: nothing to clean up.
end
