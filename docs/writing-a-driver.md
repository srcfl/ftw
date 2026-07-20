# Writing a Lua driver

Drivers are the hardware boundary. A driver translates one vendor protocol to
FTW's site convention and runs in its own capability-scoped Lua 5.1 VM. No Go
build is needed.

For a Sourceful-supported device, create or change the canonical source and
package metadata in `srcful-device-support`. Start from David's hardware-near
Blixt implementation when it is the best protocol reference, then prove an
explicit `ftw-core` target against this host. The `drivers/` tree here is the
bundled FTW recovery snapshot; operator-only drivers may still live locally.

## Metadata

Every driver declares one authoritative catalog block:

```lua
DRIVER = {
  id = "example",
  name = "Example inverter",
  manufacturer = "Example",
  version = "0.1.0",
  host_api_min = 1,
  host_api_max = 1,
  protocols = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description = "Supported product family.",
  authors = { "FTW contributors" },
  tested_models = {},
  verification_status = "experimental",
}
```

The `DRIVER` table feeds target validation and FTW's in-app catalog. Its id,
version, host API and `read_only` value must agree with the signed Device
Support package. Do not duplicate the catalog in Markdown. Executable or
public metadata changes require one canonical package version bump.

## Lifecycle

```lua
function driver_init(config)
  -- read driver config, subscribe/connect, report identity
end

function driver_poll()
  -- read device and call host.emit / host.emit_metric
end

function driver_command(action, power_w, command)
  -- translate a site-convention command to the vendor protocol
  return true
end

function driver_default_mode()
  -- cancel forced control and restore safe autonomous operation
  return true
end

function driver_cleanup() -- optional
end
```

Controllable devices require a real `driver_default_mode`; it is called for
stale telemetry, relevant reloads, removal and shutdown. Polling must not keep
re-emitting an indefinitely cached value as fresh telemetry. Age vendor data
and stop emitting when it is stale so core's watchdog can work.

`driver_fingerprint(target)` is an optional passive setup probe. It must never
reconfigure the device.

## Sign convention

Translate before calling `host.emit` and translate commands in the opposite
direction:

- meter import positive, export negative;
- PV generation negative;
- battery/vehicle charge positive, discharge negative;
- SoC telemetry uses the fraction `0..1` unless a field explicitly says
  percent.

Read [site-convention.md](site-convention.md). Sign conversion anywhere above
the driver is a bug.

## Host capabilities

`go/internal/drivers/lua.go` is the complete, current host API. It exposes
telemetry, diagnostic metrics, identity, time/JSON helpers and capability-gated
MQTT, Modbus, HTTP, WebSocket and raw TCP operations.

A YAML driver entry grants only what the file needs:

```yaml
drivers:
  - name: example
    lua: drivers/example.lua
    is_site_meter: true
    capabilities:
      modbus:
        host: 192.168.1.20
        port: 502
        unit_id: 1
```

Calls without a granted capability return an error. HTTP destinations are
allowlisted. Never add an unrestricted network escape to solve one driver's
setup problem.

Call `host.set_make` and `host.set_sn` as soon as stable identity is known.
Core then keys durable device state by hardware identity rather than the YAML
name. Use `host.emit_metric(name, value, unit)` for scalar diagnostics that do
not belong in structured meter/PV/battery/EV telemetry.

## Implementation sequence

1. Add or update the canonical driver and package source in Device Support.
2. Implement read-only polling and verify signs against real vendor values.
3. Build the explicit FTW GopherLua/Lua 5.1 target and run FTW host tests.
4. Add stable identity and stale-cache handling.
5. Add commands only after telemetry is trustworthy.
6. Implement and test default mode, leases and structured command results
   before enabling automatic dispatch.
7. Add configuration example only when the integration needs non-obvious
   operator input.
8. Add Go-hosted Lua tests beside `go/internal/drivers`.

Useful checks:

```bash
cd go
go test ./internal/drivers
go test ./internal/driverrepo
```

For live work, start with telemetry only and a physically supervised device.
Compare FTW, vendor UI and the site meter before sending a non-zero command.
Test charge, discharge, zero, offline/default mode and reconnect. Record
device-specific safety knowledge in the driver next to the code it constrains.

## Local overrides and release

Custom drivers belong in the persistent user-driver directory, not inside a
container layer. Managed drivers are signed, installed atomically and
rollbackable; see [device-repository.md](device-repository.md).

Device Support publishes new driver packages to `beta` first. Promote the same
reviewed source, target hashes and version to `stable` only after hardware
validation. FTW does not fork or independently renumber that release.
