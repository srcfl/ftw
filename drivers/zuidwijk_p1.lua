-- zuidwijk_p1.lua
-- Zuidwijk P1 Reader Ethernet driver (Dutch DSMR 5.0 smart-meter passthrough).
-- Emits: Meter (read-only)
-- Protocol: raw TCP — the device is a Serial-to-Ethernet bridge that streams
--           the unsolicited P1 telegram from the meter once per second on
--           a configurable TCP port (default 23). No request is needed and
--           no decryption is performed by the device itself; encrypted
--           meter data (rare in NL) must be decrypted by an external proxy
--           before reaching this driver.

DRIVER = {
  id           = "zuidwijk-p1",
  name         = "Zuidwijk P1 Reader Ethernet",
  manufacturer = "Zuidwijk",
  version      = "1.0.0",
  protocols    = { "tcp" },
  capabilities = { "meter" },
  description  = "Dutch DSMR P1 smart-meter via Zuidwijk Serial-to-Ethernet bridge (raw TCP, port 23).",
  homepage     = "https://www.zuidwijk.com/product/p1-reader-ethernet/",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "Sagemcom T210-D", "Kaifa MA105/MA304", "Iskra ME382" },
  verification_status = "experimental",
  verification_notes = "Implemented from DSMR 5.0 spec; not yet exercised against live hardware.",
  connection_defaults = {
    port = 23,
  },
}
--
-- Sign convention (site / EMS):
--   meter.w  : positive = importing from grid, negative = exporting
-- DSMR reports import (1.7.0) and export (2.7.0) as separate non-negative
-- values; the driver computes meter.w = import - export so the site
-- convention is satisfied without any extra flipping.

PROTOCOL = "tcp"

----------------------------------------------------------------------------
-- Config + state
----------------------------------------------------------------------------

local HOST    = nil
local PORT    = 23
local ADDR    = nil
local buffer  = ""
local last_telegram_ms = -1
local sn_set = false
local crc_errors = 0

-- Cap on retained buffer size between polls so a stuck framer (meter spewing
-- non-DSMR bytes, partial decryption garbage) doesn't grow unbounded. Real
-- telegrams are ~1 KB; 8 KB leaves plenty of slack for one telegram plus
-- one in-flight follow-up before we drop oldest.
local MAX_BUFFER = 8192

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- Bitwise helpers. Lua 5.1 has no bit operators and gopher-lua does not
-- expose `bit32`, so the CRC implementation reaches for these arithmetic
-- equivalents. Cost is ~16 iterations per XOR — negligible for a 1 KB
-- telegram once per second.
local function xor16(a, b)
    local r, p = 0, 1
    for _ = 1, 16 do
        local aa, bb = a % 2, b % 2
        if aa ~= bb then r = r + p end
        a = math.floor(a / 2)
        b = math.floor(b / 2)
        p = p * 2
    end
    return r
end

-- DSMR CRC16: polynomial x^16+x^15+x^2+1 (0xA001 in reflected form), init
-- 0x0000, computed over bytes from the leading '/' through the trailing
-- '!' inclusive. Result is printed in the telegram as 4 uppercase hex
-- digits followed by CR LF. This matches CRC-16/IBM (a.k.a. ARC) and the
-- formulation used by Sagemcom / Kaifa / Iskra DSMR-5 meters.
local function crc16_dsmr(data)
    local crc = 0
    for i = 1, #data do
        crc = xor16(crc, string.byte(data, i))
        for _ = 1, 8 do
            if (crc % 2) == 1 then
                crc = xor16(math.floor(crc / 2), 0xA001)
            else
                crc = math.floor(crc / 2)
            end
        end
    end
    return crc
end

-- Decode a hex-ASCII serial like "4530303834303031383239353439393137"
-- into "E0084001829549917". DSMR stores 96.1.1 / 96.1.0 that way.
local function hex_to_ascii(s)
    if not s or s == "" then return "" end
    local out = string.gsub(s, "(%x%x)", function(h)
        local b = tonumber(h, 16)
        if b == nil then return "" end
        return string.char(b)
    end)
    return out
end

-- Split a DSMR value string like "00123.456*kWh" into (number, unit).
-- Tolerates a missing unit ("0001" for tariff index, etc.).
local function num_unit(s)
    if not s then return nil, nil end
    local n, u = string.match(s, "^([%-%d%.]+)%*?(.*)$")
    if not n then return tonumber(s), nil end
    return tonumber(n), u
