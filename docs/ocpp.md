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

You point the charger at FTW and it appears on **Settings → Chargers** the
moment it sends its first `BootNotification`. Its identity is the last segment
of the URL it connected to:

```
ws://<ftw-host>:8887/garage-left
                     └── this becomes the device key
```

Give each charger a distinct identity segment. Reuse one and two chargers will
collapse into a single device.

Connecting alone does not make it part of the site. A charge point no charger
entry names is **pending**: FTW shows its vendor, dialect and live state so
you can adopt it, but ignores its telemetry and never commands it. To adopt
it, add a charger entry on the same tab with the charge point's id as the
charger driver and save — charger entries hot-reload, so it joins the site on
that save, and removing the entry returns it to pending just as immediately.

The quarantine is deliberate. Every charge point shares one basic-auth secret,
so "it authenticated" proves the password, not the device. If pending chargers
fed telemetry, anything on the LAN holding that password could invent EV load
— and the dispatch clamp would obligingly stop the home battery discharging
into a charge that does not exist. Naming the id in a charger entry is what
turns *seen* into *trusted*.

Once adopted it behaves like any other EV reading: `MeterValues` and
`StatusNotification` become telemetry, and dispatch stops the home battery
discharging into an active EV charge. It also gets a row in `/api/devices`
alongside the driver-backed hardware, under **Settings → Devices**.

That row is keyed on the vendor and serial from `BootNotification`, not on the
name above — a name an installer typed and the charger's own web page can
change is not hardware identity, and state keyed on it would not survive a
re-commissioning. Rename a charger and its row follows it; the persistent
state stays attached to the box on the wall. A charger that reports no serial
(plenty do not) falls back to the dialled name, recorded as an endpoint so it
reads as what it is: stable only until someone changes it.

Pending chargers get no row. A device row says this hardware is part of the
site, and quarantine says an unadopted charge point is not; it gets one on the
save that adopts it.

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

## Security: the socket is on every interface

The OCPP library builds its listen address from the port alone, so the socket
is open on every interface the host has and nothing FTW can configure changes
that. `bind` therefore works one layer up: a connection that arrived on any
other address is refused at the WebSocket handshake, before it can speak OCPP.

That is an access control, not a smaller attack surface. A port scan still
finds the port open on every interface; what it cannot do is talk to it.

```yaml
ocpp:
  bind: 192.168.1.10   # only chargers reaching the box this way
```

On a Raspberry Pi with one LAN connection the default (every interface) is
usually fine. Set `bind` when the host also has a VPN interface, a second NIC,
or a public one.

Whatever you set:

- Keep the port closed at your router. Never forward it from the internet.
- Treat the password as a real secret.

### The four gates

1. **Basic auth.** Required — an enabled server without a username and
   password is refused at startup.
2. **Identity binding.** A charger listed under `ocpp.chargers` has a password
   of its own and must present it under its own name. This is what closes
   impersonation: without it, identity is client-chosen and shared, so a device
   that knows the password *and* an adopted charger's id can pose as it. When
   `client_ca_file` is set, the client certificate's CN or DNS SAN must also
   match that name — any cert from the CA is not enough to claim another
   charger's id. A per-charger password remains an additional gate, not a
   substitute.
3. **Bind.** As above.
4. **Quarantine.** A charge point no charger entry names stays pending, outside
   telemetry and dispatch. A stolen password gets an attacker a row in the
   Chargers table, not influence over the site.

### Per-charger credentials

```yaml
ocpp:
  username: ftw
  password: "the-shared-one"
  chargers:
    - id: garage
      password: "a-different-long-random-string"
```

The `id` is what the charger dials with — the last segment of its URL, and the
same string a charger entry adopts. On OCPP the basic-auth username *is* that
identity, so a listed charger presents `garage` / its own password, and the
shared credential no longer buys its name.

This is opt-in per charger: anything not listed keeps using the shared
username and password, so adding one entry does not lock the others out. A
charger you have not listed can still be impersonated by something holding the
shared password — list the ones that matter.

### TLS

```yaml
ocpp:
  tls:
    cert_file: /etc/ftw/ocpp/server.crt
    key_file: /etc/ftw/ocpp/server.key
    client_ca_file: /etc/ftw/ocpp/charger-ca.crt   # optional
```

Chargers then dial `wss://` instead of `ws://`. Without it, basic auth over
`ws://` sends the credential unencrypted and anyone who can sniff your LAN can
read it.

