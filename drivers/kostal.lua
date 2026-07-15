-- Kostal Plenticore / Piko IQ hybrid inverter driver (SunSpec)
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP, port 1502 (Kostal default), holding registers (FC 0x03)
-- Reference: Kostal SunSpec map. SunSpec is big-endian for u32 values.
-- READ-ONLY: no battery / curtail control.

DRIVER = {
  id           = "kostal",
  name         = "Kostal Plenticore",
  manufacturer = "Kostal",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Kostal Plenticore Plus and Piko IQ via Modbus TCP (SunSpec plus Kostal custom map).",
  homepage     = "https://www.kostal-solar-electric.com",
  authors      = { "FTW contributors" },
  tested_models = { "Plenticore Plus", "Piko IQ" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- SunSpec helpers (inline — the FTW host has no host.scale / host.decode_f32)
----------------------------------------------------------------------------

-- Apply a SunSpec signed scale factor: result = value * 10^sf.
-- sf is a small int16 (typical range -4..+4). Negative sf divides.
local function apply_sf(v, sf)
    if sf >= 0 then return v * (10 ^ sf) end
    return v / (10 ^ -sf)
end

-- Read a single holding register as signed i16 (e.g. scale factors).
-- Returns the decoded value, or the default if the read fails.
local function read_i16(addr, default)
    local ok, regs = pcall(host.modbus_read, addr, 1, "holding")
    if ok and regs and regs[1] then
        return host.decode_i16(regs[1])
    end
    return default
end

-- Read a single holding register as unsigned u16.
local function read_u16(addr, default)
    local ok, regs = pcall(host.modbus_read, addr, 1, "holding")
    if ok and regs and regs[1] then
        return regs[1]
    end
    return default
end

-- Read a u32 big-endian pair (SunSpec standard word order).
local function read_u32_be(addr, default)
    local ok, regs = pcall(host.modbus_read, addr, 2, "holding")
    if ok and regs and regs[1] and regs[2] then
        return host.decode_u32_be(regs[1], regs[2])
    end
    return default
end

----------------------------------------------------------------------------
-- Lifecycle
----------------------------------------------------------------------------

local sn_read = false

function driver_init(config)
    host.set_make("Kostal")
    host.log("info", "Kostal: initialised, SunSpec Modbus TCP (holding registers)")
end

function driver_poll()
    -- -----------------------------------------------------------------
    -- Serial number (SunSpec Common Model 1: SN at regs 40052-40067,
    -- 16 regs = 32 chars ASCII. NOTE: 40004-40019 is Md (manufacturer),
    -- 40020-40035 is Opt (model string) — NOT serial number.)
    -- -----------------------------------------------------------------
    if not sn_read then
        local ok_sn, sn_regs = pcall(host.modbus_read, 40052, 16, "holding")
        if ok_sn and sn_regs then
            local sn = ""
            for i = 1, 16 do
                local hi = math.floor(sn_regs[i] / 256)
                local lo = sn_regs[i] % 256
                if hi > 32 and hi < 127 then sn = sn .. string.char(hi) end
                if lo > 32 and lo < 127 then sn = sn .. string.char(lo) end
            end
            if string.len(sn) > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- -----------------------------------------------------------------
    -- Scale factors (SunSpec inverter model 103)
    -- -----------------------------------------------------------------
    local ac_power_sf = read_i16(40084, 0)  -- AC power SF
    local hz_sf       = read_i16(40086, 0)  -- Hz SF
    local energy_sf   = read_i16(40095, 0)  -- Lifetime energy SF
    local temp_sf     = read_i16(40106, 0)  -- Heatsink temp SF

    -- MPPT scale factors (SunSpec model 160, base 40253)
    local mppt_a_sf   = read_i16(40255, 0)  -- DCA_SF (+2 from base)
    local mppt_v_sf   = read_i16(40256, 0)  -- DCV_SF (+3 from base)

    -- -----------------------------------------------------------------
    -- PV (SunSpec inverter model 103)
    -- -----------------------------------------------------------------
    local ac_w_raw = read_i16(40083, 0)
    local ac_w     = apply_sf(ac_w_raw, ac_power_sf)

    local hz_raw = read_u16(40085, 0)
    local hz     = apply_sf(hz_raw, hz_sf)

    local lifetime_raw = read_u32_be(40093, 0)
    local lifetime_wh  = apply_sf(lifetime_raw, energy_sf)

    local temp_raw = read_i16(40103, 0)
    local temp_c   = apply_sf(temp_raw, temp_sf)

    -- MPPT1: module 1 current @ 40260, voltage @ 40261
    local mppt1_a = apply_sf(read_u16(40260, 0), mppt_a_sf)
    local mppt1_v = apply_sf(read_u16(40261, 0), mppt_v_sf)

    -- MPPT2: module 2 current @ 40280, voltage @ 40281
    local mppt2_a = apply_sf(read_u16(40280, 0), mppt_a_sf)
    local mppt2_v = apply_sf(read_u16(40281, 0), mppt_v_sf)

    -- PV emit: w is always negative (generation leaves the array)
    host.emit("pv", {
        w           = -ac_w,
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        lifetime_wh = lifetime_wh,
        temp_c      = temp_c,
    })
    host.emit_metric("pv_mppt1_v",      mppt1_v)
    host.emit_metric("pv_mppt1_a",      mppt1_a)
    host.emit_metric("pv_mppt2_v",      mppt2_v)
    host.emit_metric("pv_mppt2_a",      mppt2_a)
    host.emit_metric("inverter_temp_c", temp_c)
    host.emit_metric("grid_hz",         hz)

    -- -----------------------------------------------------------------
    -- Battery (SunSpec storage model 124 — Kostal custom offsets)
    -- -----------------------------------------------------------------
    -- SoC @ 40137, %; battery power @ 40138, i16 W (+ = charge, - = discharge)
    local bat_soc_raw = read_u16(40137, 0)
    local bat_soc     = bat_soc_raw / 100.0    -- percent → 0-1 fraction

    local bat_w = 0
    local ok_bw, bw_regs = pcall(host.modbus_read, 40138, 1, "holding")
    if ok_bw and bw_regs and bw_regs[1] then
        -- Kostal reports battery power with EMS-compatible sign already:
        -- positive = charging, negative = discharging.
        bat_w = host.decode_i16(bw_regs[1])
    end

    host.emit("battery", {
        w   = bat_w,
        soc = bat_soc,
    })

    -- -----------------------------------------------------------------
    -- Meter (SunSpec meter model 203 — Kostal custom offsets)
    -- -----------------------------------------------------------------
    local meter_w_sf      = read_i16(40210, 0)  -- total W SF (also phase W SF)
    local meter_a_sf      = read_i16(40194, 0)  -- per-phase A SF
    local meter_v_sf      = read_i16(40203, 0)  -- per-phase V SF
    local meter_energy_sf = read_i16(40242, 0)  -- meter energy SF

    -- Total grid power: 40100, I16 (Kostal reports meter W at inverter 40100)
    local meter_w_raw = read_i16(40100, 0)
    -- SunSpec convention at this map negates vs site convention (+ = export).
    -- Flip to site convention: + = import, - = export.
    local meter_w = -apply_sf(meter_w_raw, meter_w_sf)

    -- Per-phase current: 40191-40193, I16
    local l1_a, l2_a, l3_a = 0, 0, 0
    local ok_la, la_regs = pcall(host.modbus_read, 40191, 3, "holding")
    if ok_la and la_regs then
        l1_a = -apply_sf(host.decode_i16(la_regs[1]), meter_a_sf)
        l2_a = -apply_sf(host.decode_i16(la_regs[2]), meter_a_sf)
        l3_a = -apply_sf(host.decode_i16(la_regs[3]), meter_a_sf)
    end

    -- Per-phase voltage: 40196-40198, I16
    local l1_v, l2_v, l3_v = 0, 0, 0
    local ok_lv, lv_regs = pcall(host.modbus_read, 40196, 3, "holding")
    if ok_lv and lv_regs then
        l1_v = apply_sf(host.decode_i16(lv_regs[1]), meter_v_sf)
        l2_v = apply_sf(host.decode_i16(lv_regs[2]), meter_v_sf)
        l3_v = apply_sf(host.decode_i16(lv_regs[3]), meter_v_sf)
    end

    -- Per-phase power: 40207-40209, I16
    local l1_w, l2_w, l3_w = 0, 0, 0
    local ok_lw, lw_regs = pcall(host.modbus_read, 40207, 3, "holding")
    if ok_lw and lw_regs then
        l1_w = -apply_sf(host.decode_i16(lw_regs[1]), meter_w_sf)
        l2_w = -apply_sf(host.decode_i16(lw_regs[2]), meter_w_sf)
        l3_w = -apply_sf(host.decode_i16(lw_regs[3]), meter_w_sf)
    end

    -- Energy counters: 40226-40227 export, 40234-40235 import, U32 BE
    local export_wh = apply_sf(read_u32_be(40226, 0), meter_energy_sf)
    local import_wh = apply_sf(read_u32_be(40234, 0), meter_energy_sf)

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
-- Control (READ-ONLY for this driver — Kostal write map is tier-locked)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    -- Read-only driver: accept "init" (EMS handshake) and ignore the rest.
    if action == "init" then
        return true
    end
    return false
end

function driver_default_mode()
    -- Read-only driver — device stays in its own autonomous mode.
end

function driver_cleanup()
    -- Nothing to clean up.
end
