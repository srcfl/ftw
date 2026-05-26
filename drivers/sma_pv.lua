-- SMA non-hybrid PV inverter driver
-- Emits: PV (+ Meter when an SMA Energy Meter / Sunny Home Manager is paired)
-- Controls: PV curtailment via Modbus active-power setpoint
-- Protocol: Modbus TCP (input regs for telemetry, holding reg 40915 for control)
--
-- Scope: commercial string PV inverters that don't have an integrated battery.
-- Use drivers/sma.lua for Sunny Boy Storage and other hybrid units. Verified
-- against Sunny Tripower STP 50-40 (CORE1) and STP 50-41 (CORE2 / "X-class")
-- on the same address space — fleet-uniform register map.

DRIVER = {
  id           = "sma_pv",
  name         = "SMA PV inverter (non-hybrid)",
  manufacturer = "SMA",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "pv", "meter", "pv_curtail" },
  description  = "SMA Sunny Tripower commercial string inverters (STP 50-40 / 50-41 / X-class) via Modbus TCP, with active-power curtailment over Modbus holding register 40915.",
  homepage     = "https://www.sma.de",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "Sunny Tripower STP 50-40 (CORE1)", "Sunny Tripower STP 50-41 (CORE2)" },
  verification_status = "tested",
  verification_notes = "Telemetry + curtailment verified live on 3× STP 50-40 and 1× STP 50-41. Curtailment settles within ≈2 s; steady-state tracking error ≤0.6% of setpoint.",
  connection_defaults = {
    port    = 502,
    unit_id = 3, -- SMA Sunny Tripower defaults to unit_id 3, not 1
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Helpers
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
-- populated return these magic numbers; clamping them to 0 keeps telemetry
-- sane.
local function u32_valid(v)
    return v ~= 4294967295        -- 0xFFFFFFFF = U32 NaN
end
local function i32_valid(v)
    return v ~= -2147483648       -- 0x80000000 (as signed) = I32 NaN
end
local function u64_valid(h1, h2, h3, h4)
    return not (h1 == 65535 and h2 == 65535 and h3 == 65535 and h4 == 65535)
end

-- Raw-word NaN check. Some SMA firmware variants leave "not applicable"
-- registers as 0xFFFF on every word regardless of the field's declared type
-- — string PV models do this for battery/meter sections rather than emitting
-- the canonical 0x80000000 I32 sentinel. Checking the raw words catches both.
local function regs_all_ffff(regs, n)
    for i = 1, n do
        if regs[i] ~= 65535 then return false end
    end
    return true
end

----------------------------------------------------------------------------
-- Control surface
----------------------------------------------------------------------------

-- Active-power curtailment register. Holding reg, U32 BE, watts. Same address
-- and semantics on CORE1 and CORE2 firmware: write a watt cap to curtail,
-- write the inverter's nameplate W to release. The inverter must have been
-- commissioned with "External setpoint of active power via communication"
-- enabled (one-time installer step in the WebUI / Sunny Portal). On a
-- non-configured inverter the write is accepted silently and ignored, which
-- is the safe failure mode.
local REG_W_SETPOINT = 40915

----------------------------------------------------------------------------
-- Per-driver state
----------------------------------------------------------------------------
-- Resolved on the first poll cycle by probing optional sections. We pay this
-- discovery cost exactly once per app restart rather than failing-and-falling-
-- back on every poll.
--
--   sn_read        — true once the SunSpec serial has been latched
--   has_meter      — does this inverter expose a paired SMA Energy Meter?
--   lifetime_reg   — 30513 (U64) on CORE1+; 30529 (U32) on the rare older
--                    firmware that returns exception 0x02 at 30513
--   release_w      — value to write to REG_W_SETPOINT when releasing a
--                    curtailment. Sampled from the inverter's current
--                    setpoint at probe time (i.e. its natural ceiling).
--   curtail_active — true while a curtailment is in effect. Used by the
--                    watchdog path to know whether to issue a release.
local sn_read = false
local has_meter = nil
local lifetime_reg = nil
local release_w = 50000
local curtail_active = false

-- Probe the inverter once to figure out which optional sections it populates.
-- Called from driver_poll on the first cycle.
local function probe_capabilities()
    -- Meter: total power @ 30885 (I32). NaN here means no SMA Energy Meter /
    -- Sunny Home Manager is paired with the inverter — most commercial string
    -- inverters don't have one.
    local ok, regs = pcall(host.modbus_read, 30885, 2, "input")
    has_meter = ok and regs ~= nil and not regs_all_ffff(regs, 2)

    -- PV lifetime generation. Prefer U64 at 30513; fall back to U32 at 30529
    -- for older firmware that raises exception 0x02 there.
    local ok2, regs2 = pcall(host.modbus_read, 30513, 4, "input")
    if ok2 and regs2 and u64_valid(regs2[1], regs2[2], regs2[3], regs2[4]) then
        lifetime_reg = 30513
    else
        lifetime_reg = 30529
    end

    -- Curtailment release ceiling. Whatever 40915 reads right now is the
    -- inverter's natural cap — and that's what we restore on curtail_disable.
    local ok3, sp_regs = pcall(host.modbus_read, REG_W_SETPOINT, 2, "holding")
    if ok3 and sp_regs then
        local v = host.decode_u32_be(sp_regs[1], sp_regs[2])
        if u32_valid(v) and v >= 1000 then release_w = v end
    end

    host.log("info", string.format(
        "SMA_PV capabilities resolved: meter=%s lifetime_reg=%d release_w=%d",
        tostring(has_meter), lifetime_reg, release_w))
end

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

    -- Resolve which optional sections this inverter populates. One-shot,
    -- cached for the lifetime of this driver instance.
    if has_meter == nil then probe_capabilities() end

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

    -- PV lifetime generation. Register location resolved once by
    -- probe_capabilities and cached — no per-poll exception handling needed.
    local pv_gen_wh = 0
    if lifetime_reg == 30513 then
        local ok_pvgen, pvgen_regs = pcall(host.modbus_read, 30513, 4, "input")
        if ok_pvgen and pvgen_regs and u64_valid(pvgen_regs[1], pvgen_regs[2], pvgen_regs[3], pvgen_regs[4]) then
            pv_gen_wh = decode_u64_be(pvgen_regs[1], pvgen_regs[2], pvgen_regs[3], pvgen_regs[4])
        end
    else
        local ok_pvgen, pvgen_regs = pcall(host.modbus_read, 30529, 2, "input")
        if ok_pvgen and pvgen_regs then
            local v = host.decode_u32_be(pvgen_regs[1], pvgen_regs[2])
            if u32_valid(v) then pv_gen_wh = v end
        end
    end

    -- Inverter heatsink temp: 30953-30954, I32 BE × 0.1 C
    local ok_itemp, itemp_regs = pcall(host.modbus_read, 30953, 2, "input")
    local inv_temp = 0
    if ok_itemp and itemp_regs then
        local v = host.decode_i32_be(itemp_regs[1], itemp_regs[2])
        if i32_valid(v) then inv_temp = v * 0.1 end
    end

    -- Rated power: 31085-31086, U32 BE, watts.  Note: CORE2 firmware can
    -- report 0 here — release_w from probe_capabilities is more reliable.
    local ok_rated, rated_regs = pcall(host.modbus_read, 31085, 2, "input")
    local rated_w = 0
    if ok_rated and rated_regs then
        local v = host.decode_u32_be(rated_regs[1], rated_regs[2])
        if u32_valid(v) then rated_w = v end
    end

    -- Current event number (manufacturer). Useful diagnostic — non-zero
    -- values indicate the inverter has an active event/warning. The actual
    -- event description isn't exposed over Modbus on STP X / CORE2.
    local ok_evt, evt_regs = pcall(host.modbus_read, 30247, 2, "input")
    local event_no = 0
    if ok_evt and evt_regs then
        local v = host.decode_u32_be(evt_regs[1], evt_regs[2])
        if u32_valid(v) then event_no = v end
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
    host.emit_metric("sma_event_no",    event_no)
    host.emit_metric("curtail_cap_w",   release_w)

    --------------------------------------------------------------- Meter --
    -- Skipped on inverters without a paired SMA Energy Meter / Sunny Home
    -- Manager — the registers return all-0xFFFF which would otherwise emit
    -- as meter_w = -1 (decoded as signed I32) and confuse the site meter.
    if has_meter then

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

    end -- if has_meter

    return 5000
end

----------------------------------------------------------------------------
-- Control — PV curtailment
--
-- Behaviour:
--   "curtail" power_w=N  → clamp output to N watts (0 ≤ N ≤ release_w)
--   "curtail_disable"    → restore the natural ceiling (release_w)
--   "init"               → no-op handshake
----------------------------------------------------------------------------

-- Write a U32 big-endian watt value to the SMA active-power setpoint
-- register. Returns nil on success, error string on failure (matches
-- host capability error convention).
local function write_w_setpoint(watts)
    local w = math.floor(math.max(0, watts))
    local hi = math.floor(w / 65536) % 65536
    local lo = w % 65536
    return host.modbus_write_multi(REG_W_SETPOINT, { hi, lo })
end

function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "curtail" then
        local target = math.abs(power_w or 0)
        if target > release_w then target = release_w end
        local err = write_w_setpoint(target)
        if err then
            host.log("warn", "SMA_PV curtail failed: " .. tostring(err))
            return false
        end
        curtail_active = true
        host.log("info", string.format("SMA_PV curtail → %d W (cap=%d W)", target, release_w))
        return true
    elseif action == "curtail_disable" or action == "deinit" then
        local err = write_w_setpoint(release_w)
        if err then
            host.log("warn", "SMA_PV curtail_disable failed: " .. tostring(err))
            return false
        end
        curtail_active = false
        host.log("info", string.format("SMA_PV curtail released → %d W", release_w))
        return true
    end
    host.log("warn", "SMA_PV: unknown action: " .. tostring(action))
    return false
end

-- Watchdog / fallback path. If we'd issued a curtailment and then lost the
-- planner, the inverter would sit at the last setpoint indefinitely. Release
-- it so the inverter returns to autonomous full-power output.
function driver_default_mode()
    if not curtail_active then return end
    local err = write_w_setpoint(release_w)
    if err then
        host.log("warn", "SMA_PV default_mode release failed: " .. tostring(err))
        return
    end
    curtail_active = false
    host.log("info", "SMA_PV watchdog: released curtailment")
end

function driver_cleanup()
    -- Don't leave a curtailment sticking after a driver hot-reload or shutdown.
    if curtail_active then write_w_setpoint(release_w) end
end
