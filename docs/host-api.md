# Host API Reference

> Complete reference for the `host` capability interface exposed to
> **Lua** driver runtimes. For a tutorial-style introduction to writing
> Lua drivers, see [writing-a-driver.md](writing-a-driver.md). The
> authoritative Lua-specific source is the top-of-file comment in
> `go/internal/drivers/lua.go`.
>
> **Note:** The legacy WASM driver runtime has been removed from the
> codebase. This document covers the Lua host API only. For historical
> WASM ABI details, see the git history of `go/internal/drivers/runtime.go`.

The `host` table is available to all Lua drivers. It provides functions for logging, device communication, data decoding, JSON handling, and telemetry emission. These are the same APIs available on the Sourceful Zap gateway, so drivers are portable between forty-two-watts and the Zap.

## Core

### `host.log(message)` / `host.log(level, message)`

Log a message to the forty-two-watts structured logging system.

**Parameters:**
- `message` (string) -- the log message (single-argument form logs at `info` level)
- `level` (string, optional) -- log level: `"debug"`, `"info"`, `"warn"`, `"error"` (or short forms `"D"`, `"I"`, `"W"`, `"E"`)

**Returns:** nothing

**Example:**
```lua
host.log("driver started")
host.log("debug", "register 100 = " .. tostring(val))
host.log("warn", "communication timeout, retrying")
```

**Tip:** Set `RUST_LOG=lua=debug` when running forty-two-watts to see debug-level driver logs.

---

### `host.millis()`

Returns the number of milliseconds since the forty-two-watts process started. Useful for timing intervals within a driver.

**Parameters:** none

**Returns:** integer -- uptime in milliseconds

**Example:**
```lua
local start = host.millis()
-- ... do work ...
local elapsed = host.millis() - start
host.log("debug", "poll took " .. elapsed .. "ms")
```

---

### `host.timestamp()`

Returns the current Unix timestamp as a floating-point number (seconds since epoch).

**Parameters:** none

**Returns:** number -- Unix timestamp in seconds (with sub-second precision)

**Example:**
```lua
local now = host.timestamp()
host.log("debug", "timestamp: " .. tostring(now))
```

---

### `host.sleep(ms)`

Sleep the current driver thread for the specified number of milliseconds. Capped at 5000ms to prevent blocking the thread indefinitely.

**Parameters:**
- `ms` (integer) -- duration to sleep in milliseconds (max 5000)

**Returns:** nothing

**Example:**
```lua
host.sleep(500)  -- wait 500ms between register reads
```

---

### `host.pool_free()`

Returns memory usage information for the driver's Lua VM.

**Parameters:** none

**Returns:** two values:
- `free_bytes` (integer) -- available memory in bytes
- `total_bytes` (integer) -- total memory limit in bytes

**Example:**
```lua
local free, total = host.pool_free()
host.log("debug", string.format("memory: %d/%d bytes free", free, total))
```

---

### `host.set_make(brand)`

Set the device manufacturer/brand name. Call this in `driver_init()`. Used in telemetry payloads and the API.

**Parameters:**
- `brand` (string) -- manufacturer name (e.g., `"Sungrow"`, `"Ferroamp"`)

**Returns:** nothing

**Example:**
```lua
function driver_init(config)
    host.set_make("Sungrow")
end
```

---

### `host.set_sn(serial)`

Override the device serial number. Useful when the driver reads the serial from the device itself (e.g., from Modbus registers).

**Parameters:**
- `serial` (string) -- device serial number

**Returns:** nothing

**Example:**
```lua
local sn_regs = host.modbus_read(4990, 10, "input")
if sn_regs then
    local sn = ""
    for i = 1, 10 do
        local hi = math.floor(sn_regs[i] / 256)
        local lo = sn_regs[i] % 256
        if hi > 32 and hi < 127 then sn = sn .. string.char(hi) end
        if lo > 32 and lo < 127 then sn = sn .. string.char(lo) end
    end
    host.set_sn(sn)
end
```

---

## MQTT

Available when the driver's config includes an `mqtt` section. The runtime automatically connects to the MQTT broker before `driver_init()` is called.

### `host.mqtt_subscribe(topic)`

Subscribe to an MQTT topic. Supports MQTT wildcards (`#` for multi-level, `+` for single-level).

**Parameters:**
- `topic` (string) -- MQTT topic pattern

**Returns:** boolean -- `true` on success, `false` on failure

**Example:**
```lua
function driver_init(config)
    host.mqtt_subscribe("device/data/#")
    host.mqtt_subscribe("device/+/status")
end
```

---

