-- Sungrow SH Hybrid Inverter Driver
-- Emits: PV, Battery, Meter
-- Protocol: Modbus TCP (port 502, unit ID 1)
-- Reference: https://github.com/mkaiser/Sungrow-SHx-Inverter-Modbus-Home-Assistant

DRIVER = {
  id           = "sungrow-shx",
  name         = "Sungrow SH Hybrid Inverter",
  manufacturer = "Sungrow",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Sungrow SH-series hybrid inverters with LFP battery, via Modbus TCP.",
  homepage     = "https://en.sungrowpower.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "SH5.0RT", "SH6.0RT", "SH8.0RT", "SH10RT" },
  verification_status = "production",
  verified_by = { "frahlg@homelab-rpi:14d" },
  verified_at = "2026-04-18",
  verification_notes = "Battery control + telemetry in continuous use on homelab-rpi. Device type 0x0E0E (SH10RT).",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Register conventions:
--   Input registers (FC 0x04): read-only telemetry
--   Holding registers (FC 0x03/0x06): read/write configuration & control
--   Multi-register values: Little-Endian word order (low word first)
--
-- Battery sign convention (EMS unified):
--   positive W = charging (grid → battery)
--   negative W = discharging (battery → grid)
--
-- Control registers:
--   13049: EMS mode (0=self-consumption, 2=forced, 3=external EMS)
--   13050: Force cmd (0xAA=charge, 0xBB=discharge, 0xCC=stop)
--   13051: Force power (0-5000W)
--   33046: Max charge power (scale 0.01 kW, holding)
--   33047: Max discharge power (scale 0.01 kW, holding)
--   13057: Max SoC (scale 0.1%, holding)
--   13058: Min SoC (scale 0.1%, holding)

PROTOCOL = "modbus"

local sn_read = false
local init_verified = false

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Sungrow")

    -- Read and log device info
    local ok, dev = pcall(host.modbus_read, 4999, 1, "input")
    if ok and dev then
        host.log("info", "Device type code: " .. tostring(dev[1]))
    end

    -- Verify and configure power limits for battery control
    configure_power_limits()

    -- Read current EMS mode and log it
    local ok_ems, ems = pcall(host.modbus_read, 13049, 3, "holding")
    if ok_ems and ems then
        host.log("info", "EMS state: mode=" .. tostring(ems[1])
            .. " cmd=0x" .. string.format("%04x", ems[2])
            .. " power=" .. tostring(ems[3]) .. "W")
    end
end

-- Ensure power limits allow full charge/discharge (5kW each)
-- Some Sungrow units ship with discharge capped at 100W
function configure_power_limits()
    -- Max charge power: register 33046, scale 0.01 kW
    local ok_chg, chg = pcall(host.modbus_read, 33046, 1, "holding")
    if ok_chg and chg then
        local chg_kw = chg[1] * 0.01
        host.log("info", "Max charge power: " .. string.format("%.2f", chg_kw) .. " kW")
        if chg[1] < 500 then
            host.log("info", "Setting max charge power to 5 kW")
            local err = host.modbus_write(33046, 500)
            if err ~= nil and err ~= "" then
                host.log("warn", "Sungrow: max charge power write failed: " .. tostring(err))
            end
        end
    end

    -- Max discharge power: register 33047, scale 0.01 kW
    local ok_dis, dis = pcall(host.modbus_read, 33047, 1, "holding")
    if ok_dis and dis then
        local dis_kw = dis[1] * 0.01
        host.log("info", "Max discharge power: " .. string.format("%.2f", dis_kw) .. " kW")
        if dis[1] < 500 then
            host.log("info", "Setting max discharge power to 5 kW")
            local err = host.modbus_write(33047, 500)
            if err ~= nil and err ~= "" then
                host.log("warn", "Sungrow: max discharge power write failed: " .. tostring(err))
            end
        end
    end

    -- Read SoC limits
    local ok_soc, soc_lim = pcall(host.modbus_read, 13057, 2, "holding")
    if ok_soc and soc_lim then
        host.log("info", "SoC limits: max=" .. string.format("%.1f", soc_lim[1] * 0.1)
            .. "% min=" .. string.format("%.1f", soc_lim[2] * 0.1) .. "%")
    end

    init_verified = true
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Read serial number once
    if not sn_read then
        local ok, sn_regs = pcall(host.modbus_read, 4990, 10, "input")
        if ok and sn_regs then
            local sn = ""
            for i = 1, 10 do
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

    -- Running status: input 13000
    -- Bit 2 (0x0004) = discharging, Bit 1 (0x0002) = charging
    local ok_status, status_regs = pcall(host.modbus_read, 13000, 1, "input")
    local status = 0
    if ok_status and status_regs then status = status_regs[1] end
    local is_discharging = (math.floor(status / 4) % 2) == 1

    -- PV power: 5016-5017, U32 LE, watts
    -- Observed on at least one SH-series firmware: this register stays
    -- at 0 even while MPPT1/MPPT2 V×I clearly indicate generation. So we
    -- also read the MPPT voltage+current pairs separately and use their
    -- product as a fallback when the top-level register doesn't match.
    local ok_pv, pv_regs = pcall(host.modbus_read, 5016, 2, "input")
    local pv_w_primary = 0
    local pv_raw_u32 = 0
    if ok_pv and pv_regs then
        pv_w_primary = host.decode_u32_le(pv_regs[1], pv_regs[2])
        pv_raw_u32   = pv_w_primary
    end

    -- PV MPPT: 5010-5013 (V×0.1, A×0.1 per string)
    local ok_mppt, mppt_regs = pcall(host.modbus_read, 5010, 4, "input")
    local mppt1_v, mppt1_a, mppt2_v, mppt2_a = 0, 0, 0, 0
    if ok_mppt and mppt_regs then
        mppt1_v = mppt_regs[1] * 0.1
        mppt1_a = mppt_regs[2] * 0.1
        mppt2_v = mppt_regs[3] * 0.1
        mppt2_a = mppt_regs[4] * 0.1
    end
    local mppt1_w = mppt1_v * mppt1_a
    local mppt2_w = mppt2_v * mppt2_a
    local pv_w_mppt = mppt1_w + mppt2_w

    -- Resolve PV power with a fallback ladder:
    --   1. Trust 5016-5017 if it reports > 50 W (clearly live).
    --   2. Otherwise use MPPT sum if that reports > 50 W (primary
    --      register stuck / firmware quirk).
    --   3. Zero — panels aren't generating right now.
    -- 50 W threshold filters out noise-floor readings without swallowing
    -- genuine low-light output.
    local pv_w = 0
    local pv_source = "zero"
    if pv_w_primary > 50 then
        pv_w = pv_w_primary
        pv_source = "primary_reg"
    elseif pv_w_mppt > 50 then
        pv_w = pv_w_mppt
        pv_source = "mppt_sum"
    end

    -- PV lifetime energy: 13002-13003, U32 LE × 0.1 kWh
    local ok_pvgen, pvgen_regs = pcall(host.modbus_read, 13002, 2, "input")
    local pv_gen_wh = 0
    if ok_pvgen and pvgen_regs then
        pv_gen_wh = host.decode_u32_le(pvgen_regs[1], pvgen_regs[2]) * 0.1 * 1000
    end

    -- Rated power: 5000, U16 × 0.1 kW
    local ok_rated, rated_regs = pcall(host.modbus_read, 5000, 1, "input")
    local rated_w = 0
    if ok_rated and rated_regs then
        rated_w = rated_regs[1] * 0.1 * 1000
    end

    -- Heatsink temp: 5007, I16 × 0.1 C
    local ok_temp, temp_regs = pcall(host.modbus_read, 5007, 1, "input")
    local heatsink_c = 0
    if ok_temp and temp_regs then
        heatsink_c = host.decode_i16(temp_regs[1]) * 0.1
    end

    -- Grid frequency: 5241, U16 × 0.01 Hz
    local ok_hz, hz_regs = pcall(host.modbus_read, 5241, 1, "input")
    local hz = 0
    if ok_hz and hz_regs then
        hz = hz_regs[1] * 0.01
    end

    host.emit("pv", {
        w           = -pv_w,  -- negative = generation (EMS convention)
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        mppt1_w     = mppt1_w,
        mppt2_w     = mppt2_w,
        pv_source   = pv_source,  -- "primary_reg" | "mppt_sum" | "zero"
        lifetime_wh = pv_gen_wh,
        rated_w     = rated_w,
        temp_c      = heatsink_c,
    })
    -- Diagnostics: long-format TS DB. Surface BOTH the primary-register
    -- reading and the MPPT-derived fallback so operators can see when
    -- the two disagree (= firmware register quirk) directly in the
    -- metric browser.
    host.emit_metric("pv_w_primary",     pv_w_primary)
    host.emit_metric("pv_w_mppt_sum",    pv_w_mppt)
    host.emit_metric("pv_raw_u32",       pv_raw_u32)
    host.emit_metric("pv_mppt1_v",       mppt1_v)
    host.emit_metric("pv_mppt1_a",       mppt1_a)
    host.emit_metric("pv_mppt1_w",       mppt1_w)
    host.emit_metric("pv_mppt2_v",       mppt2_v)
    host.emit_metric("pv_mppt2_a",       mppt2_a)
    host.emit_metric("pv_mppt2_w",       mppt2_w)
    host.emit_metric("inverter_temp_c",  heatsink_c)
    host.emit_metric("grid_hz",          hz)

    -- Battery: 13019-13022 (voltage, current, power, SoC)
    local ok_bat, bat_regs = pcall(host.modbus_read, 13019, 4, "input")
    local bat_v, bat_a, bat_w, bat_soc = 0, 0, 0, 0
    if ok_bat and bat_regs then
        bat_v   = bat_regs[1] * 0.1
        bat_a   = bat_regs[2] * 0.1
        bat_w   = bat_regs[3]
        bat_soc = bat_regs[4] * 0.1 / 100  -- 0-1 fraction
    end

    -- Apply sign: Sungrow reports power as unsigned, direction from status register
    -- EMS convention: positive = charging, negative = discharging
    if is_discharging then
        bat_w = -bat_w
    end

    -- Battery charge energy: 13040-13041, U32 LE × 0.1 kWh
    local ok_bchg, bchg_regs = pcall(host.modbus_read, 13040, 2, "input")
    local bat_charge_wh = 0
    if ok_bchg and bchg_regs then
        bat_charge_wh = host.decode_u32_le(bchg_regs[1], bchg_regs[2]) * 0.1 * 1000
    end

    -- Battery discharge energy: 13026-13027, U32 LE × 0.1 kWh
    local ok_bdis, bdis_regs = pcall(host.modbus_read, 13026, 2, "input")
    local bat_discharge_wh = 0
    if ok_bdis and bdis_regs then
        bat_discharge_wh = host.decode_u32_le(bdis_regs[1], bdis_regs[2]) * 0.1 * 1000
    end

    host.emit("battery", {
        w            = bat_w,
        v            = bat_v,
        a            = bat_a,
        soc          = bat_soc,
        charge_wh    = bat_charge_wh,
        discharge_wh = bat_discharge_wh,
    })
    host.emit_metric("battery_dc_v", bat_v)
    host.emit_metric("battery_dc_a", bat_a)

    -- EMS state diagnostics — what the inverter *actually* has latched
    -- in its control registers right now. With the #164 write-order fix
    -- these should track whatever the dispatcher sent last tick; any
    -- drift between target and ems_force_w points at external writers
    -- (iSolarCloud, HA integration, another EMS) racing the driver.
    local ok_emsd, emsd = pcall(host.modbus_read, 13049, 3, "holding")
    if ok_emsd and emsd then
        host.emit_metric("sungrow_ems_mode",  emsd[1]) -- 0=self, 2=forced, 3=ext
        host.emit_metric("sungrow_force_cmd", emsd[2]) -- 0xAA=170 chg, 0xBB=187 dis, 0xCC=204 stop
        host.emit_metric("sungrow_force_w",   emsd[3])
    end

    -- Grid meter power: 5600-5601, I32 LE, watts (positive=import, negative=export)
    local ok_mw, mw_regs = pcall(host.modbus_read, 5600, 2, "input")
    local meter_w = 0
    if ok_mw and mw_regs then
        meter_w = host.decode_i32_le(mw_regs[1], mw_regs[2])
    end

    -- Per-phase power: 5602-5607, I32 LE pairs
    local ok_mp, mp_regs = pcall(host.modbus_read, 5602, 6, "input")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_mp and mp_regs then
        l1_w = host.decode_i32_le(mp_regs[1], mp_regs[2])
        l2_w = host.decode_i32_le(mp_regs[3], mp_regs[4])
        l3_w = host.decode_i32_le(mp_regs[5], mp_regs[6])
    end

    -- Per-phase voltage: 5740-5742, U16 × 0.1 V
    local ok_mv, mv_regs = pcall(host.modbus_read, 5740, 3, "input")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_mv and mv_regs then
        l1_v = mv_regs[1] * 0.1
        l2_v = mv_regs[2] * 0.1
        l3_v = mv_regs[3] * 0.1
    end

    -- Per-phase current: 5743-5745, U16 × 0.01 A
    local ok_ma, ma_regs = pcall(host.modbus_read, 5743, 3, "input")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_ma and ma_regs then
        l1_a = ma_regs[1] * 0.01
        l2_a = ma_regs[2] * 0.01
        l3_a = ma_regs[3] * 0.01
    end

    -- Import energy: 13036-13037, U32 LE × 0.1 kWh
    local ok_imp, imp_regs = pcall(host.modbus_read, 13036, 2, "input")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = host.decode_u32_le(imp_regs[1], imp_regs[2]) * 0.1 * 1000
    end

    -- Export energy: 13045-13046, U32 LE × 0.1 kWh
    local ok_exp, exp_regs = pcall(host.modbus_read, 13045, 2, "input")
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

    return 5000
end

----------------------------------------------------------------------------
-- Battery control
----------------------------------------------------------------------------

-- Battery control command handler
-- EMS convention: positive power_w = charge, negative = discharge
-- Verified: charge 200W and discharge 200W both tested and confirmed
function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        return set_battery_power(power_w)
    elseif action == "curtail" then
        return set_export_limit(math.abs(power_w))
    elseif action == "curtail_disable" or action == "deinit" then
        return set_self_consumption()
    end
    return false
end

-- Set battery to a specific charge/discharge power
-- power_w > 0: charge at power_w watts
-- power_w < 0: discharge at |power_w| watts
-- power_w = 0: forced idle at 0W. Default/watchdog mode is the only
-- path that returns the inverter to autonomous self-consumption.
-- Write order matters. SH-series ignores the force_cmd (13050) and
-- force_power (13051) registers while EMS mode (13049) is still 0
-- (self-consumption). Writing power + cmd FIRST and mode LAST means
-- the cmd/power writes land in self-consumption mode and get buffered-
-- and-discarded or implicitly reset by the mode transition; the
-- inverter ends up in mode=2 with no forced command and the battery
-- sits idle despite a non-zero target. Reference implementations
-- (mkaiser/Sungrow-SHx Home Assistant, openWB) write mode → cmd →
-- power for exactly this reason. Issue #164.
function set_battery_power(power_w)
    local want_cmd, watts
    if power_w > 0 then
        watts    = math.floor(math.min(power_w, 5000))
        want_cmd = 0xAA -- force charge
    elseif power_w < 0 then
        watts    = math.floor(math.min(math.abs(power_w), 5000))
        want_cmd = 0xBB -- force discharge
    else
        return set_battery_idle()
    end

    -- Order: mode first (so the inverter is ready to latch cmd/power),
    -- then cmd (which register is honoured), then power setpoint.
    host.modbus_write(13049, 2)         -- 1. forced mode
    host.modbus_write(13050, want_cmd)  -- 2. force charge/discharge cmd
    host.modbus_write(13051, watts)     -- 3. power setpoint
    host.log("debug", string.format("Sungrow: force %s %dW",
        want_cmd == 0xAA and "charge" or "discharge", watts))

    -- Verify all three latched. Previous version only checked mode and
    -- logged success on mode=2 even when cmd was still 0 → the exact
    -- class of silent failure #164 describes. Emit mismatches as WARN
    -- so operators see the drift in logs without spamming on ok cases.
    local ok, ems = pcall(host.modbus_read, 13049, 3, "holding")
    if not ok or not ems then
        return true -- transient read failure; assume writes are good
    end
    if ems[1] ~= 2 then
        host.log("warn", "Sungrow: EMS mode not latched (got " .. tostring(ems[1]) .. " want 2)")
        return false
    end
    if ems[2] ~= want_cmd then
        host.log("warn", string.format("Sungrow: force_cmd not latched (got 0x%02x want 0x%02x)",
            ems[2], want_cmd))
        return false
    end
    if math.abs(ems[3] - watts) > 1 then -- 1W rounding tolerance
        host.log("warn", string.format("Sungrow: force_power not latched (got %dW want %dW)",
            ems[3], watts))
        return false
    end
    return true
end

-- Hold the battery at 0W without handing control back to the inverter's
-- autonomous self-consumption mode. Otherwise a 0W planner target can still
-- let Sungrow charge from PV surplus on its own.
function set_battery_idle()
    host.modbus_write(13049, 2)     -- forced mode
    host.modbus_write(13050, 0xCC)  -- stop forced charge/discharge
    host.modbus_write(13051, 0)     -- zero power setpoint
    host.log("debug", "Sungrow: force idle 0W")

    local ok, ems = pcall(host.modbus_read, 13049, 3, "holding")
    if not ok or not ems then
        return true -- transient read failure; assume writes are good
    end
    if ems[1] ~= 2 then
        host.log("warn", "Sungrow: EMS idle mode not latched (got " .. tostring(ems[1]) .. " want 2)")
        return false
    end
    if math.abs(ems[3]) > 1 then -- 1W rounding tolerance
        host.log("warn", string.format("Sungrow: idle force_power not latched (got %dW want 0W)", ems[3]))
        return false
    end
    return true
end

-- Return to self-consumption mode (safe default)
function set_self_consumption()
    host.modbus_write(13050, 0xCC)  -- stop forced charge/discharge
    host.modbus_write(13049, 0)     -- self-consumption mode
    host.log("debug", "Sungrow: self-consumption mode")
    return true
end

-- Set export power limit
function set_export_limit(watts)
    host.modbus_write(13073, math.floor(watts))
    host.log("debug", "Sungrow: export limit " .. tostring(watts) .. "W")
    return true
end

-- Watchdog fallback: always revert to self-consumption
function driver_default_mode()
    host.log("info", "Sungrow: watchdog → reverting to self-consumption")
    set_self_consumption()
end

function driver_cleanup()
    set_self_consumption()
end
