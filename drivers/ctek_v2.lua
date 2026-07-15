-- CTEK Chargestorm EV Charger Driver (Automation API v2)
-- Emits: EV
-- Protocol: Modbus/TCP
--
-- This file is the API v2 twin of drivers/ctek.lua. The two variants
-- are identical in every respect EXCEPT the base register addresses
-- (v1 uses 0x1xxx, v2 uses 0x2xxx). CTEK introduced the api_version
-- selector in CSOS 4.8.2 — v2 is required whenever you run both the
-- internal-meter Modbus server and the NANOGRID™ limit-control server
-- on the same CCU (CSOS allows at most one of them on api_version=1).
--
-- Hardware + firmware:
--   Requires a CTEK Chargestorm Connected 2 or Connected 3 running
--   CSOS (Chargestorm OS) firmware 4.9.3 or later. On the CCU:
--
--     Automation → ModbusTCPEnable                   = true
--     Automation → modbus_tcp_automation_api_version = 2
--
--   Use drivers/ctek.lua if the CCU is still on api_version=1.
--
-- Unit identifier selects the outlet on dual-outlet stations:
--   unit_id = 1 → EVSE1 (left outlet, or single-outlet station)
--   unit_id = 2 → EVSE2 (right outlet)
--
-- Register map (source: CTEK "Automation interface" v1.0, rev 6b4af7,
-- API v2 column):
--
--   Identity / meter type:
--     0x2000         API version       (u16)
--     0x2001         API status, 0=OK  (u16)
--     0x2002         EnergyMeterType   (u16 enum)
--     0x2003..0x2008 Serial (12 ASCII chars, 2 per register, big-endian bytes)
--
--   Telemetry (one contiguous read 0x2100..0x2108 = 9 regs):
--     0x2100..0x2101 Lifetime energy (Wh, u32 big-endian, high word first)
--     0x2102..0x2104 Per-phase current, L1/L2/L3 (u16 × 10⁻³ A, i.e. mA)
--     0x2105..0x2107 Per-phase voltage L1-N/L2-N/L3-N (u16 × 10⁻¹ V)
--     0x2108         Total active power (u16 W)
--
--   Control:
--     0x2200         Charging limit (u16 A, read/write) — 0 disables
--                    charging; values 1..5 are treated as 0 by the
--                    charger (IEC 61851 minimum is 6 A). Setpoint is
--                    lost on charger restart.
--     0x2201         Maximum assignment (u16 A, read-only) — upper
--                    bound the charger will accept given current
--                    de-rating, schedules, NANOGRID™ curtailment, etc.
--
-- Sign convention (SITE = positive W flows INTO the site):
--   ev.w: always positive when charging — an EVSE is a one-way load;
--   there's no vehicle-to-grid path on Chargestorm.
--
-- Config example (config.yaml):
--   drivers:
--     - name: ctek
--       lua: drivers/ctek_v2.lua
--       capabilities:
--         modbus:
--           host: 192.168.1.190
--           port: 502
--           unit_id: 1         # 1 = EVSE1, 2 = EVSE2
--       config:
--         phases:    3          # 1 or 3; default 3
--         min_a:     6          # minimum charge current (A); default 6
--         max_a:     16         # fuse-limited max (A); default 16
--         voltage_v: 230        # nominal per-phase voltage; default 230

