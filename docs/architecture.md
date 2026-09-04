# Architecture

FTW is a local-first home energy management system. Its architecture has
three explicit modules: **core**, **drivers**, and **optimizer**. Core is the
safety boundary. Drivers translate hardware protocols. The optimizer proposes
plans. A failure or upgrade outside core must never stop local measurement or
make dispatch unsafe.

## Module boundaries

| Module | Source | Runtime | Responsibility |
|---|---|---|---|
| Core | [`go/cmd/ftw`](../go/cmd/ftw), [`go/internal`](../go/internal), [`web`](../web) | One Go binary | Configuration, telemetry, state, API/UI, safety, control and fallback planning |
| Drivers | Editable source in [`srcfl/device-drivers`](https://github.com/srcfl/device-drivers); bundled recovery in `drivers/*.lua`; host in [`go/internal/drivers`](../go/internal/drivers) | One sandboxed Lua VM per configured device | Vendor protocol, sign conversion and device commands |
| Optimizer | [`optimizer`](../optimizer), contract in [`go/internal/mpc`](../go/internal/mpc) | Optional Python service/process | Solve the long-horizon mathematical plan |

Core can run without the optimizer. Hardware cannot be accessed without a
driver, but one failed driver is isolated from the others. Optional
integrations such as Home Assistant, CalDAV, notifications and Nova attach at
core's API, state or telemetry boundaries; they do not own dispatch safety.

A future module belongs outside core only when it has:

- a small, explicit and versioned contract;
- independent failure and update semantics;
- no authority to bypass core's validation or safety limits;
- a useful fallback or a cleanly unavailable state.

## Power convention

Above the driver boundary, positive power flows into the site and negative
power flows out. Examples: grid import is positive, PV production is negative,
battery charge is positive and battery discharge is negative.

Only drivers convert vendor signs. Core, storage, API, UI and optimizer all use
the site convention. See [site-convention.md](site-convention.md) before
changing power math.

## Core

[`go/cmd/ftw/main.go`](../go/cmd/ftw/main.go) is the composition root. It wires
configuration, driver registry, telemetry, persistent state, control, planning,
API and integrations.
Packages under [`go/internal`](../go/internal) should stay cohesive and communicate through
narrow Go interfaces or data types instead of reaching into one another's
storage.

The main flow is:

```text
device
  ↕ vendor protocol
Lua driver                 optional optimizer
  ↕ site-convention data       ↓ proposed trajectory
telemetry → control/planner → core validation and safety → driver command
     ↘ SQLite/history       ↘ API/UI and integrations
```

The in-memory telemetry store owns latest readings and driver health. SQLite
owns durable configuration state, history, forecasts, prices, device identity
and learned model state. Database access stays in
[`go/internal/state`](../go/internal/state).

The control loop computes a site target, allocates it across capable assets,
applies safety constraints, then sends commands through the driver registry.
Planner output is an input to that loop, never a direct device command.

## Drivers

The public `srcfl/device-drivers` repo owns editable driver source, versions,
contracts, tests and FTW's signed release channel. FTW downloads only an
explicitly selected, content-addressed Lua asset after it verifies the signed
manifest. It never runs raw code from the repository branch. Device Support
may later consume an exact public commit for other products or a higher support
level.

Each Lua artifact still contains its own `DRIVER` metadata and implements the
FTW lifecycle. [`go/internal/drivers/lua.go`](../go/internal/drivers/lua.go) is
the source of truth for FTW's
host API and capability sandbox. Network and protocol capabilities must be
granted in configuration.

Drivers are the only hardware-specific layer. They must:

- translate telemetry and commands to the site sign convention;
- report stable make and serial identity when available;
- implement a safe autonomous default mode;
- avoid policy decisions that belong in core;
- remain independently testable and hot-editable.

Bundled drivers provide the offline recovery set. A signed distribution index
is discovery only; FTW independently verifies the selected package and
artifact, while activation remains explicit and atomic. See
[writing-a-driver.md](writing-a-driver.md) and
[device-repository.md](device-repository.md).

## Optimizer

Core plans. Its DP solves the same problem the Python/CVXPY optimizer does, in
process, against the per-slot PV downside — measured within öre per plan of the
external MILP on replayed site snapshots (#1020).

The Python/CVXPY optimizer is optional and separately deployable. By default it
runs behind Core as a comparison shadow: after each replan it solves the same
inputs, and the terminal-corrected cost difference is logged and recorded on
the diagnostic. Shadow output never reaches dispatch, never delays a replan and
cannot fail one. `planner.engine: python` restores it as the champion during
the transition; then core sends a versioned planning request, accepts only a
complete valid trajectory, and falls back to its own DP if the socket/process
fails, times out or returns invalid output.

The optimizer never reads hardware or issues commands, so its deployment and
dependency churn do not enlarge the safety-critical runtime.

## Versioning a module contract

Drivers and the optimizer release on their own schedules, so core cannot assume
the version on the other side of either contract. Both use the same rule.

Each side declares the **window** of contract versions it speaks — core in
[`go/internal/components`](../go/internal/components) and
[`go/internal/optimizercontract`](../go/internal/optimizercontract), a driver in its
`host_api_min`/`host_api_max` metadata, the optimizer in its handshake reply.
An overlap of one version is enough. Declaring a single version means a window
of one.

Grow the contract by adding **features** to the handshake, not by bumping the
version. A feature an old peer does not advertise costs nothing — core simply
does not ask it for what it cannot do — while a version bump makes every peer
outside the new window incompatible at once. That is the mistake the `champion`
requirement made: it landed in core before any optimizer image advertised it,
and every site that had not updated the optimizer silently fell back to the Go
planner.

When the framing or the request shape genuinely changes, bump the version and
**widen** the window rather than moving it, so sites that have not updated the
module keep working.

## Failure boundaries

Core enforces these invariants regardless of mode or module:

- stale site-meter data stops dispatch;
- stale or failed drivers are put in their autonomous default mode;
- configured power, fuse, SoC and slew limits are enforced after planning;
- incomplete or invalid optimizer output is rejected;
- external integrations fail soft and cannot block the control loop;
- persistent writes and activated driver artifacts are atomic.

The concise safety rationale is in [safety.md](safety.md). Tests next to the
relevant code are the detailed executable specification.

## Configuration and interfaces

[`config.example.yaml`](../config.example.yaml) and the structs plus validation
in [`go/internal/config`](../go/internal/config) define the configuration
schema. The handlers registered in
[`go/internal/api/api.go`](../go/internal/api/api.go) define the HTTP surface. Driver metadata defines
the device catalog. These sources replace manually duplicated reference docs.

External data sources do not all work everywhere. Weather and PV forecasts
are worldwide; live day-ahead spot is European. A site outside ENTSO-E
uses `price.provider: static` — a flat rate or time-of-use schedule —
which is how price-driven planning works without a market feed. See
[data-coverage.md](data-coverage.md).

Some startup bindings cannot be hot-reloaded, including state paths, API
listener and selected integration transports. Normal device and control
configuration is reloaded through
[`go/internal/configreload`](../go/internal/configreload).

## Remote access boundary

Remote access is the FTW app and nothing else. The box holds one outbound WSS
connection to `wss://relay.ftw.energy`; there is no inbound listener, no
NAT-traversal layer, no cloud account and no browser-managed site directory.
See [ADR 0006](adr/0006-app-uplink.md) for why Home Link was removed rather
than kept alongside it.

The path is five packages:

- [`go/internal/apiauth`](../go/internal/apiauth) — who is asking. One value,
  one context key and a scope set, importing nothing of the box so that every
  source of callers can produce one;
- [`go/internal/appenroll`](../go/internal/appenroll) — the Noise static key,
  the rotatable rendezvous secret, the single-use pairing code, the QR payload
  and the list of app keys that have been let in. All of it is boot-time
  material stored beside `nova.key`, not in SQLite;
- [`go/internal/appwire`](../go/internal/appwire) — frames, the Noise IK
  responder and the transport;
- [`go/internal/appproto`](../go/internal/appproto) — the message layer;
- [`go/internal/appuplink`](../go/internal/appuplink) — the outbound
  connection, the per-epoch rendezvous handle and session demultiplexing.

The properties that matter:

- the app's trust anchor arrives optically. The QR payload is a URL fragment,
  which is never sent in an HTTP request, so a hostile or compelled
  `app.ftw.energy` can deny service but cannot impersonate a box;
- the box is not a WebAuthn relying party and stores no user credential. It
  authorises a first connection by the pairing code inside the Noise handshake
  and later ones by the app's pinned static key;
- the relay forwards encrypted frames and holds no keys. The handle the box
  joins under is derived per epoch from the rendezvous secret, so it rotates
  hourly and leaves no stable household ID in the protocol. The relay still
  sees source IP, timing and connection continuity, which can correlate a
  household across handle rotations;
- the machine identity in [`go/internal/gatewayidentity`](../go/internal/gatewayidentity)
  is separate and does not authenticate this connection. It resolves to a
  hardware-protected P-256 key where the hardware exists and a bound software
  key with the same public-key and signature wire contract otherwise, with a
  deterministic adjective-color-animal display name derived from the stable
  18-hex gateway ID. The name is a label, never an authenticator;
- authority is unchanged by the transport. A command carries an expiry and
  preconditions, and core revalidates against fresh state before acting. Core
  remains authoritative for telemetry freshness, validation, clamps, planning
  and dispatch. An unavailable relay leaves local control and local recovery
  intact.

### Who is let in, and as what

An enrolment carries a role from `contract/registry.yaml`: `owner`, which may
change things, or `viewer`, which may only look. It is stamped by the box when
a pairing code is spent, and it is stamped from the role the box minted that
code for — never from anything the app sends, because a role a holder can edit
is not a role.

An invite is therefore not a new cryptographic object. It is the same
single-use pairing code with a different role behind it, so the QR payload does
not change shape and the app's scanner needs to know nothing about sharing. A
guest scans what an owner scans and learns what they are from `hello_ok`. What
stops a code being replayed is unchanged and holds for both: it is spent at the
first success, it expires, it survives five wrong guesses and is then burned,
and it rides encrypted inside Noise handshake message 1, so the relay carrying
it never sees it.

Two rules protect a household from locking itself out, and both live in
`appenroll` rather than in the API layer — otherwise the box's own web UI could
do what the app cannot:

- **the first enrolment on a box is an owner**, whatever code it used. A box
  with no owner cannot be administered by anybody, from anywhere, ever again;
- **the last owner cannot be removed or stepped down.** A household can always
  pair a second owner first and then remove the first.

Sharing has no screen of its own. A guest's phone is a paired phone, so it is a
row in the same device list, with the same Remove button: locking out a stray
key and taking a guest's access away are one action, not two with two sets of
bugs. Changing a role takes effect on a session that is already open, because
both doors re-read the grant on every privileged request.

The routes behind that list have two doors, and what each one PROVES decides
what it opens:

- **the LAN proves presence.** Somebody is in the building. That is the whole
  authority behind a printed square, a guest pass and a spoken code, so it is
  the only door that admits a new owner — by minting an owner's code or by
  promoting a row — and the only one that mints a code to read aloud;
- **an app session proves enrolment.** This is a phone the box already trusts,
  authenticated by its Noise static key, carrying a role re-read on every
  request. It proves nothing about where the phone is. It may see the roster,
  invite a viewer and lock a phone out, gated on `ftw.members.read` and
  `ftw.members.write`, which a viewer's grant does not carry.

The role is never a default. A request that names none is refused rather than
filled in for: the fallback used to be `owner`, so a caller whose role went
missing — the app put it in a query string the box does not read — asked to
share a view and was handed a house. What a caller did not say is a question,
and the answer is 400.

### The code that can be read aloud

A phone with no printed QR and no other paired device still has to be able to
get back into its home. The floor is somebody standing at the box, reading
eight characters down a phone: `XXXX-XXXX`, forty bits from `crypto/rand`, in
Crockford base32 so that I, L and O fold back to 1 and 0 for a listener who
wrote down what they heard.

It is redeemed exactly where a scanned code is, in Noise handshake message 1,
so there is no new endpoint and no new carrier. The wire does not change: the
app decodes the characters back to five bytes and sends those.

**What it cannot do, and the box's page says so on the screen:** it does not
let in a phone that has never seen this box. The typed characters are the
pairing code and nothing else, while the box's static key and its rendezvous
secret travel only in the QR payload — so a phone with no record of this box
has no way to find it and no way to be sure the box answering is the right one.
A typed code re-admits a phone that already holds those, from its own site
record or from a recovery copy. Closing that gap would mean putting a key in a
code somebody reads aloud, or trusting the relay to hand one over, and the
second gives up the property the optical anchor exists for.

Forty bits is safe to say out loud because of what surrounds it, not because of
its size:

- it is minted only through `POST /api/app-link/pairing`, and only from the
  LAN. An app session reaches that route to invite a viewer by QR and is
  refused a spoken code whatever role it asks for, because this bullet is the
  argument that makes forty bits enough and a code mintable from anywhere in
  the world would take it away. A forwarding header is grounds for refusal
  rather than something to parse. Every minting costs somebody a walk to the
  box;
- it is shown only on the box's own page, it is spent on first use, and it
  lives five minutes rather than a scanned code's ten;
- **five wrong guesses burn it.** The counter is on the code, not on the
  caller: an address-keyed counter would inherit the relay's own limiter bug,
  where the documented TLS terminator makes the whole fleet one address. So a
  guesser gets five tries per minting, and each minting needs a person standing
  at the box;
- a wrong guess costs a full Noise handshake and returns nothing at all.

The cost is real and accepted: anyone who can reach the box can burn a live
code by guessing at it, and the household has to ask for another. Denying
somebody a code they can re-mint in a second is a far smaller harm than a code
that can be ground down at leisure.

### The app's window onto the HTTP API

The app can ask the message layer for a handful of things; the box's own web UI
asks its HTTP API for 124. Rather than grow the message layer one view at a
time, an app session can carry an ordinary HTTP request: `api.req` in,
`api.head`, `api.chunk` and `api.end` back, all on the bulk lane because every
one of those varies in length with what was asked.

It runs in process — no socket, no port, no TLS — through
`api.Server.ServeHTTP`, which is the same handler the LAN listener serves,
trust boundary included. This is a security improvement rather than a
relaxation: that API is already served on the home LAN with no authentication
at all, and this door is pinned to an enrolled device, gated by role and tier,
and refused outright for anything that moves energy.

Every route names its own tier, beside the handler it governs, in
`api.routes`. The tier is a fact about what the handler DOES, and the request's
method is never consulted:

- **`Read`** answers a question, changes nothing, and hands back nothing that
  could be replayed as authority. A shared viewer may ask for it;
- **`Configure`** changes a setting. Owner role, and `stepUp` on the request. A
  late execution is the same instruction, only later. `POST /api/config` also
  carries `api.ReplacesAll` and is refused with `E_WHOLE_DOCUMENT`, because it
  writes the whole document and a phone a year behind the box would drop every
  field it never knew about;
- **`Actuate`** moves energy, or takes control of what is moving it. Refused
  with `E_USE_CMD` for everybody. Actuation has one door and it is `cmd`: a
  command carries an expiry and the box revalidates against fresh state, and an
  HTTP request carries neither. `api.Via(op)` names the command that does it,
  where one exists;
- **`Local`** is served only on the box's own page, at home. Either the answer
  holds a credential or a whole file, or doing it needs somebody standing at
  the box. Refused with `E_LOCAL_ONLY`, and neither a role nor a ceremony
  changes that.

The method used to decide this, and it was wrong twice. `GET
/api/caldav/credentials` was priced as a read and handed a shared viewer a
password that is a write channel back into dispatch. `POST
/api/self_tune/start` was priced as ordinary configuration while it pauses
control and drives every battery through ±3000 W for minutes. A verb cannot
know what a handler does, so it is no longer asked.

The cost is one line per route and no free reads: a view written in the app
next year needs the box to have named the path. Two things make that the right
direction to be wrong in. `api.handle` takes the tier as a required argument,
so leaving it out does not compile and an unknown value panics at startup. And
a route that reaches the gate with no tier the gate knows — registered any
other way — is refused as `Local` rather than served. Allow-list over
deny-list, one level up.

The gate itself is one switch in `appproto.gateAPI`, with a branch for every
tier and a closed default. That shape is deliberate: it replaced a chain of
cases with no read branch at all, which is why a route the router happened to
call a read met no check to fail.

The honest limits, which belong here rather than in a comment nobody reads:

- **step-up is a client-side gate.** `api.req` carries `stepUp`, and the box
  cannot verify that a passkey ceremony happened — it has no relationship with
  the authenticator, and being a WebAuthn relying party would need an origin,
  which the box deliberately never has. It stops a phone left unlocked on a
  table from being used to reconfigure the site. It stops nothing that a
  modified client on an enrolled device could not already do through `cmd`;
- **revocation is immediate at the box.** Three layers: the session is torn
  down and the call it was making is cancelled, the grant is re-read from
  `appenroll` on every privileged request so a socket cannot outlive a revoke,
  and the next handshake fails. Nothing can un-send bytes already in a phone's
  cache;
- **the LAN is unauthenticated unless `api.lan_auth` is on.** With the flag
  off, `api.Authenticate` mints a local owner for anything that arrives
  without a caller. With it on, a LAN peer must present the house password
  (Bearer or session cookie) to act as owner; live reads stay open as a
  viewer. Every handler downstream reads its caller from `apiauth.From`.

## Fleet ping

The box's other outbound path, and the only one that carries readable content:
once a day it posts its FTW version and channel, the driver types in use, a
battery-capacity bucket, its price zone and an install-age bucket. It answers
how many reports arrive and what they describe, which helps size engineering
bets.

It goes over HTTPS to a separate `/fleet` door on `relay.ftw.energy`. It never
enters the encrypted WebSocket path: that stays blind and stores no routed
traffic. The HTTP door validates the fixed payload, adds it to the UTC day's
counters and drops it. It keeps 90 days of totals, not raw reports.

The constraint is the one above: Sourceful must not be able to follow a
household. So the message carries no gateway ID, no key, no serial, no site
name, no counter and no timestamp — nothing in it says which box sent it —
values are bucketed rather than reported, the version travels only when it is a
release tag so a developer's build reports as unknown, and the send time is
drawn fresh each day rather than sitting in one slot.

Driver names get their own rule, because a driver file's name is whatever the
thing that installed it called it, and a household can install its own. A name
travels only if it is on one of two lists, and neither list is the contents of
a directory:

- the drivers this build ships with, generated from
  `drivers/BUNDLED_SOURCE.json` into `fleetping.ShippedCatalogue` when the
  binary is built. Every box on a release carries the same list, and nothing
  on a running box can add to it;
- the box's own install history, asked per file: does the row `driverrepo`
  wrote when it installed this exact filename say the manifest verified
  against FTW's own signing key — the one compiled into the binary?

The first list used to be a scan of the directory the bundled drivers sit in,
which was wrong, because `device_repository.root_dir` says where installed
drivers are kept and a config can point it inside that directory. Discovering
the shipped catalogue at boot is asking the config what shipped; fixing it at
build time is not asking. The same reasoning is why the second list is asked
per file. The active directory is still listed, but only to drop records whose
file has gone: a listing can take a name off that list and never put one on it,
so where `root_dir` points does not matter, and an install record is not
something a config writes.

Everything else reports as `other`: a driver somebody wrote, a file copied into
place, a file renamed after it was installed, one from any repository but FTW's
however carefully it signs its own manifests, and one that was installed before
FTW started recording where drivers come from. The last two are what this
costs: a driver another publisher ships is counted but not named, and a box's
existing drivers go unnamed until they are next installed.

The rule is deliberately not asked of the config. The config belongs to the
household and can be rewritten after the fact — an entry can claim the id the
binary pins for the beta channel, be switched off, be deleted outright while
the file it installed stays where it is, or move `root_dir` under the bundled
drivers. What happened during the install is not theirs to rewrite, so that is
what the box records and what the ping reads. This is a rule about names, not
about code: it stops nobody from running the drivers they like, and the one way
left through it is writing an FTW-signed row into the box's own `state.db` by
hand, which this design does not claim to stop.

Three things this does not fix, and each is said on the Settings screen or the
relay's local stats response. The fields still describe a household, so the
payload remains a quasi-identifier: a beta box in a small price zone with a big
battery may be the only one like it, and coarse buckets with a small field set
are what keep that population large rather than a proof that it is. The endpoint
also sees the source IP while the request is open, though neither the relay nor
Caddy writes it. And with no id there is no way to dedupe, so the totals count
reports, not unique boxes.

A failed send is forgotten, never retried. Settings → FTW app → Fleet statistics renders the
exact payload from the same call the sender uses, so the claim is checkable
rather than promised. It is on by default; saving `enabled: false` opts out
without a restart. See [`go/internal/fleetping`](../go/internal/fleetping).

## Releases

There are two channels:

- `beta`: every new release candidate, used for real-site validation;
- `stable`: promotion of the exact commit already published and tested as beta.

Core, Optimizer and signed Drivers may release independently, but all use the
same beta-to-stable progression. Core and its privileged updater remain a
paired control plane; optional components negotiate compatibility with Core.
There is no edge channel. See [self-update.md](self-update.md).

## Start reading

1. [site-convention.md](site-convention.md)
2. this document
3. [`go/cmd/ftw/main.go`](../go/cmd/ftw/main.go)
4. the package or driver being changed and its colocated tests
5. [writing-a-driver.md](writing-a-driver.md) for hardware support
6. [operations.md](operations.md) for deployment and recovery