end

-- Pull the LAST parenthesised group out of a DSMR value chunk. Lines like
-- "0-1:24.2.1(241015095500S)(00123.456*m3)" carry a timestamp followed
-- by the actual reading; we want the reading.
local function last_paren(args)
    if not args then return nil end
    local last
    for chunk in string.gmatch(args, "%(([^()]*)%)") do
        last = chunk
    end
    return last
end

-- Extract the most recent complete DSMR frame from `buf`. Returns
-- (frame_body, crc_hex, remaining_buffer) where:
--   * frame_body excludes the trailing '!' + CRC + CRLF, ready for OBIS parse
--   * crc_hex is the 4-char uppercase hex from the wire, or "" when the
--     meter sent the no-CRC fallback ('!\r\n', DSMR 4 / passthrough firmware)
-- Returns (nil, "", buf) if no complete frame is present yet.
--
-- A frame looks like:
--     /XMX5LGBBFFB231215493\r\n
--     \r\n
--     <obis lines>\r\n
--     !ABCD\r\n
-- where ABCD is the CRC16 over '/' through '!' inclusive. We prefer the
-- variant with CRC; if absent, fall back to the bare '!' terminator
-- (DSMR 4 firmware, encrypted-passthrough proxies).
local function take_frame(buf)
    -- Locate end-of-telegram. '!XXXX\r\n' first (DSMR 5), bare '!\r\n' fallback.
    local bang_s, eot = string.find(buf, "!%x%x%x%x\r?\n")
    local crc_hex = ""
    if bang_s then
        crc_hex = string.upper(buf:sub(bang_s + 1, bang_s + 4))
    else
        bang_s, eot = string.find(buf, "!\r?\n")
    end
    if not bang_s then
        return nil, "", buf
    end
    -- Find the latest '/' at or before the bang — start-of-frame marker.
    local start_s
    local from = 1
    while true do
        local s = string.find(buf, "/", from, true)
        if not s or s > bang_s then break end
        start_s = s
        from    = s + 1
    end
    if not start_s then
        -- '!' with no preceding '/': junk before the first frame. Discard
        -- through the bang so we don't re-scan the same byte every poll.
        return nil, "", buf:sub(eot + 1)
    end
    local body = buf:sub(start_s, bang_s - 1)
    return body, crc_hex, buf:sub(eot + 1)
end

-- Verify the wire CRC matches a CRC computed over '/' .. body .. '!'.
-- Returns true only when the CRC matches OR when no CRC bytes were on
-- the wire at all (the DSMR 4 / encrypted-passthrough '!\r\n' fallback,
-- which take_frame surfaces as crc_hex == ""). The literal hex "0000" is
-- treated as a real CRC value — a frame that genuinely computes to 0x0000
-- must round-trip cleanly, and we mustn't blanket-accept any "!0000\r\n"
-- trailer regardless of body content.
local function crc_ok(body, crc_hex)
    if crc_hex == "" then
        return true
    end
    local want = tonumber(crc_hex, 16)
    if want == nil then return false end
    local got = crc16_dsmr(body .. "!")
    return got == want
end

-- Parse a DSMR frame body into a {obis_code -> raw_args_string} table.
-- Args is the full parenthesised tail of the line, e.g. "(01.234*kW)".
local function parse_obis(frame)
    local out = {}
    for line in string.gmatch(frame, "[^\r\n]+") do
        local code, rest = string.match(line, "^([%d%-:.]+)(%(.*)$")
        if code then
            out[code] = rest
        end
    end
    return out
end

-- Pull a numeric DSMR value with optional unit suffix. Returns nil when the
-- key is absent so callers can leave the meter field unset.
local function obis_num(o, code)
    local args = o[code]
    if not args then return nil end
    local v = last_paren(args)
    local n = num_unit(v)
    return n
end

