-- solaredge.lua
-- SolarEdge inverter + meter driver (SunSpec over Modbus TCP).
-- Emits: PV, Meter. READ-ONLY. Tested on HD-Wave and StorEdge.
--
-- Ported from sourceful-hugin/device-support/drivers/lua/solaredge.lua.
-- Differences vs hugin source:
--   * Uses 42W v2.1 host idiom (host.log(level,msg), decode_u32_be,
--     host.emit_metric).
--   * SunSpec scale factor + pow10 applied inline in Lua (host.scale is
--     not available in v2.1).
--   * Adds SunSpec common-block SN read (register 40052, 16 regs ASCII)
--     so device identity resolves to make:serial.
--   * Diagnostics (MPPT, heatsink, grid Hz, per-phase) routed through
--     host.emit_metric into the long-format TS DB.

DRIVER = {
  id           = "solaredge",
  name         = "SolarEdge inverter + meter",
  manufacturer = "SolarEdge",
  version      = "1.1.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "pv-curtail" },
  description  = "SolarEdge HD-Wave / StorEdge via Modbus TCP (SunSpec) with PV active-power-limit curtail.",
  homepage     = "https://www.solaredge.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "HD-Wave", "StorEdge" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Curtail path (F000/F001 registers) not yet verified against live hardware on a 42W site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- PV curtail (action="curtail" / "curtail_disable")
--
-- Uses SolarEdge's proprietary "Advanced Power Control" registers
-- (Application Note: Power Reduction Interface):
--   0xF000 (61440)  Advanced Power Control enable: 0 = off, 1 = on
--   0xF001 (61441)  Active Power Limit:           u16, percent 0..100
-- Writes go to FC 0x06 (single-register holding write) via
-- host.modbus_write. SetApp setting "Limit Control Mode = Export
-- Control / Production" must be enabled on the inverter — without it,
-- writes succeed but the inverter ignores the limit.
--
-- power_w → percent conversion uses `nominal_w` from the YAML driver
-- config block (the inverter's rated active power output in W). If
-- nominal_w isn't set, curtail logs a warning and returns false so the
-- operator notices the missing config rather than silently producing
-- bogus limits.
--
-- Failsafe: F000/F001 do NOT auto-revert on SolarEdge. The driver
-- writes F000=0 on `curtail_disable`, `deinit`, and `driver_cleanup`.
-- If the daemon dies unexpectedly while curtailed, the inverter stays
-- capped until SetApp manually clears it.
--
-- SunSpec register map (FC 0x04 / "input" on SolarEdge; they intentionally
-- mirror the SunSpec common + inverter + meter blocks there):
--
--   Common block (device identity):
--     40052-40067  SN (16 regs, ASCII, null-padded)
--
--   Inverter model (101/102/103):
--     40083        AC power (I16)        * 10^ac_power_sf
--     40084        AC power SF (I16)
--     40085        Frequency (U16)       * 10^hz_sf
--     40086        Frequency SF (I16)
--     40093-40094  Lifetime Wh (U32 BE)  * 10^energy_sf
--     40095        Energy SF (I16 via i16 decode; datasheet calls it U16)
--     40103        Heat-sink °C (I16)    * 10^temp_sf
--     40106        Temp SF (I16)
--     40123        MPPT current SF (I16)
--     40124        MPPT voltage SF (I16)
--     40140-40141  MPPT1 A/V (U16 each)
--     40160-40161  MPPT2 A/V (U16 each)
--
--   Meter model (203 — 3-phase wye):
--     40100        Total W (I16)         * 10^meter_w_sf
--     40101        Meter W SF (I16)
--     40191-40193  Per-phase A (I16)     * 10^meter_a_sf
--     40194        Meter A SF (I16)
--     40196-40198  Per-phase V (I16)     * 10^meter_v_sf
--     40203        Meter V SF (I16)
--     40207-40209  Per-phase W (I16)     * 10^phase_w_sf
--     40210        Phase W SF (I16)
--     40226-40227  Export Wh (U32 BE)    * 10^meter_energy_sf
--     40234-40235  Import Wh (U32 BE)    * 10^meter_energy_sf
--     40242        Meter energy SF (I16)
--
-- Sign translation to site convention (positive = into site):
--   AC power out of the inverter = generation → PV w = -ac_w.
--   SolarEdge meter reports with utility-meter convention inverted
--   (+ = export in their datasheet), so we negate W and A to match
--   site convention (+ = import).

PROTOCOL = "modbus"

-- Cached per-device metadata. Scale factors are factory-set constants
-- (SunSpec guarantees they never change during a session), so we read
-- them once and cache. That cuts 11 Modbus round trips per poll.
-- However, if the first read attempt fails (returns zeros), we retry on
-- subsequent polls until all SFs are non-zero or we exhaust retries.
local sn_read = false
local sf_cache = nil
local sf_retries = 0
local SF_MAX_RETRIES = 5

-- Curtail state. nominal_w comes from the YAML driver config block.
-- curtail_active tracks whether F000 is currently set to 1, so we
-- don't redundantly re-enable on every refresh tick.
local nominal_w = 0
local curtail_active = false

-- SolarEdge "Advanced Power Control" register addresses (proprietary).
local REG_APC_ENABLE = 61440  -- 0xF000  u16  0 = disabled, 1 = enabled
local REG_APC_LIMIT  = 61441  -- 0xF001  u16  percent 0..100

----------------------------------------------------------------------------
-- SunSpec helpers
----------------------------------------------------------------------------

-- Raw integer power of ten — avoids math.pow (Lua 5.1 still has it, but
-- 5.3+ removed it and we prefer portable code). Scale factors are small
-- integers, typically -3..+3, so a fixed table is fastest and clearest.
local POW10 = {
    [-6] = 1e-6, [-5] = 1e-5, [-4] = 1e-4, [-3] = 1e-3,
    [-2] = 1e-2, [-1] = 1e-1, [0] = 1,
    [1] = 10, [2] = 100, [3] = 1000, [4] = 10000, [5] = 100000, [6] = 1e6,
}

-- SunSpec scale factors use 0x8000 (= -32768 after i16 decode) as a
-- "not implemented" sentinel. Treat that, and any out-of-range sf, as 0
-- (i.e. don't scale) — better to report a raw register than NaN out.
local function pow10(sf)
    if sf == -32768 then return 1 end
    local p = POW10[sf]
    if p ~= nil then return p end
    return 1
end

-- Apply a SunSpec scale factor inline:  value * 10^sf.
-- value may be any lua number (already decoded i16/u16/u32/i32).
local function scale(value, sf)
    return value * pow10(sf)
end

-- Read a single register and return it as an i16 (signed) scale factor.
-- Returns 0 on read failure so downstream scaling becomes a no-op (the
-- caller will get raw register values until the next retry).
local function read_sf(addr)
    local ok, regs = pcall(host.modbus_read, addr, 1, "input")
    if ok and regs then
        return host.decode_i16(regs[1])
    end
    return 0
end

-- Populate sf_cache with every SunSpec scale factor we need. Returns the
-- table whether or not every read succeeded — a failed read just leaves
-- that SF at 0 until we retry (see load_scale_factors call site).
local function load_scale_factors()
    return {
        ac_power     = read_sf(40084),
        hz           = read_sf(40086),
        energy       = read_sf(40095),
        temp         = read_sf(40106),
        mppt_a       = read_sf(40123),
        mppt_v       = read_sf(40124),
        meter_w      = read_sf(40101),
        meter_a      = read_sf(40194),
        meter_v      = read_sf(40203),
        phase_w      = read_sf(40210),
        meter_energy = read_sf(40242),
    }
end

-- Decode a null-/space-padded ASCII string from a run of registers
-- (SunSpec common-block strings — high byte first inside each reg).
local function decode_ascii(regs, n)
    local s = ""
    for i = 1, n do
        local r = regs[i] or 0
        local hi = math.floor(r / 256)
        local lo = r % 256
        if hi > 32 and hi < 127 then s = s .. string.char(hi) end
        if lo > 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
end

----------------------------------------------------------------------------
-- Lifecycle
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("SolarEdge")
    if type(config) == "table" then
        local n = tonumber(config.nominal_w)
        if n and n > 0 then
            nominal_w = n
            host.log("info", "SolarEdge: nominal_w = " .. tostring(nominal_w) .. " W (curtail enabled)")
        end
    end
    if nominal_w <= 0 then
        host.log("info", "SolarEdge: nominal_w not set in config — curtail action will be unavailable")
    end
end

-- NOTE: no driver_fingerprint here. This driver reads telemetry over INPUT
-- registers (FC 0x04), but probing input to identify a device is unsafe: a
-- SolarEdge reached through solaredge-proxy answers SunSpec on HOLDING only,
-- and a timed-out input read wedges that proxy's single upstream socket for
-- seconds (verified on an SE8K). The holding-based solaredge_legacy.lua owns
-- SolarEdge fingerprinting for the scan flow — its holding map works on direct
-- units too. Operators who specifically need this input-register variant can
-- still pick it by hand in the wizard.

function driver_poll()
    -- ---- Serial number (SunSpec common block, one-shot) ----
    if not sn_read then
        local ok_sn, sn_regs = pcall(host.modbus_read, 40052, 16, "input")
        if ok_sn and sn_regs then
            local sn = decode_ascii(sn_regs, 16)
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---- Scale factors (cached with retry on zero reads) ----
    -- A failed modbus read returns 0 from read_sf(), which would cause raw
    -- register values to be emitted unscaled — wrong by orders of magnitude.
    -- Re-read all SFs until none are 0 or we exhaust retries.
    local need_sf_read = (sf_cache == nil)
    if not need_sf_read and sf_retries < SF_MAX_RETRIES then
        for _, v in pairs(sf_cache) do
            if v == 0 then need_sf_read = true; break end
        end
    end
    if need_sf_read then
        local fresh = load_scale_factors()
        if sf_cache == nil then
            -- First read: accept everything (zeros will trigger retries).
            sf_cache = fresh
        else
            -- Merge: only overwrite with non-zero values so a transient
            -- read failure doesn't clobber a previously good SF.
            for k, v in pairs(fresh) do
                if v ~= 0 then sf_cache[k] = v end
            end
        end
        sf_retries = sf_retries + 1
        if sf_retries >= SF_MAX_RETRIES then
            host.log("warn", "SolarEdge: accepting scale factors after "
                .. tostring(SF_MAX_RETRIES) .. " retries (some may be 0)")
        end
    end
    local sf = sf_cache

    -- ---- Inverter AC ----

    -- AC power: 40083, I16
    local ok_acw, acw_regs = pcall(host.modbus_read, 40083, 1, "input")
    local ac_w = 0
    if ok_acw and acw_regs then
        ac_w = scale(host.decode_i16(acw_regs[1]), sf.ac_power)
    end

    -- Frequency: 40085, U16
    local ok_hz, hz_regs = pcall(host.modbus_read, 40085, 1, "input")
    local hz = 0
    if ok_hz and hz_regs then
        hz = scale(hz_regs[1], sf.hz)
    end

    -- Lifetime energy: 40093-40094, U32 BE (Wh once scaled)
    local ok_le, le_regs = pcall(host.modbus_read, 40093, 2, "input")
    local lifetime_wh = 0
    if ok_le and le_regs then
        lifetime_wh = scale(host.decode_u32_be(le_regs[1], le_regs[2]), sf.energy)
    end

    -- Heat-sink temperature: 40103, I16
    local ok_temp, temp_regs = pcall(host.modbus_read, 40103, 1, "input")
    local temp_c = 0
    if ok_temp and temp_regs then
        temp_c = scale(host.decode_i16(temp_regs[1]), sf.temp)
    end

    -- MPPT1 A/V: 40140-40141, U16 each
    local ok_m1, m1_regs = pcall(host.modbus_read, 40140, 2, "input")
    local mppt1_a, mppt1_v = 0, 0
    if ok_m1 and m1_regs then
        mppt1_a = scale(m1_regs[1], sf.mppt_a)
        mppt1_v = scale(m1_regs[2], sf.mppt_v)
    end

    -- MPPT2 A/V: 40160-40161, U16 each
    local ok_m2, m2_regs = pcall(host.modbus_read, 40160, 2, "input")
    local mppt2_a, mppt2_v = 0, 0
    if ok_m2 and m2_regs then
        mppt2_a = scale(m2_regs[1], sf.mppt_a)
        mppt2_v = scale(m2_regs[2], sf.mppt_v)
    end

    -- Emit PV (site convention: generation is negative W)
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

    -- ---- Meter ----

    -- Total W: 40100, I16
    local ok_mw, mw_regs = pcall(host.modbus_read, 40100, 1, "input")
    local meter_w = 0
    if ok_mw and mw_regs then
        meter_w = scale(host.decode_i16(mw_regs[1]), sf.meter_w)
    end

    -- Per-phase current: 40191-40193, I16 each
    local ok_la, la_regs = pcall(host.modbus_read, 40191, 3, "input")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_la and la_regs then
        l1_a = scale(host.decode_i16(la_regs[1]), sf.meter_a)
        l2_a = scale(host.decode_i16(la_regs[2]), sf.meter_a)
        l3_a = scale(host.decode_i16(la_regs[3]), sf.meter_a)
    end

    -- Per-phase voltage: 40196-40198, I16 each
    local ok_lv, lv_regs = pcall(host.modbus_read, 40196, 3, "input")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_lv and lv_regs then
        l1_v = scale(host.decode_i16(lv_regs[1]), sf.meter_v)
        l2_v = scale(host.decode_i16(lv_regs[2]), sf.meter_v)
        l3_v = scale(host.decode_i16(lv_regs[3]), sf.meter_v)
    end

    -- Per-phase power: 40207-40209, I16 each
    local ok_lw, lw_regs = pcall(host.modbus_read, 40207, 3, "input")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_lw and lw_regs then
        l1_w = scale(host.decode_i16(lw_regs[1]), sf.phase_w)
        l2_w = scale(host.decode_i16(lw_regs[2]), sf.phase_w)
        l3_w = scale(host.decode_i16(lw_regs[3]), sf.phase_w)
    end

    -- Export energy: 40226-40227, U32 BE
    local ok_exp, exp_regs = pcall(host.modbus_read, 40226, 2, "input")
    local export_wh = 0
    if ok_exp and exp_regs then
        export_wh = scale(host.decode_u32_be(exp_regs[1], exp_regs[2]), sf.meter_energy)
    end

    -- Import energy: 40234-40235, U32 BE
    local ok_imp, imp_regs = pcall(host.modbus_read, 40234, 2, "input")
    local import_wh = 0
    if ok_imp and imp_regs then
        import_wh = scale(host.decode_u32_be(imp_regs[1], imp_regs[2]), sf.meter_energy)
    end

    -- SolarEdge meter reports with sign inverted vs site convention.
    -- Site: + = into site (import). SolarEdge: + = out (export).
    -- So negate W and A to flip to site convention. V and Hz are unsigned.
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

    return 5000
end

----------------------------------------------------------------------------
-- Control (PV curtail only — meter + PV emission is read-only)
----------------------------------------------------------------------------

-- Atomic write of both F000 (enable) and F001 (limit) — single FC 0x10
-- (Write Multiple Holding Registers) transaction so the inverter never
-- sees a half-applied state (e.g. enable=1 still paired with the
-- previous tick's old limit, briefly capping at a stale value).
-- REG_APC_ENABLE is at 61440, REG_APC_LIMIT at 61441 → adjacent, so
-- one multi-register write covers both.
local function write_apc(enable, limit_pct)
    local err = host.modbus_write_multi(REG_APC_ENABLE, { enable, limit_pct })
    if err ~= nil and err ~= "" then
        return false, tostring(err)
    end
    return true, nil
end

-- Apply a curtail limit in watts. Returns true on success, false on
-- any error (no nominal_w configured, modbus write failure).
local function apply_curtail(power_w)
    if nominal_w <= 0 then
        host.log("warn",
            "SolarEdge: curtail requested but nominal_w not configured; ignoring")
        return false
    end
    if power_w == nil or power_w < 0 then power_w = 0 end
    local pct = math.floor((power_w / nominal_w) * 100 + 0.5)
    if pct < 0   then pct = 0   end
    if pct > 100 then pct = 100 end

    local ok, err = write_apc(1, pct)
    if not ok then
        host.log("warn", "SolarEdge: write APC enable+limit failed: " .. err)
        return false
    end
    curtail_active = true
    host.log("info",
        "SolarEdge: curtail " .. tostring(pct) .. "% (" .. tostring(power_w) ..
        " W of " .. tostring(nominal_w) .. " W nominal)")
    return true
end

-- Release the curtail cap. Atomically writes F000=0 and F001=100 so
-- both enable-bit-honoring and limit-value-honoring firmwares see a
-- coherent "no cap" state in a single transaction. We need both
-- writes: some HD-Wave / StorEdge firmwares ignore F000 and follow
-- F001 directly (stuck-at-3% bug if F001 isn't reset), others honor
-- F000 alone (cap held at F001's value while enabled).
local function release_curtail()
    local ok, err = write_apc(0, 100)
    if not ok then
        host.log("warn", "SolarEdge: release APC enable+limit failed: " .. err)
        return false
    end
    if curtail_active then
        host.log("info", "SolarEdge: curtail released (APC_LIMIT=100, APC_ENABLE=0)")
    end
    curtail_active = false
    return true
end

function driver_command(action, power_w, cmd)
    if action == "curtail" then
        return apply_curtail(power_w)
    elseif action == "curtail_disable" or action == "deinit" then
        return release_curtail()
    end
    host.log("debug", "SolarEdge: ignoring unsupported action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- "Default mode" on the watchdog path means "drop back to the
    -- safe autonomous behavior". For a PV inverter that's just
    -- releasing any curtail cap so the panels can produce normally.
    release_curtail()
end

function driver_cleanup()
    -- Best-effort: leave the inverter at full production when the
    -- driver is unloaded or the process shuts down cleanly. If this
    -- write fails (e.g. modbus connection already torn down), the
    -- operator must clear the cap manually via SetApp.
    release_curtail()
end
