# The FTW Lua driver host

Drivers are the hardware boundary. A driver translates one vendor protocol to
FTW's site convention and runs in its own capability-scoped Lua 5.1 VM. No Go
build is needed.

**To write a driver, start upstream.** Source, manifests and the authoring
guide live in the public
[`srcfl/device-drivers`](https://github.com/srcfl/device-drivers) repo, whose
signed channel is FTW's default source:

- [`docs/WRITING-A-DRIVER.md`](https://github.com/srcfl/device-drivers/blob/main/docs/WRITING-A-DRIVER.md)
  — entry points, sign convention, what never to fabricate, how to add and test
  a driver;
- [`blueprint/BLUEPRINT.lua`](https://github.com/srcfl/device-drivers/blob/main/blueprint/BLUEPRINT.lua)
  — a complete working driver, and the specification the guide explains;
- [device driver catalog](https://srcfl.github.io/device-drivers/) — which
  devices are already covered, and on what evidence.

This page is the other half: what FTW's host gives a driver, how FTW grants it,
where FTW loads it from, and how to test one against a running instance. It is
deliberately not a second authoring guide — two of those drift, and the one
here is the copy that would go stale.

## Host API

[`go/internal/drivers/lua.go`](../go/internal/drivers/lua.go) is the complete,
current host API and the source of truth for it. Today it registers:

| Group | Calls |
|---|---|
| Telemetry | `emit`, `emit_metric` |
| Identity and nameplate | `set_make`, `set_sn`, `set_model`, `set_rated_w` |
| Driver control | `set_poll_interval`, `set_warmup_s`, `set_watchdog_timeout_s`, `set_device_fault` |
| Helpers | `log`, `sleep`, `millis`, `now_ms`, `json_encode`, `json_decode`, `persist_secret` |
| Decoding | `decode_string`, `decode_i16`, `decode_i32_be`, `decode_i32_le`, `decode_u32_be`, `decode_u32_le` |
| Modbus | `modbus_read`, `write`, `write_registers` (canonical); `modbus_write`, `modbus_write_multi` (legacy aliases) |
| MQTT | `mqtt_pub`, `mqtt_sub`, `mqtt_publish`, `mqtt_subscribe`, `mqtt_messages` |
| HTTP | `http_get`, `http_post`, `http_patch` |
| WebSocket | `ws_open`, `ws_send`, `ws_messages`, `ws_is_open`, `ws_close` |
| Raw TCP | `tcp_open`, `tcp_recv`, `tcp_close`, `tcp_is_open` |
| Serial | `serial_read` |
| Crypto | `aes_gcm_decrypt` |

Some calls answer to two names. `srcfl/device-drivers` treats the Blixt L1 host
API as its naming reference, so the canonical `write`, `write_registers` and
`now_ms` names resolve to `modbus_write`, `modbus_write_multi` and `millis`.
`host.emit` likewise
reads `W` and `SoC_nom_fract` when `w` and `soc` are absent; when both are
present the lowercase key wins. Both spellings are correct — prefer the
canonical one in a new driver.

`http_patch` is the mutating verb and is gated twice: the plain `http` grant is
not enough, it also needs `capabilities.http.allow_write`. It refuses to follow
redirects, because Go re-issues a redirected `PATCH` as a body-less GET and a
device write that never landed would otherwise report success.

## Capability grants

A YAML driver entry grants only what the file needs. The resource keys are
`modbus`, `mqtt`, `serial`, `http`, `websocket` and `tcp`. A driver that needs
no external I/O can use `standalone: true` instead; `standalone` is a boolean,
not a resource block:

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

For a driver with no external I/O, use:

```yaml
drivers:
  - name: clock
    lua: drivers/clock.lua
    capabilities:
      standalone: true
```

Calls without a granted capability return an error rather than reaching the
network. HTTP destinations are allowlisted per driver and can pin a certificate
with `tls_pin_sha256`. Never add an unrestricted network escape to solve one
driver's setup problem.

## What the host does around a driver

Sign conversion happens only here. Read
[site-convention.md](site-convention.md); a conversion anywhere above the
driver is a bug.

A controllable device needs a real `driver_default_mode`. The host calls it for
stale telemetry, relevant reloads, removal and shutdown, so it is the path that
returns hardware to safe autonomous operation when FTW stops being able to
steer it. Polling must not keep re-emitting an indefinitely cached value as
fresh telemetry: age vendor data and stop emitting when it is stale, or core's
watchdog cannot see the fault.

`driver_fingerprint(target)` is an optional passive setup probe. It must never
reconfigure the device.

Call `host.set_make` and `host.set_sn` as soon as stable identity is known.
Core then keys durable device state by hardware identity rather than the YAML
name. `host.set_model` and `host.set_rated_w` record the rest of the nameplate;
the host repeats both on every emit, so read them once in `driver_init` rather
than every poll. Neither takes part in device-id resolution.

A device that answers Modbus before its registers mean anything can call
`host.set_warmup_s(seconds)` in `driver_init` to hold off the first poll.

## Where FTW loads a driver from

FTW resolves a driver file as local, then managed signed, then bundled.
Settings and fleet inventory mark the first case `local / unsigned`.

Operator-only drivers belong in the persistent user-driver directory, not
inside a container layer:

- Docker: `/app/data/drivers`, which is the host's `./data/drivers` bind;
- systemd: `/var/lib/ftw/drivers`;
- another native run: pass `-user-drivers <dir>`.

Local code works offline and never needs GitHub or Device Support. It gets no
auto-update or promotion and cannot claim signed package control. The normal
host capabilities and lifecycle still apply.

The bundled set under `drivers/` is FTW's offline recovery snapshot, generated
from the commit pinned in
[`drivers/BUNDLED_SOURCE.json`](../drivers/BUNDLED_SOURCE.json) and fetched by
`make drivers`. It is not editable here: the files are gitignored and CI fails
if one is committed. Fix a driver upstream and move the pin. Managed drivers
are signed, installed atomically and rollbackable; see
[device-repository.md](device-repository.md).

## Testing against a running FTW

Use **Test connection** in Settings, or call `POST /api/drivers/test`. The test
starts a short-lived driver, runs init and poll with the declared hardware
capabilities, and does not save config. Start read-only on a test device. The
endpoint reads the Lua file again for each test.

```bash
curl -sS -X POST http://127.0.0.1:8080/api/drivers/test \
  -H 'Content-Type: application/json' \
  -d '{"name":"example-test","lua":"drivers/example.lua","capabilities":{"modbus":{"host":"192.168.1.20","port":502,"unit_id":1}}}'
```

FTW does not watch Lua files. An active driver needs an FTW restart, or a real
config change that restarts its registry entry, after the file changes. Restart
Docker with `docker compose restart ftw`, or systemd with
`sudo systemctl restart ftw`. To roll back a local overlay, move the local file
out of the user-driver directory and restart; FTW then selects the managed or
bundled file without changing its managed cache.

Host-side Lua tests live beside [`go/internal/drivers`](../go/internal/drivers):

```bash
cd go
go test ./internal/drivers
go test ./internal/driverrepo
```

For live work, start with telemetry only and a physically supervised device.
Compare FTW, vendor UI and the site meter before sending a non-zero command.
Test charge, discharge, zero, offline/default mode and reconnect. Record
device-specific safety knowledge in the driver next to the code it constrains.
