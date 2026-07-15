-- ferroamp_modbus.lua
-- Ferroamp EnergyHub Modbus TCP driver (alternative transport to drivers/ferroamp.lua)
-- Emits: PV, Battery, Meter telemetry
-- Ported from sourceful-hugin/device-support/drivers/lua/ferroamp_modbus.lua
-- Port notes (FTW v2.1 API drift vs hugin):
--   host.log(msg)                 → host.log("info", msg)
--   host.decode_f32               → inline IEEE-754 (decode_f32_be, word-swap helper)
--   host.modbus_write_multiple(…) → host.modbus_write_multi(…)
--
-- Port: 502, Unit ID: 1 (default)
-- Float format: IEEE 754, word-swapped (low word at lower address)
-- Register map: Ferroamp proprietary (not SunSpec)
--
-- Sign convention (site/EMS):
--   meter.w  : positive = importing from grid, negative = exporting
--   pv.w     : always <= 0 (generation)
--   battery.w: positive = charging (into battery), negative = discharging
--   Ferroamp reports battery power as positive = discharging, so we negate
--   at the driver boundary to match drivers/ferroamp.lua (the MQTT variant).

DRIVER = {
  id           = "ferroamp-modbus",
  name         = "Ferroamp EnergyHub (Modbus)",
  manufacturer = "Ferroamp",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Ferroamp EnergyHub XL via Modbus TCP (alternative transport to drivers/ferroamp.lua).",
  homepage     = "https://ferroamp.com",
  authors      = { "FTW contributors" },
  tested_models = { "EnergyHub XL" },
  verification_status = "experimental",
  verification_notes = "Ported from sourceful-hugin. Read-only telemetry; control still goes through drivers/ferroamp.lua (MQTT). Not yet verified against live hardware.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Local decoders / encoders
----------------------------------------------------------------------------

-- Decode IEEE 754 single-precision float from two u16 registers,
-- big-endian word order: hi = first register, lo = second.
-- Returns 0 for NaN / ±Inf (treated as "not implemented" sentinels).
local function decode_f32_be(hi, lo)
    hi = hi % 0x10000
    lo = lo % 0x10000
    local bits = hi * 0x10000 + lo
    if bits == 0 then return 0 end
    local sign = 1
    if bits >= 0x80000000 then
        sign = -1
        bits = bits - 0x80000000
    end
    local exp = math.floor(bits / 0x800000)
    local frac = bits % 0x800000
    if exp == 0xFF then return 0 end   -- NaN / Inf → not present
    if exp == 0 then
        return sign * frac / 0x800000 * (2 ^ -126)
    end
    return sign * (1 + frac / 0x800000) * (2 ^ (exp - 127))
end

-- Decode a word-swapped float32 from a register block at offset idx.
-- Ferroamp stores the low word at the lower address; our decoder expects
-- (hi, lo), so we swap here. `regs` is the 1-indexed table returned by
-- host.modbus_read, `idx` is the position of the LOW word.
local function decode_f32_ws_at(regs, idx)
    if not regs then return 0 end
    local lo = regs[idx]
    local hi = regs[idx + 1]
    if lo == nil or hi == nil then return 0 end
    return decode_f32_be(hi, lo)
end

-- Encode a float32 to a word-swapped uint16 pair for Modbus holding
-- register writes. Returns {lo_word, hi_word} suitable for
-- host.modbus_write_multi. Treats non-finite inputs as 0.
local function encode_f32_ws(value)
    if value == 0 or value ~= value then return { 0, 0 } end  -- zero or NaN

    local sign = 0
    if value < 0 then
        sign = 0x80000000
        value = -value
    end

    -- Normalise to [1, 2) and track the exponent.
    local exp = 127
    if value >= 2 then
        while value >= 2 do
            value = value / 2
            exp = exp + 1
            if exp >= 255 then return { 0, 0 } end  -- overflow → zero
        end
    elseif value < 1 then
        while value < 1 do
            value = value * 2
            exp = exp - 1
            if exp <= 0 then return { 0, 0 } end    -- underflow → zero
        end
    end

    local mantissa = math.floor((value - 1) * 0x800000 + 0.5)
    local bits = sign + exp * 0x800000 + mantissa
    local hi = math.floor(bits / 0x10000)
    local lo = bits % 0x10000

    return { lo, hi }  -- word-swapped: lo first, hi second
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Ferroamp")
    -- Ferroamp EnergyHub does not expose its serial on a stable Modbus
    -- register, so device_id resolves via ARP MAC (preferred) or the
    -- configured endpoint. No set_sn here.
    host.log("info", "Ferroamp Modbus: init complete (Unit ID 1, port 502)")
end

function driver_poll()
    --------------------------------------------------------------------------
    -- Meter (grid connection point)
    --------------------------------------------------------------------------

    -- Grid frequency: input 2016, float32, Hz (2 regs, word-swapped)
    local ok_hz, hz_regs = pcall(host.modbus_read, 2016, 2, "input")
    local hz = 0
    if ok_hz and hz_regs then hz = decode_f32_ws_at(hz_regs, 1) end

    -- Grid voltage L1/L2/L3: input 2032/2036/2040, float32, Vrms.
    -- Each value occupies 4 regs (2 f32 + 2 unused), so read 10 regs for 3 values.
    local ok_v, v_regs = pcall(host.modbus_read, 2032, 10, "input")
    local l1_v, l2_v, l3_v = 0, 0, 0
    if ok_v and v_regs then
        l1_v = decode_f32_ws_at(v_regs, 1)   -- 2032-2033
        l2_v = decode_f32_ws_at(v_regs, 5)   -- 2036-2037
        l3_v = decode_f32_ws_at(v_regs, 9)   -- 2040-2041
    end

    -- Grid active power (total): input 3100, float32, kW
    local ok_gw, gw_regs = pcall(host.modbus_read, 3100, 2, "input")
    local grid_w = 0
    if ok_gw and gw_regs then grid_w = decode_f32_ws_at(gw_regs, 1) * 1000 end

    -- Grid active current L1/L2/L3: input 3112/3116/3120, float32, Arms
    local ok_ga, ga_regs = pcall(host.modbus_read, 3112, 10, "input")
    local l1_a, l2_a, l3_a = 0, 0, 0
    if ok_ga and ga_regs then
        l1_a = decode_f32_ws_at(ga_regs, 1)   -- 3112-3113
        l2_a = decode_f32_ws_at(ga_regs, 5)   -- 3116-3117
        l3_a = decode_f32_ws_at(ga_regs, 9)   -- 3120-3121
    end

    -- Per-phase power: V * I_active (no per-phase power registers available)
    local l1_w = l1_v * l1_a
    local l2_w = l2_v * l2_a
    local l3_w = l3_v * l3_a

    -- Grid energy: export at 3064, import at 3068, float32, kWh (8 regs, two values)
    local ok_ge, ge_regs = pcall(host.modbus_read, 3064, 8, "input")
    local export_wh, import_wh = 0, 0
    if ok_ge and ge_regs then
        export_wh = decode_f32_ws_at(ge_regs, 1) * 1000   -- 3064-3065
        import_wh = decode_f32_ws_at(ge_regs, 5) * 1000   -- 3068-3069
    end

    host.emit("meter", {
        w         = grid_w,
        hz        = hz,
        l1_w      = l1_w,
        l2_w      = l2_w,
        l3_w      = l3_w,
        l1_v      = l1_v,
        l2_v      = l2_v,
        l3_v      = l3_v,
        l1_a      = l1_a,
        l2_a      = l2_a,
        l3_a      = l3_a,
        import_wh = import_wh,
        export_wh = export_wh,
    })
    -- Diagnostics: long-format TS DB
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

    --------------------------------------------------------------------------
    -- PV (solar generation)
    --------------------------------------------------------------------------

    -- Solar power: input 5100, float32, kW (always positive from Ferroamp)
    local ok_pv, pv_regs = pcall(host.modbus_read, 5100, 2, "input")
    local pv_w = 0
    if ok_pv and pv_regs then pv_w = decode_f32_ws_at(pv_regs, 1) * 1000 end

    -- Solar energy produced: input 5064, float32, kWh
    local ok_pe, pe_regs = pcall(host.modbus_read, 5064, 2, "input")
    local pv_lifetime_wh = 0
    if ok_pe and pe_regs then pv_lifetime_wh = decode_f32_ws_at(pe_regs, 1) * 1000 end

    host.emit("pv", {
        w           = -pv_w,   -- negative = generation (site convention)
        lifetime_wh = pv_lifetime_wh,
    })

    --------------------------------------------------------------------------
    -- Battery
    --------------------------------------------------------------------------

    -- Battery power: input 6100, float32, kW.
    -- Ferroamp: positive = discharging. Site convention: positive = charging.
    -- Negate and convert kW → W (matches drivers/ferroamp.lua's sign handling).
    local ok_bw, bw_regs = pcall(host.modbus_read, 6100, 2, "input")
    local bat_w = 0
    if ok_bw and bw_regs then bat_w = -decode_f32_ws_at(bw_regs, 1) * 1000 end

    -- Battery SoC: input 6016, float32, percent → 0-1 fraction
    local ok_soc, soc_regs = pcall(host.modbus_read, 6016, 2, "input")
    local bat_soc = nil
    if ok_soc and soc_regs then bat_soc = decode_f32_ws_at(soc_regs, 1) / 100 end

    -- Battery energy: discharge at 6064, charge at 6068, float32, kWh (8 regs)
    local ok_be, be_regs = pcall(host.modbus_read, 6064, 8, "input")
    local bat_discharge_wh, bat_charge_wh = 0, 0
    if ok_be and be_regs then
        bat_discharge_wh = decode_f32_ws_at(be_regs, 1) * 1000   -- 6064-6065
        bat_charge_wh    = decode_f32_ws_at(be_regs, 5) * 1000   -- 6068-6069
    end

    -- Omit soc from the emit table when the read failed — emitting 0
    -- would cause the control loop to think the battery is empty.
    local bat_data = {
        w            = bat_w,
        charge_wh    = bat_charge_wh,
        discharge_wh = bat_discharge_wh,
    }
    if bat_soc ~= nil then bat_data.soc = bat_soc end
    host.emit("battery", bat_data)

    return 5000
end

----------------------------------------------------------------------------
-- Control
----------------------------------------------------------------------------

-- Battery mode at holding 6000 (uint16), power ref at holding 6064 (float32).
-- Ferroamp: Mode 0 = default/auto, Mode 1 = power-mode
-- Power ref: negative kW = charge, positive kW = discharge
--
-- Curtailment via grid power control: holding 8010 (enable), 8012 (limit W, f32),
-- 8016 (apply).
--
-- EMS convention (our side): positive power_w = charge, negative = discharge.
function driver_command(action, power_w, cmd)
    if action == "init" then
        return true

    elseif action == "battery" then
        if power_w == 0 then
            -- Zero setpoint: release to auto mode instead of holding the
            -- inverter in forced-zero power mode.
            host.modbus_write(6000, 0)  -- auto mode
            host.log("debug", "Ferroamp Modbus: battery ref 0 → auto mode")
            return true
        end
        -- Site convention: positive power_w = charge
        -- Ferroamp: negative kW = charge → negate, convert W to kW
        local ref_kw = -power_w / 1000
        host.modbus_write_multi(6064, encode_f32_ws(ref_kw))
        host.modbus_write(6000, 1)  -- power mode
        host.log("debug", "Ferroamp Modbus: battery ref " .. tostring(ref_kw) .. " kW")
        return true

    elseif action == "curtail" then
        -- Limit export to |power_w| watts
        host.modbus_write(8010, 1)  -- enable export limit
        host.modbus_write_multi(8012, encode_f32_ws(math.abs(power_w)))
        host.modbus_write(8016, 1)  -- apply
        host.log("debug", "Ferroamp Modbus: export limit " .. tostring(math.abs(power_w)) .. " W")
        return true

    elseif action == "curtail_disable" then
        host.modbus_write(8010, 0)  -- disable export limit
        host.modbus_write(8016, 1)  -- apply
        return true

    elseif action == "deinit" then
        -- Restore auto mode and remove export limits
        host.modbus_write(6000, 0)
        host.modbus_write(8010, 0)
        host.modbus_write(8016, 1)
        return true
    end
    return false
end

-- Watchdog fallback: revert to autonomous self-consumption (mode 0).
-- Matches drivers/ferroamp.lua's "auto" command for the MQTT variant.
function driver_default_mode()
    host.log("info", "Ferroamp Modbus: watchdog → auto / self-consumption")
    local err = host.modbus_write(6000, 0)
    if err ~= nil and err ~= "" then
        host.log("warn", "Ferroamp Modbus: watchdog auto failed: " .. tostring(err))
        return false
    end
    return true
end

function driver_cleanup()
    -- Return to auto on shutdown so the device doesn't stay in a forced mode.
    local err = host.modbus_write(6000, 0)
    if err ~= nil and err ~= "" then
        host.log("warn", "Ferroamp Modbus: cleanup auto failed: " .. tostring(err))
        return false
    end
    return true
end
