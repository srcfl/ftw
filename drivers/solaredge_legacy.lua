-- solaredge_legacy.lua
-- SolarEdge legacy K-series inverter driver (SunSpec over Modbus TCP).
-- Optimized for forty-two-watts EMS to reduce night-time timeouts.

DRIVER = {
  id           = "solaredge-legacy",
  name         = "SolarEdge legacy (K-series with display)",
  manufacturer = "SolarEdge",
  version      = "0.2.1",
  protocols    = { "modbus" },
  capabilities = { "pv", "pv-curtail" },
  description  = "SolarEdge K-series PV inverter via Modbus TCP — Night-optimized version.",
  homepage     = "https://www.solaredge.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "SE17K (display, legacy firmware)" },
  verification_status = "experimental",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

-- Lokala variabler för tillstånd
local sn_read    = false
local sunspec_ok = nil  
local nominal_w  = 0
local curtail_active = false

local REG_APC_ENABLE = 61440  
local REG_APC_LIMIT  = 61441  

-- Snabbreferens för stängda tabeller (Prestandaoptimering)
local POW10 = {
    [-6] = 1e-6, [-5] = 1e-5, [-4] = 1e-4, [-3] = 1e-3,
    [-2] = 1e-2, [-1] = 1e-1, [0] = 1,
    [1] = 10, [2] = 100, [3] = 1000, [4] = 10000, [5] = 100000, [6] = 1e6,
}

local function pow10(sf)
    if sf == -32768 then return 1 end
    return POW10[sf] or 1
end

local function scale(value, sf)
    return value * pow10(sf)
end

-- Läser Model 103 blocket
local function read_inverter_block()
    local ok, regs = pcall(host.modbus_read, 40069, 52, "holding")
    if not ok or not regs then return nil end
    return regs
end

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

local function probe_sunspec()
    if sunspec_ok == true then return true end
    local ok, regs = pcall(host.modbus_read, 40000, 2, "holding")
    if not ok or not regs or #regs < 2 then
        sunspec_ok = false
        -- Loggför bara som debug på natten för att undvika gula trianglar i HA
        host.log("debug", "SolarEdge-legacy: SunSpec probe timeout/fail (inverter might be sleeping)")
        return false
    end
    
    local hi, lo = regs[1], regs[2]
    if hi == 0x5375 and lo == 0x6e53 then
        sunspec_ok = true
        host.log("info", "SolarEdge-legacy: SunSpec ID confirmed at 40000-40001")
        return true
    end
    sunspec_ok = false
    host.log("warn", string.format("SolarEdge-legacy: SunSpec probe got 0x%04X 0x%04X (expected 0x5375 0x6e53)", hi, lo))
    return false
end

function driver_poll()
    -- SunSpec sanity gate
    if not probe_sunspec() then
        return 30000  -- Backa undan 30 sekunder om växelriktaren sover djupt
    end

    -- Serial number (Körs bara en gång vid uppstart)
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

    -- Huvudblock (Model 103)
    local regs = read_inverter_block()
    if regs == nil then
        -- Ändrat från 'warn' till 'debug' för att göra natten tyst
        host.log("debug", "SolarEdge-legacy: inverter block read failed (sleeping?)")
        return 10000  -- Försök igen om 10 sekunder
    end

    -- Avkoda AC-effekt
    local ac_w_sf = host.decode_i16(reg(regs, 40084))
    local ac_w    = scale(host.decode_i16(reg(regs, 40083)), ac_w_sf)

    local hz_sf = host.decode_i16(reg(regs, 40086))
    local hz    = scale(reg(regs, 40085), hz_sf)

    local energy_sf   = host.decode_i16(reg(regs, 40095))
    local lifetime_wh = scale(host.decode_u32_be(reg(regs, 40093), reg(regs, 40094)), energy_sf)

    local temp_sf = host.decode_i16(reg(regs, 40106))
    local temp_c  = scale(host.decode_i16(reg(regs, 40103)), temp_sf)

    -- OPTIMERING: Läs bara MPPT-data om växelriktaren faktiskt producerar ström (ac_w > 0)
    local mppt1_a, mppt1_v, mppt2_a, mppt2_v = 0, 0, 0, 0
    if ac_w > 0 then
        local ok_mppt, mppt_regs = pcall(host.host.modbus_read or host.modbus_read, 40123, 39, "holding")
        if ok_mppt and mppt_regs and #mppt_regs >= 39 then
            local function reg_off(addr) return mppt_regs[addr - 40123 + 1] end
            local mppt_a_sf = host.decode_i16(reg_off(40123))
            local mppt_v_sf = host.decode_i16(reg_off(40124))
            mppt1_a = scale(reg_off(40140), mppt_a_sf)
            mppt1_v = scale(reg_off(40141), mppt_v_sf)
            mppt2_a = scale(reg_off(40160), mppt_a_sf)
            mppt2_v = scale(reg_off(40161), mppt_v_sf)
        end
    end

    -- Skicka data till styrsystemet
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
-- Control (PV curtail)
----------------------------------------------------------------------------

local function write_apc(enable, limit_pct)
    local err = host.modbus_write_multi(REG_APC_ENABLE, { enable, limit_pct })
    if err ~= nil and err ~= "" then
        return false, tostring(err)
    end
    return true, nil
end

local function apply_curtail(power_w)
    if nominal_w <= 0 then
        host.log("warn", "SolarEdge-legacy: curtail requested but nominal_w not configured")
        return false
    end
    if power_w == nil or power_w < 0 then power_w = 0 end
    local pct = math.floor((power_w / nominal_w) * 100 + 0.5)
    if pct < 0   then pct = 0   end
    if pct > 100 then pct = 100 end

    local ok, err = write_apc(1, pct)
    if not ok then
        host.log("warn", "SolarEdge-legacy: write APC failed: " .. err)
        return false
    end
    curtail_active = true
    host.log("info", "SolarEdge-legacy: curtail " .. tostring(pct) .. "%")
    return true
end

local function release_curtail()
    local ok, err = write_apc(0, 100)
    if not ok then
        return false
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
    return false
end

function driver_default_mode() release_curtail() end
function driver_cleanup()      release_curtail() end
