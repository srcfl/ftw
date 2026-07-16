-- Sofar Solar HYD Series Hybrid Inverter Driver
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP, ALL HOLDING registers (FC 0x03), Big-Endian word order
--
-- Ported from sourceful-hugin/device-support/drivers/lua/sofar.lua
-- for FTW Lua host v2.1.
--
-- Register map derived from the wills106/homeassistant-solax-modbus
-- community project. Hex register addresses converted to decimal here.
--
-- Sign convention (site convention — positive W = INTO the site):
--   pv.w:       always negative (generation)
--   battery.w:  positive = charging, negative = discharging
--   meter.w:    positive = import from grid, negative = export
--
-- Sofar reports:
--   battery power (reg 1542) signed, positive = charge (matches site)
--   meter power   (reg 530)  signed, positive = import (matches site)
--   PV power      (reg 1414) unsigned magnitude (driver negates for site)
--
-- Read-only driver: no control surface implemented.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "sofar",
  name         = "Sofar hybrid inverter",
  manufacturer = "Sofar Solar",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Sofar Solar HYD-ES / HYD-EP hybrid inverters via Modbus TCP.",
  homepage     = "https://www.sofarsolar.com",
  authors      = { "FTW contributors" },
  tested_models = { "HYD-ES", "HYD-EP" },
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
    host.set_make("Sofar")
    host.log("info", "Sofar HYD: driver_init")
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- ---- PV ----

    -- PV1 V/A: 1412-1413 (0x0584-0x0585). U16 × 0.1 V, U16 × 0.01 A.
    local ok_pv1, pv1_regs = pcall(host.modbus_read, 1412, 2, "holding")
    local mppt1_v, mppt1_a = 0, 0
    if ok_pv1 and pv1_regs then
        mppt1_v = pv1_regs[1] * 0.1
        mppt1_a = pv1_regs[2] * 0.01
    end

    -- PV total power: 1414 (0x0586). U16, 10 W resolution.
    local ok_pvw, pvw_regs = pcall(host.modbus_read, 1414, 1, "holding")
    local pv_w = 0
    if ok_pvw and pvw_regs then
        pv_w = pvw_regs[1] * 10
    end

    -- PV2 V/A: 1415-1416 (0x0587-0x0588). U16 × 0.1 V, U16 × 0.01 A.
    local ok_pv2, pv2_regs = pcall(host.modbus_read, 1415, 2, "holding")
    local mppt2_v, mppt2_a = 0, 0
    if ok_pv2 and pv2_regs then
        mppt2_v = pv2_regs[1] * 0.1
        mppt2_a = pv2_regs[2] * 0.01
    end

    -- Grid frequency: 524 (0x020C). U16 × 0.01 Hz.
    local ok_hz, hz_regs = pcall(host.modbus_read, 524, 1, "holding")
    local hz = 0
    if ok_hz and hz_regs then
        hz = hz_regs[1] * 0.01
    end

    -- PV lifetime yield: 1668-1669 (0x0684-0x0685). U32 BE × 0.1 kWh.
    local ok_pvgen, pvgen_regs = pcall(host.modbus_read, 1668, 2, "holding")
    local pv_gen_wh = 0
    if ok_pvgen and pvgen_regs then
        pv_gen_wh = host.decode_u32_be(pvgen_regs[1], pvgen_regs[2]) * 0.1 * 1000
    end

    -- Emit PV telemetry (site convention: generation is negative).
    host.emit("pv", {
        w           = -pv_w,
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = pv_gen_wh,
    })
    host.emit_metric("pv_mppt1_v", mppt1_v)
    host.emit_metric("pv_mppt1_a", mppt1_a)
    host.emit_metric("pv_mppt2_v", mppt2_v)
    host.emit_metric("pv_mppt2_a", mppt2_a)
    host.emit_metric("grid_hz",    hz)

    -- ---- Battery ----

    -- Battery voltage: 1540 (0x0604). U16 × 0.1 V.
    local ok_bv, bv_regs = pcall(host.modbus_read, 1540, 1, "holding")
    local bat_v = 0
    if ok_bv and bv_regs then
        bat_v = bv_regs[1] * 0.1
    end

    -- Battery current: 1541 (0x0605). I16 × 0.01 A.
    local ok_ba, ba_regs = pcall(host.modbus_read, 1541, 1, "holding")
    local bat_a = 0
    if ok_ba and ba_regs then
        bat_a = host.decode_i16(ba_regs[1]) * 0.01
    end

    -- Battery power: 1542 (0x0606). I16, 10 W resolution.
    -- Sofar sign: positive = charge, negative = discharge (matches site convention).
    local ok_bw, bw_regs = pcall(host.modbus_read, 1542, 1, "holding")
    local bat_w = 0
    if ok_bw and bw_regs then
        bat_w = host.decode_i16(bw_regs[1]) * 10
    end

    -- Battery temperature: 1543 (0x0607). I16, °C.
    local ok_btemp, btemp_regs = pcall(host.modbus_read, 1543, 1, "holding")
    local bat_temp = 0
    if ok_btemp and btemp_regs then
        bat_temp = host.decode_i16(btemp_regs[1])
    end

    -- Battery SoC: 1544 (0x0608). U16 percent → 0..1 fraction.
    local ok_bsoc, bsoc_regs = pcall(host.modbus_read, 1544, 1, "holding")
    local bat_soc = 0
    if ok_bsoc and bsoc_regs then
        bat_soc = bsoc_regs[1] / 100
    end

    host.emit("battery", {
        w      = bat_w,
        v      = bat_v,
        a      = bat_a,
        soc    = bat_soc,
        temp_c = bat_temp,
    })
    host.emit_metric("battery_dc_v",   bat_v)
    host.emit_metric("battery_dc_a",   bat_a)
    host.emit_metric("battery_temp_c", bat_temp)

    -- ---- Meter ----

    -- Grid total power: 530 (0x0212). I16, 10 W resolution.
    -- Sofar sign: positive = import (matches site convention).
    local ok_mw, mw_regs = pcall(host.modbus_read, 530, 1, "holding")
    local meter_w = 0
    if ok_mw and mw_regs then
        meter_w = host.decode_i16(mw_regs[1]) * 10
    end

    -- Per-phase V/A interleaved: 518-523 (0x0206-0x020B).
    --   518 L1_v, 519 L1_a, 520 L2_v, 521 L2_a, 522 L3_v, 523 L3_a
    --   voltages U16 × 0.1 V, currents U16 × 0.01 A.
    local ok_phase, phase_regs = pcall(host.modbus_read, 518, 6, "holding")
    local l1_v, l1_a, l2_v, l2_a, l3_v, l3_a = 0, 0, 0, 0, 0, 0
    if ok_phase and phase_regs then
        l1_v = phase_regs[1] * 0.1
        l1_a = phase_regs[2] * 0.01
        l2_v = phase_regs[3] * 0.1
        l2_a = phase_regs[4] * 0.01
        l3_v = phase_regs[5] * 0.1
        l3_a = phase_regs[6] * 0.01
    end

    -- Import energy: 1672-1673 (0x0688-0x0689). U32 BE × 0.1 kWh.
    local ok_imp, imp_regs = pcall(host.modbus_read, 1672, 2, "holding")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = host.decode_u32_be(imp_regs[1], imp_regs[2]) * 0.1 * 1000
    end

    -- Export energy: 1674-1675 (0x068A-0x068B). U32 BE × 0.1 kWh.
    local ok_exp, exp_regs = pcall(host.modbus_read, 1674, 2, "holding")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = host.decode_u32_be(exp_regs[1], exp_regs[2]) * 0.1 * 1000
    end

    host.emit("meter", {
        w         = meter_w,
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
    host.emit_metric("meter_l1_v", l1_v)
    host.emit_metric("meter_l2_v", l2_v)
    host.emit_metric("meter_l3_v", l3_v)
    host.emit_metric("meter_l1_a", l1_a)
    host.emit_metric("meter_l2_a", l2_a)
    host.emit_metric("meter_l3_a", l3_a)

    return 5000
end

----------------------------------------------------------------------------
-- Control (not yet implemented — read-only driver)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    host.log("warn", "Sofar: control not implemented, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- No control surface → nothing to revert. The device already runs
    -- autonomously in its configured mode.
end

function driver_cleanup()
    -- nothing to clean up
end
