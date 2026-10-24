# Architecture

FTW is a local-first home energy management system. Its architecture has
three explicit modules: **core**, **drivers**, and **optimizer**. Core is the
safety boundary. Drivers translate hardware protocols. The optimizer proposes
plans and supplies primary forecasts. A failure or upgrade outside core must
never stop local measurement or make dispatch unsafe.

[VISION.md](../VISION.md) defines the product direction and
[the roadmap](roadmap.md) defines acceptance evidence. This document describes
the running system. Product requirements below guide future work and do not
by themselves add runtime behaviour or new protocol capabilities.

## Module boundaries

| Module | Source | Runtime | Responsibility |
|---|---|---|---|
| Core | [`go/cmd/ftw`](../go/cmd/ftw), [`go/internal`](../go/internal), [`web`](../web) | One Go binary | Configuration, telemetry, state, API/UI, safety, control and fallback planning |
| Drivers | Editable source in [`srcfl/device-drivers`](https://github.com/srcfl/device-drivers); the release's own copies in `drivers/*.lua`; host in [`go/internal/drivers`](../go/internal/drivers) | One sandboxed Lua VM per configured device | Vendor protocol, sign conversion and device commands |
| Optimizer | [`optimizer`](../optimizer), contracts in [`go/internal/mpc`](../go/internal/mpc) and [`go/internal/energyforecast`](../go/internal/energyforecast) | Compiled Energyplan worker | Solve the long-horizon plan and supply primary PV and household-load forecasts |

Core can run without the optimizer. Hardware cannot be accessed without a
driver, but one failed driver is isolated from the others. Optional
integrations such as Home Assistant, notifications and Nova attach at
core's API, state or telemetry boundaries; they do not own dispatch safety.

Choose the design with the least total complexity. Moving code out of Core
is useful only when it improves the product's operation or maintenance.
A separate module requires a concrete benefit and:

- a small, explicit and versioned contract;
- independent failure and update semantics;
- no authority to bypass core's validation or safety limits;
- a useful fallback or a cleanly unavailable state.

### Vehicle charge-limit goals

A percentage goal and a goal to reach the car's own limit are distinct.
State schema 7 blocks rollback to a Core that would ignore the saved goal mode.
`GET /api/loadpoints` advertises `vehicle_limit_goal_supported`; clients must
require that flag before saving `schedule.finish_at_vehicle_limit`. Existing
percentage goals keep their meaning. In vehicle-limit mode, the planner uses
a fresh vehicle limit where available. Without one, 100% is a planning bound,
not a claimed vehicle setting. Final charging continues through Core's safety
clamps until the car stops accepting current; an estimate cannot prove it is
finished. A manual Stop still wins.

The current connection's deadline uses verified charger and session identity.
It stays due after the deadline and survives restart when `goal_retention` is
`session`. `unavailable` means that identity is missing; `error` means the
session checkpoint failed. Neither means the saved schedule disappeared.
`pending` means the checkpoint is queued and cannot yet be claimed durable.
Core assigns each saved vehicle-limit goal an `intent_id` and a one-shot
`first_deadline_ms`; clients send user choices, not those bookkeeping fields.
A fresh vehicle Complete can finish a one-shot goal across restart and later
plug sessions. Completion requires a live report from the session car's own
source, made after the observed connection, and no measured charging. A car
the box has not named cannot finish the goal. A charger declining current is reported as a refusal, never
as an invented battery level or proof that the target was reached.

### Cars

A car belongs to its owner and visits chargers. [`go/internal/cars`](../go/internal/cars)
keeps the owners' cars, decides which car each charging session is, and
hands that car's capacity, goal, surplus choice and level to the loadpoint,
which keeps the session's energy and the estimate the planner reads. A car's
reading stays exact until 1 kWh has reached the car. See [cars.md](cars.md).

### History measurement quality

An offline flow leaves working devices recording. Missing inputs make their
values and the derived household balance unknown. Separate additive schema-7
tables preserve partial chart and energy rows, including missing-value evidence,
through retention, downsampling and full backup. A recovered sample cannot fill
an earlier gap. Older Cores see only complete rows in the original tables;
their integration cursor cannot bridge a partial interval after rollback.
Partial rows remain stored and become visible again on the newer Core.


### Control state and disk writes

Core loads EV restart records before starting control. During operation, a
bounded queue writes immutable snapshots outside the loadpoint and model
locks. Repeated updates replace a pending snapshot; an in-flight write keeps
its order. Manual holds commit their hardware binding and old fallback keys
in one FULL-sync transaction. Reads cannot restore a key being replaced.

HTTP and app command acknowledgements wait up to two seconds for the relevant
write queue outside control locks. A timeout reports an active but unconfirmed
choice. It does not undo a Stop or claim that memory survived a power loss.
Shutdown drains accepted writes and reports any failure. This removes disk
waits from these control paths; it does not establish the SD card's goal-save
latency under backup or other filesystem load.

## Product requirements across these boundaries

Discovery, first-day models and controlled commissioning should establish
useful operation without extensive configuration. Keep verified limits,
learned capabilities and user estimates distinct. A failed control integration
must not remain available to planning merely because it still emits telemetry.

The UI and agent clients need a trace from requested intent through acceptance,
dispatch, device result and measured effect, including age and uncertainty.
Reading a value and controlling its source are separate capabilities.

Agents and external automation follow the same Core authority as the planner.
The target includes durable schedule changes and submitted plans, alongside
renewable temporary control. Expired temporary control returns to local
operation; disconnecting a client does not delete a stored household goal.

Cloud MCP should reuse the encrypted client/session path where it fits. The
relay and escrow remain blind. An authorized agent endpoint can read only
what its grant allows, and its access must be revocable. Protocol extensions
require registry changes and paired implementation tests; this section does
not introduce wire names or bypass existing admission rules.

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
     ↘ SQLite                 ↘ API/UI and integrations
```

The in-memory telemetry store owns latest readings and driver health.
SQLite history.db owns bucketed history, dashboard summaries and the energy
ledger. A separate state.db owns goals, device identity, learned state and
planner diagnostics. Prices and forecasts live in cache.db; full backups
keep its past prices, which savings history needs.
Database access stays in
[`go/internal/state`](../go/internal/state).

The control loop computes a site target, allocates it across capable assets,
applies safety constraints, then sends commands through the driver registry.
Planner output is an input to that loop, never a direct device command.

A bounded queue copies each telemetry tick before the SQLite writer commits
its history buckets, energy ledger and retry receipt in one transaction.
Admission to memory is separate from commit. A full queue returns a collection
error; health reports pending, committed and rejected ticks. Reads use WAL
snapshots. Goals and session state use a separate database and sync their WAL
before returning success, so history maintenance does not hold their writer.

Scalar history is stored as buckets in history.db, not as raw polls:
10-second buckets for seven days, one-minute buckets for 90 days and hourly
buckets for five years. Hourly maintenance rolls complete minute hours into
hourly buckets and deletes expired rows in short transactions that yield to
live collection. It reads and writes no Parquet. A tick older than an
interval that maintenance has closed returns an explicit collection error.
Planner diagnostics stay in state.db for seven days. The old
`state.cold_retention_days` setting no longer controls this policy; startup
reports a stored nonzero value.

Dashboard site summaries keep 10-second rows for 24 hours, one-minute rows
for 30 days and five-minute rows for two years.
Energy and cost consume original observed intervals recorded before chart
averaging. Minute energy totals remain for two years, then quarter-hour totals
preserve local day boundaries and normal tariff periods. A range edge or price
change inside a retained interval stays uncovered; no reader invents a split.
Missing site observations break integration. Chart detail keeps the last
observed JSON and its timestamp; the reader marks it as aggregate data that
forecast training must not use. Existing legacy chart tiers remain readable.

Every scalar bucket keeps count, sum, min, max, last value and the first and
last observed timestamps. Means are weighted by sample count, not by elapsed
time. An empty interval stays empty; observed bounds do not prove continuous
coverage. The series API reports the source resolution. Latest-value reads use
the last actual measurement. Gauge averages never replace counter deltas or
power integration in the energy ledger, which consumes original observations.
Five-minute energy detail becomes hourly after 30 days and daily after two
years; totals remain. Reads have time and output limits.

Fresh installations create these buckets directly. Older layouts convert at
startup: frozen legacy history imports in bounded transactions, a history.db
that still holds raw polls is replaced by a copy with only buckets and the
ledger, and old Parquet bucket files are folded into hourly rows before they
are removed. An old Parquet sample day is removed only once its hourly summary
matches the file; until then it stays readable. Old planner-diagnostics
Parquet files stay on the box and remain readable. Only DuckDB beta installations need the separate offline
[history converter](history-conversion.md). Core and normal release builds
have no DuckDB dependency. The [FTWDB experiment is retired](ftwdb-shadow.md).

State schema 5 added aggregate history and binds state.db to a specific
history.db generation. Portable backups export a SQLite read snapshot with row
counts and hashes checked, plus any Parquet files still on the box. A
config-only snapshot cannot recover missing history. To return to an older
Core across a state-schema change, stop Core and restore a verified full backup
with its matching version; rolling back the program alone across that boundary
is refused.

## Drivers

The public `srcfl/device-drivers` repo owns editable driver source, versions,
contracts, tests and FTW's signed release channel. FTW downloads only an
explicitly selected, content-addressed Lua asset after it verifies the signed
manifest. It never runs raw code from the repository branch.

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

Bundled drivers are the release's own drivers and normally run. An owner's
selected signed version runs instead while it is at least as new as the
release's copy, or when it was chosen over a newer one. The signed manifest is
discovery only; FTW verifies the selected artifact against it, and activation
remains explicit and atomic. See
[writing-a-driver.md](writing-a-driver.md) and
[device-repository.md](device-repository.md).

## Optimizer

Beta and stable releases use the bundled Energyplan worker when `planner.engine` is unset
on a supported host. It solves Core's downside PV forecast. Core validates its
plan before publishing it, then runs a bounded Core DP shadow on the same input.
A worker error, timeout or rejected plan invokes Core DP fallback. Core validates
fallback plans too; a failed validation leaves the prior plan in place.

`planner.engine: core` or `energyplan` selects an engine explicitly.
Development builds and other hosts default to Core. Older `engine: python` values
migrate to Energyplan; retired optimizer settings are ignored and omitted
when the configuration is saved.
Energyplan ships as compiled binaries with its own license; source and builds
stay in the private Energyplan repository. It ships in the Core release
package and updates with Core.
The optimizer never reads hardware or issues commands, so its deployment and
dependency churn do not enlarge the safety-critical runtime.

The same worker also supplies the primary PV and household-load forecast through
a separate versioned contract. At the start of each replan, Core freezes the
legacy forecast, weather, occupancy and saved model state. It calls the forecast
worker once under a deadline, outside control and dispatch locks. Core accepts
PV and load independently for each covered interval. If either signal is
missing, late, partial or invalid, Core retains the matching legacy value.
When recent scored errors show one source clearly better for a signal,
Core uses that source; otherwise the worker's quality label decides. Both
signals choose sources within each lead bucket; PV uses daylight errors. A
distant forecast cannot pick the next hour's source or replace its cold-start
load prior. PV and net-load bands use separate day and night
errors where the site's location is known. Each needs its own sample and day
coverage before claiming empirical calibration. The
resulting `champion` can therefore contain Energyplan PV with legacy load, or
the reverse. Each slot's planning margin comes from the errors its own sources
made on earlier issues. `legacy_shadow` keeps both legacy signals from the same frozen
capture for a fair later comparison.

Complete qualified 15-minute observations update the local models outside
dispatch. SQLite stores the latest Energyplan state under
`forecast/energyplan_state_v1`; an update becomes visible only after its full
state has been saved, and startup restores that saved state. The learning
revision binds state to forecast inputs and stable hardware identities. A
binding or input change starts fresh learning, while a compatible program
upgrade can reuse the state. Issued forecasts use a stricter revision that also
includes the worker bytes and pipeline policy, so a Core update that leaves
forecasting alone keeps its scored errors and bands.

Core keeps issued forecasts, frozen inputs, model-state references and qualified
truth in a bounded local archive. The read-only `ftw-forecast-evaluate` source
command defaults to matched `champion` versus `legacy_shadow` results from the
same issue. It does not treat missing or censored truth as evidence.

## Versioning a module contract

Drivers and Energyplan ship with Core. A driver from the signed channel can
still run on another Core, and Core still checks the worker contract before
accepting plans.

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
requirement made while the optimizer still shipped separately: it landed in
core before any optimizer image advertised it, and every site that had not
updated the optimizer silently fell back to the Go planner.

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

Core imports YAML into a versioned document in SQLite once. The seed file then
holds `config_database`, a path relative to that file. Settings saves commit
the document and credential rows together with SQLite `synchronous=FULL`
before applying them through [`go/internal/configreload`](../go/internal/configreload).
The file watcher has been removed; editing the seed does not change live settings.
The first import keeps older YAML fields so a failed update can return to its
previous Core. If that older Core later saves settings, it removes the
unknown database locator. The next upgrade detects that changed source and
imports the newer save. An interrupted import with unchanged source bytes
reuses the committed document.
An unreadable settings database stops startup instead of restoring old seed values.

The first import needs write access to the seed file so Core can record which
database owns it. For a read-only mount, copy the seed into the data directory
and point `-config` there before upgrading. Keep a full backup before migration.
Use Settings for later edits. Moving the state database is an offline operation;
API listener and selected integration changes still need a restart.

State schema 2 marks this settings migration, so an update from older Core
versions takes a full backup first. To return to a Core that reads YAML, stop
Core and restore a full backup with its matching Core version. A downgrade of
the program alone to state schema 1 is refused; the import seed can be older
than the settings saved in SQLite.


Document revisions only prevent stale Settings forms from overwriting a newer
save. They do not change forecast learning revisions, hardware identity, model
weights or the exact bytes of stored forecast snapshots. Backups export YAML
from the same SQLite snapshot so older Core versions also read current settings.

## Remote access boundary

The web client and native iOS app use FTW Cloud. The box keeps one outbound
connection to `cloud.ftw.energy`, over QUIC or WSS. Browsers use WSS. Each
client's own Noise IK session still ends at the box and travels as `tun` inside
the Cloud connection. Core opens no socket to the retired Frankfurt relay and
advertises no old command exchange. Older box software needs an update; older
client pairings may need a new QR. There is no legacy bridge.

The path uses these packages:

- [`go/internal/apiauth`](../go/internal/apiauth) names the caller and scopes.
- [`go/internal/appenroll`](../go/internal/appenroll) holds the box's Noise key,
  single-use pairing codes, QR payload and enrolled client keys beside
  `nova.key`.
- [`go/protocol/appwire`](../go/protocol/appwire) defines frames and Noise IK;
  [`go/internal/appproto`](../go/internal/appproto) handles client requests.
- [`go/internal/appuplink`](../go/internal/appuplink) holds the box's Noise
  sessions and serves them over the Cloud tunnel.
- [`go/internal/cloudlink`](../go/internal/cloudlink) holds the one Cloud
  connection, the signed device roster and the optional readable copy.

The QR pins the box's Noise key. A forwarding service cannot impersonate the
box. This does not protect a web client from code served by a compromised web
origin. The box is not a WebAuthn relying party and holds no passkey: it admits
the first session by a code inside Noise and later sessions by the client's
static key.

The machine identity in [`go/internal/gatewayidentity`](../go/internal/gatewayidentity)
is separate from app enrolment and the Cloud link key. It uses a protected
P-256 key where the hardware supports one and a bound software key elsewhere.
The gateway's display name is a label, never an authenticator.

The service sees a stable home id, public keys, roster roles and scopes, pairing
code hashes, source addresses and traffic timing. It cannot read the inner
Noise session. A separate owner grant enables a signed, readable copy for
Cloud features; that grant is off by default. Core's agent checks require a
separate Noise session and a current owner grant on every request. Neither
transport access nor a readable-copy grant grants an agent control.

Lane 0 frames keep their fixed length. On the Cloud tunnel the box sends on
each reading and on a beat. Commands retain their expiry, preconditions and
Core checks. Loss of Cloud leaves local control, stored goals and local
recovery running.

Browser offline notices travel in bounded, service-only roster rows. The box
encrypts the notice and signs a short-lived VAPID authorization; Cloud checks
the current device and epoch before sending after an outage. Anonymous daily
fleet reports use the separate `/v1/fleet` route, which keeps only daily totals
for 90 days. Escrow remains a separate service. See
[protocol-cloud.md](protocol-cloud.md) for the wire and privacy limits.

The native Watch uses its phone for box commands. Its Cloud one-shot reads and
push registrations do not open a box command exchange. [ADR 0009](adr/0009-watch-exchange.md)
records the old relay design; Core no longer advertises or connects that path.

### Who is let in, and as what

An enrolment carries a role from `contract/registry.yaml`: `owner`, which may
change things, `viewer`, which may only look, or `watch`, below. It is stamped
by the box when a pairing code is spent, and it is stamped from the role the
box minted that code for — never from anything the app sends, because a role a
holder can edit is not a role.

An invite is therefore not a new cryptographic object. It is the same
single-use pairing code with a different role behind it, so the QR payload does
not change shape and the app's scanner needs to know nothing about sharing. A
guest scans what an owner scans and learns what they are from `hello_ok`. What
stops a code being replayed is unchanged and holds for both: it is spent at the
first success, it expires, it survives five wrong guesses and is then burned,
and it rides encrypted inside Noise handshake message 1, so the service carrying
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

A watch is a device of its own that an owner's phone added. Only an owner's
session may ask for its code, and only with a passkey step-up on that request;
the code binds to that phone and has a slot of its own, so it never cancels an
invite. The watch holds the `watch` role's scopes that its phone holds at that
moment, spends its phone's command ids, and is listed and removed with its
phone. The native Watch sends box commands through its phone. Cloud one-shot
reads remain read-only; the retired relay exchange is not a current route.

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

Core retains a LAN-issued spoken-code form for clients that already hold the
box's trust material. Current web and iOS pairing uses a QR over Cloud: the
service lists no spoken-code hash, so a new client key needs a new QR. The
encoding and Core checks for spoken codes are described below.

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

The method used to decide this, and it was wrong. `POST
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
  table from being used to reconfigure the site. Routes marked `NoStepUp` skip
  the ceremony: owner is enough. The charging schedule is the case. It stops
  nothing that a modified client on an enrolled device could not already do
  through `cmd`;
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

The box's anonymous daily report is separate from the Cloud readable-copy
grant. Once a day it posts its FTW version and channel, the driver types in use, a
battery-capacity bucket, its price zone and an install-age bucket. It answers
how many reports arrive and what they describe, which helps size engineering
bets. It is one of two anonymous daily reports under one statistics answer, with
the energy report beside it (ADR 0008).

Since `ftw.fleet/2` it also says, in four buckets, how the box ran: the
slowest plan of the last 24 hours (`none`, `0-1s`, `1-5s`, `5s+`), how many of
those plans fell back (`none`, `0`, `1-3`, `4+`), the share of driver polls
that failed (`none`, `0`, `0-1%`, `1%+`) and the newest update of the last 7
days (`none`, `ok`, `failed`). Core reads them from the plans it already keeps
(`planner_diagnostics`), a 24-hour count of the driver registry's polls in
the telemetry store and the updater's status. The poll count lives in memory,
so until it covers six hours, as after a restart, the share says `none`.
An OCPP charger's messages are not polls and are not counted. A box that does not update itself says `none`, so how it
was installed does not become a field. The service accepts both schemas and
counts health only from reports that carry it.

Once a release names an Oblivious HTTP relay (`go/internal/statsrelay`), it goes
through that relay to FTW Cloud's gateway, sealed, so FTW Cloud never sees the
box's address. Until then it goes over HTTPS to
`https://cloud.ftw.energy/v1/fleet`, and only from a box that said yes. It never
enters the Noise session. The HTTP handler validates the fixed payload, adds it to the UTC
day's counters and drops it. It keeps 90 calendar days of totals, not raw
reports. An operator reads totals only on the loopback metrics listener. An
explicitly saved former default resolves to Cloud; a private collector keeps
its chosen address.

Fleet totals must not form a household record. The message carries no gateway ID, no key, no serial, no site
name, no counter and no timestamp — nothing in it says which box sent it —
values are bucketed rather than reported, the version travels only when it is a
release tag so a developer's build reports as unknown, and the send time is
drawn fresh each day rather than sitting in one slot. Through the relay the
sealed request also carries a random number, drawn new for each report so FTW
Cloud counts a report once if it arrives twice, and the UTC day it was sent, so
an old request is refused. Neither names the box, and FTW Cloud keeps a hash of
the number for three days only.

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
Cloud service's loopback stats response. The fields still describe a household, so the
payload remains a quasi-identifier: a beta box in a small price zone with a big
battery may be the only one like it, and coarse buckets with a small field set
are what keep that population large rather than a proof that it is. The health
buckets are each at most four words wide, describe the last day or week rather
than the house, and most boxes share the common answer; the service keeps each
field's daily count apart, never the combination. The endpoint
also sees the source IP while the request is open when the report goes straight
to it, though the Cloud service writes no request log or source address; through
the relay it does not. And nothing ties two reports to one box, so the totals
count reports, not unique boxes.

A failed send is forgotten, never retried. Settings → FTW app → Anonymous
statistics renders the exact payload from the same call the sender uses, and
what goes beside it, so the claim is checkable rather than promised. It follows
the one statistics answer, whose switch is on that screen. See
[`go/internal/fleetping`](../go/internal/fleetping).

## Releases

There are two channels:

- `beta`: every new release candidate, used for real-site validation;
- `stable`: promotion of the exact commit already published and tested as beta.

Core ships as one release package, `ftw-linux-<arch>.tar.gz`, with the
launcher, the `ftw` command, web files, the release's drivers and Energyplan. On
a native install, systemd starts `ftw-launcher`, which runs Core from release
slots under `/opt/ftw`. `ftw update` downloads the next package into a slot;
the launcher runs it as a trial and commits it only once it becomes ready,
otherwise it goes back to the release that ran before
([ADR 0007](adr/0007-self-updating-binary.md)). No privileged updater runs
on native. The frozen 1.x–3.x Docker line paired Core with a privileged
`ftw-updater` sidecar.

Core and signed Drivers use the same beta-to-stable progression. Optional
components negotiate compatibility with Core. There is no edge channel. See
[self-update.md](self-update.md) and [linux-packages.md](linux-packages.md).

## Start reading

1. [site-convention.md](site-convention.md)
2. this document
3. [`go/cmd/ftw/main.go`](../go/cmd/ftw/main.go)
4. the package or driver being changed and its colocated tests
5. [writing-a-driver.md](writing-a-driver.md) for hardware support
6. [operations.md](operations.md) for deployment and recovery
