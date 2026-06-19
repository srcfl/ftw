# Writing a Lua driver

A driver is a single Lua file that translates one physical device (inverter,
battery, meter, gateway) into the EMS's unified telemetry and control
vocabulary. This guide is the authoritative path for new drivers on v2.1+.

Reference implementations to copy from:

- `drivers/sungrow.lua` — full Modbus TCP example with SN read, battery
  control, curtailment, watchdog fallback.
- `drivers/ferroamp.lua` — full MQTT example with cached topic state,
  multi-topic enrichment, JSON command payloads.

The host side lives in `go/internal/drivers/lua.go` (every `host.*`
function), `go/internal/drivers/host.go` (the env that backs them), and
`go/internal/drivers/registry.go` (spawn / poll / command dispatch).

## 1. Why Lua

Lua via [gopher-lua](https://github.com/yuin/gopher-lua) is the current
driver runtime. The reasons, in order of how much they matter:

- **Contributor-friendly**: no toolchain to install. Edit a `.lua` file
  and restart — no `cargo`, no `wasm-opt`, no cross-compile.
- **Hot-editable on the device**: the operator can `ssh` into a Zap, tweak
  a register offset, restart, done. This is load-bearing for field work.
- **Small host surface**: the Lua host is one Go file plus the capability
  environment, with no separate cross-compiled driver artifact.
- **Good enough performance**: a poll at 1 Hz that reads a dozen Modbus
  registers and emits three telemetry tables is nowhere near the Lua VM
  budget. The EMS's hot loop is the controller, not the driver.

The legacy WASM runtime has been removed. New and bundled drivers are
plain `.lua` files.

## 2. The contract

Every Lua driver lives in `drivers/` and defines one metadata table plus
five top-level functions. The runtime calls them in this order:

```
NewLuaDriver(path)  → file loaded, top-level runs once (DRIVER table populated)
driver_init(cfg)    → one-shot setup (subscribe, read SN, verify limits)
driver_poll()       → called forever; returns next-poll-ms
driver_command(…)   → on each EMS control tick
driver_default_mode() → watchdog fallback if EMS goes offline
driver_cleanup()    → on shutdown / reload
```

### 2.1 The DRIVER metadata table

The `DRIVER` global is parsed by the catalog loader
(`go/internal/drivers/catalog.go`) via regex — no Lua VM is spun up. That
means the metadata is readable even from a broken driver, and it's what
populates the Settings → Devices "Add from catalog" dropdown.

```lua
DRIVER = {
  id           = "my-device",
  name         = "My Device Brand X",
  manufacturer = "BrandCo",
  version      = "1.0.0",
  protocols    = { "modbus" },          -- or { "mqtt" } or both
  capabilities = { "meter", "pv", "battery" },
  description  = "Short blurb describing the device.",
  homepage     = "https://example.com",
  authors      = { "Your Name" },
  tested_models = { "BX-5000", "BX-10000" },
}
```

Keep it well-formed: the regex in `catalog.go:95` requires a
`DRIVER = { … \n}` block with plain `key = "value"` string fields and
`key = { "a", "b" }` lists. If the regex can't pick up your metadata,
your driver still runs — it just won't show up in the UI.

### 2.2 The lifecycle functions

```lua
-- One-time setup. Subscribe to MQTT topics, read SN over Modbus, etc.
function driver_init(config)
    host.set_make("BrandCo")
    -- ... read serial number, log ...
    -- host.set_sn(serial)
end

-- Called repeatedly. Return the next-poll-interval in milliseconds.
-- Cadence ≈ 1 Hz is plenty for home batteries.
function driver_poll()
    -- Pull MQTT messages OR read Modbus registers
    -- Emit telemetry: host.emit("meter"|"pv"|"battery", { … })
    return 5000   -- ms until next call
end

-- Receive a control command from the EMS.
--   action  = "init" | "battery" | "curtail" | "curtail_disable" | "deinit"
--   power_w = positive = charge, negative = discharge, 0 = revert to self-consumption
--   cmd     = full decoded command table (extra fields, if any)
function driver_command(action, power_w, cmd)
    if action == "battery" then
        return set_battery_power(power_w)
    end
    return false
end

-- Watchdog fallback when the EMS goes offline. ALWAYS revert to a safe
-- autonomous state so the device doesn't get stuck in a forced mode.
function driver_default_mode()
    set_self_consumption()
end

-- Cleanup at shutdown / reload. Clear cached tables, close anything you own.
function driver_cleanup()
    set_self_consumption()
end
```

All five are optional — a missing function is a no-op (see
`lua.go:80`, `95`, `119`, `154`). But a real driver defines all of them:
you cannot participate in EMS control without `driver_command`, and
skipping `driver_default_mode` means your device stays in forced mode if
the EMS crashes.

Dispatch is serialized per driver (`registry.go:188`), so you will never
see two callbacks racing for the same VM.

## 3. The host API

Everything below is exposed as a `host.*` global. The authoritative list
is `go/internal/drivers/lua.go` — grep for `RawSetString` to see every
entry. The function signatures here match that file exactly.

### 3.1 Logging and identity

| Call | Purpose |
|---|---|
| `host.log(level, message)` | `level = "debug" \| "info" \| "warn" \| "error"`. Routed to `slog` (`host.go:88`). |
| `host.set_make(name)` | Manufacturer string. Feeds device_id resolution. |
| `host.set_sn(serial)` | Serial number. Promotes device_id to canonical `make:serial`. |
| `host.set_poll_interval(ms)` | Request a different poll cadence. Same effect as returning `ms` from `driver_poll`. |
| `host.millis()` | Monotonic milliseconds since host startup (`host.go:83`). |

Call `set_make` and `set_sn` as early as you reliably can — before them,
the device shows up under its config name only.

### 3.2 Telemetry

The shared telemetry shape is a JSON-flat table keyed on `type`. Extra
fields beyond the standard ones are preserved verbatim in the reading's
`Data` payload for the UI and API (`host.go:117`).

```lua
host.emit("meter", {
    w         = 1500,    -- required. Positive = importing, negative = exporting.
    l1_w      = 500,     -- per-phase power (W)
    l1_v      = 230.1,   -- per-phase voltage (V)
    hz        = 50.01,   -- grid frequency
    import_wh = 12345.6, -- lifetime counter
    export_wh = 7890.1,
})

host.emit("pv", {
    w           = -3200, -- required. ALWAYS negative (generation flows out of the array).
    mppt1_v     = 380.5,
    lifetime_wh = 1234567,
    rated_w     = 6000,
})

host.emit("battery", {
    w            = 2000, -- required. Positive = charging (energy INTO battery),
                         --           negative = discharging.
    soc          = 0.65, -- 0.0 to 1.0 fraction (NOT percent)
    v            = 48.2,
    a            = 41.5,
    charge_wh    = 98765,
    discharge_wh = 54321,
})

host.emit("ev", {
    w          = 7200,  -- required. Charge power in W (positive when pulling
                        -- from the grid). Zero when plug is idle.
    connected  = true,  -- required. Plug inserted in the car.
    charging   = true,  -- required. Current is actually flowing.
    session_wh = 14500, -- optional. Energy delivered this plug session.
    max_a      = 16,    -- optional. Charger current cap.
    phases     = 3,     -- optional. 1 or 3.
})

host.emit("v2x_charger", {
    w                  = -5000, -- required. Positive = charging vehicle,
                                --           negative = V2X discharge.
    vehicle_soc        = 0.64,  -- 0.0 to 1.0 fraction (NOT percent)
    connected          = true,
    dc_w               = -5200,
    session_discharge_wh = 1250,
    rated_power_w      = 20000,
})
```

EV readings feed directly into the dispatch clamp that keeps home
batteries from discharging into the car (`dispatch.go`). If your driver
only knows `w`, set `connected = (w > 0)` and `charging = (w > 0)`.

For anything that doesn't fit the pv/battery/meter shape — temperatures,
DC voltages, MPPT currents, grid frequency — use:

```lua
host.emit_metric("inverter_temp_c", 42.3)
host.emit_metric("battery_dc_v",    48.7)
host.emit_metric("grid_hz",         50.01)
```

Naming convention: snake_case with a unit suffix. These land in the
long-format TSDB where the UI charts them on demand. There's no
allow-list — pick a stable name and keep using it.

### 3.3 MQTT (granted only if the driver has `mqtt:` in its config)

| Call | Purpose |
|---|---|
| `host.mqtt_subscribe(topic)` | Subscribe to one topic. Call from `driver_init`. |
| `host.mqtt_publish(topic, payload)` | Publish a string payload. |
| `host.mqtt_messages()` | Return the array of `{topic, payload}` received since the last call, then clear the buffer. |

`mqtt_sub` and `mqtt_pub` exist as aliases (`lua.go:252, 268`). If the
driver has no MQTT capability, these calls return an error string. See
`drivers/ferroamp.lua:96-115` for a working init + poll loop.

### 3.4 Modbus (granted only if the driver has `modbus:` in its config)

| Call | Purpose |
|---|---|
| `host.modbus_read(addr, count, kind)` | `kind = "input" \| "holding" \| "coil" \| "discrete"`. Returns a 1-indexed Lua table of uint16s. |
| `host.modbus_write(addr, value)` | Single holding-register write (FC 0x06). |
| `host.modbus_write_multi(addr, values)` | Multi-register write (FC 0x10). `values` is a Lua array of uint16s. |

Always wrap reads in `pcall` — a single failed register read should not
crash the whole poll cycle. See `drivers/sungrow.lua:127-129` for the
pattern.

### 3.5 Raw TCP (granted only if the driver has `capabilities.tcp:` in its config)

| Call | Purpose |
|---|---|
| `host.tcp_open(addr)` | Open a long-lived TCP socket to `"host:port"`. Idempotent — doubles as the reconnect path. |
| `host.tcp_recv()` | Drain any bytes received since the last call (non-blocking, empty string when idle). |
| `host.tcp_is_open()` | Boolean — false after EOF / read error. Re-open on the next poll. |
| `host.tcp_close()` | Tear down the socket. |

For passthrough Serial-to-Ethernet bridges (Dutch P1 smart-meter readers,
some Modbus-RTU-to-TCP devices in raw mode) that stream unsolicited bytes
on a fixed port. TCP is byte-stream — the driver does its own framing on
the accumulated buffer. Read-only today: there's no `tcp_send`, because
the supported targets don't expect input. See `drivers/zuidwijk_p1.lua`
for a full DSMR 5 parser (frame detection + CRC16 + OBIS decode) built on
this capability.

### 3.6 Decoders

Modbus returns raw uint16s. These helpers combine pairs back into the
integer you actually want. Parameter order matters — `_le` takes
`(lo, hi)`, `_be` takes `(hi, lo)`.

| Call | Returns |
|---|---|
| `host.decode_u32_le(lo, hi)` | Unsigned 32-bit, little-endian word order (Sungrow's habit). |
| `host.decode_u32_be(hi, lo)` | Unsigned 32-bit, big-endian. |
| `host.decode_i32_le(lo, hi)` | Signed 32-bit, little-endian. |
| `host.decode_i32_be(hi, lo)` | Signed 32-bit, big-endian. |
| `host.decode_i16(reg)` | Sign-extend a single uint16 to int16. |

### 3.7 JSON

| Call | Purpose |
|---|---|
| `host.json_decode(str)` | Returns a Lua table on success, `nil, err_msg` on failure. |
| `host.json_encode(tbl)` | Returns a JSON string. |

## 4. Sign convention

This is the single most important rule in driver-land. Emit every value
in **site convention**; the EMS, battery models, controller, UI, and
API all assume it.

| Channel | Positive | Negative |
|---|---|---|
| `meter.w` | importing from grid | exporting to grid |
| `pv.w`   | (never — always ≤ 0) | generating |
| `battery.w` | charging (energy INTO battery) | discharging (energy OUT of battery) |
| `ev.w` | vehicle charging | (never) |
| `v2x_charger.w` | vehicle charging | vehicle discharging into site/grid |

Drivers convert at the boundary. If your device reports PV as a positive
number (almost all do), negate it before `host.emit`. If your device
encodes battery direction in a status register (Sungrow does —
`drivers/sungrow.lua:127-130, 207-209`), decode direction first, then
apply sign, then emit.

Above the driver layer, everything is in site convention. Full rationale
in `docs/site-convention.md`.

## 5. Step-by-step: adding a new device

1. Copy `drivers/sungrow.lua` (Modbus) or `drivers/ferroamp.lua` (MQTT)
   to `drivers/my-device.lua`.
2. Update the `DRIVER` metadata table: `id`, `name`, `manufacturer`,
   `version`, `protocols`, `capabilities`, `description`, `homepage`,
   `authors`, `tested_models`.
3. Replace the protocol code (Modbus register reads or MQTT topic subs)
   with your device's spec. Convert signs at the boundary.
4. Test in isolation:

   ```bash
   cd /Users/fredde/repositories/forty-two-watts/go
   go test -count=1 -run TestLuaDriverLifecycle ./internal/drivers/
   ```

5. Wire the driver into `config.yaml`:

   ```yaml
   drivers:
     - name: my-device
       lua: drivers/my-device.lua
       is_site_meter: false
       battery_capacity_wh: 10000
       capabilities:
         modbus:
           host: 192.168.1.50
           port: 502
           unit_id: 1
   ```

   For an MQTT driver, swap `capabilities.modbus` for
   `capabilities.mqtt` with `host`, `port`, `username`, and `password`.

6. Restart the service (or wait for `fsnotify` to hot-reload
   `config.yaml`).
7. Open Settings → Devices in the UI and confirm your driver appears
   with the correct `device_id`.

## 6. Common pitfalls

- **Forgetting the sign flip.** PV inverters report generation as
  positive; the EMS expects negative. Quick check:
  `curl localhost:8080/api/status` — `pv_w` should be negative when the
  sun is up.
- **Blocking `driver_poll`.** The runtime calls poll on a single
  goroutine per driver. Never sleep or block on long IO. Read, emit,
  return the next-poll-ms.
- **Updating config without a restart.** `fsnotify` catches writes to
  `config.yaml`, but a syntax error means the reload silently fails —
  check the service log after editing.
- **Ignoring `driver_default_mode`.** If you don't revert to autonomous
  in this function, your device will get stuck in the last-commanded
  forced mode if the EMS crashes. Always revert.
- **Emitting telemetry too often.** Poll cadence ≈ 1 Hz is plenty.
  Faster doesn't help control quality and saturates Modbus. The EMS
  control loop runs at `control_interval_s` (default 2 s) — your
  driver doesn't need to outrun it.
- **Using `soc` as a percent.** The EMS expects `soc` as a 0.0–1.0
  fraction. If the device reports 0–100, divide.

## 7. Testing your driver

Lua syntax + ABI check:

```bash
cd /Users/fredde/repositories/forty-two-watts/go
go test -count=1 -run TestLuaDriverLifecycle ./internal/drivers/
```

The harness in `lua_test.go` spins up a real `LuaDriver`, calls `Init`,
`Poll`, and `Command` three times, and asserts that telemetry and
identity flow correctly. If you want to add assertions for your new
driver, follow that pattern.

Live with the simulators:

```bash
make run-sim   # starts sim-ferroamp + sim-sungrow
# In another terminal:
make dev       # starts main app
```

After deploying to a real device:

```bash
curl localhost:8080/api/status
curl localhost:8080/api/drivers/catalog
curl localhost:8080/api/series/catalog
```

## 8. Driver catalog

For the current list of shipped drivers (manufacturer, protocol,
capabilities, control support, tested models), see
[`docs/driver-catalog.md`](driver-catalog.md).

The `DRIVER` metadata table is parsed by
`go/internal/drivers/catalog.go:LoadCatalog` with a regex — no Lua VM
is spun up. The `GET /api/drivers/catalog` endpoint returns the parsed
entries. Settings → Devices uses this for the "Add from catalog"
dropdown.

If the regex can't extract your metadata, the driver still runs but
won't appear in the catalog. Keep the block plain:

```lua
DRIVER = {
  id   = "value",
  list = { "a", "b" },
}
```

No function calls, no concatenation, no variables — the catalog loader
reads the source as text.
