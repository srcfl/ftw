-- GoodWe ET-Plus / EH Hybrid Inverter Driver
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP (port 502)
--
-- Register type: HOLDING (FC 0x03)
-- Byte/word order: Big-Endian for multi-register values
-- Tested: ET-Plus, EH series (GoodWe v2 LAN+WiFi dongle required)
-- READ-ONLY: control not implemented.

DRIVER = {
  id           = "goodwe",
  name         = "GoodWe hybrid inverter",
  manufacturer = "GoodWe",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "GoodWe ET-Plus / EH series hybrid inverters via Modbus TCP.",
  homepage     = "https://en.goodwe.com",
  authors      = { "FTW contributors" },
  tested_models = { "ET-Plus", "EH series" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("GoodWe")
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    ------------------------------------------------------------------------
    -- PV
    ------------------------------------------------------------------------

    -- PV total power: 35105-35106, U32 BE × 0.1 W
    local ok_pvw, pvw_regs = pcall(host.modbus_read, 35105, 2, "holding")
    local pv_w = 0
    if ok_pvw and pvw_regs then
        pv_w = host.decode_u32_be(pvw_regs[1], pvw_regs[2]) * 0.1
    end

    -- PV1: voltage @ 35103 (U16 × 0.1 V), current @ 35104 (U16 × 0.1 A)
    local ok_m1, m1_regs = pcall(host.modbus_read, 35103, 2, "holding")
    local mppt1_v, mppt1_a = 0, 0
    if ok_m1 and m1_regs then
        mppt1_v = m1_regs[1] * 0.1
        mppt1_a = m1_regs[2] * 0.1
    end

    -- PV2: voltage @ 35109 (U16 × 0.1 V), current @ 35110 (U16 × 0.1 A)
    local ok_m2, m2_regs = pcall(host.modbus_read, 35109, 2, "holding")
    local mppt2_v, mppt2_a = 0, 0
    if ok_m2 and m2_regs then
        mppt2_v = m2_regs[1] * 0.1
        mppt2_a = m2_regs[2] * 0.1
    end

    -- Grid frequency: 35113, U16 × 0.01 Hz
    local ok_hz, hz_regs = pcall(host.modbus_read, 35113, 1, "holding")
    local hz = 0
    if ok_hz and hz_regs then
        hz = hz_regs[1] * 0.01
    end

    -- PV lifetime generation: 35191-35192, U32 BE × 0.1 kWh
    local ok_pvgen, pvgen_regs = pcall(host.modbus_read, 35191, 2, "holding")
    local pv_gen_wh = 0
    if ok_pvgen and pvgen_regs then
        pv_gen_wh = host.decode_u32_be(pvgen_regs[1], pvgen_regs[2]) * 0.1 * 1000
    end

    host.emit("pv", {
        w           = -pv_w,  -- negative = generation (site convention)
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = pv_gen_wh,
    })
    -- Diagnostics: long-format TS DB
    host.emit_metric("pv_mppt1_v", mppt1_v)
    host.emit_metric("pv_mppt1_a", mppt1_a)
    host.emit_metric("pv_mppt2_v", mppt2_v)
    host.emit_metric("pv_mppt2_a", mppt2_a)
    host.emit_metric("grid_hz",    hz)

    ------------------------------------------------------------------------
    -- Battery
    ------------------------------------------------------------------------

    -- Battery voltage: 35178, U16 × 0.1 V
    local ok_bv, bv_regs = pcall(host.modbus_read, 35178, 1, "holding")
    local bat_v = 0
    if ok_bv and bv_regs then
        bat_v = bv_regs[1] * 0.1
    end

    -- Battery current: 35179, I16 × 0.1 A
    local ok_ba, ba_regs = pcall(host.modbus_read, 35179, 1, "holding")
    local bat_a = 0
    if ok_ba and ba_regs then
        bat_a = host.decode_i16(ba_regs[1]) * 0.1
    end

    -- Battery power: 35180, I16, W (positive=charge, negative=discharge — matches site convention)
    local ok_bw, bw_regs = pcall(host.modbus_read, 35180, 1, "holding")
    local bat_w = 0
    if ok_bw and bw_regs then
        bat_w = host.decode_i16(bw_regs[1])
    end

    -- Battery SoC: 35182, U16, percent
    local ok_bsoc, bsoc_regs = pcall(host.modbus_read, 35182, 1, "holding")
    local bat_soc = 0
    if ok_bsoc and bsoc_regs then
        bat_soc = bsoc_regs[1] / 100  -- percent → 0-1 fraction
    end

    -- Battery temperature: 35183, I16 × 0.1 C
    local ok_btemp, btemp_regs = pcall(host.modbus_read, 35183, 1, "holding")
    local bat_temp = 0
    if ok_btemp and btemp_regs then
        bat_temp = host.decode_i16(btemp_regs[1]) * 0.1
    end

    host.emit("battery", {
        w      = bat_w,
        v      = bat_v,
        a      = bat_a,
        soc    = bat_soc,
        temp_c = bat_temp,
    })
    host.emit_metric("battery_dc_v",  bat_v)
    host.emit_metric("battery_dc_a",  bat_a)
    host.emit_metric("battery_temp_c", bat_temp)

    ------------------------------------------------------------------------
    -- Meter (grid connection point)
    ------------------------------------------------------------------------

    -- GoodWe reports grid meter power with the OPPOSITE sign from site
    -- convention (positive = export). Negate at the boundary so everything
    -- above the driver layer sees positive = import.

    -- Total: 35140-35141, I32 BE, W
    local ok_mw, mw_regs = pcall(host.modbus_read, 35140, 2, "holding")
    local meter_w = 0
    if ok_mw and mw_regs then
        meter_w = -host.decode_i32_be(mw_regs[1], mw_regs[2])
    end

    -- Per-phase V/A interleaved at 35121-35126: L1_V, L1_A, L2_V, L2_A, L3_V, L3_A
    -- (U16, voltage × 0.1 V, current × 0.1 A). One read, six registers.
    local ok_va, va_regs = pcall(host.modbus_read, 35121, 6, "holding")
    local l1_v, l1_a, l2_v, l2_a, l3_v, l3_a = 0, 0, 0, 0, 0, 0
    if ok_va and va_regs then
        l1_v = va_regs[1] * 0.1
        l1_a = va_regs[2] * 0.1
        l2_v = va_regs[3] * 0.1
        l2_a = va_regs[4] * 0.1
        l3_v = va_regs[5] * 0.1
        l3_a = va_regs[6] * 0.1
    end

    -- Per-phase power at 35132-35137: L1, L2, L3 (I32 BE each pair). One read.
    local ok_lw, lw_regs = pcall(host.modbus_read, 35132, 6, "holding")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_lw and lw_regs then
        l1_w = -host.decode_i32_be(lw_regs[1], lw_regs[2])
        l2_w = -host.decode_i32_be(lw_regs[3], lw_regs[4])
        l3_w = -host.decode_i32_be(lw_regs[5], lw_regs[6])
    end

    -- Total import energy: 35195-35196, U32 BE × 0.1 kWh
    local ok_imp, imp_regs = pcall(host.modbus_read, 35195, 2, "holding")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = host.decode_u32_be(imp_regs[1], imp_regs[2]) * 0.1 * 1000
    end

    -- Total export energy: 35199-35200, U32 BE × 0.1 kWh
    local ok_exp, exp_regs = pcall(host.modbus_read, 35199, 2, "holding")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = host.decode_u32_be(exp_regs[1], exp_regs[2]) * 0.1 * 1000
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

    return 5000
end

----------------------------------------------------------------------------
-- Control (read-only driver — not implemented)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    end
    host.log("warn", "GoodWe: control not implemented (action=" .. tostring(action) .. ")")
    return false
end

function driver_default_mode()
    -- Read-only driver: nothing to revert.
end

function driver_cleanup()
    -- Read-only driver: nothing to clean up.
end