`client_ca_file` additionally requires every charge point to present a
certificate signed by that CA — OCPP 2.0.1 security profile 3 — and the
certificate's CN or DNS SAN must match the identity in the URL. A cert from
the same CA that names a different charger is refused. Unlike a password, the
private key cannot be copied out of one charger's configuration and replayed
by another device. A per-charger password, if configured, is an additional
gate, not a substitute for the certificate name.

Half a TLS section is refused at startup rather than quietly serving `ws://`.
An operator who asked for `wss://` and silently got plaintext would have no way
to tell the link was never encrypted.

## Pointing a charger at FTW

The backend URL is always `ws://<ftw-host>:<port><path><identity>`. What differs
is how you reach the charger to set it.

**Reserve FTW's IP address first.** Chargers store the backend URL at
commissioning time, and some also whitelist which addresses may talk to them —
an FTW host that later gets a different address from DHCP silently orphans
every charger pointed at it. Give the FTW machine a DHCP reservation (a fixed
IP) in your router before commissioning the first charger.

`<ftw-host>` can be a hostname instead of an IP if the charger's firmware can
resolve it: plain DNS names (a router DNS entry, a local zone) work on most
chargers, but mDNS `.local` names usually do not — charger firmware rarely
ships an mDNS resolver. When in doubt, use the reserved IP.

The **Settings → Chargers** tab shows the exact URL to enter, and every charge
point that has connected appears there with its vendor, OCPP dialect and live
state — as pending, until you adopt it. Add it as a charger entry on the same
tab (its identity appears in the driver dropdown once it has connected) and
save; that both lets the planner steer it and admits its telemetry into the
site, immediately — no restart needed.

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

## Local simulation

`go/cmd/sim-ocpp` dials the built-in Central System as every OCPP charger
in the Evify catalogue recorded on 11 September 2026 (Easee Charge Up/Max, Zaptec Go/Go 2, NexBlue Edge 2,
go-e Gemini Flex 2.0, Charge Amps Luna/Halo/Aura/Dawn, Wallbox Pulsar Max,
DEFA Power). Tesla Wall Connector is in that catalog but has no OCPP — FTW
already talks to it over local HTTP.

```bash
make dev          # enable ocpp in config.local.yaml (the example template does)
make sim-ocpp     # all OCPP models plug in and start metering
```

Vendor quirks FTW already defends against are encoded: Charge Amps ACK a
remote stop and keep charging, Aura refuses a connector-0 profile, Zaptec
dials as its serial. Run the Core integration test explicitly:

```bash
cd go
FTW_E2E=1 go test ./test/e2e -run 'Test(EvifyOCPPInventoryE2E|PendingEvifyChargerIsQuarantined)' -count=1 -timeout 120s
```

The test covers simulated boot, adoption, current limits and pause through
Core. It does not prove behavior on those physical charger models. Ordinary
unit tests keep the protocol sequence and response-lag regressions.

## Can this charger be steered?

Not every OCPP charger accepts control. FTW asks each one, once, shortly after
it connects: `GetConfiguration(SupportedFeatureProfiles)` on 1.6, and the
`SmartChargingCtrlr.Available` variable on 2.0.1. The answer lands in the
**Control** column of the Chargers tab and in `GET /api/ocpp/chargers`:

| Column says | Meaning |
|---|---|
| smart charging | The charger advertises `SmartCharging`; FTW can throttle and pause it. |
| telemetry only | The charger answered without `SmartCharging` — expect metering, no planning. |
| not reported | The charger never answered the probe. Unknown, not incapable. |

The raw answer is kept (hover the column) and re-probed on every reconnect
until one arrives.

**The verdict is advisory, never a gate.** FTW still sends charging profiles to
a charger that claims it cannot take them, because vendors under-report and a
firmware update can change the answer without changing the advertisement. What
actually decides is the response to a real command: a charger that refuses
`SetChargingProfile` is walked to its autonomous default and reported through
the same path as any driver that cannot actuate. The probe exists so the UI can
warn *before* you bind a metering-only charger to a charger entry and wonder
why the planner never moves it — and some vendors ship smart charging behind a
firmware update or an installer-app setting, which is worth checking when you
see the warning.

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

### How the profile is shaped, and why

The limit goes out as a `TxDefaultProfile` with one schedule period at second
zero and no end: *hold this until I send another*. Two details of that are
load-bearing, and both come from chargers disagreeing with the specification
in ways that fail silently:

- **The profile kind is `Relative`, not `Absolute`.** An absolute schedule
  states when it starts; FTW's has no start, and the specification says an
  absolute schedule without one is relative to the start of charging anyway.
  A charger that instead reads the missing timestamp as "not valid yet"
  answers **Accepted** and charges on at full rate — the worst failure
  available here, because FTW logs a limit it never imposed. Relative carries
  no timestamp to misread, and does not depend on the charger's clock agreeing
  with ours.
- **Connector 0, then connector 1.** A profile on connector 0 applies to every
  connector, which avoids depending on per-connector ids — unreliable on
  dual-socket units such as the Charge Amps Aura. Some chargers read the
  connector-0 rule as `ChargePointMaxProfile`-only and reject it, so a refusal
  is retried once on connector 1. Refusing both is reported as an error: a
  charger FTW cannot steer must not look like one it can.

## One capacity, several cars: vehicle profiles

`vehicle_capacity_wh` on a charger entry describes the **one** car the charger
usually serves. For a charger shared by several cars, add **vehicle profiles**:

```yaml
vehicles:
  - id: leaf
    name: Nissan Leaf
    capacity_wh: 40000
    identifiers: ["04A2B3C4"]         # RFID tag uid (1.6), MAC / eMAID (2.0.1)
    surplus_only: true                # this car charges from PV surplus alone
  - id: model3
    capacity_wh: 75000
    identifiers: ["aa:bb:cc:dd:ee:ff"]
    target_soc: 0.80                  # planner fills to 80 % in the cheapest hours
```

When a charging session identifies the car, FTW switches the charger to that
car's capacity and charging policy for the session. The capacity reverts on
plug-out; `surplus_only` and the target are set the same way the dashboard
sets them, so they persist until another car (or the operator) changes them.
The identity each session presented is shown in the Chargers tab's Vehicle
column — paste it into a profile's Identifiers from there. The same profiles
are edited under **Settings → Chargers → Vehicles**.

A car matching no profile changes **nothing**. That is the visitor default:
the loadpoint keeps its own settings, and the visitor charges under them.

What "identifies" means depends on the dialect:

- **OCPP 1.6**: the RFID `idTag` that started the transaction. It names the
  card, not the car — profiles work when each card lives permanently in one
  car.
- **OCPP 2.0.1**: idTokens can name the actual vehicle — `MacAddress`
  (autocharge) or `eMAID` (ISO 15118 Plug & Charge) — no card involved.

A wrong or missing capacity skews planning accuracy only, never safety — the
car's own BMS always protects it. An unprofiled car larger than the configured
capacity makes the SoC estimate rise too fast, so a target charge can stop
early; correct the SoC in the dashboard EV modal, or use Force start.

## When the car speaks for itself

With ISO 15118 hardware on OCPP 2.0.1 the car states its own needs, and the
charger forwards them as `NotifyEVChargingNeeds`. That outranks every figure
above: a profile and `vehicle_capacity_wh` are both an operator's estimate of
the car that usually parks here, this is the car actually plugged in.

What FTW takes from it, for the session only:

| The car says | FTW does |
|---|---|
| battery capacity (DC) | replaces the session capacity, over a profile's too |
| present state of charge (DC) | re-anchors the session SoC estimate |
| energy requested | with the two above, derives the target the planner fills to |
| departure time | becomes the loadpoint's target time |

All of it reverts on plug-out, exactly like a profile. A departure time the car
does not state never erases one the operator set.

An **AC** session states energy without a battery size, so there is no fraction
to derive and the target is left alone — a guess there would feed the planner a
number the car never claimed. A departure time still applies.

The last report is shown in the Chargers tab and on `GET /api/ocpp/chargers` as
`charging_needs`. Quarantine applies as everywhere else: a pending charge
point's needs are visible so you can see what asked, and reach no loadpoint.

Most chargers never send this. It needs ISO 15118 on both the charger and the
car; without it, profiles and `vehicle_capacity_wh` remain the whole story.

## Current limits

- **The socket cannot be pinned to one interface.** `bind` refuses the
  handshake instead, so the port stays open on every interface even when only
  one is served.
- Chargers that also have a native protocol may work better through a driver.
  Easee over Modbus and Zaptec over its cloud API already have drivers; OCPP is
  the option when you want the cloud out of the loop.

## When you still need a driver

Only for chargers that do not speak OCPP, or where a vendor protocol exposes
something OCPP does not. The Ambibox V2G / InterControl ambiCHARGE is the
example on the bench: it is DC-coupled and bidirectional, runs over MQTT, and
uses the `ambibox_v2x` driver.

See [writing-a-driver.md](writing-a-driver.md) for that path.
