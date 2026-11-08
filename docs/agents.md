# Agents and support access

This document defines the agent contract. Some operations remain proposed;
the connected box reports what its build supports. The contract is normative: "must" is a
requirement on the box, FTW Cloud or the app. It extends the app protocol
([srcfl/ftw-webapp docs/protocol.md](https://github.com/srcfl/ftw-webapp/blob/main/docs/protocol.md))
and the [FTW Cloud protocol](protocol-cloud.md). Names in `code` that are
scopes, roles, operations, error codes, capabilities, push kinds, agent
arguments or reads live in `contract/registry.yaml` and
`contract/push-catalogue.yaml`. The message shapes, the approval signature
and the signed offer live in `go/protocol/agents.go`, with golden vectors in
`go/protocol/testdata/approval-vectors.json`.

## What it is for

An owner can let something other than a paired phone reach the box:

- a personal assistant (ChatGPT, Claude, Grok) through MCP at
  `https://cloud.ftw.energy/mcp`;
- Sourceful support, for one ticket;
- the release check, which reads a health summary while the box runs a beta.

FTW Cloud's `keys` part holds the agent's private key and alone opens its
session, for a call through MCP that the agent's OAuth token allows. The box
decides everything: the role, the scopes, the operations, the arguments, the
caps and the expiry. FTW Cloud can admit an agent; only the box can let it
act. Each check runs again on every call, so a revoke or an expiry bites at
the next call whatever FTW Cloud does.

**What FTW Cloud sees.** It holds the agent's key, so it can read every
answer an agent gets. Typed routine reads use opaque source, charger and car ids and redact logs
and configuration. Full technical diagnostics preserve names, addresses,
source code, exact measurements and exports, including backups that can hold
saved credentials. The owner-approved diagnostic scope covers those reads
without extra endpoint grants. An agent has no `sub` stream.

## Principals

| Role | Who | Granted by | Longest grant | Ends |
|---|---|---|---|---|
| `agent` | A personal assistant | An owner, in the app or on the box page | One year, renewable | Expiry, revoke, or removal of the approving phone |
| `support` | Sourceful, for one ticket | An owner, from "Get help" | 14 days | Expiry, revoke, the ticket closing, or removal of the approving phone |
| `release` | The release check | An owner, on joining the beta | 90 days, renewable | Expiry, revoke, or removal of the approving phone |

- Every agent row has a wall-clock expiry. There is no grant without one.
- **Scopes come from the approver at call time.** An agent holds the scopes
  its row lists that its approver holds now: the approving phone's role
  today, or every scope for a grant made on the box page. A phone demoted to
  viewer narrows its agents at the next call. Removing the phone removes its
  agents, as it removes its watches. When another owner widens a grant, that
  owner becomes its approver.
- **A support call names who made it.** Each envelope a `support` session
  sends carries `by`: `staff:<id>` for a person at Sourceful or
  `bot:<name>` for Sourceful's own tooling, at most 64 bytes of letters,
  digits and `._@-` after the colon. The box refuses a support call without
  it, refuses `by` from every other session, and writes it to the audit log.
  The box cannot check it; FTW Cloud signs the staff member in and asserts
  it, and the app labels it "named by FTW Cloud".
- **The ticket closing ends the grant.** When a ticket closes, Sourceful's
  tooling sends `agent.revoke` for its own row. A box offline at that moment
  is revoked again when it next connects; the tooling keeps closed tickets
  until their rows are gone. An agent may always revoke itself. The expiry
  is the backstop.
- **The release check reads one thing.** Its role carries `ftw.health.read`
  and nothing else, so it may send `hello`, `health.get`, `drivers.get`, and
  `agent.list` and `agent.revoke` for its own row.
- **One session per row.** The box holds one session per agent row, as per
  device. The `keys` part carries every MCP call for a row on that one
  session; `by` names who made each support call.

## Scopes

Agent scopes reuse the phone scopes where they mean the same thing and add
these:

| Scope | Lets an agent |
|---|---|
| `ftw.logs.read` | Read a log excerpt that the box redacts before it leaves |
| `ftw.config.read` | Read the configuration, redacted on the box |
| `ftw.diag.run` | Read full technical diagnostics and exports; backups can contain saved credentials |
| `ftw.setup.write` | Read and edit full setup drafts, save the exact confirmed draft and restart to apply it after confirmation |
| `ftw.dispatch.hold` | Hold a charger for a capped time; the hold ends unless renewed |
| `ftw.goals.request` | Ask for a durable change, which waits for an owner |
| `ftw.update.request` | Reserved: no op uses it yet |

The roles `agent` and `support` carry at most `ftw.live.read`,
`ftw.history.read`, `ftw.plan.read`, `ftw.health.read`, `ftw.events.read`,
`ftw.logs.read`, `ftw.config.read`, `ftw.diag.run`, `ftw.setup.write`, `ftw.dispatch.hold` and
`ftw.goals.request`. A grant names a subset. Agents read by default; an owner
ticks control to add `ftw.dispatch.hold` or `ftw.goals.request`. Setup has its
own choice, off by default. Control alone never adds setup access.

## What an agent may send

An agent session is the app session, `ftw.session.v1`, between the agent's
key and the box. It uses the messages below and no others. The box answers
any other request from an agent with `E_SCOPE_DENIED`.

| Message | Needs | Answer |
|---|---|---|
| `hello` | — | `hello_ok`, with the role and the scopes the agent holds now, and no `cloud`. A `sub` in an agent's `hello` is ignored |
| A read in the registry's `reads` | Its scope there | See [Reads](#reads) |
| `cmd` | The op's agent scope, `ftw.dispatch.hold` | `cmd.ack`, `cmd.result` |
| `agent.request` | The op's agent scope, `ftw.goals.request` | `agent.ok` |
| `agent.list` | — | `agent.ok` with the agent's own row and requests |
| `agent.revoke` | — | `agent.ok`; only for the agent's own id |

`sub` and `api.req` are refused to every agent with `E_SCOPE_DENIED`: a
`sub` stream's source table names the devices, and the passthrough's routes
are priced for an owner. A box that lacks one of the reads answers
`E_UNKNOWN_OP`, as it does for any message it does not know. A phone may
send the reads too, under the same scopes.

Every answer to an agent names things by agent id. An error's `args` keep
only keys the box knows to be safe, such as `op`, `arg`, `cap` and `max`,
with charger and source names turned into agent ids and any other text
dropped: a handler written for a phone may name a device, and FTW Cloud
reads every answer an agent gets.

## Operations

Each op in the registry may name an `agent` scope. An op without one is never
open to an agent.

| Op | Agent scope | For an agent |
|---|---|---|
| `loadpoint.hold` | `ftw.dispatch.hold` | `cmd`, under the caps and the hold rules below |
| `site.mode.set` | `ftw.goals.request` | `agent.request` only |
| `planner.prefs.set` | `ftw.goals.request` | `agent.request` only |
| `loadpoint.soc.set` | `ftw.goals.request` | `agent.request` only |
| `loadpoint.surplus_only.set` | `ftw.goals.request` | `agent.request` only |
| `car.goal.set`, `car.soc.set` | `ftw.goals.request` | `agent.request` only, naming the car by its agent id or the charger by `charger_id` ([cars.md](cars.md)) |
| `battery.hold`, `loadpoint.boost` | none | Never. They return when the box can bound them: `battery.hold` has no handler, and a boost has no power of its own to hold to `maxW` |

- **A per-grant allowlist.** A grant lists `ops`, a subset of the ops whose
  agent scope the grant holds. The box refuses an op the grant does not list
  with `E_OP_NOT_GRANTED`, even when the agent holds its scope.
- **An allowlist of arguments.** The registry's `agent_args` says, per op,
  which keys an agent may send, of which kind, in which range, which are
  required, and which grant cap bounds them, with a label the app shows the
  owner. The box refuses any other key, kind or value, in a `cmd` and in an
  `agent.request`, as it arrives, with `E_UNKNOWN_OP {op, arg}`. For
  `loadpoint.hold` that is `id`, `power_w`, `hold_s`, `phase_mode` and
  `clear`; `voltage`, `max_amps_per_phase`, `site_phases`, `phase_split_w`
  and `min_phase_hold_s` stay with owners.
- **Ids are the agent's.** A `loadpoint` argument is the opaque charger id
  the agent read from `loadpoints.get`, and a `car` argument the car id it
  read from `cars.get`. The box resolves each with its own agent key, so an
  id from another box, or from the readable copy, names nothing
  (`E_UNKNOWN_OP {arg: "id"}`). A command's outcome names a car and a
  charger's car by the same ids.
- **A durable change waits.** An agent that sends a `ftw.goals.request` op as
  `cmd` gets `E_NEEDS_OWNER`, with `{use: "agent.request"}`.
- **The checks run in Core's order:** the command's expiry, a known op, the
  live grant (revoke, expiry and clock), the op's agent scope, the
  allowlist, the arguments, the rate, the caps and the hold rules, then
  `expect.rev`, the dispatch gate and the guards.
- **Its own command log.** An agent's command ids live under its own root,
  `agent:<id>`, never an owner's.

### Holds

- **A hold ends unless renewed.** `hold_s` runs from 1 to `caps.holdS`. A
  `loadpoint.hold` with `hold_s` 0 or none is the persistent operator hold,
  and an agent may never send one.
- **The owner's hold wins.** Every hold carries its setter. An agent's hold
  carries `agent:<id>`. While a hold an owner set is active on a charger,
  an agent's `loadpoint.hold` there gets `E_NEEDS_OWNER`, and an agent never
  replaces or clears it. `clear` releases only a hold the same agent set,
  and with none to release gets `E_NOT_FOUND`. The charger's controller
  checks the setter and writes in one step, so an owner's hold that lands
  between the check and the write still wins.
- **One hold per charger every 30 seconds.** An agent that sets holds on
  one charger faster gets `E_RATE_LIMITED {reason: "hold_gap"}` with
  `retryAfterMs`, so it cannot flap a charger between nothing and full
  power.
- **The box ends an agent's holds** when its grant is revoked, narrowed or
  expires, and when the owner phone that approved it is removed or stops
  being an owner, at once, whatever the hold had left.
- **Renewal is a tool call.** FTW Cloud never renews a hold on its own; it
  sends one only in answer to the agent's tool call. Charging for longer than
  `holdS` is a durable goal: a `ftw.goals.request` op, such as the car's goal
  in the car contract.

## Caps

| Cap | Meaning | Box limits |
|---|---|---|
| `holdS` | The longest hold, in seconds | 60 to 1,800. Required with `ftw.dispatch.hold` |
| `maxW` | The largest power a hold may set, in W, in either direction | 1 to 100,000. Required with `ftw.dispatch.hold` |
| `perMin` | `cmd` and `agent.request` per rolling minute | 1 to 60. 0 in a grant means the default, 6 |

- A command past `holdS` or `maxW` gets `E_CAP_EXCEEDED`, with
  `{cap, max}`. The box never clips an argument; it refuses, so the agent
  knows what happened.
- A command past `perMin` gets `E_RATE_LIMITED`, with `retryAfterMs` in the
  error body. The minute is the agent's across all its sessions.
- Reads are limited to 60 a minute per agent, by the box, whatever the caps
  say.

## Durable changes

1. The agent sends `agent.request {op, args, why}`. `why` is at most 280
   bytes of the agent's own words, shown to the owner as the agent's, never
   as the box's.
2. The box checks the op as for a `cmd` (known, granted, scope, rate) and
   checks `args` against `agent_args` and the op's own validation, now, as it
   arrives. It stores the request with a 128-bit `reqId` and answers
   `agent.ok {reqId, expiresAtMs}`.
3. The box pushes `agent.request` to every owner through its event path, and
   writes an audit row.
4. An owner reads it with `agent.list`. The app shows the op and each
   argument by its label, and the agent's `why` as the agent's words. The
   owner answers with `agent.confirm`. Approving needs an approval signature
   over the request id, the agent, the op and the hash of the args, and a
   wall clock the box trusts, or it gets `E_UNAVAILABLE {subsystem: clock}`;
   declining needs neither.
5. On approval the box runs the op on the confirming owner's session as a
   command with id `req:<reqId>`, under the owner's scopes and against fresh
   state, and sends that session `cmd.ack` and `cmd.result`. Nothing the
   agent sent decides whether it may act; the owner's confirmation does.
6. When a request is decided or expires, the box sends the agent's session,
   if one is open, `agent.decided {reqId, state, cmdId, result}`. A request
   no owner answers within 12 hours expires. The box keeps a decided or
   expired request for 24 hours, so the agent can read its state with
   `agent.list`.

Pending requests live in memory. A restart drops them, and the agent asks
again.

## Messages

All of these ride in the app session. Each request carries an envelope `id`
and gets `agent.ok` with that id, or `error` with that id and a code. The
box decodes a signed message strictly: a key it does not know, or a key
twice, is `E_UNKNOWN_OP {field}`.

### `approval.key.set` — an owner phone registers its approval key

| Field | Type | Meaning |
|---|---|---|
| `pub` | bstr, 65 | P-256 public key, SEC1 uncompressed |
| `atMs` | int | The phone's wall clock |
| `sig` | bstr, 64 | The new key's signature: proof the phone holds it |
| `prevSig` | bstr, 64, optional | The phone's current key's signature over the same bytes |

Answer `{deviceId, setAtMs}`. Only an owner phone may send it, and only for
itself.

- A phone with no key yet registers one with `sig` alone.
- Replacing a key needs `prevSig` from the key it replaces. A phone that lost
  its key (a new Face ID enrolment voids it) replaces it on the box page
  with the house password.
- The box pushes `approval.key.set` to every owner and writes an audit row.

Errors: `E_SCOPE_DENIED` (not an owner), `E_BAD_SIGNATURE` (`sig` or a
missing or wrong `prevSig`), `E_CLOCK_SKEW`, `E_UNKNOWN_OP` with `{field}`.

### `agent.add` — an owner grants access

| Field | Type | Meaning |
|---|---|---|
| `key` | bstr, 32 | The agent's X25519 Noise static key, from the signed offer |
| `role` | text | `agent`, `support` or `release` |
| `scopes` | [text] | Sorted, no repeats, within the role |
| `ops` | [text] | Sorted, no repeats, each with an agent scope in `scopes` |
| `caps` | `{holdS, maxW, perMin}` | Uints; 0 where not used |
| `expiresAtMs` | int | Wall clock, within the role's longest grant |
| `principal` | `{client, clientId, ticket}` | Text, each at most 256 bytes, empty where not used. `ticket` is required for `support` |
| `atMs` | int | The phone's wall clock |
| `sig` | bstr, 64 | Approval signature |

Answer `{agent}`, the new [agent row](#agent-row). The box signs a new
roster that lists it, pushes `agent.added` to every owner and writes an audit
row. Errors: `E_SCOPE_DENIED` (not an owner phone), `E_NO_APPROVAL_KEY`,
`E_BAD_SIGNATURE`, `E_CLOCK_SKEW`, `E_CAP_EXCEEDED` (`{cap, max}`, for a
cap, the expiry, or 16 agent rows on the box), `E_UNAVAILABLE`
(`{subsystem: "clock"}`), `E_UNKNOWN_OP` (`{field}`). A key whose row id is
already taken, by a phone or an agent, is `E_UNKNOWN_OP` with
`{field: "key"}`.

### `agent.widen` — an owner changes or renews a grant

| Field | Type | Meaning |
|---|---|---|
| `id` | text | The agent row's id |
| `epoch` | uint | The row's epoch as the owner saw it |
| `scopes`, `ops`, `caps`, `expiresAtMs` | | The whole new grant, as in `agent.add` |
| `atMs`, `sig` | | As in `agent.add`; the signature covers the row's key too |

Any change other than a revoke needs a signature, narrowing included; one
rule is easier to check than two. The epoch rises, and the widening owner
becomes the approver. Errors as `agent.add`, plus `E_NOT_FOUND`, and
`E_CONFLICT` (`{expected, actual}`) for a stale epoch, sent with
`retryable: false`: the owner reads the row again before signing again.

### `agent.revoke` — remove a grant

`{id}`. From an owner, any agent row; from an agent, its own. No signature.
The row goes at once, its sessions end with `session.terminate {reason:
"revoked"}`, its holds end, its pending requests are dropped, and the next
roster leaves it out. Answer `{id}`. Error: `E_NOT_FOUND`.

### `agent.list` — who has access

`{}`. To an owner: `{agents: [agent row], requests: [request], keys:
[{deviceId, setAtMs}], longestMs: {role: ms}}`, every row on the box and the
longest grant it takes for each role. To an agent: its own row and its own
requests. A viewer gets `E_SCOPE_DENIED`.

### `agent.request` — an agent asks for a durable change

`{op, args, why}`. `args` is the op's argument map, at most 1 KiB encoded,
within `agent_args`. Answer `{reqId, expiresAtMs}`. Errors:
`E_OP_NOT_GRANTED`, `E_SCOPE_DENIED`, `E_RATE_LIMITED`, `E_CAP_EXCEEDED`
(`{cap: "pending", max: 4}` per agent, 16 per box), `E_UNKNOWN_OP`
(`{op, arg}` for an argument).

### `agent.confirm` — an owner answers a request

| Field | Type | Meaning |
|---|---|---|
| `reqId` | text | The request |
| `approve` | bool | Run it, or decline it |
| `atMs`, `sig` | | Required to approve. Absent to decline |

Answer `{reqId, state, cmdId}`: `state` is `approved` or `declined`, and
`cmdId` is the command the box now runs on this session. Errors:
`E_NOT_FOUND` (unknown, decided or expired), `E_NO_APPROVAL_KEY`,
`E_BAD_SIGNATURE`, `E_CLOCK_SKEW`, `E_SCOPE_DENIED`.

### `agent.decided` — the box tells an agent how a request ended

Box to agent, unsolicited, when its session is open: `{reqId, state, cmdId,
result}`. `result` is the command's `cmd.result` body when the request ran.

### `audit.get` — what agents and owners did

`{beforeSeq, n}`; `n` at most 200. To an owner phone:
`{rows: [audit row], more}`, newest first. Agents and viewers get
`E_SCOPE_DENIED`.

### Agent row

| Field | Type | Meaning |
|---|---|---|
| `id` | text | First 8 characters of the key in base64url, like a phone's device id |
| `key` | bstr, 32 | X25519 Noise static key |
| `role` | text | `agent`, `support` or `release` |
| `scopes`, `ops` | [text] | As granted |
| `caps` | `{holdS, maxW, perMin}` | `perMin` as the box applies it |
| `expiresAtMs` | int | Wall clock |
| `principal` | `{client, clientId, ticket}` | |
| `approvedBy` | text | `app:<deviceId>` or `lan` |
| `addedAtMs`, `lastSeenMs` | int | Wall clock, 0 when unknown |
| `epoch` | uint | Rises with every change |

### Request

`{reqId, agent, op, args, why, atMs, expiresAtMs, state}`. `state` is
`pending`, `approved`, `declined` or `expired`. `reqId` is 16 random bytes in
base64url.

### Audit row

`{seq, atMs, actor, role, principal, by, what, target, result, count}`.
`actor` is the caller subject (`agent:<id>`, `app:<id>`, `lan:<address>` or
`box`); `principal` is the client or ticket; `by` is who made a support call;
`what` is the message type or op; `result` is `ok` or the error code.
`count` folds an agent's reads and refusals: one row per agent, message,
`by`, result and minute, with how many there were. Grants, keys, commands
and requests get a row each, from a budget of their own.

### Reads

The registry's `reads` table names each read and the scope it needs.

| Message | Request | Answer |
|---|---|---|
| `live.get` | `{}` | `live`: FTW Cloud's `live` body, with agent ids |
| `loadpoints.get` | `{}` | `loadpoints`: FTW Cloud's `lp` body, each charger and its car by agent id |
| `cars.get` | `{}` | `cars`: FTW Cloud's car class body, each car's level, source, age and goal by agent id |
| `plan.get`, `price.get` | As for a phone | `plan`, `price` |
| `hist.query` | As for a phone | `hist.chunk`, `hist.end` |
| `health.get` | `{}` | `health {version, uptimeS, clock, drivers: {ok, degraded, faulted, offline}, meter: {srcState, ageMs}, dispatchBlockedBy}` |
| `drivers.get` | `{}` | `drivers {drivers: [{src, kind, state, ageMs}]}` |
| `events.get` | `{n, beforeMs, afterMs}`, `n` at most 50 | `events {events, more}`, each FTW Cloud's `event` body with agent ids |
| `logs.get` | `{n, level, beforeMs, afterMs}`; `n` at most 200, `level` `WARN` by default | `logs {lines: [{ts, level, src, msg}], dropped, more}` |
| `config.get` | `{section}` | `config {sections}` without a section; `config {section, doc}` with one |
| `diag.run` | `{check, src}`; `check` is `clock`, `link`, `meter`, `planner` or `driver` | `diag {check, ok, facts}` |

- **Agent ids.** Every id in an answer to an agent, for a source, a charger
  or a car, is cut the way the readable copy cuts its ids
  (`cloudwire.SourceID`, `cloudwire.LoadpointID`, and the car contract's car
  id), under the box's agent key instead of `grantKey`: HMAC-SHA-256 under a
  key the box derives from its own static secret. It is stable on one box,
  says nothing on another, and does not depend on a readable copy that may
  be off. A log line's `src` is the same id. The box resolves an agent id in
  a `cmd` or an `agent.request` with the same key.
- **Cursors.** `beforeMs` asks for lines or events older than that wall time,
  `afterMs` for newer ones; the answer is oldest first, and `more` says more
  remain on the asked side.
- Each answer fits one bulk frame (16 KiB). `logs.get` drops its oldest
  lines to fit and counts them in `dropped`. A config section that does not
  fit gets `E_RANGE_TOO_LARGE`; an unknown one `E_NOT_FOUND`.
- `health` carries no readings and no names: drivers are counted, and a
  source that blocks dispatch goes by its agent id. Planner solve times,
  fallbacks, refusals and plan against actual join it when the box keeps
  them; a field the box cannot fill is absent.
- The box answers `events.get` and `diag.run` with `E_UNKNOWN_OP` until it
  builds them. `cars.get` carries no name, maker, VIN, identifier or source,
  and the redactor takes those out of `logs.get` and `config.get`; a box
  without the car store answers it `E_UNAVAILABLE`. `plan.get` names each
  charger and its car by agent id.
- `logs.get` looks back over the box's whole log ring, its newest 2,000
  lines. `beforeMs` and `afterMs` together pick the lines between them; an
  `afterMs` at or past `beforeMs` gets `E_UNKNOWN_OP {field: "beforeMs"}`.

## Technical reads and setup

`api.read` carries `APIRead` from `go/protocol/agentapi.go` and requires
`ftw.diag.run`. Start with `/api/diagnostics/catalog` to get this build's
routes, source links and bug-report workflow. It carries GET reads only.
Large exports use byte pages and a digest to detect a changed answer.
Typed routine reads keep the redaction described below.

The registry's `agent_calls` table defines seven setup messages:

| Message | Right | JSON request |
|---|---|---|
| `setup.schema` | `ftw.diag.run` | `{}`; returns full typed schema, driver catalogue and price zones |
| `setup.status` | `ftw.diag.run` | `{}`; returns masked draft, revisions, digest and save receipt |
| `setup.edit` | `ftw.setup.write` | `{revision, patch}` or `{revision, operations}` |
| `setup.reset` | `ftw.setup.write` | `{revision}`; starts a new draft from current settings; does not restart Core |
| `setup.validate` | `ftw.setup.write` | `{revision, base_revision, sha256}` |
| `setup.commit` | `ftw.setup.write` | `{revision, base_revision, sha256, confirmed}` |
| `setup.restart` | `ftw.setup.write` | `{saved_revision, sha256, confirmed}`; only the exact saved setup |

Each request is `SetupCall {json: bytes}` from `go/protocol/setup.go`, at most
10 KiB within one bulk frame. Replies use `api.head/chunk/end`. A merge patch
replaces whole arrays. JSON Patch `add`, `replace` and `remove` can edit one
entry without resending a large list. Indexes refer only to the exact draft
revision; a concurrent edit conflicts. Core rejects unknown config fields.

A read never discovers equipment or applies settings. The agent asks for
missing facts in small groups, accepts unknown answers, validates the exact
draft and shows changes and restart needs before asking the user to confirm.
Commit binds both revisions and the digest, checks the live grant again at
the final save, then uses the normal Settings transaction and applier. The
receipt commits with Settings; retrying that exact save returns the receipt.
Replies distinguish saved, applied, restart pending and hardware verification.
A later draft or setup save keeps earlier restart needs until a real process
restart. A confirmed `setup.restart` checks the saved receipt, live revision
and live grant again just before signalling the existing graceful restart.
A lost reply leaves the outcome unknown; read setup status and the receipt
before retrying the same operation. A restart request never proves application.

Setup reads use the existing full diagnostic grant because they retain names,
addresses and connection details while masking credentials. They do not widen
an old redacted `ftw.config.read` grant. Replies to setup writes carry the same
masked draft under the owner's new, explicit setup grant.

Setup keeps the agent's identity. It opens no general HTTP writes, access
grants, Cloud consent, house-password changes or direct hardware commands.
Saved equipment settings can affect operation under Core's safety checks.
Physical measurement and control need separate evidence. First-run claiming,
the guided interview and sharing codes follow [ADR 0011](adr/0011-agent-led-setup.md).

## Redaction on the box

`logs.get` and `config.get` build their answer on the box. Nothing leaves it
unredacted, and nothing is stored in the cloud.

- **Logs.** The box keeps the time, the level and the box's own message
  text, scrubbed. A driver's free text never leaves: its line keeps the
  level, the driver's agent id and the attributes the allowlist below
  keeps. Text is scrubbed of every name the configuration knows (site,
  drivers, chargers, vehicles and their identifiers; the car store's names
  join when the box has one), IP and MAC addresses, host names, URLs,
  e-mail addresses, quoted text, serial-like words (letters and digits
  mixed, six or more), runs of seven or more digits, and anything that
  looks like a credential: bearer and basic headers, secret-named keys and
  long tokens. A host name is a dotted name that ends in any two-letter
  label, as every country's does, or in a common longer one such as `.com`
  or `.xyz`. A code word such as `planner.prefs.set` ends in neither.
- **Attributes.** A line's attributes keep only keys on the box's
  allowlist. A value survives when its key is allowed and the value is a
  flag, a duration, a short lowercase code word, or a number for a key the
  allowlist marks numeric. So coordinates, phone numbers and energy totals
  under any other key do not leave.
- **Config.** Every secret-named key is replaced by `***`. Names become
  agent ids. Location, addresses, host names, URLs, serials, MAC addresses,
  vehicle and car ids, VINs, e-mail addresses and notification targets are
  removed. A number stays only under a key that ends in its unit (`_w`,
  `_kwh`, `_s`, `_pct`, `_soc` and the like) or names a known setting such
  as `phases`; any other number goes, so a port, a Modbus unit id or an
  account number does not leave.
- **Vectors.** `go/protocol/testdata/redaction-vectors.json` holds lines in
  and out, so a change to the rules shows.

Redaction is a list of patterns, so text in a shape none of them matches can
pass. That is why driver free text stays home.

## The agent's roster entry

The roster the box signs for FTW Cloud lists each agent row as a `Device`
with one extra map, `agent`, and carries the box's Noise key in `noise`:

| Field | Type | Meaning |
|---|---|---|
| `id` | text | The row's id |
| `key` | bstr, 32 | X25519 Noise static key |
| `role` | text | `agent`, `support` or `release` |
| `scopes` | [text] | Empty: an agent reads the box, never the readable copy |
| `epoch` | uint | |
| `tun` | bool | False: no door admits an agent row; only the `keys` part opens its session |
| `agent.scopes`, `agent.ops` | [text] | The grant, at the approver's scopes now |
| `agent.caps` | `{holdS, maxW, perMin}` | |
| `agent.expiresAtMs` | int | Wall clock |
| `agent.principal` | `{client, clientId, ticket}` | |

- The box leaves out a row that has expired by its clock. While its clock is
  `none`, it lists rows as they are and refuses their calls.
- FTW Cloud must stop admitting a row at `expiresAtMs` by its own clock,
  without waiting for a roster. It must never admit an agent row on
  `/v1/app`, `/v1/app/once` or the relay, and it may tighten caps but never
  loosen them.

## Agents use the tunnel only

An agent's session reaches the box through FTW Cloud as `tun`, opened by the
`keys` part. The relay refuses it: a handshake over the relay from an agent's
key ends as from a stranger. The watch exchange refuses it too.

## Expiry, revoke and the clock

- **Expiry is wall clock.** The box checks it at the handshake, on every
  call and once a second for an open session. An expired row ends its
  session with `session.terminate {reason: "expired"}`, answers its next
  call with `E_GRANT_EXPIRED`, loses its holds and leaves the roster. A sweep
  removes it and writes an audit row.
- **No clock, no agents.** While the box clock is `none` (NTP has not
  synchronised it), every call from an agent gets `E_UNAVAILABLE` with
  `{subsystem: "clock"}`, and `agent.add` and `agent.widen` are refused the
  same way. Owners' phones are not affected.
- **No going back.** The box keeps the highest wall time it has seen on disk.
  A clock below it counts as `none` for agents, so setting the clock back
  cannot revive an expired grant.
- **Revoke is immediate.** The row goes, the sessions end, the holds end,
  and the next call fails with `E_GRANT_REVOKED`, whatever the cloud admits.
- **A warning first.** A day before a grant runs out, the box pushes
  `agent.expiring` to every owner, whatever the grant's length.
- **The owner picks the length.** For an `agent`, the app and the box page
  offer 1, 7, 30 and 90 days and a year. The app starts at 30 days, the box
  page at a week. Neither offers more than the offer's `ttlS` or the box's
  `longestMs` for the role.

### Mixed versions

Boxes before `longestMs` took at most 30 days for an `agent`, and refuse a
longer expiry with `E_CAP_EXCEEDED` rather than cut it short. A client that
finds no `longestMs` in `agent.list` therefore offers an `agent` 30 days at
most. FTW Cloud asks for a year in every offer, since it makes the offer
before the owner picks a home; apps and box pages from before this change
offer 30 days at most whatever the offer asks.

## The approval signature

The passkey is the web app's (`app.ftw.energy`) and the box cannot check it,
so `stepUp` is only the app's word. Agent grants need something the box can
check.

- **The key.** Each owner phone makes a P-256 key in the Secure Enclave,
  usable only after Face ID or Touch ID with the current enrolment
  (`biometryCurrentSet`). It cannot leave the phone. The phone registers the
  public half with `approval.key.set` over its owner session.
- **What it signs.** `agent.add`, `agent.widen`, `agent.confirm` to approve,
  and `approval.key.set` itself as proof of possession. `agent.revoke` and a
  decline need no signature.
- **The bytes.** The signature is ECDSA P-256 with SHA-256 over the
  deterministic CBOR (RFC 8949, section 4.2.1, floats in their shortest
  exact form) of one array, encoded `r ‖ s` in 64 bytes. CryptoKit's
  `signature(for:)` hashes the bytes itself, so the phone passes the array's
  bytes, not their digest.

  ```
  ["ftw.approval.v1", what, boxKey, deviceId, atMs, ...fields]
  ```

  | `what` | Fields after `atMs` |
  |---|---|
  | `approval.key.set` | `pub` |
  | `agent.add` | `key, role, scopes, ops, [holdS, maxW, perMin], expiresAtMs, client, clientId, ticket` |
  | `agent.widen` | `id, key, epoch, scopes, ops, [holdS, maxW, perMin], expiresAtMs` |
  | `agent.confirm` | `reqId, agent, op, argsHash` |

  `boxKey` is the box's X25519 static key (32 bytes, as in the QR code), so a
  signature for one box is worth nothing at another. `deviceId` is the
  signing phone's id, and the box checks it is the session's. In
  `agent.widen`, `key` is the row's whole key from `agent.list`. In
  `agent.confirm`, `agent` and `op` are the request's and `argsHash` is
  SHA-256 over the deterministic CBOR of the request's `args` as the phone
  received and showed them. Lists are sorted with no repeats, empty text is
  `""`, and integers take their shortest form.
- **Freshness.** `atMs` must be within 10 minutes of the box's wall clock;
  outside it the box answers `E_CLOCK_SKEW {boxMs}`, and the phone may sign
  again with the box's time. An `atMs` from before the box last started is
  refused, because the box remembers what it took only since then. It keeps
  the SHA-256 of each signed array it accepted for 20 minutes and refuses
  the same bytes again, whatever the signature looks like.
- **What it proves.** Someone passed Face ID on that phone and approved
  exactly these bytes. It does not stop a modified app on an enrolled owner
  phone that registers a key of its own while the phone has none: that app
  could already send commands. Once a phone has a key, replacing it needs the
  old key or the house password. It stops a phone left unlocked on a table,
  and it lets the box check approval without trusting the app or the cloud.
  With `api.lan_auth` off, anyone on the home network can make an owner
  code and pair an owner phone; the house password is what closes that.

## Approval on the box page

On the home network an owner manages agents at `/api/agents`. These routes
are `local`: the passthrough never carries them, so a phone uses the messages
above and its approval key, and an agent can never reach them.

| Route | Who |
|---|---|
| `GET /api/agents` | A client on the LAN. Rows, requests and keys, the box's facts and the registry's agent ops |
| `GET /api/agents/audit?before=&n=` | A client on the LAN |
| `POST /api/agents/offer {offer}` | A client on the LAN. The offer as the box reads it; grants nothing |
| `POST /api/agents {offer, scopes, ops, caps, ttlS}` | A browser signed in with the house password, on the LAN or on the box |
| `POST /api/agents/{id}/widen {epoch, scopes, ops, caps, ttlS}` | The same |
| `POST /api/agent-requests/{reqId} {approve: true}` | The same |
| `POST /api/agents/approval-key {deviceId, pub}` | The same: a phone that lost its key |
| `POST /api/agents/clock-reset` | The same. Ends every agent's grant |
| `POST /api/agent-requests/{reqId} {approve: false}` | A client on the LAN |
| `DELETE /api/agents/{id}` | A client on the LAN |

- A request that brings a house password, and a wrong one, is refused on
  every route, the narrowing ones too.
- **The page checks before it asks for a code,** since a code works once.
  `GET /api/agents` carries `box {offers, lookup, clock, nowMs, markMs,
  widen, longestMs}`: whether this build pins an offer key, FTW Cloud's code
  lookup on the host this build links to, whether the clock is `ok`,
  `unsynced` or `behind` its mark, whether this request could widen access,
  and the longest grant the box takes for each role. It also
  carries `registry {agentOps, writeScopes,
  agentArgs}`, so the page names no scope, op or argument by hand.
- **The page names the charger and the car.** A request's `args` hold the
  box's own ids, as an owner's `agent.list` does. On this route alone each
  request also carries `names`: for every argument of kind `loadpoint` or
  `car` that the box still knows, what the box calls it. A charger's name is
  its id; a car's is the name the owner gave it. The page is local and its
  caller an owner; nothing an agent or FTW Cloud reads carries `names`.
- **The box says why it will not take an offer.** `POST /api/agents/offer`
  and `POST /api/agents` answer an offer the box will not take with 400 and
  `{error, reason, boxMs}`: `reason` is `no_offer_keys`, `unsigned`,
  `expired`, `malformed` or `other_home`. A clock the box cannot trust is
  `E_UNAVAILABLE {subsystem: "clock"}` first, since every offer would
  otherwise read as expired.
- `clock-reset` is for a clock that once ran ahead. The box never trusts a
  wall time below the highest it has seen, so until the clock passes that
  time again every agent is locked out. The reset makes now the mark and
  ends every agent's grant, since a grant made while the clock ran ahead
  would end years late.
- `offer` is the [signed offer](#the-grant-offer), base64url. The box checks
  its signature against the offer keys built into it and refuses one that is
  unsigned, expired, or for another home. The page gets it from the owner:
  the owner types the consent page's code, and the page, in the owner's
  browser, fetches the signed offer from FTW Cloud's code lookup and posts
  it here. The owner may grant less: `scopes`, `ops`, `caps` and `ttlS`
  replace the offer's when given. Without `ttlS` the grant lasts 30 days, or
  the offer's length if shorter. A `ttlS` past the role's longest grant is
  `E_CAP_EXCEEDED {cap: "expiresAtMs", max}`.
- **Every route that widens access needs a browser signed in with the
  house password,** on another device or on the box itself. The password,
  not the address, proves the owner. Loopback stays an owner for settings
  without it, so it could replace the password and then sign in: it is not
  a wall against programs on the box. A driver cannot reach the API
  (below), and the session passthrough is refused. `box.widen` in
  `GET /api/agents` says whether this request would pass, so the page stops
  before a code is spent.
- **Drivers never reach the box's API.** A driver's HTTP, TCP or WebSocket
  connection to a port the API listens on is refused, whether it goes to a
  loopback address or to an address of the box. The check runs on the
  address dialled, after any name lookup. Other ports stay open: a driver
  may talk to a helper on the box that its configuration names, such as a
  bridge on `127.0.0.1:8787`. A reverse proxy, or Home Assistant ingress,
  that serves the API on another port is not covered by this check. There
  the house password still guards every widening route, and the proxy's
  forwarding headers make those routes refuse.

What the sign-in is worth today:

- **Off until someone chooses it.** A new box asks for it during setup; an
  older box may still have it off. With `api.lan_auth` off every client on
  the home network is an owner with no sign-in. Granting a remote agent
  from there would let any device on the Wi-Fi give the outside world a way
  in, so those routes refuse with 403 until the house password is on. The
  Access page asks for it right above the code: any browser that reaches
  the box may choose the first one, which signs that browser in. While the
  lock is off that browser is already an owner, so choosing it grants
  nothing new (Fredrik, 25 October 2026). An owner locked out by someone
  else's password, or who forgot it, runs `ftw password reset` on the box;
  see [operations.md](operations.md#house-password).
- **With the house password on.** Its length is the owner's choice: the
  page warns below 8 characters but takes any password that is not empty
  (Fredrik, 25 October 2026). It is stored as argon2id (64 MiB, 3
  passes). Five wrong guesses lock every sign-in for 30 seconds, box-wide.
  A sign-in sets a random 32-byte cookie,
  `HttpOnly` and `SameSite=Strict`, that lasts 30 days, survives a restart
  and ends when the password changes. Browser requests from other sites are
  refused by Origin and `Sec-Fetch-Site` checks.
- **What it lacks.** The page is plain HTTP, so anyone who can read the home
  network's traffic can read the password and the cookie. There is one
  shared password and no person behind it, so the audit row names the
  address.

Narrowing stays open to any LAN client, as `DELETE /api/cloud/grant` does,
because removing access is safe. With `api.lan_auth` on, every route here,
the reads and the narrowing ones too, needs the sign-in, as every protected
route on the box does.

## Agent invitations

A box with `access.invites` supports `agent.invite.create`,
`agent.invite.list` and `agent.invite.cancel`. These messages require a current
owner phone. Creating also requires its P-256 approval signature. An agent
cannot create invitations, including with `ftw.setup.write`.

The owner chooses scopes, operations, caps, a label, a claim deadline within
24 hours and a separate access expiry within the agent role's longest grant.
The label helps the owner find the invitation; it does not verify a person's
identity. A code grants the first recipient who uses it a separate agent key,
never the owner's token, key or role. Technical diagnostics include complete
backups, which can contain saved credentials. Setup and control require an
explicit choice.

Create carries `{home, id, hash, label, scopes, ops, caps, claimUntilMs,
expiresAtMs, atMs, sig}`. The client draws a 16-byte random `id` and a 32-byte
random secret. It signs these fields in that order after the normal approval
prefix, with caps as `[holdS, maxW, perMin]` and sorted scopes and operations.
The hash and Noise claim format are in [the Cloud protocol](protocol-cloud.md#agent-invitations-through-the-service).
The strong publishing proof is base64url of deterministic CBOR `{v: 1, home, box, secret}`.
Core stores only the verifier, not the secret. The LAN page may send the
secret in its protected POST so Core can compute the verifier on plain LAN
HTTP, where browser hashing APIs may be unavailable. It never logs it.

An exact create retry returns the existing receipt, including claimed,
cancelled or expired states. A changed request with the same id conflicts.
Clients keep the exact signed request and secret in memory while the code is
on screen. A lost reply never starts a fresh invitation. A list contains
metadata and claim receipts, never a code. When the code is no longer held
locally, the owner can cancel the old invitation and create a new one.
The app list also returns `nowMs` and `clockReady`. Read the box's time before
signing a new invitation so phone clock differences cannot exceed the claim
or grant limits. A known signature refusal may retry the same fields with
the phone's replacement key; an unknown outcome must keep the exact request.

Cancelling needs no new signature, like revoking an agent, and returns the
invitation's current state. If claim already won, it returns `claimed` with
the agent id; the owner removes that ordinary connection instead. A clock
reset cancels pending invitations as well as ending existing agent grants.
Core admits at most 16 pending invitations. It retains live claim windows and
receipts for current agents, plus 32 old receipts, with a 128-row storage
bound. No expired or cancelled record can reopen a valid code after pruning.

The local routes are `GET/POST /api/agent-invites` and
`DELETE /api/agent-invites/{id}`. POST uses the same house-password gate
as an ordinary grant. Signed app messages allow an
owner to share access away from home. The recipient connects FTW from its
own assistant, selects **I have an invitation code** on the consent page and
pastes the code there. Core must be online. The normal signed-roster,
PKCE and refresh-token flow then applies.

The owner views publish that proof through TLS in JSON to
`POST https://cloud.ftw.energy/oauth/invitations`, without cookies. Cloud
checks its home, box and verifier against the current signed roster before
returning a unique six-character code such as `K7M-4PX`. The code works for
at most ten minutes, within Core's claim deadline. The granted access can
last longer. The normal owner views use a ten-minute Core claim window.

Cloud holds the code-to-proof mapping only in memory. Expiry stops use at
once; its normal minute sweep drops expired mappings. SQLite keeps only a
keyed digest reservation and expiry, with its MAC key outside that database,
so a restart cannot give an unexpired code to another home. After a Cloud
restart the owner can check status and republish the same strong proof to
get a working code, without another Core grant. A retry before restart gets
the same code. No secret enters a URL or log. This short-code service
currently runs on the single OAuth instance; multiple instances require
routing to the instance holding that mapping.

## The grant offer

The MCP consent page shows a short code, a QR code and a universal link. The
link and the QR code carry one signed offer; the code finds the same offer at
FTW Cloud's code lookup. The app shows the offer to the owner before Face ID.

**The signed offer** is CBOR `{o, kid, sig}`, base64url without padding in
the link's fragment, and what FTW Cloud's code lookup returns for a code:

- `o` is the deterministic CBOR of the offer below;
- `sig` is ECDSA P-256 with SHA-256 over the deterministic CBOR of
  `["ftw.offer.v1", o]`, `r ‖ s`, by FTW Cloud's offer key named `kid`.

The app and the box pin a list of offer keys, as they pin the service key,
and refuse an offer no pinned key signed. A build that pins none opens no
offer. A key's `kid` is the first 16 characters of base64url, without
padding, of SHA-256 over its SEC1 point (`protocol.OfferKid`); the box drops a
pinned entry whose `kid` does not match its key. FTW Cloud's `keys` part
makes the key on the host and keeps it sealed; FTW Cloud publishes `kid` and
the public key at `https://cloud.ftw.energy/.well-known/ftw-offer-key`, and a
release pins them. A new key sits beside the old one until every offer the
old one signed has expired.

| Field | Type | Meaning |
|---|---|---|
| `v` | uint | 1 |
| `home` | text | Empty: the owner picks the home |
| `key` | bstr, 32 | The agent's X25519 key, made by FTW Cloud's `keys` part |
| `role` | text | `agent`, `support` or `release` |
| `client` | text | The name the client gives itself |
| `clientId` | text | The client id |
| `clientHost` | text | The host of the client's metadata document or redirect URI |
| `verified` | bool | FTW Cloud knows the client from its own list |
| `scopes`, `ops` | [text] | What the client asks for |
| `caps` | `{holdS, maxW, perMin}` | Proposed caps |
| `ttlS` | uint | The longest the owner may grant. FTW Cloud asks for an `agent`'s longest, a year |
| `ticket` | text | The ticket id, for `support`; empty otherwise |
| `code` | text | The code on the consent page |
| `exp` | int | Wall-clock ms when the offer stops being good, at most 10 minutes after FTW Cloud made it |

- **The app** shows `client`, `clientHost` and whether it is `verified`. A
  universal link, tapped or opened by the phone's camera or the app's own
  scanner from the QR code, goes straight to that review, and the review
  then also shows the code for an owner who wants to compare it with the
  page. Nothing asks them to, so a link someone else sent also reaches the
  review. What stops it there: the pinned-key check before anything shows,
  for an unverified client the owner's own "I started this" step, Face ID,
  and the offer's 10 minutes. The owner types the code only when they have
  neither the QR code nor the link. The app picks the home the owner
  chooses, asks when there are several, and refuses a home it does not
  have.
- **Support** offers count only from `clientId`
  `https://cloud.ftw.energy/clients/sourceful-support`, the support tooling's
  client at FTW Cloud. The app and the box refuse a support offer from any
  other client.
- **The box** takes an offer whose `home` is empty or its own.
- The owner may grant less than the offer asks, never more. The box page
  refuses a scope, an op, a cap or a `ttlS` beyond the offer's. FTW Cloud
  learns the outcome from the roster: the grant is whatever the roster lists
  for that key.

## Push kinds

- `agent.added` goes to every owner when an agent row is added, as
  `cloud.granted` does: access from outside is news every owner should hear.
- `agent.request` goes to every owner when an agent asks for a durable change.
- `agent.expiring` goes to every owner a day before a grant runs out.
- `approval.key.set` goes to every owner when a phone registers or replaces
  its approval key.
- `owner.added` goes to every owner when a phone pairs as an owner.

These are security notices. They go whatever the owner's notification
settings say, through the box's own transports, and to FTW Cloud as a
`notice {kind}` whenever the link is up, copy or not (docs/protocol-cloud.md).
A notice carries nothing but the kind, except that `agent.added` names the
client while the readable copy is on and carries events. FTW Cloud sends
each notice, as the box signed it, in an alert to the home's owner devices
only, never to a viewer or a watch. These kinds never go to FTW Cloud as an
`event`, so an owner hears of each change once through it.

FTW Cloud knows each agent's client name whatever the copy: the roster lists
it in the agent row (`principal.client`), and the name came from its own
consent page. The copy decides only whether the name also goes to Apple in a
notice.

## The audit log

The box writes a row for every call an agent makes and for every change to
an agent grant or an approval key, whoever makes it. An agent's reads and
refusals fold by the minute: one row per agent, message, `by` and result,
with a count, so junk cannot crowd out the rows that matter. An agent's
rows come from a budget of their own and never push out an owner's or the
box's. Rows live in the box's state store, at most 10,000 and
at most 90 days. An owner reads them with `audit.get` or on the box page.
FTW Cloud keeps its own log of what it forwarded; neither replaces the other.

## Numbers in one place

| What | Value |
|---|---|
| Longest grant | `agent` one year, `support` 14 days, `release` 90 days |
| Agent rows per box | 16 |
| `holdS` | 60 to 1,800 s |
| `maxW` | 1 to 100,000 W, either direction |
| `perMin` | 1 to 60, default 6 |
| Reads per agent | 60 a minute |
| Pending requests | 4 per agent, 16 per box |
| Request id | 16 random bytes |
| Request lifetime | 12 hours; kept 24 hours after it ends |
| `agent.request` args, `why` | 1 KiB, 280 bytes |
| Approval `atMs` window | 10 minutes either way |
| Signature replay memory | 20 minutes, since the box started |
| Signed offer life | 10 minutes |
| `logs.get` | 200 lines, one 16 KiB frame |
| `events.get` | 50 events |
| `audit.get` | 200 rows a page; 10,000 rows and 90 days kept |
| `by` on a support call | 64 bytes |
| `agent.expiring` | A day before the expiry |
