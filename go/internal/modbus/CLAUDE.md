# modbus — per-driver Modbus TCP capability built on simonvetter/modbus

## What it does

Wraps one `simonvetter/modbus.ModbusClient` per driver and implements `drivers.ModbusCap`. TCP-only (no serial/RTU). All calls are serialized behind a `sync.Mutex` so a driver's poll-read can't race its command-write on the same connection.

## Key types

| Type | Purpose |
|---|---|
| `Capability` | `simonvetter/modbus.ModbusClient` + `sync.Mutex`. Implements `drivers.ModbusCap`. |

## Public API surface

- `Dial(host, port, unitID int) (*Capability, error)` — opens TCP, sets unit ID when `> 0`, 5 s timeout (`client.go:22-33`).
- `(*Capability).Close() error` — releases the underlying socket.
- `(*Capability).Read(addr, count uint16, kind int32) ([]uint16, error)` — `kind` uses the ABI constants from `drivers/abi.go` (`ModbusInput=3`, `ModbusHolding=2`). Anything else falls back to input registers (`client.go:47-51`).
- `(*Capability).WriteSingle(addr, value uint16) error` — single holding register.
- `(*Capability).WriteMulti(addr uint16, values []uint16) error` — bulk holding register write.

## How it talks to neighbors

`../drivers` Registry holds a `ModbusFactory` wired in `cmd/ftw/main.go` that calls `Dial(c.Host, c.Port, c.UnitID)` per driver. The returned `*Capability` is bound to the driver's `HostEnv.Modbus`. Lua drivers call `host.modbus_read / host.modbus_write / host.modbus_write_multi`, which route through `drivers.ModbusCap`. The endpoint string `modbus://<host>:<port>` is recorded on the HostEnv for device-identity and the host IP gets ARP-resolved for MAC-stable `device_id`.

## What to read first

`client.go` — the entire package is ~68 LOC. Coils and discrete inputs are NOT currently implemented even though the ABI defines their codes (`ModbusCoil`, `ModbusDiscrete`). Add a switch case here if a driver needs them.

## What NOT to do

- **Do NOT share one `Capability` across drivers.** Mutex serializes the single socket; two drivers would stall each other. The factory builds one per driver on purpose.
- **Do NOT add RTU/serial here without a separate type.** `sv.NewClient` is locked to `tcp://` URL form in `Dial` (`client.go:23`); a serial capability belongs in a sibling file (keep TCP free of serial-only timing code).
- **Do NOT re-open on every call.** `Dial` opens once; reconnection comes from `simonvetter/modbus`'s internal retry. If a driver sees persistent errors it should log and surface health via `env.Telemetry.DriverHealthMut`.
- **Do NOT decode `u32` / `i32` inside Lua by hand.** Use the `host.decode_u32_le` / `decode_u32_be` / `decode_i32_le` / `decode_i32_be` / `decode_i16` helpers exposed in `../drivers/lua.go:348-376` — Sungrow's little-endian pair order has caught people before.
- **Do NOT pass a `kind` code outside `drivers.Modbus*`.** An unknown kind silently downgrades to input registers (`client.go:50`); that's a debugging nightmare if you treat it as "holding default".
