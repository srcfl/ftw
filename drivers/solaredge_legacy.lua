-- solaredge_legacy.lua
-- SolarEdge legacy K-series inverter driver (SunSpec over Modbus TCP).
-- Emits: PV. READ-ONLY. Targets older display-era inverters.
--
-- Cloned from drivers/solaredge.lua. Differences vs the HD-Wave driver:
--
--   * Reads use FC 0x03 (holding), not FC 0x04 (input). Older SolarEdge
--     firmware mirrors the SunSpec block only under holding registers;
--     issuing FC 0x04 against a K-series inverter (SE7K / SE10K / SE17K /
--     SE25K — the ones with the LCD on the inverter housing) times out
--     silently. Same diagnosis as drivers/solaredge_pv.lua, just for the
--     full inverter case.
--
--   * Meter block (SunSpec Model 203) is intentionally OMITTED in v1.
--     Newer HD-Wave firmware places the meter at 40190+, but K-series
--     firmware places it at 40121+ — and we don't yet have a verified
--     legacy-meter offset to ship. Operators who need grid-meter data
--     on a legacy install should pair this driver with a separate
--     meter driver (Pixii's Model 203 chain, an SDM630, etc.). Once
--     we have a confirmed K-series meter map we'll either extend
--     this driver or fold both into a unified driver with a
--     `function_code` config knob.
--
--   * driver_init runs a one-shot SunSpec ID probe on 40000-40003 to
--     verify the device is actually SunSpec-speaking before we trust
--     the rest of the register map. Pre-SunSpec firmware (very old
--     installations) won't reply to this and we log a clear failure
--     instead of silently emitting zeros forever.

DRIVER = {
  id           = "solaredge-legacy",
  name         = "SolarEdge legacy (K-series with display)",
  manufacturer = "SolarEdge",
  version      = "0.2.0",
  protocols    = { "modbus" },
  capabilities = { "pv", "pv-curtail" },
  description  = "SolarEdge K-series (SE7K / SE10K / SE17K / SE25K) PV inverter via Modbus TCP — reads use FC 0x03 holding; curtail writes use FC 0x10 multi-holding on the same Advanced Power Control registers (0xF000/0xF001) as HD-Wave.",
  homepage     = "https://www.solaredge.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "SE17K (display, legacy firmware)" },
  verification_status = "experimental",
  verification_notes = "First-cut clone of solaredge.lua targeting K-series display inverters. Curtail path mirrors solaredge.lua (verified there in 25/50/75% sweep on HD-Wave 8 kW). K-series support documented in SolarEdge Power Reduction Application Note but not yet verified on this firmware family.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- PV curtail (action="curtail" / "curtail_disable")
--
-- Identical mechanism to drivers/solaredge.lua — SolarEdge documents
-- the same proprietary Advanced Power Control registers for K-series
-- (SE7K..SE25K display inverters):
--
--   0xF000 (61440)  Advanced Power Control enable: 0=off, 1=on
--   0xF001 (61441)  Active Power Limit:           u16, percent 0..100
--
-- Writes go through FC 0x10 (Write Multiple Holding Registers) so
-- both registers update in a single atomic transaction — the
-- inverter never sees enable=1 paired with a stale previous-tick
-- limit value. SetApp setting "Limit Control Mode = Export Control
-- / Production" must be enabled on the inverter; without it writes
-- succeed at the Modbus layer but the inverter ignores them.
--
-- nominal_w comes from the YAML driver config block (the inverter's
-- rated AC output in W — SE7K → 7000, SE10K → 10000, SE17K → 17000,
-- SE25K → 25000). Without it the driver still emits PV readings but
-- rejects curtail commands with a logged warning.
--
-- Failsafe: F000/F001 don't auto-revert on SolarEdge. Cleanup paths
-- write {0, 100} on curtail_disable / deinit / driver_cleanup so a
-- clean shutdown returns the inverter to full production. If the
-- daemon crashes uncleanly while curtailed the cap stays applied
-- until SetApp manually releases it.

PROTOCOL = "modbus"

-- The previous version cached scale factors across polls and retried up
-- to 5x on a per-register `read_sf()` call. That was wrong on K-series
-- legacy firmware: those per-register reads sporadically return 0, and
-- 0 is ALSO a valid SunSpec scale factor (×1) — so a failed read was
-- indistinguishable from a real-but-zero SF. After 5 retries the
-- driver would permanently latch sf.ac_power = 0 and display 1490 W
-- as 149 W (the raw register value with no ×10 applied). Switching to
-- the same one-shot block read the working solaredge_pv.lua uses
-- guarantees value + SF come from the same atomic Modbus transaction
-- and removes the failure-vs-zero ambiguity entirely.
local sn_read    = false
local sunspec_ok = nil  -- true / false / nil (= not yet probed)

-- Curtail state — see header comment for the protocol.
local nominal_w = 0
local curtail_active = false
local REG_APC_ENABLE = 61440  -- 0xF000
local REG_APC_LIMIT  = 61441  -- 0xF001

----------------------------------------------------------------------------
-- SunSpec helpers
----------------------------------------------------------------------------

local POW10 = {
    [-6] = 1e-6, [-5] = 1e-5, [-4] = 1e-4, [-3] = 1e-3,
    [-2] = 1e-2, [-1] = 1e-1, [0] = 1,
    [1] = 10, [2] = 100, [3] = 1000, [4] = 10000, [5] = 100000, [6] = 1e6,
}

local function pow10(sf)
    if sf == -32768 then return 1 end
    local p = POW10[sf]
    if p ~= nil then return p end
    return 1
end

local function scale(value, sf)
    return value * pow10(sf)
end

-- Read the Model 103 block in one transaction so every value pairs
-- atomically with its SF. 40069 is the SunSpec model header (id +
-- length); 40069..40120 (52 regs) covers the header plus everything
-- we care about: AC W/V/A + SFs, energy + SF, heat-sink temp + SF.
-- Returns the raw register slice or nil on error.
local function read_inverter_block()
    local ok, regs = pcall(host.modbus_read, 40069, 52, "holding")
    if not ok or not regs then return nil end
    return regs
end

-- Index helper for the inverter block: doc address → 1-based block
-- index. reg(40083) → regs[40083 - 40069 + 1] = regs[15].
local function reg(regs, addr)
    return regs[addr - 40069 + 1]
end

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
            host.log("info", "SolarEdge-legacy: nominal_w = " .. tostring(nominal_w) .. " W (curtail enabled)")
        end
    end
    if nominal_w <= 0 then
        host.log("info", "SolarEdge-legacy: nominal_w not set in config — curtail action will be unavailable")
    end
end

-- isKSeriesModel reports whether a SunSpec C_Model name is one of the K-series
-- commercial display inverters this driver targets (SE7K / SE10K / SE17K /
-- SE25K). The model name is read from the holding C_Model register (40020) and
-- is what separates these legacy units from HD-Wave models like SE8K, which
-- belong to solaredge_pv.lua / solaredge.lua.
local function isKSeriesModel(model)
    model = string.upper(model or "")
    for _, m in ipairs({ "SE7K", "SE10K", "SE17K", "SE25K" }) do
        if string.sub(model, 1, #m) == m then
            return true
        end
    end
    return false
end

-- driver_fingerprint: READ-ONLY identity probe for the network-scan setup flow.
-- This driver is specifically for the K-series display inverters, so we match
-- ONLY those — identified by the SunSpec C_Model name at holding register 40020.
-- An HD-Wave (e.g. SE8K) returns matched=false here and is claimed by
-- solaredge_pv.lua instead. We read holding only (FC 0x03): SolarEdge serves
-- SunSpec there directly and through solaredge-proxy, and a stray FC 0x04
-- (input) read wedges such a proxy's single upstream socket on timeout.
function driver_fingerprint()
    local hok, hregs = pcall(host.modbus_read, 40000, 2, "holding")
    if not hok or not hregs or decode_ascii(hregs, 2) ~= "SunS" then
        return { matched = false }
    end
    local mok, mregs = pcall(host.modbus_read, 40004, 16, "holding")
    local mfr = (mok and mregs) and decode_ascii(mregs, 16) or ""
    if not string.find(string.lower(mfr), "solaredge", 1, true) then
        return { matched = false }
    end
    local model = ""
    local dok, dregs = pcall(host.modbus_read, 40020, 16, "holding")
    if dok and dregs then model = decode_ascii(dregs, 16) end
    if not isKSeriesModel(model) then
        return { matched = false } -- HD-Wave etc. → solaredge_pv.lua
    end
    local serial = ""
    local sok, sregs = pcall(host.modbus_read, 40052, 16, "holding")
    if sok and sregs then serial = decode_ascii(sregs, 16) end

    return {
        matched      = true,
        make         = "SolarEdge",
        model        = model,
        serial       = serial,
        capabilities = { "pv", "pv-curtail" },
    }
end

-- One-shot SunSpec ID probe. SunSpec-compliant devices place the magic
-- bytes "SunS" (0x53756e53) at registers 40000-40001. Without them, the
-- rest of the register map we trust is meaningless — the inverter is
-- either pre-SunSpec firmware or a totally different device behind the
-- same TCP port (e.g. a meter-only gateway). Returns true once we've
-- confirmed SunSpec; subsequent calls short-circuit. Failure is sticky
-- *for this poll* but we re-probe on later polls in case the link was
-- transiently slow during the first attempt.
local function probe_sunspec()
    if sunspec_ok == true then return true end
    local ok, regs = pcall(host.modbus_read, 40000, 2, "holding")
    if not ok or not regs or #regs < 2 then
        sunspec_ok = false
        host.log("warn", "SolarEdge-legacy: SunSpec probe failed (read 40000-40001 returned nothing) — check unit_id and that the device speaks SunSpec over Modbus TCP")
        return false
    end
    -- "SunS" = 0x5375, 0x6e53 — SunSpec magic.
    local hi, lo = regs[1], regs[2]
    if hi == 0x5375 and lo == 0x6e53 then
        sunspec_ok = true
        host.log("info", "SolarEdge-legacy: SunSpec ID confirmed at 40000-40001")
        return true
    end
    sunspec_ok = false
    host.log("warn", string.format(
        "SolarEdge-legacy: SunSpec probe got 0x%04X 0x%04X (expected 0x5375 0x6e53) — device may be pre-SunSpec firmware or a non-SolarEdge gateway",
        hi, lo))
    return false
end

function driver_poll()
    -- ---- SunSpec sanity gate ----
    -- Refuse to emit anything until we've verified this is actually a
    -- SunSpec-speaking device. Otherwise a wrong unit_id or wrong host
    -- would silently produce a stream of "0 W generation" readings that
    -- the dashboard treats as legitimate.
    if not probe_sunspec() then
        return 30000  -- back off; SunSpec probe re-runs on next poll
    end

    -- ---- Serial number (SunSpec common block, one-shot) ----
    if not sn_read then
        local ok_sn, sn_regs = pcall(host.modbus_read, 40052, 16, "holding")
        if ok_sn and sn_regs then
            local sn = decode_ascii(sn_regs, 16)
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    -- ---- Inverter Model 103 in one shot — value + SF are atomic ----
    local regs = read_inverter_block()
    if regs == nil then
        host.log("warn", "SolarEdge-legacy: inverter block read 40069..40120 failed")
        return 5000
    end

    -- AC power: 40083 I16, SF at 40084
    local ac_w_sf = host.decode_i16(reg(regs, 40084))
    local ac_w    = scale(host.decode_i16(reg(regs, 40083)), ac_w_sf)

    -- Frequency: 40085 U16, SF at 40086
    local hz_sf = host.decode_i16(reg(regs, 40086))
    local hz    = scale(reg(regs, 40085), hz_sf)

    -- Lifetime energy: 40093-40094 U32 BE, SF at 40095
    local energy_sf   = host.decode_i16(reg(regs, 40095))
    local lifetime_wh = scale(host.decode_u32_be(reg(regs, 40093), reg(regs, 40094)), energy_sf)

    -- Heat-sink temperature: 40103 I16, SF at 40106
    local temp_sf = host.decode_i16(reg(regs, 40106))
    local temp_c  = scale(host.decode_i16(reg(regs, 40103)), temp_sf)

    -- MPPT readings live OUTSIDE Model 103 in SolarEdge's proprietary
    -- block (40123 SFs, 40140 MPPT1 A/V, 40160 MPPT2 A/V). Read the
    -- whole span 40123..40161 (39 regs) in ONE transaction so SF and
    -- value come from the same Modbus snapshot — same atomicity rule
    -- as the Model 103 block above. Without this, an SF read that
    -- transiently returns 0 (legacy K-series firmware does this on
    -- holding-register reads) would scale the corresponding value at
    -- ×1 for the rest of the poll, the exact failure-vs-zero
    -- ambiguity this PR is fixing for AC power.
    --
    -- Block layout: SFs at offset 1 (40123), 2 (40124); MPPT1 A/V at
    -- offset 18-19 (40140-40141); MPPT2 A/V at offset 38-39
    -- (40160-40161). reg_off below converts a doc address into the
    -- 1-based block index.
    local mppt1_a, mppt1_v, mppt2_a, mppt2_v = 0, 0, 0, 0
    local ok_mppt, mppt_regs = pcall(host.modbus_read, 40123, 39, "holding")
    if ok_mppt and mppt_regs and #mppt_regs >= 39 then
        local function reg_off(addr) return mppt_regs[addr - 40123 + 1] end
        local mppt_a_sf = host.decode_i16(reg_off(40123))
        local mppt_v_sf = host.decode_i16(reg_off(40124))
        mppt1_a = scale(reg_off(40140), mppt_a_sf)
        mppt1_v = scale(reg_off(40141), mppt_v_sf)
        -- Single-string K-series units return zeros for MPPT2; that's
        -- the correct emit, no warning needed.
        mppt2_a = scale(reg_off(40160), mppt_a_sf)
        mppt2_v = scale(reg_off(40161), mppt_v_sf)
    end

    -- Site convention: generation is negative W.
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

    return 5000
end

----------------------------------------------------------------------------
-- Control (PV curtail only — PV emission is read-only)
----------------------------------------------------------------------------

-- See solaredge.lua write_apc for the FC 0x10 atomic-write rationale.
local function write_apc(enable, limit_pct)
    local err = host.modbus_write_multi(REG_APC_ENABLE, { enable, limit_pct })
    if err ~= nil and err ~= "" then
        return false, tostring(err)
    end
    return true, nil
end

local function apply_curtail(power_w)
    if nominal_w <= 0 then
        host.log("warn",
            "SolarEdge-legacy: curtail requested but nominal_w not configured; ignoring")
        return false
    end
    if power_w == nil or power_w < 0 then power_w = 0 end
    local pct = math.floor((power_w / nominal_w) * 100 + 0.5)
    if pct < 0   then pct = 0   end
    if pct > 100 then pct = 100 end

    local ok, err = write_apc(1, pct)
    if not ok then
        host.log("warn", "SolarEdge-legacy: write APC enable+limit failed: " .. err)
        return false
    end
    curtail_active = true
    host.log("info",
        "SolarEdge-legacy: curtail " .. tostring(pct) .. "% (" .. tostring(power_w) ..
        " W of " .. tostring(nominal_w) .. " W nominal)")
    return true
end

local function release_curtail()
    local ok, err = write_apc(0, 100)
    if not ok then
        host.log("warn", "SolarEdge-legacy: release APC enable+limit failed: " .. err)
        return false
    end
    if curtail_active then
        host.log("info", "SolarEdge-legacy: curtail released (APC_LIMIT=100, APC_ENABLE=0)")
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
    host.log("debug", "SolarEdge-legacy: ignoring unsupported action=" .. tostring(action))
    return false
end

function driver_default_mode()
    release_curtail()
end

function driver_cleanup()
    release_curtail()
end