### `host.mqtt_messages()`

Drain all buffered MQTT messages received since the last call. Messages are buffered by a background reader; this function returns them all at once.

**Parameters:** none

**Returns:** table -- array of message tables, each with:
- `topic` (string) -- the topic the message was published on
- `payload` (string) -- the message payload (typically JSON)

The returned table is a Lua array (1-indexed). If no messages are available, an empty table is returned.

**Example:**
```lua
function driver_poll()
    local messages = host.mqtt_messages()
    for _, msg in ipairs(messages) do
        host.log("debug", "topic=" .. msg.topic .. " payload=" .. msg.payload)
        local ok, data = pcall(host.json_decode, msg.payload)
        if ok and data then
            -- process data
        end
    end
    return 1000
end
```

**Note:** The internal message queue is capped at 1000 messages. If the queue is full, the oldest message is discarded when a new one arrives.

---

### `host.mqtt_publish(topic, payload)`

Publish a message to an MQTT topic (QoS 0).

**Parameters:**
- `topic` (string) -- MQTT topic
- `payload` (string) -- message payload

**Returns:** boolean -- `true` on success, `false` on failure

**Example:**
```lua
local payload = string.format('{"cmd":"charge","power":%d}', 3000)
local ok = host.mqtt_publish("device/control", payload)
if not ok then
    host.log("warn", "failed to publish control command")
end
```

---

## Modbus

Available when the driver's config includes a `modbus` section. The runtime automatically establishes the Modbus TCP connection before `driver_init()` is called.

### `host.modbus_read(register, count, type)`

Read consecutive registers from the Modbus device.

**Parameters:**
- `register` (integer) -- starting register address
- `count` (integer) -- number of registers to read (1-125)
- `type` (string, optional) -- register type: `"holding"` (function code 0x03, default) or `"input"` (function code 0x04)

**Returns:**
- On success: table -- 1-indexed Lua table of uint16 values
- On failure: `nil`

**Example:**
```lua
-- Read 2 input registers starting at address 5016
local regs = host.modbus_read(5016, 2, "input")
if regs then
    local power = host.decode_u32_le(regs[1], regs[2])
    host.log("debug", "power = " .. power .. "W")
end

-- Read holding registers (default type)
local holding = host.modbus_read(40000, 4)
```

**Best practice:** Always wrap in `pcall()` for graceful error handling:

```lua
local ok, regs = pcall(host.modbus_read, 5016, 2, "input")
if not ok or not regs then
    host.log("warn", "failed to read registers")
    return 5000
end
```

---

### `host.modbus_write(register, value)`

Write a single holding register (function code 0x06).

**Parameters:**
- `register` (integer) -- register address
- `value` (integer) -- uint16 value to write (0-65535)

**Returns:** boolean -- `true` on success, `false` on failure

**Example:**
```lua
-- Set operating mode to auto (register 13049, value 0)
local ok = host.modbus_write(13049, 0)
```

---

### `host.modbus_write_multiple(register, values)`

Write multiple consecutive holding registers (function code 0x10).

**Parameters:**
- `register` (integer) -- starting register address
- `values` (table) -- 1-indexed Lua table of uint16 values

**Returns:** boolean -- `true` on success, `false` on failure

**Example:**
```lua
-- Write 3 consecutive registers starting at address 40000
local ok = host.modbus_write_multiple(40000, {1000, 2000, 3000})
```

---

## TCP

Raw TCP socket access. Available when the driver's config grants `capabilities.tcp` with an optional `allowed_hosts` allowlist. Used by drivers that talk to serial-to-Ethernet passthrough bridges or other protocols that stream unsolicited bytes over a long-lived TCP connection — Dutch P1 smart-meter readers streaming DSMR telegrams on port 23 being the canonical example. See `drivers/zuidwijk_p1.lua` for a full DSMR 5 parser built on this capability.

The host runs a background read pump per driver that buffers incoming bytes in a 64 KiB ring. The driver drains the buffer at its own poll cadence and does its own framing — TCP is byte-stream, not message-framed. Read-only by design today: the supported targets never need driver-initiated writes, and arbitrary outbound TCP would dramatically widen the blast radius if a driver ever ran untrusted code.

**Config:**
```yaml
- name: house-meter
  lua: drivers/zuidwijk_p1.lua
  capabilities:
    tcp:
      allowed_hosts: ["192.168.1.40:23"]   # bare "host" allows any port
  config:
    host: "192.168.1.40"
    port: 23
```

### `host.tcp_open(addr)`

Open a TCP connection to `"host:port"`. Idempotent — calling on an already-open socket is a no-op, so this is also the reconnect path.

