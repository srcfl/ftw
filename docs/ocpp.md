# OCPP chargers

FTW has a built-in OCPP Central System speaking **1.6J and 2.0.1**. An EV
charger that speaks either connects straight to FTW over your local network.

**An OCPP charger does not need a driver.** There is no Lua file to write, no
entry in `drivers:`, and nothing to add to the device catalog. OCPP is a vendor-
neutral protocol, so one server in core handles every charger that speaks it —
Charge Amps, Easee, Zaptec, ABB, Alfen and the rest are all the same code path.

This is the opposite of how the rest of FTW works. Every other device needs a
driver because every vendor invented its own protocol. OCPP is the standard that
makes drivers unnecessary, so the driver boundary does not apply.

## How a charger becomes a device

Drivers poll outward: FTW opens the connection and asks for data. OCPP runs the
other way — the charger dials FTW and pushes.

So there is nothing to configure per charger. You point the charger at FTW, and
it appears as a device the moment it sends its first `BootNotification`. Its
identity is the last segment of the URL it connected to:

```
ws://<ftw-host>:8887/garage-left
                     └── this becomes the device key
```

Give each charger a distinct identity segment. Reuse one and two chargers will
collapse into a single device.

From there it behaves like any other EV reading: `MeterValues` and
`StatusNotification` become telemetry, and dispatch stops the home battery
discharging into an active EV charge.

## Protocol versions

| Version | Status | Port |
|---|---|---|
| 1.6J | supported | `port`, default 8887 |
| 2.0.1 | supported | `port_v201`, off unless set |
| 2.1 | not yet — see below | — |

**Each version needs its own port.** A charger picks its dialect in the
WebSocket handshake, before any message is sent, and the underlying library
keeps one message handler per listener — so one port cannot serve both. Point
each charger at the port matching what it speaks.

Everything above the protocol is shared. Both dialects land in the same charger
map, produce the same telemetry, and take the same commands, so a 2.0.1 charger
is throttled and paused exactly like a 1.6 one and nothing downstream knows the
difference.

### On OCPP 2.1

2.1 is not supported, because no production-grade Go implementation of it
exists. `lorenzodonini/ocpp-go`, the library FTW uses and the only mature option
at 367 stars, implements 1.6 and 2.0.1 and has no 2.1 support. The Go projects
that do claim 2.1 are all early — single-digit stars, and mostly message
validators or emulators rather than servers.

This is not a blocker in practice. Every charger on the bench speaks 1.6J only,
and 2.0.1 is what current hardware is migrating to.

Adding 2.1 later means writing one more handler file and one more listener. The
version-neutral core — charger state, telemetry mapping, control semantics —
does not change.

## Where the code comes from

