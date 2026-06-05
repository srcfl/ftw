# Lua Host API Reference

This is the reference for the `host` table exposed to Lua drivers.
For lifecycle and driver-writing guidance, start with
[`writing-a-driver.md`](writing-a-driver.md).

Authoritative source: [`go/internal/drivers/lua.go`](../go/internal/drivers/lua.go).
Grep for `RawSetString` when checking the exact runtime surface.

The legacy WASM driver runtime has been removed. Current bundled drivers are
Lua files under `drivers/`.

## Driver Lifecycle

Drivers may define these top-level functions:

```lua
function driver_init(config) end
function driver_poll() return 1000 end
function driver_command(action, power_w, cmd) return true end
function driver_default_mode() end
function driver_cleanup() end
```

`driver_command` receives:

- `action`: command string such as `"battery"`, `"curtail"`,
  `"curtail_disable"`, `"ev_set_current"`, or driver-specific actions.
- `power_w`: signed site-convention setpoint when present.
- `cmd`: full decoded command table.

Returning `false` or a non-empty string is treated as an error.

## Core Calls

| Call | Notes |
|---|---|
| `host.log(level, message)` | `level` is `"debug"`, `"info"`, `"warn"`, or `"error"`. Two arguments are required. |
| `host.millis()` | Milliseconds since driver start. |
| `host.sleep(ms)` | Blocks this driver goroutine; use only for vendor-required inter-write pacing. |
| `host.set_poll_interval(ms)` | Overrides the next poll cadence. Returning `ms` from `driver_poll` has the same effect. |
| `host.set_watchdog_timeout_s(seconds)` | Per-driver watchdog override. `0` clears it. |
| `host.set_make(name)` | Manufacturer used for device identity. |
| `host.set_sn(serial)` | Serial number used for stable `device_id`. |

There is no `host.timestamp()`, `host.pool_free()`, or one-argument
`host.log()` in the current runtime.

## Telemetry

```lua
host.emit("meter", { w = 1200 })
host.emit("pv", { w = -3200 })
host.emit("battery", { w = 1500, soc = 0.64 })
host.emit("ev", { w = 7200, connected = true, charging = true })
host.emit("v2x_charger", { w = -3000, vehicle_soc = 0.70, connected = true })
host.emit("vehicle", { soc = 62, charging_state = "Stopped" })
```

Power signs use site convention:

| Type | Positive W | Negative W |
|---|---|---|
| `meter` | grid import | grid export |
| `pv` | invalid in normal operation | generation |
| `battery` | charging | discharging |
| `ev` | vehicle charging | not used for one-way EVSEs |
| `v2x_charger` | vehicle charging | vehicle discharging into site/grid |

For scalar diagnostics that do not fit the structured readings:

```lua
host.emit_metric("battery_temp_c", 31.2)
host.emit_metric("grid_hz", 50.01)
```

Use stable snake_case names with a unit suffix. These samples go into the
long-format time-series DB.

## MQTT Capability

Available only when the driver has `capabilities.mqtt`.

| Call | Notes |
|---|---|
| `host.mqtt_subscribe(topic)` | Subscribe to a topic. |
| `host.mqtt_sub(topic)` | Alias for `mqtt_subscribe`. |
| `host.mqtt_publish(topic, payload)` | Publish a string payload. |
| `host.mqtt_pub(topic, payload)` | Alias for `mqtt_publish`. |
| `host.mqtt_messages()` | Returns an array of `{topic, payload}` received since the last call. |

If MQTT is not granted, subscribe/publish return an error string and
`mqtt_messages()` returns an empty table.

## Modbus Capability

Available only when the driver has `capabilities.modbus`.

| Call | Notes |
|---|---|
| `host.modbus_read(addr, count, kind)` | `kind` is `"coil"`, `"discrete"`, `"holding"`, or `"input"`. Returns a 1-indexed table, or `nil, err`. |
| `host.modbus_write(addr, value)` | Write one holding register. |
| `host.modbus_write_multi(addr, values)` | Write multiple holding registers. |

Wrap reads in `pcall` or handle the `nil, err` form so one failed read does
not crash the poll cycle.

## HTTP Capability

Available only when the driver has `capabilities.http`.

```lua
local body, err = host.http_get("https://example.invalid/api", {
  ["Authorization"] = "Bearer token",
})

local body, err = host.http_post(
  "https://example.invalid/api",
  '{"mode":"auto"}',
  { ["Content-Type"] = "application/json" }
)
```

Both calls return `(body, nil)` on success or `(nil, error_string)` on
failure. Schemes other than `http` and `https` are rejected. Host allowlists
are configured per driver.

## WebSocket Capability

Available only when the driver has `capabilities.websocket`.

| Call | Notes |
|---|---|
| `host.ws_open(url, headers)` | Opens one WebSocket connection. Returns `true, nil` or `nil, err`. |
| `host.ws_send(text)` | Sends one text frame. |
| `host.ws_messages()` | Drains inbound text frames. Empty table means no messages. |
| `host.ws_is_open()` | Boolean connection state. |
| `host.ws_close()` | Closes and frees the connection. |

Reconnect by calling `ws_close()` and `ws_open()` again on a later poll.

## Raw TCP Capability

Available only when the driver has `capabilities.tcp`.

| Call | Notes |
|---|---|
| `host.tcp_open("host:port")` | Opens a long-lived TCP socket. Returns `true, nil` or `nil, err`. |
| `host.tcp_recv()` | Drains buffered bytes as a Lua string. Empty string means idle. |
| `host.tcp_is_open()` | Boolean connection state. |
| `host.tcp_close()` | Closes and frees the socket. |

The current TCP surface is read-oriented and is used for devices such as
P1 meter bridges that stream unsolicited frames.

## Decode Helpers

| Call | Notes |
|---|---|
| `host.decode_u32_le(lo, hi)` | Unsigned 32-bit, little-endian word order. |
| `host.decode_u32_be(hi, lo)` | Unsigned 32-bit, big-endian word order. |
| `host.decode_i32_le(lo, hi)` | Signed 32-bit, little-endian word order. |
| `host.decode_i32_be(hi, lo)` | Signed 32-bit, big-endian word order. |
| `host.decode_i16(reg)` | Sign-extend one uint16 to int16. |

There is no unsuffixed `decode_u32`, `decode_i32`, `decode_f32`,
`decode_u64`, or `scale` helper. Drivers that need those conversions should
implement them locally.

## JSON Helpers

| Call | Notes |
|---|---|
| `host.json_decode(str)` | Returns a Lua table, or `nil, err`. |
| `host.json_encode(tbl)` | Returns a JSON string, or `nil, err`. |

## Capability Config Examples

```yaml
drivers:
  - name: tibber
    lua: drivers/tibber.lua
    is_site_meter: true
    capabilities:
      http:
        allow_hosts: ["api.tibber.com"]
      websocket:
        allow_hosts: ["websocket-api.tibber.com"]

  - name: p1
    lua: drivers/zuidwijk_p1.lua
    is_site_meter: true
    capabilities:
      tcp:
        allow_hosts: ["192.168.1.50:23"]
```

See [`configuration.md`](configuration.md) for the full driver config shape.
