-- Eastron SDM630 / SDM72D-M three-phase energy meter driver
-- Ported from sourceful-hugin/device-support/drivers/lua/sdm630.lua (v1.1.0).
-- Emits: Meter telemetry only. Read-only.
-- Protocol: Modbus TCP/RTU, INPUT registers, IEEE 754 F32 BE pairs.

DRIVER = {
  id           = "sdm630",
  name         = "Eastron SDM630 meter",
  manufacturer = "Eastron",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter" },
  description  = "Eastron SDM630 and SDM72D-M three-phase energy meters via Modbus TCP/RTU.",
  homepage     = "https://www.eastrongroup.com",
  authors      = { "FTW contributors" },
  tested_models = { "SDM630 Modbus", "SDM72D-M" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Register conventions:
--   All telemetry in INPUT registers (FC 0x04), starting at decimal 0.
--   Each reading is a pair of u16 registers encoding one IEEE 754 big-endian
--   float32 (hi word first, then lo word).
--
-- Site sign convention (docs/site-convention.md):
--   Grid meter: positive W = import (grid → site), negative W = export.
--   The SDM630 reports per-phase active power as signed watts with import
--   positive when CTs are wired correctly — forward it unchanged.

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- Decode a big-endian IEEE 754 float32 from two u16 Modbus registers
-- (hi word first, then lo word). FTW's host has no decode_f32, so we do
-- it inline in Lua. Treats inf/NaN as 0 to keep downstream consumers safe.
local function decode_f32_be(hi, lo)
    local combined = hi * 65536 + lo
    if combined == 0 then return 0 end
    local sign = (combined >= 0x80000000) and -1 or 1
    local exp = math.floor(combined / 0x800000) % 0x100
    local mantissa = combined % 0x800000
    if exp == 0 then return sign * mantissa * 2^-149 end
    if exp == 0xFF then return 0 end
    return sign * (1 + mantissa / 0x800000) * 2^(exp - 127)
end

-- Read a block of F32 BE values starting at `addr`. Returns an array of
-- `count` floats on success, or nil on read failure.
local function read_f32_block(addr, count)
    local regs_needed = count * 2
    local ok, regs = pcall(host.modbus_read, addr, regs_needed, "input")
    if not ok or not regs or #regs < regs_needed then
        return nil
    end
    local out = {}
    for i = 1, count do
        local hi = regs[(i - 1) * 2 + 1]
        local lo = regs[(i - 1) * 2 + 2]
        out[i] = decode_f32_be(hi, lo)
    end
    return out
end

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Eastron")
    host.log("info", "SDM630: initialized (Eastron three-phase meter)")
    -- No stable serial-number register on SDM630 — identity falls back to
    -- mac:<arp-resolved> or ep:<endpoint> via the registry.
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    -- Per-phase active power (W): registers 12-17, three F32 BE pairs.
    -- SDM630 reports signed watts (import positive) — aligns with site convention.
    -- If this primary read fails, skip the entire emit so the watchdog catches
    -- staleness — publishing zeros would mislead the control loop.
    local watts = read_f32_block(12, 3)
    if watts == nil then
        host.log("warn", "SDM630: power read failed, skipping emit")
        return 5000
    end
    local l1_w, l2_w, l3_w = watts[1], watts[2], watts[3]

    -- Total active power: register 52-53. Sum of phases is preferred for
    -- robustness; fall back to the device total if phases all read 0.
    local total_w = l1_w + l2_w + l3_w
    if total_w == 0 then
        local tw = read_f32_block(52, 1)
        if tw then total_w = tw[1] end
    end

    -- Per-phase voltage (V): registers 0-5, three F32 BE pairs.
    -- Diagnostic reads — failures are not fatal.
    local volts = read_f32_block(0, 3)
    local l1_v, l2_v, l3_v = 0, 0, 0
    if volts then l1_v, l2_v, l3_v = volts[1], volts[2], volts[3] end

    -- Per-phase current (A): registers 6-11, three F32 BE pairs.
    local amps = read_f32_block(6, 3)
    local l1_a, l2_a, l3_a = 0, 0, 0
    if amps then l1_a, l2_a, l3_a = amps[1], amps[2], amps[3] end

    -- Grid frequency: registers 70-71, F32 BE Hz.
    local hz_block = read_f32_block(70, 1)
    local hz = 0
    if hz_block then hz = hz_block[1] end

    -- Import/export energy: registers 72-75, two F32 BE kWh values → Wh.
    local energy = read_f32_block(72, 2)
    local import_wh, export_wh = 0, 0
    if energy then
        import_wh = energy[1] * 1000
        export_wh = energy[2] * 1000
    end

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

    -- Diagnostics: long-format TS DB.
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
-- Control (read-only meter — no actuators)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    -- SDM630 is a read-only meter. Accept "init" for completeness; reject
    -- anything that would try to actuate.
    if action == "init" then
        return true
    end
    host.log("debug", "SDM630: ignoring command (read-only meter): " .. tostring(action))
    return false
end

function driver_default_mode()
    -- No-op: nothing to revert, meter has no control surface.
end

function driver_cleanup()
    -- No persistent state to release.
end
