-- Eastron SDM630 three-phase Modbus meter.
--
-- Canonical Sourceful pilot driver, based on David's Blixt L1 implementation.
-- The protocol choices are intentionally preserved: one bundled FC04 read,
-- optional serial discovery from 0xFC00, and import-positive signed power.
-- This source is portable across the FTW GopherLua and Blixt LuaJIT host
-- profiles; each host still validates and loads a target-specific artifact.

PROTOCOL = "modbus"

DRIVER = {
    host_api_min = 1,
    host_api_max = 1,
    id = "sdm630",
    name = "Eastron SDM630 meter",
    manufacturer = "Eastron",
    version = "1.1.1",
    protocols = { "modbus" },
    capabilities = { "meter" },
    read_only = true,
    description = "Eastron SDM630 three-phase meter via Modbus TCP or RTU.",
    homepage = "https://www.eastrongroup.com",
    authors = { "David and Blixt L1 contributors", "Sourceful contributors" },
    tested_models = { "SDM630 Modbus" },
    verification_status = "experimental",
    verification_notes = "Read-only canonical package pilot; physical HIL verification is still required.",
    connection_defaults = {
        port = 502,
        unit_id = 1,
    },
}

DRIVER_MANIFEST = {
    name = "sdm630",
    version = "1.1.1",
    role = "meter",
    requires = {},
    options = {},
    provides = {
        live = {
            "meter.W", "meter.Hz",
            "meter.L1_V", "meter.L2_V", "meter.L3_V",
            "meter.L1_A", "meter.L2_A", "meter.L3_A",
            "meter.L1_W", "meter.L2_W", "meter.L3_W",
            "meter.total_import_Wh", "meter.total_export_Wh",
        },
        static = { "make" },
    },
}

-- Decode IEEE-754 float32 from two big-endian 16-bit registers. Keeping this
-- in Lua avoids depending on a helper that currently exists only in Blixt.
local function decode_f32_be(hi, lo)
    local combined = hi * 65536 + lo
    if combined == 0 then return 0 end
    local sign = (combined >= 0x80000000) and -1 or 1
    local exponent = math.floor(combined / 0x800000) % 0x100
    local mantissa = combined % 0x800000
    if exponent == 0 then return sign * mantissa * 2^-149 end
    if exponent == 0xFF then return 0 end
    return sign * (1 + mantissa / 0x800000) * 2^(exponent - 127)
end

local function f32(regs, base, address)
    if not regs then return nil end
    local index = address - base + 1
    local hi = regs[index]
    local lo = regs[index + 1]
    if hi == nil or lo == nil then return nil end
    return decode_f32_be(hi, lo)
end

-- Keep the canonical source independent of runtime-specific binary helpers.
-- Lua numbers represent this unsigned 32-bit serial exactly in both target
-- profiles (GopherLua/Lua 5.1 semantics and Blixt LuaJIT).
local function decode_u32_be(hi, lo)
    return hi * 65536 + lo
end

function driver_init(config)
    host.set_make("Eastron")

    -- Serial discovery is useful but never allowed to block telemetry. An
    -- absent serial remains absent so the host can apply its stable fallback.
    local ok, regs = pcall(host.modbus_read, 0xFC00, 2, "holding")
    if ok and regs and regs[1] and regs[2] then
        local serial = decode_u32_be(regs[1], regs[2])
        if serial and serial ~= 0 then
            host.set_sn(tostring(serial))
        end
    end
    return true
end

function driver_poll()
    -- One FC04 request covers the measurement window 0x0000..0x004B.
    local ok, regs = pcall(host.modbus_read, 0x0000, 76, "input")
    if not ok or not regs or #regs < 76 then
        -- Do not emit fabricated zeros on a failed primary read. Both hosts
        -- then expose staleness to their own safety/watchdog layer.
        return 1000
    end

    local base = 0x0000
    local l1_v = f32(regs, base, 0x00) or 0
    local l2_v = f32(regs, base, 0x02) or 0
    local l3_v = f32(regs, base, 0x04) or 0
    local l1_a = f32(regs, base, 0x06) or 0
    local l2_a = f32(regs, base, 0x08) or 0
    local l3_a = f32(regs, base, 0x0A) or 0
    local l1_w = f32(regs, base, 0x0C) or 0
    local l2_w = f32(regs, base, 0x0E) or 0
    local l3_w = f32(regs, base, 0x10) or 0
    local total_w = f32(regs, base, 0x34) or 0
    local hz = f32(regs, base, 0x46) or 0
    local import_wh = math.floor((f32(regs, base, 0x48) or 0) * 1000 + 0.5)
    local export_wh = math.floor((f32(regs, base, 0x4A) or 0) * 1000 + 0.5)

    -- The lowercase names are the canonical Sourceful telemetry v2 fields.
    -- Mixed-case aliases keep the current Blixt L1 data-model adapter working
    -- until it consumes the canonical names directly.
    host.emit("meter", {
        w = total_w,
        hz = hz,
        l1_v = l1_v,
        l2_v = l2_v,
        l3_v = l3_v,
        l1_a = l1_a,
        l2_a = l2_a,
        l3_a = l3_a,
        l1_w = l1_w,
        l2_w = l2_w,
        l3_w = l3_w,
        import_wh = import_wh,
        export_wh = export_wh,
        W = total_w,
        Hz = hz,
        L1_V = l1_v,
        L2_V = l2_v,
        L3_V = l3_v,
        L1_A = l1_a,
        L2_A = l2_a,
        L3_A = l3_a,
        L1_W = l1_w,
        L2_W = l2_w,
        L3_W = l3_w,
        total_import_Wh = import_wh,
        total_export_Wh = export_wh,
    })

    return 1000
end

function driver_command(action, value, context)
    if action == "init" or action == "deinit" then
        return true
    end
    return false
end

function driver_default_mode()
    -- Read-only meter: no control state to restore.
end

function driver_cleanup()
    -- No resources are retained between calls.
end
