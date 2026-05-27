-- solaredge_pv.lua
-- SolarEdge inverter driver, PV-only variant (SunSpec over Modbus TCP).
-- Emits: PV. READ-ONLY.
--
-- Clone of drivers/solaredge.lua with the SunSpec Model 203 meter block
-- stripped out — use this when the grid meter comes from a different
-- driver (e.g. the Pixii PowerShaper via its Model 203 chain) and you
-- only want PV generation from the SolarEdge inverter.

DRIVER = {
  id           = "solaredge-pv",
  name         = "SolarEdge inverter (PV only)",
  manufacturer = "SolarEdge",
  version      = "1.1.0",
  protocols    = { "modbus" },
  capabilities = { "pv", "pv-curtail" },
  description  = "SolarEdge HD-Wave / StorEdge PV-only via Modbus TCP (SunSpec) with PV active-power-limit curtail.",
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
-- PV curtail — see drivers/solaredge.lua header comment. Uses the same
-- SolarEdge proprietary registers (0xF000 enable + 0xF001 percent
-- 0..100). nominal_w must be set in the YAML driver config block for
-- curtail to function; without it the driver still emits PV readings
-- but rejects curtail commands with a logged warning.
--
-- SunSpec register map. This SolarEdge gateway serves FC 0x03 (holding)
-- only; FC 0x04 (input) times out, so every read uses "holding".
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
--     40095        Energy SF (I16)
--     40103        Heat-sink °C (I16)    * 10^temp_sf
--     40106        Temp SF (I16)
--     40123        MPPT current SF (I16)
--     40124        MPPT voltage SF (I16)
--     40140-40141  MPPT1 A/V (U16 each)
--     40160-40161  MPPT2 A/V (U16 each)
--
-- Sign translation to site convention (positive = into site):
--   AC power out of the inverter = generation → PV w = -ac_w.

PROTOCOL = "modbus"

-- Per-device identity is read once; SunSpec scale factors however are
-- DYNAMIC (the spec explicitly says they can change at runtime for
-- optimal resolution) so they must be read together with the values
-- they scale — never cached. We do one big batch read of the Model 103
-- block every poll so values + SFs come from a consistent snapshot.
local sn_read = false

-- Curtail state — see solaredge.lua header for the protocol.
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

-- SunSpec scale factors use 0x8000 (= -32768 after i16 decode) as a
-- "not implemented" sentinel. Treat that as 0 (don't scale).
local function pow10(sf)
    if sf == -32768 then return 1 end
    local p = POW10[sf]
    if p ~= nil then return p end
    return 1
end

local function scale(value, sf)
    return value * pow10(sf)
end

-- Read a contiguous Model 103 block starting at 40069 so every value
-- and every paired scale factor come from the same Modbus transaction.
-- Returns the raw register slice, or nil on error.
local function read_inverter_block()
    -- 40069..40120 covers the header (id + len) + everything we care
    -- about: AC V/A/W + SFs, DC V/A/W + SFs, temperatures + SF, state.
    local ok, regs = pcall(host.modbus_read, 40069, 52, "holding")
    if not ok or not regs then return nil end
    return regs
end

-- Offset helper: Model 103 data starts at doc offset 40069 → Lua index 1.
-- reg(40083) -> regs[40083 - 40069 + 1] = regs[15]
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
            host.log("info", "SolarEdge-PV: nominal_w = " .. tostring(nominal_w) .. " W (curtail enabled)")
        end
    end
    if nominal_w <= 0 then
        host.log("info", "SolarEdge-PV: nominal_w not set in config — curtail action will be unavailable")
    end
end

function driver_poll()
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

    -- ---- Model 103 in one shot: value + SF reads are atomic ----
    local regs = read_inverter_block()
    if regs == nil then
        host.log("warn", "SolarEdge: inverter block read failed")
        return 5000
    end

    -- AC power: 40083 I16, SF at 40084 — read from the same snapshot.
    local ac_w_sf = host.decode_i16(reg(regs, 40084))
    local ac_w    = scale(host.decode_i16(reg(regs, 40083)), ac_w_sf)

    -- Frequency: 40085 U16, SF at 40086
    local hz_sf = host.decode_i16(reg(regs, 40086))
    local hz    = scale(reg(regs, 40085), hz_sf)

    -- Lifetime Wh: 40093-40094 U32 BE, SF at 40095
    local energy_sf  = host.decode_i16(reg(regs, 40095))
    local lifetime_wh = scale(host.decode_u32_be(reg(regs, 40093), reg(regs, 40094)), energy_sf)

    -- Heat-sink temperature: 40103 I16, SF at 40106
    local temp_sf = host.decode_i16(reg(regs, 40106))
    local temp_c  = scale(host.decode_i16(reg(regs, 40103)), temp_sf)

    -- MPPT readings live outside Model 103 (in SolarEdge's proprietary
    -- block past the standard inverter model), so they need their own
    -- reads. These are occasional / optional, so failures are silent.
    local mppt_a_sf, mppt_v_sf = 0, 0
    local ok_mppt_sf, mppt_sf_regs = pcall(host.modbus_read, 40123, 2, "holding")
    if ok_mppt_sf and mppt_sf_regs then
        mppt_a_sf = host.decode_i16(mppt_sf_regs[1])
        mppt_v_sf = host.decode_i16(mppt_sf_regs[2])
    end
    local mppt1_a, mppt1_v = 0, 0
    local ok_m1, m1_regs = pcall(host.modbus_read, 40140, 2, "holding")
    if ok_m1 and m1_regs then
        mppt1_a = scale(m1_regs[1], mppt_a_sf)
        mppt1_v = scale(m1_regs[2], mppt_v_sf)
    end
    local mppt2_a, mppt2_v = 0, 0
    local ok_m2, m2_regs = pcall(host.modbus_read, 40160, 2, "holding")
    if ok_m2 and m2_regs then
        mppt2_a = scale(m2_regs[1], mppt_a_sf)
        mppt2_v = scale(m2_regs[2], mppt_v_sf)
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
            "SolarEdge-PV: curtail requested but nominal_w not configured; ignoring")
        return false
    end
    if power_w == nil or power_w < 0 then power_w = 0 end
    local pct = math.floor((power_w / nominal_w) * 100 + 0.5)
    if pct < 0   then pct = 0   end
    if pct > 100 then pct = 100 end

    local ok, err = write_apc(1, pct)
    if not ok then
        host.log("warn", "SolarEdge-PV: write APC enable+limit failed: " .. err)
        return false
    end
    curtail_active = true
    host.log("info",
        "SolarEdge-PV: curtail " .. tostring(pct) .. "% (" .. tostring(power_w) ..
        " W of " .. tostring(nominal_w) .. " W nominal)")
    return true
end

local function release_curtail()
    local ok, err = write_apc(0, 100)
    if not ok then
        host.log("warn", "SolarEdge-PV: release APC enable+limit failed: " .. err)
        return false
    end
    if curtail_active then
        host.log("info", "SolarEdge-PV: curtail released (APC_LIMIT=100, APC_ENABLE=0)")
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
    host.log("debug", "SolarEdge-PV: ignoring unsupported action=" .. tostring(action))
    return false
end

function driver_default_mode()
    release_curtail()
end

function driver_cleanup()
    release_curtail()
end
