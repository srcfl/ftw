-- Solis non-hybrid string inverter driver
-- Emits: PV
-- Protocol: Modbus TCP, input registers
--
-- Sign convention (site boundary -- positive W = into site):
--   PV w is always negative because generation reduces grid import.

DRIVER = {
  id           = "solis-string",
  name         = "Solis string inverter",
  manufacturer = "Ginlong Solis",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "pv" },
  description  = "Solis non-hybrid PV inverters via Modbus TCP.",
  homepage     = "https://www.ginlong.com",
  authors      = { "FTW contributors" },
  tested_models = { "S5-GC", "S6-GR1P", "3P-G4", "1P-G4" },
  verification_status = "experimental",
  verification_notes = "Read-only PV driver. Awaiting field verification on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

local sn_set = false

local function set_serial_from_config(config)
    if sn_set or type(config) ~= "table" then return end

    local sn = config.serial or config.sn
    if type(sn) == "string" then
        sn = sn:gsub("^%s+", ""):gsub("%s+$", "")
        if #sn > 0 then
            host.set_sn(sn)
            sn_set = true
        end
    end
end

function driver_init(config)
    host.set_make("Ginlong Solis")
    set_serial_from_config(config)
    host.log("info", "Solis string: driver_init")
end

local function read_input(addr, count)
    local ok, regs = pcall(host.modbus_read, addr, count, "input")
    if ok and regs then return regs end
    return nil
end

local function u16(regs, idx)
    if regs and regs[idx] then return regs[idx] end
    return 0
end

local function i16(regs, idx)
    if regs and regs[idx] then return host.decode_i16(regs[idx]) end
    return 0
end

local function u32be(regs, idx)
    if regs and regs[idx] and regs[idx + 1] then
        return host.decode_u32_be(regs[idx], regs[idx + 1])
    end
    return 0
end

function driver_poll()
    local energy_regs = read_input(3004, 6)
    local ac_w = u32be(energy_regs, 1)
    local dc_w = u32be(energy_regs, 3)
    local lifetime_wh = u32be(energy_regs, 5) * 1000

    local dc_regs = read_input(3021, 8)
    local mppt1_v = u16(dc_regs, 1) * 0.1
    local mppt1_a = u16(dc_regs, 2) * 0.1
    local mppt2_v = u16(dc_regs, 3) * 0.1
    local mppt2_a = u16(dc_regs, 4) * 0.1
    local mppt3_v = u16(dc_regs, 5) * 0.1
    local mppt3_a = u16(dc_regs, 6) * 0.1
    local mppt4_v = u16(dc_regs, 7) * 0.1
    local mppt4_a = u16(dc_regs, 8) * 0.1

    local ac_regs = read_input(3033, 6)
    local l1_v = u16(ac_regs, 1) * 0.1
    local l2_v = u16(ac_regs, 2) * 0.1
    local l3_v = u16(ac_regs, 3) * 0.1
    local l1_a = u16(ac_regs, 4) * 0.1
    local l2_a = u16(ac_regs, 5) * 0.1
    local l3_a = u16(ac_regs, 6) * 0.1

    local status_regs = read_input(3040, 4)
    local mode = u16(status_regs, 1)
    local temp_c = i16(status_regs, 2) * 0.1
    local hz = u16(status_regs, 3) * 0.01
    local status = u16(status_regs, 4)

    host.emit("pv", {
        w           = -ac_w,
        dc_w        = dc_w,
        mppt1_v     = mppt1_v,
        mppt1_a     = mppt1_a,
        mppt2_v     = mppt2_v,
        mppt2_a     = mppt2_a,
        mppt3_v     = mppt3_v,
        mppt3_a     = mppt3_a,
        mppt4_v     = mppt4_v,
        mppt4_a     = mppt4_a,
        l1_v        = l1_v,
        l2_v        = l2_v,
        l3_v        = l3_v,
        l1_a        = l1_a,
        l2_a        = l2_a,
        l3_a        = l3_a,
        hz          = hz,
        temp_c      = temp_c,
        status      = status,
        mode        = mode,
        lifetime_wh = lifetime_wh,
    })

    host.emit_metric("pv_dc_w", dc_w)
    host.emit_metric("pv_mppt1_v", mppt1_v)
    host.emit_metric("pv_mppt1_a", mppt1_a)
    host.emit_metric("pv_mppt2_v", mppt2_v)
    host.emit_metric("pv_mppt2_a", mppt2_a)
    host.emit_metric("pv_mppt3_v", mppt3_v)
    host.emit_metric("pv_mppt3_a", mppt3_a)
    host.emit_metric("pv_mppt4_v", mppt4_v)
    host.emit_metric("pv_mppt4_a", mppt4_a)
    host.emit_metric("inverter_l1_v", l1_v)
    host.emit_metric("inverter_l2_v", l2_v)
    host.emit_metric("inverter_l3_v", l3_v)
    host.emit_metric("inverter_l1_a", l1_a)
    host.emit_metric("inverter_l2_a", l2_a)
    host.emit_metric("inverter_l3_a", l3_a)
    host.emit_metric("inverter_temp_c", temp_c)
    host.emit_metric("grid_hz", hz)
    host.emit_metric("inverter_status", status)
    host.emit_metric("inverter_mode", mode)

    return 5000
end

function driver_command(action, power_w, cmd)
    return false
end

function driver_default_mode()
    -- Read-only PV driver; nothing to put back into autonomous mode.
end

function driver_cleanup()
    sn_set = false
end
