-- fronius_smart_meter.lua
-- Fronius Smart Meter (three-phase energy meter) driver
-- Emits: Meter (read-only)
-- Protocol: Modbus TCP (SunSpec), ALL HOLDING registers, F32 BE throughout
-- Ported from sourceful-hugin/device-support/drivers/lua/fronius_smart_meter.lua
-- Port notes (v2.1 API drift vs hugin):
--   host.log(msg)           → host.log("info", msg)
--   host.decode_u32/i32     → _be variants
--   host.decode_f32         → inline IEEE-754 (two u16, big-endian words)

DRIVER = {
  id           = "fronius-smart-meter",
  name         = "Fronius Smart Meter",
  manufacturer = "Fronius",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter" },
  description  = "Fronius Smart Meter three-phase energy meter via Modbus TCP (SunSpec).",
  homepage     = "https://www.fronius.com",
  authors      = { "FTW contributors" },
  tested_models = { "Smart Meter 50kA-3", "Smart Meter 63A-3", "Smart Meter TS 65A-3" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Register map (all HOLDING, all SunSpec F32 BE pairs — hi word first):
--   40074/40076/40078 — per-phase current (A)
--   40082/40084/40086 — per-phase voltage (V)
--   40096             — grid frequency (Hz)
--   40098             — total AC power (W)  positive = importing from grid
--   40100/40102/40104 — per-phase power (W)
--   40130             — total export energy (Wh)  lifetime counter
--   40138             — total import energy (Wh)  lifetime counter
--
-- Sign convention (site/EMS):
--   meter.w  : positive = importing from grid, negative = exporting
-- Fronius Smart Meter already reports import-positive, no flip needed.
--
-- This driver complements drivers/fronius.lua (inverter). Mark the meter
-- entry in config.yaml as `is_site_meter: true` when it's the household
-- grid-connection meter.

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Local decoder (replaces host.decode_f32 from hugin v1.x)
----------------------------------------------------------------------------

-- Decode IEEE 754 single-precision float from two u16 registers,
-- big-endian word order: hi = first register, lo = second.
-- Returns 0 for NaN / ±Inf (SunSpec "not implemented" sentinel is 0x7FC00000).
local function decode_f32_be(hi, lo)
    hi = hi % 0x10000
    lo = lo % 0x10000
    local bits = hi * 0x10000 + lo
    local sign = 1
    if bits >= 0x80000000 then
        sign = -1
        bits = bits - 0x80000000
    end
    local exp = math.floor(bits / 0x800000)
    local frac = bits % 0x800000
    if exp == 0xFF then
        return 0  -- NaN or Inf → treat as not-present
    end
    local value
    if exp == 0 then
        -- Subnormal (or zero)
        value = frac / 0x800000 * (2 ^ -126)
    else
        value = (1 + frac / 0x800000) * (2 ^ (exp - 127))
    end
    return sign * value
end

-- Helper: read a contiguous F32 BE pair at `addr` and return the decoded
-- float. On Modbus error, returns 0 so the poll cycle still emits a
-- well-shaped table.
local function read_f32(addr)
    local ok, regs = pcall(host.modbus_read, addr, 2, "holding")
    if ok and regs then
        return decode_f32_be(regs[1], regs[2])
    end
    return 0
end

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Fronius")
    -- Smart Meter has no accessible serial block; set_sn is skipped.
    -- device_id falls back to mac:<arp> or ep:<endpoint>.
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Per-phase current (A)
    local l1_a = read_f32(40074)
    local l2_a = read_f32(40076)
    local l3_a = read_f32(40078)

    -- Per-phase voltage (V)
    local l1_v = read_f32(40082)
    local l2_v = read_f32(40084)
    local l3_v = read_f32(40086)

    -- Grid frequency (Hz)
    local hz = read_f32(40096)

    -- Total AC power (W) — Fronius: positive = import, matches site convention
    local total_w = read_f32(40098)

    -- Per-phase AC power (W)
    local l1_w = read_f32(40100)
    local l2_w = read_f32(40102)
    local l3_w = read_f32(40104)

    -- Lifetime energy counters (Wh)
    local export_wh = read_f32(40130)
    local import_wh = read_f32(40138)

    host.emit("meter", {
        w         = total_w,
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

    return 5000
end

----------------------------------------------------------------------------
-- Control (READ-ONLY — meter exposes no writable registers)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end
    host.log("warn", "Fronius Smart Meter: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only: nothing to revert.
end

function driver_cleanup()
    -- No cached state to clear.
end