**Parameters:**
- `addr` (string) -- `"host:port"`, e.g. `"192.168.1.40:23"`

**Returns:**
- On success: `(true, nil)`
- On failure: `(nil, error_string)` — bad allowlist, dial error, etc.

---

### `host.tcp_recv()`

Drain bytes received since the last call. Non-blocking. Returns an empty string when nothing has arrived; the driver does its own framing on the accumulated buffer.

**Returns:** string -- raw bytes since the last call (Lua strings are 8-bit clean, so binary payloads work)

**Example:**
```lua
local chunk = host.tcp_recv()
if chunk ~= "" then
    buffer = buffer .. chunk
    -- ...split into frames, parse, emit
end
```

---

### `host.tcp_is_open()`

Returns `true` while the read pump is alive. Flips to `false` on EOF or read error; the driver should `tcp_close` and `tcp_open` again on the next poll to recover.

---

### `host.tcp_close()`

Close the socket and release the read pump. Safe to call on an already-closed cap.

---

## Decode Helpers

Always available regardless of protocol. Used to interpret raw register values from Modbus devices.

### `host.decode_i16(val)`

Interpret a uint16 value as a signed int16.

**Parameters:**
- `val` (integer) -- uint16 value (0-65535)

**Returns:** number -- signed value (-32768 to 32767)

**Example:**
```lua
local regs = host.modbus_read(5007, 1, "input")
local temperature = host.decode_i16(regs[1]) * 0.1  -- -3276.8 to 3276.7
```

---

### `host.decode_u32(hi, lo)`

Combine two uint16 registers (big-endian) into an unsigned 32-bit integer.

**Parameters:**
- `hi` (integer) -- high word (most significant 16 bits)
- `lo` (integer) -- low word (least significant 16 bits)

**Returns:** number -- unsigned 32-bit value

**Example:**
```lua
local regs = host.modbus_read(100, 2, "input")
local energy_wh = host.decode_u32(regs[1], regs[2])
```

---

### `host.decode_i32(hi, lo)`

Combine two uint16 registers (big-endian) into a signed 32-bit integer.

**Parameters:**
- `hi` (integer) -- high word
- `lo` (integer) -- low word

**Returns:** number -- signed 32-bit value

**Example:**
```lua
local regs = host.modbus_read(100, 2, "input")
local power_w = host.decode_i32(regs[1], regs[2])
```

---

### `host.decode_u32_le(lo, hi)`

Combine two uint16 registers (little-endian) into an unsigned 32-bit integer. Note the reversed parameter order compared to `decode_u32`.

**Parameters:**
- `lo` (integer) -- low word (least significant 16 bits)
- `hi` (integer) -- high word (most significant 16 bits)

**Returns:** number -- unsigned 32-bit value

**Example:**
```lua
-- Sungrow uses little-endian register order
local regs = host.modbus_read(5016, 2, "input")
local pv_w = host.decode_u32_le(regs[1], regs[2])
```

---

### `host.decode_i32_le(lo, hi)`

Combine two uint16 registers (little-endian) into a signed 32-bit integer.

**Parameters:**
- `lo` (integer) -- low word
- `hi` (integer) -- high word

**Returns:** number -- signed 32-bit value

**Example:**
```lua
local regs = host.modbus_read(5600, 2, "input")
local meter_w = host.decode_i32_le(regs[1], regs[2])
```

---

### `host.decode_f32(hi, lo)`

Combine two uint16 registers (big-endian) into an IEEE 754 float32.

**Parameters:**
- `hi` (integer) -- high word
- `lo` (integer) -- low word

**Returns:** number -- float value

**Example:**
```lua
local regs = host.modbus_read(100, 2, "holding")
local voltage = host.decode_f32(regs[1], regs[2])
```

---

### `host.decode_u64(w1, w2, w3, w4)`

Combine four uint16 registers (big-endian) into an unsigned 64-bit integer.

**Parameters:**
- `w1` (integer) -- most significant word
- `w2` (integer) -- second word
- `w3` (integer) -- third word
- `w4` (integer) -- least significant word

**Returns:** number -- unsigned 64-bit value (as Lua number)

**Example:**
```lua
local regs = host.modbus_read(100, 4, "input")
local total_energy = host.decode_u64(regs[1], regs[2], regs[3], regs[4])
```

---

### `host.scale(value, sf)`

Apply a SunSpec scale factor: `value * 10^sf`. The scale factor is capped at |10| for safety.

**Parameters:**
- `value` (number) -- the raw value
- `sf` (number) -- the scale factor exponent