The protocol layer is [`github.com/lorenzodonini/ocpp-go`](https://github.com/lorenzodonini/ocpp-go)
v0.19.0, under the MIT license. It is an ordinary Go module dependency, pinned in
`go/go.mod` and checksum-verified through `go/go.sum`. Nothing in FTW is copied
or forked from it.

| Layer | Owner |
|---|---|
| WebSocket transport, OCPP-J framing, message types, schema validation | ocpp-go |
| Handlers, telemetry mapping, control semantics, safety clamps | FTW, `go/internal/ocpp` |

That split is the first thing to check when something misbehaves. A malformed
message or a dropped connection is upstream; a wrong power figure or a wrong
current limit is ours.

Upstream health, as of this writing: 367 stars, MIT, no release since August
2025, and it describes its own 2.0.1 support as "examples working, but will need
more real-world testing". Treat the 2.0.1 path here as less proven than 1.6J
regardless of FTW's own tests.

If FTW ever needs a fix upstream will not take, the move is a `srcfl/ocpp-go`
fork plus a `replace` directive in `go.mod` — still an ordinary module. A git
submodule is not an option: `go build` resolves dependencies through `go.mod`
and the module cache, so a submodule checkout would be inert unless paired with
that same `replace`, while additionally breaking `go install` and any clone
made without `--recurse-submodules`. To pin the source inside this repository
instead, the Go-native answer is `go mod vendor`, which commits the dependency
tree under `vendor/` and is understood by the toolchain without extra flags.

## Enabling the server

OCPP is off by default. Add an `ocpp` section:

```yaml
ocpp:
  enabled: true
  port: 8887          # OCPP 1.6J, default 8887
  port_v201: 8888     # OCPP 2.0.1; omit or 0 to disable
  path: /             # default /
  username: ftw
  password: <a long random string>
  heartbeat_interval_s: 60
```

**Credentials are mandatory.** FTW refuses to start with `enabled: true` and an
empty username or password. That is deliberate, and the reason is below.

## Security: the listener is on every interface

The OCPP library builds its listen address from the port alone, so the socket
binds to every interface the host has. There is no bind-address setting, and
there is no TLS on this path yet.

On a Raspberry Pi with one LAN connection that is usually fine. It is not fine
if the host also has a public interface or a permissive port forward.

So:

- Keep the port closed at your router. Never forward it from the internet.
- Treat the password as a real secret; it is the only gate in front of the
  server.
- Basic auth over `ws://` sends the credential unencrypted. Anyone who can sniff
  your LAN can read it.

Credentials being required is a mitigation, not a fix. Binding to one interface
needs a change to the upstream library.

## Pointing a charger at FTW

The backend URL is always `ws://<ftw-host>:<port><path><identity>`. What differs
is how you reach the charger to set it.

| Charger | Where you set it | Cloud needed? |
|---|---|---|
| Charge Amps Halo, Aura | WiFi hotspot → `192.168.250.1` → Settings → OCPP | no |
| Charge Amps Dawn, Luna | Charge Amps Installer app over Bluetooth → CPMS settings | no |
| Easee | Easee's commissioning API, once | one-time |
| Zaptec | Zaptec Portal, needs the `Allow OCPP 1.6J` permission | one-time |

Easee and Zaptec need a one-time commissioning step through the vendor portal.
After that the charger talks only to FTW and the cloud is out of the runtime
path. Charge Amps needs no cloud at all.

Two traps worth knowing:

- **Charge Amps Bluetooth is only discoverable for 10 minutes after power-up.**
  If the unit has been on longer, cut the fuse and re-energise before searching.
- **Zaptec appends the charger serial to the URL itself.** Enter the URL without
  it.

For the full commissioning and factory-reset detail per model, see the bench
guide in the device-drivers repository.

## Control

FTW throttles, pauses and resumes an OCPP charger the same way it steers any
other EV charger. Loadpoints do not know the difference.

Every command is expressed as a **current limit**, never as a remote start or
stop. Pausing means "you may draw zero amps"; resuming raises the limit again.

That is not a stylistic choice. `RemoteStopTransaction` is unreliable on Charge
Amps hardware — units acknowledge the stop and then resume charging on their
own. A charging profile of 0 A is honoured consistently. It also leaves the
transaction open, so the session meter keeps counting across a pause instead of
being split into two sessions.

Two limits worth knowing:

- **Below 6 A, FTW sends 0 A.** IEC 61851 has no duty cycle under 6 A. When the
  allocator has less headroom than that to give, rounding up would draw current
  the site fuse was never asked to carry, so charging stops instead.
- **A pause remembers the previous rate.** Resuming returns to the last non-zero
  limit, not to the maximum.

If FTW loses contact, the charger holds its last granted limit. Dropping to zero
would strand a driver with an uncharged car because the EMS went down, and the
last limit was already judged safe for the site. This matches what every EV
driver in FTW already does.

## Current limits

- **No TLS**, and the listener cannot be pinned to one interface.
- Chargers that also have a native protocol may work better through a driver.
  Easee over Modbus and Zaptec over its cloud API already have drivers; OCPP is
  the option when you want the cloud out of the loop.

## When you still need a driver

Only for chargers that do not speak OCPP, or where a vendor protocol exposes
something OCPP does not. The Ambibox V2G / InterControl ambiCHARGE is the
example on the bench: it is DC-coupled and bidirectional, runs over MQTT, and
uses the `ambibox_v2x` driver.

See [writing-a-driver.md](writing-a-driver.md) for that path.
