# drivers — Lua driver host with capability-gated I/O

## What it does

Spawns one goroutine per device driver, running a Lua 5.1 script via `yuin/gopher-lua`. Exposes a fixed lifecycle (`driver_init` → `driver_poll` loop + `driver_command`/`driver_default` → `driver_cleanup`) plus a capability-gated host API (log, emit, MQTT, Modbus, HTTP, identity). Drivers are FAT: all protocol parsing, state machines, and retries live in the driver; the host only provides I/O and time.

## Key types

| Type | Purpose |
|---|---|
| `Registry` | Owns running drivers, runs per-driver `runLoop`, handles add/remove/reload. |
| `driverRuntime` (unexported interface) | Shape `runLoop` works against so per-runtime details stay out of it (`registry.go`). |
| `LuaDriver` | gopher-lua VM bound to one `HostEnv` (`lua.go`). |
| `HostEnv` | Per-driver context: capabilities, telemetry store, identity (`host.go`). |
| `MQTTCap` / `ModbusCap` | Capability interfaces implemented by `../mqtt` and `../modbus` (`host.go`). |
| `MQTTMessage` | Inbound message `{topic, payload}` drained via `PopMessages` (`host.go`). |
| `CatalogEntry` | Metadata scraped from the `DRIVER={…}` block at the top of each `.lua` file (`catalog.go`). |

## Public API surface

- `NewRegistry(tel)` + `Add / Remove / Reload / Restart / RestartByName / Send / SendDefault / ShutdownAll / Names / Env`.
- `NewLuaDriver(path, env)` for Lua.
- `NewHostEnv(name, tel)` + `WithMQTT / WithModbus / WithHTTP / SetEndpoint / SetMAC`.
- `HostEnv.Identity() / FullIdentity()` for `state.RegisterDevice` wiring.
- `LoadCatalog(dir)` walks `.lua` files and extracts the DRIVER metadata table.
- Constants: `ModbusCoil` / `ModbusDiscrete` / `ModbusHolding` / `ModbusInput` for modbus read kinds.

## How it talks to neighbors

`Registry.Add` resolves capabilities via the injected `MQTTFactory` / `ModbusFactory` / `ARPLookup` (wired in `cmd/ftw/main.go`). MAC resolution comes from `../arp`; endpoint is set from the MQTT/Modbus config. The HostEnv owns a pointer to `../telemetry.Store` — `emitTelemetry` routes structured pv/battery/meter readings through `Store.Update`, `emitMetric` routes scalar diagnostics through `Store.EmitMetric`, and each successful poll records a health tick via `DriverHealthMut`. The Lua backend adapts a `map[string]any` config at the boundary (`registry.go`). See `docs/writing-a-driver.md` and `docs/host-api.md`.

## What to read first

1. `registry.go` — `Add`, `runLoop`, `Reload`, and the `driverRuntime` adapter layer.
2. `host.go` — HostEnv + capability interfaces + identity fields.
3. `lua.go` — registration of the `host.*` global and the gopher-lua bridge.
4. `catalog.go` — how the UI discovers available drivers.

## What NOT to do

- **Do NOT reuse an MQTT `clientID` across drivers.** `main.go` prefixes with `ftw-<name>` for a reason — brokers disconnect duplicates.
- **Do NOT call into a nil capability.** Gate every MQTT/Modbus/HTTP access with the `env.MQTT != nil` / `env.Modbus != nil` / `env.HTTP` check (the host proxies already do — see `host.go`). Drivers get `ErrNoCapability` back.
- **Do NOT assume ARP lookup succeeds.** Cross-VLAN devices return `("", false)`; identity must fall back to the endpoint hash.
- **Do NOT bypass the command channel.** All driver mutations go through `rd.cmdCh` so they serialize with `Poll` on the same goroutine. Calling `Command` directly from another goroutine is a data race against gopher-lua's single-threaded VM.