DRIVER = {
  id           = "ctek-chargestorm-v2",
  name         = "CTEK Chargestorm (API v2)",
  manufacturer = "CTEK",
  version      = "0.2.0",
  protocols    = { "modbus" },
  capabilities = { "ev" },
  description  = "CTEK Chargestorm Connected 2/3 via Modbus/TCP Automation API v2 (CSOS ≥ 4.9.3). Full telemetry + current-limit control.",
  homepage     = "https://www.ctek.com",
  authors      = { "FTW contributors" },
  tested_models = { "Chargestorm Connected 2", "Chargestorm Connected 3" },
  verification_status = "beta",
  verification_notes = "Register map per CTEK Automation interface v1.0 (API v2 offsets); charging-limit write verified against CSOS 4.9.x. Derived charging/connected flags approximate the real EVSE state since the state code is MQTT-only.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Register addresses — API v2.
-- Mirror of drivers/ctek.lua with every 0x1xxx offset shifted to 0x2xxx.
-- Keep the two files in sync when register semantics change.
----------------------------------------------------------------------------
local REG_API_VERSION   = 0x2000
local REG_API_STATUS    = 0x2001
local REG_METER_TYPE    = 0x2002
local REG_SERIAL_BASE   = 0x2003   -- 6 regs → 12 ASCII chars
local REG_TELEMETRY     = 0x2100   -- 9 regs: energy(2) + I(3) + V(3) + W(1)
local REG_CHARGE_LIMIT  = 0x2200   -- r/w
local REG_MAX_ASSIGN    = 0x2201   -- r/o

----------------------------------------------------------------------------
-- Runtime config (overridden from config.yaml in driver_init)
----------------------------------------------------------------------------
local phases    = 3
local min_a     = 6
local max_a     = 16
local voltage_v = 230

local last_set_a = 0
local sn_read = false

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

local function clamp_amps(a)
    if a == nil then return 0 end
    a = math.floor(a + 0.5)
    if a <= 0 then return 0 end
    if a < min_a then return 0 end
    if a > max_a then return max_a end
    return a
end

local function watts_to_amps(power_w)
    if not power_w or power_w <= 0 then return 0 end
    return math.floor((power_w / voltage_v / phases) + 0.5)
end

local function decode_ascii(regs, n)
    local s = ""
    for i = 1, n do
        local r = regs[i] or 0
        local hi = math.floor(r / 256)
        local lo = r % 256
        if hi == 0 then break end
        if hi >= 32 and hi < 127 then s = s .. string.char(hi) end
        if lo == 0 then break end
        if lo >= 32 and lo < 127 then s = s .. string.char(lo) end
    end
    return s
end

local function write_setpoint(amps)
    local err = host.modbus_write(REG_CHARGE_LIMIT, amps)
    if err ~= nil and err ~= "" then
        host.log("warn", "CTEK: write charging limit failed: " .. tostring(err))
        return false
    end
    last_set_a = amps
    return true
end

local function read_setpoint()
    local ok, regs = pcall(host.modbus_read, REG_CHARGE_LIMIT, 1, "holding")
    if not ok or not regs or not regs[1] then return nil end
    return regs[1]
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("CTEK")

    if config then
        if tonumber(config.phases) then
            local p = math.floor(tonumber(config.phases))
            if p == 1 or p == 3 then phases = p end
        end
        if tonumber(config.min_a)     then min_a     = math.floor(tonumber(config.min_a))     end
        if tonumber(config.max_a)     then max_a     = math.floor(tonumber(config.max_a))     end
        if tonumber(config.voltage_v) then voltage_v = tonumber(config.voltage_v)             end
    end

    if min_a < 6 then min_a = 6 end
    if max_a < min_a then max_a = min_a end

    local ok, api_regs = pcall(host.modbus_read, REG_API_VERSION, 2, "holding")
    if ok and api_regs and api_regs[1] then
        local api_ver    = api_regs[1]
        local api_status = api_regs[2] or 0
        host.log("info", string.format(
            "CTEK: API v%d, status %d (expected v2 from this driver; use drivers/ctek.lua for v1)",
            api_ver, api_status))
    end

    host.log("info", string.format(
        "CTEK: driver initialized (phases=%d, min=%dA, max=%dA, V=%.0f)",
        phases, min_a, max_a, voltage_v))

    local cur = read_setpoint()
    if cur then
        last_set_a = cur
        host.log("info", "CTEK: charge limit readback = " .. tostring(cur) .. "A")
    end
end

function driver_poll()
    if not sn_read then
        local ok_sn, sn_regs = pcall(host.modbus_read, REG_SERIAL_BASE, 6, "holding")
        if ok_sn and sn_regs then
            local sn = decode_ascii(sn_regs, 6)
            if #sn > 0 then
                host.set_sn(sn)
                sn_read = true
            end
        end
    end

    local ok_tel, tel = pcall(host.modbus_read, REG_TELEMETRY, 9, "holding")
    local ev_w     = 0
    local i_l1, i_l2, i_l3 = 0, 0, 0
    local v_l1, v_l2, v_l3 = 0, 0, 0
    local lifetime_wh = 0
    if ok_tel and tel then
        lifetime_wh = host.decode_u32_be(tel[1], tel[2])
        i_l1        = (tel[3] or 0) / 1000
        i_l2        = (tel[4] or 0) / 1000
        i_l3        = (tel[5] or 0) / 1000
        v_l1        = (tel[6] or 0) / 10
        v_l2        = (tel[7] or 0) / 10
        v_l3        = (tel[8] or 0) / 10
        ev_w        = tel[9] or 0
    else
        host.log("warn", "CTEK: telemetry block read failed")
    end

    local limit, max_assign = last_set_a, max_a
    local ok_ctl, ctl = pcall(host.modbus_read, REG_CHARGE_LIMIT, 2, "holding")
    if ok_ctl and ctl then
        limit       = ctl[1] or last_set_a
        max_assign  = ctl[2] or max_a
        last_set_a  = limit
    end

    local max_phase_a = math.max(i_l1, i_l2, i_l3)
    local charging = (ev_w > 100) or (max_phase_a > 1.0)
    local connected = charging or (limit >= min_a and max_assign > 0)

    host.emit("ev", {
        w           = ev_w,
        connected   = connected,
        charging    = charging,
        max_a       = limit,
        phases      = phases,
        l1_v        = v_l1, l2_v = v_l2, l3_v = v_l3,
        l1_a        = i_l1, l2_a = i_l2, l3_a = i_l3,
        lifetime_wh = lifetime_wh,
    })

    host.emit_metric("ev_set_current_a",  limit)
    host.emit_metric("ev_max_assign_a",   max_assign)
    host.emit_metric("ev_l1_a",           i_l1)
    host.emit_metric("ev_l2_a",           i_l2)
    host.emit_metric("ev_l3_a",           i_l3)
    host.emit_metric("ev_l1_v",           v_l1)
    host.emit_metric("ev_l2_v",           v_l2)
    host.emit_metric("ev_l3_v",           v_l3)
    host.emit_metric("ev_power_w",        ev_w)
    host.emit_metric("ev_lifetime_wh",    lifetime_wh)

    return 5000
end

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end

    if action == "ev_set_current" then
        local amps = clamp_amps(watts_to_amps(power_w))
        host.log("debug", "CTEK: ev_set_current "
            .. tostring(power_w) .. "W → " .. tostring(amps) .. "A")
        return write_setpoint(amps)
    end

    if action == "ev_pause" then
        return write_setpoint(0)
    end

    if action == "ev_start" or action == "ev_resume" then
        local amps = (last_set_a and last_set_a >= min_a) and last_set_a or max_a
        return write_setpoint(amps)
    end

    host.log("warn", "CTEK: unknown action " .. tostring(action))
    return false
end

function driver_default_mode()
end

function driver_cleanup()
end
