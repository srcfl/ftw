# Writing a Lua driver

Drivers are the hardware boundary. A driver translates one vendor protocol to
FTW's site convention and runs in its own capability-scoped Lua 5.1 VM. No Go
build is needed.

For a shared device, create or change the source and manifest in the public
[`srcfl/device-drivers`](https://github.com/srcfl/device-drivers) repo. Its
signed channel is FTW's default source. Device Support may later consume an
exact reviewed commit and does not own a second editable source. The `drivers/`
tree here is the bundled FTW recovery snapshot; operator-only drivers may still
live locally.

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
version, host API and `read_only` value must agree with the signed package. Do
not duplicate the catalog in Markdown. Executable or public metadata changes
require one public package version bump.

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

1. Add or update the driver and manifest in `srcfl/device-drivers`.
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
container layer:

- Docker: `/app/data/drivers`, which is the host's `./data/drivers` bind;
- systemd: `/var/lib/ftw/drivers`;
- another native run: pass `-user-drivers <dir>`.

Keep the config path portable:

```yaml
drivers:
  - name: example
    lua: drivers/example.lua
    capabilities:
      modbus:
        host: 192.168.1.20
        port: 502
        unit_id: 1
```

FTW resolves that file as local, then managed signed, then bundled. Settings
and fleet inventory mark the first case `local / unsigned`. Local code works
offline and never needs GitHub or Device Support.
It gets no auto-update or promotion and cannot claim signed package control.
The normal host capabilities and lifecycle still apply.

Use **Test connection** in Settings, or call `POST /api/drivers/test`. The test
starts a short-lived driver, runs init and poll with the declared hardware
capabilities, and does not save config. Start read-only on a test device. The
endpoint reads the Lua file again for each test. FTW does not watch Lua files:
an active driver needs an FTW restart or a real config change that restarts its
registry entry after the file changes.

```bash
curl -sS -X POST http://127.0.0.1:8080/api/drivers/test \
  -H 'Content-Type: application/json' \
  -d '{"name":"example-test","lua":"drivers/example.lua","capabilities":{"modbus":{"host":"192.168.1.20","port":502,"unit_id":1}}}'
```

Restart Docker with `docker compose restart ftw`, or systemd with
`sudo systemctl restart ftw`. To roll back a local overlay, move the local file
out of the user-driver directory and restart. FTW then selects the managed or
bundled file without changing its managed cache.

Before a public pull request, clone `srcfl/device-drivers`, place the driver in
its public layout, and run:

```bash
make test-driver ID=example
make package-driver ID=example TARGET=ftw-core
make check
```

Managed drivers are signed, installed atomically and rollbackable; see
[device-repository.md](device-repository.md).

The public repository publishes new drivers to `drivers-beta` first. Promote
the exact signed beta commit after hardware validation. Each changed artifact
needs a higher driver SemVer. FTW does not fork or renumber that release.