**Returns:** number -- scaled value

**Example:**
```lua
-- SunSpec: power register = 1500, scale factor = -1 -> 150.0 W
local power = host.scale(1500, -1)  -- returns 150.0
```

---

## JSON

Always available regardless of protocol.

### `host.json_decode(str)`

Parse a JSON string into a Lua table.

**Parameters:**
- `str` (string) -- JSON string

**Returns:** two values:
- On success: `table, nil` -- the parsed Lua value (table, string, number, boolean, or nil)
- On failure: `nil, error_string` -- nil and an error message

**Example:**
```lua
local data, err = host.json_decode('{"power": 1500, "soc": 85}')
if data then
    local power = data.power  -- 1500
    local soc = data.soc      -- 85
else
    host.log("warn", "JSON parse error: " .. tostring(err))
end
```

**Best practice:** Use `pcall` when decoding untrusted data:

```lua
local ok, data = pcall(host.json_decode, msg.payload)
if ok and data then
    -- process data
end
```

---

### `host.json_encode(table)`

Encode a Lua value (table, string, number, boolean, nil) to a JSON string.

**Parameters:**
- `table` (any) -- Lua value to encode

**Returns:** string -- JSON string

**Errors:** Throws a Lua error if the value contains unsupported types (functions, userdata).

**Example:**
```lua
local payload = host.json_encode({
    cmd = "charge",
    power = 3000,
    enabled = true,
})
-- payload = '{"cmd":"charge","power":3000,"enabled":true}'
host.mqtt_publish("device/control", payload)
```

---

## Telemetry

### `host.emit(der_type, data)`

Emit telemetry for a DER (Distributed Energy Resource) type. This writes the data to the shared TelemetryStore where the control loop reads it.

**Parameters:**
- `der_type` (string) -- one of `"meter"`, `"pv"`, `"battery"`
- `data` (table) -- Lua table with float values. Must contain at least `w` for power.

**Returns:** nothing

**Field reference by DER type:**

#### Meter fields

| Field       | Unit | Required | Description                          |
|-------------|------|----------|--------------------------------------|
| `w`         | W    | yes      | Total power (+import / -export)      |
| `hz`        | Hz   | no       | Grid frequency                       |
| `l1_w`..`l3_w` | W | no      | Per-phase power                      |
| `l1_v`..`l3_v` | V | no      | Per-phase voltage                    |
| `l1_a`..`l3_a` | A | no      | Per-phase current                    |
| `import_wh` | Wh   | no       | Lifetime energy imported             |
| `export_wh` | Wh   | no       | Lifetime energy exported             |

#### PV fields

| Field          | Unit | Required | Description                       |
|----------------|------|----------|-----------------------------------|
| `w`            | W    | yes      | Total PV power (always negative)  |
| `rated_w`      | W    | no       | Nameplate capacity                |
| `mppt1_v`      | V    | no       | MPPT 1 voltage                    |
| `mppt1_a`      | A    | no       | MPPT 1 current                    |
| `mppt2_v`      | V    | no       | MPPT 2 voltage                    |
| `mppt2_a`      | A    | no       | MPPT 2 current                    |
| `temp_c`       | C    | no       | Inverter temperature              |
| `lifetime_wh`  | Wh   | no       | Lifetime generation               |
| `lower_limit_w`| W    | no       | Min curtailment limit             |
| `upper_limit_w`| W    | no       | Max curtailment limit             |

#### Battery fields

| Field          | Unit     | Required | Description                    |
|----------------|----------|----------|--------------------------------|
| `w`            | W        | yes      | Power (+charge / -discharge)   |
| `v`            | V        | no       | Voltage                        |
| `a`            | A        | no       | Current                        |
| `soc`          | fraction | no       | State of charge (0.0-1.0)      |
| `temp_c`       | C        | no       | Temperature                    |
| `charge_wh`    | Wh       | no       | Lifetime energy charged        |
| `discharge_wh` | Wh       | no       | Lifetime energy discharged     |
| `upper_limit_w`| W        | no       | Max charge power               |
| `lower_limit_w`| W        | no       | Max discharge power            |

**Example:**
```lua
host.emit("meter", {
    w         = 1500,
    l1_w      = 500,
    l2_w      = 500,
    l3_w      = 500,
    hz        = 50.01,
    import_wh = 12345678,
})

host.emit("pv", {
    w       = -3200,    -- negative = generating
    rated_w = 6000,
})

host.emit("battery", {
    w   = 2000,         -- positive = charging
    soc = 0.65,         -- 65%
    v   = 48.2,
})
```