local function obis_str(o, code)
    local args = o[code]
    if not args then return nil end
    return last_paren(args)
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Zuidwijk")
    -- Bridge SN we don't know yet; meter serial flows in via OBIS 96.1.1 on
    -- the first received telegram and becomes the anchored hardware identity.

    if config then
        if config.host then HOST = tostring(config.host) end
        if config.port then PORT = tonumber(config.port) or 23 end
    end
    if HOST == nil or HOST == "" then
        host.log("error", "zuidwijk-p1: config.host is required (e.g. \"192.168.1.40\")")
        return
    end
    ADDR = string.format("%s:%d", HOST, PORT)

    -- Poll the buffer every second — telegrams arrive at ~1 Hz, faster polls
    -- just spin. The host's read pump runs independently in Go and buffers
    -- bytes as they land; we drain it here.
    host.set_poll_interval(1000)
    host.log("info", "zuidwijk-p1: configured for " .. ADDR)
end

function driver_poll()
    -- (Re)establish the TCP socket. The cap's Open is idempotent — calling
    -- it on an already-open connection is a no-op, so this is also the
    -- reconnect path after a dropped session (router reboot, P1 reader
    -- power-cycle).
    if not ADDR then return 5000 end
    if not host.tcp_is_open() then
        local ok, err = host.tcp_open(ADDR)
        if not ok then
            host.log("warn", "zuidwijk-p1: tcp_open " .. ADDR .. " failed: " .. tostring(err))
            -- Back off briefly before retrying so a misconfigured host
            -- doesn't hammer the network with reconnects every second.
            return 5000
        end
        buffer = ""
    end

    -- Drain any new bytes from the read pump and append to our framing buffer.
    local chunk = host.tcp_recv()
    if chunk and chunk ~= "" then
        buffer = buffer .. chunk
        if #buffer > MAX_BUFFER then
            -- Drop the oldest half so a runaway non-DSMR stream can't pin
            -- memory while we wait for a valid frame boundary.
            buffer = buffer:sub(#buffer - (MAX_BUFFER / 2))
        end
    end

    -- Pull every complete frame the buffer holds. If multiple have arrived
    -- (e.g. we polled twice as slowly as the meter speaks) we keep the
    -- LAST one with a valid CRC — that's the freshest TRUSTED snapshot
    -- of meter state. Frames with a bad CRC are counted and dropped; a
    -- noisy connection that produces only bad frames simply emits
    -- nothing, the watchdog then catches it as stale.
    local latest = nil
    while true do
        local frame, crc_hex, rest = take_frame(buffer)
        buffer = rest
        if not frame then break end
        if crc_ok(frame, crc_hex) then
            latest = frame
        else
            crc_errors = crc_errors + 1
            host.log("warn", "zuidwijk-p1: CRC mismatch — dropping frame (total bad: " .. crc_errors .. ")")
        end
    end
    host.emit_metric("p1_crc_errors", crc_errors)
    if not latest then
        return 1000
    end

    local o = parse_obis(latest)

    -- Meter serial (hex-ASCII) — anchor hardware identity. Retry on every
    -- telegram until 96.1.1 actually decodes: if the first frame we see
    -- happens to be missing the SN line (truncated, partial buffer, or a
    -- meter that publishes the long form only every Nth telegram) we'd
    -- otherwise spend the whole process lifetime without a make:serial
    -- anchor, falling back to MAC/endpoint and orphaning the persistent
    -- battery model on the next driver rename.
    if not sn_set then
        local sn_hex = obis_str(o, "0-0:96.1.1")
        if sn_hex and sn_hex ~= "" then
            local sn = hex_to_ascii(sn_hex)
            if sn ~= "" then
                host.set_sn(sn)
                sn_set = true
            end
        end
    end
    last_telegram_ms = host.millis()

    -- Active power in kW. DSMR sends import + export as separate positive
    -- values; the difference gives site-convention W.
    local p_imp_kw = obis_num(o, "1-0:1.7.0") or 0
    local p_exp_kw = obis_num(o, "1-0:2.7.0") or 0
    local p_imp_l1 = obis_num(o, "1-0:21.7.0") or 0
    local p_imp_l2 = obis_num(o, "1-0:41.7.0") or 0
    local p_imp_l3 = obis_num(o, "1-0:61.7.0") or 0
    local p_exp_l1 = obis_num(o, "1-0:22.7.0") or 0
    local p_exp_l2 = obis_num(o, "1-0:42.7.0") or 0
    local p_exp_l3 = obis_num(o, "1-0:62.7.0") or 0

    local meter = {
        w    = (p_imp_kw - p_exp_kw) * 1000,
        l1_w = (p_imp_l1 - p_exp_l1) * 1000,
        l2_w = (p_imp_l2 - p_exp_l2) * 1000,
        l3_w = (p_imp_l3 - p_exp_l3) * 1000,
    }

    -- Per-phase voltage (DSMR units: V) — always reported on DSMR 5.
    meter.l1_v = obis_num(o, "1-0:32.7.0")
    meter.l2_v = obis_num(o, "1-0:52.7.0")
    meter.l3_v = obis_num(o, "1-0:72.7.0")
    -- Per-phase current (DSMR units: A). Unsigned on the wire — direction
    -- is recoverable from l*_w. Pass through unsigned to match the meter.
    meter.l1_a = obis_num(o, "1-0:31.7.0")
    meter.l2_a = obis_num(o, "1-0:51.7.0")
    meter.l3_a = obis_num(o, "1-0:71.7.0")

    -- Lifetime energy counters. DSMR reports them split by tariff (T1 = low
    -- / off-peak, T2 = high / peak). The dashboard wants a single import
    -- and export figure, so sum the tariffs. Units are kWh → Wh.
    local imp_t1 = obis_num(o, "1-0:1.8.1") or 0
    local imp_t2 = obis_num(o, "1-0:1.8.2") or 0
    local exp_t1 = obis_num(o, "1-0:2.8.1") or 0
    local exp_t2 = obis_num(o, "1-0:2.8.2") or 0
    if imp_t1 > 0 or imp_t2 > 0 then
        meter.import_wh = (imp_t1 + imp_t2) * 1000
    end
    if exp_t1 > 0 or exp_t2 > 0 then
        meter.export_wh = (exp_t1 + exp_t2) * 1000
    end

    host.emit("meter", meter)

    -- Long-format TS diagnostics. The same fields the dashboard meter card
    -- consumes, plus DSMR-specific niceties (per-tariff counters, failure
    -- log scalars, gas reading) for operators that want the raw stream.
    if meter.l1_w then host.emit_metric("meter_l1_w", meter.l1_w) end
    if meter.l2_w then host.emit_metric("meter_l2_w", meter.l2_w) end
    if meter.l3_w then host.emit_metric("meter_l3_w", meter.l3_w) end
    if meter.l1_v then host.emit_metric("meter_l1_v", meter.l1_v) end
    if meter.l2_v then host.emit_metric("meter_l2_v", meter.l2_v) end
    if meter.l3_v then host.emit_metric("meter_l3_v", meter.l3_v) end
    if meter.l1_a then host.emit_metric("meter_l1_a", meter.l1_a) end
    if meter.l2_a then host.emit_metric("meter_l2_a", meter.l2_a) end
    if meter.l3_a then host.emit_metric("meter_l3_a", meter.l3_a) end

    -- Tariff index (1 = low, 2 = high, etc.) — useful for cost analytics.
    local tariff = obis_num(o, "0-0:96.14.0")
    if tariff then host.emit_metric("p1_tariff", tariff) end

    -- Power failure counters (short + long). Climbing values indicate grid
    -- stability problems that an operator wants to see in the TS DB.
    local pf  = obis_num(o, "0-0:96.7.21")
    local lpf = obis_num(o, "0-0:96.7.9")
    if pf  then host.emit_metric("p1_power_failures",      pf)  end
    if lpf then host.emit_metric("p1_long_power_failures", lpf) end

    -- Gas counter (M-Bus channel 1, usually). Cumulative m³ reading. Logged
    -- as a metric so a daily-delta query yields gas consumption without
    -- adding a new emit type to the rest of the stack.
    local gas_m3 = obis_num(o, "0-1:24.2.1")
    if gas_m3 then host.emit_metric("gas_m3", gas_m3) end

    return 1000
end

----------------------------------------------------------------------------
-- Control (READ-ONLY — P1 is one-way out of the meter)
----------------------------------------------------------------------------

function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end
    host.log("warn", "zuidwijk-p1: read-only driver, ignoring action=" .. tostring(action))
    return false
end

function driver_default_mode()
    -- Read-only: nothing to revert.
end

function driver_cleanup()
    pcall(host.tcp_close)
    buffer = ""
    last_telegram_ms = -1
    sn_set = false
    crc_errors = 0
end
