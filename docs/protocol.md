# Wire protocol

The contract between an FTW box and its clients: the web client and the
native apps. It runs inside a Noise IK session, which runs inside whichever
carrier is active.

Names shared with the clients — scopes, capabilities, error codes, field ids —
come from [`contract/registry.yaml`](../contract/registry.yaml). Core owns that
file and generates Go constants from it. The official clients keep
byte-identical snapshots and compare them with Core; see
[`contract/README.md`](../contract/README.md). Never hand-write one of those
names anywhere else.

Core keeps this document. It came from the web client on 10 November 2026,
when that repository became private. "The app" means any client.

## Product direction for agent clients

[The shared vision](../VISION.md) calls for structured
analysis, durable schedule and goal changes, and proposed-plan submission by
authorized agents, locally and through cloud MCP. These are targets. They do
not imply that the operations below already implement them. Extend the registry
and Core/client implementations together with authorization, expiry, replay,
revocation and result tests. Core still owns admission and physical dispatch.
Temporary external control must expire; a saved household goal must survive
client disconnect. Relay and escrow remain unable to read session contents.

## Frames

Each Noise transport message carries exactly one frame.

```
offset  field    type     note
0       ver      u8       frame layout version
1       lane     u8       0 = telemetry/control, 1 = bulk
2       flags    u8       0x02 TRUNC
3       rsvd     u8       0
4       len      u16 BE   payload bytes
6       payload  u8[len]  CBOR
6+len   pad      u8[]     zeros to the bucket size
```

The receiver ignores everything past `len`. Padding is never validated — AEAD
already covers it.

**Lane 0** carries `tick`, `delta`, `cmd`, `cmd.ack`, `cmd.result` and
`event`. It is exactly one 512 B bucket for the life of the session and never
fragments. Cadence changes only after an explicit visibility `sub`: 1 Hz while
the document is visible and 0.2 Hz while it is hidden. It never changes with
the telemetry it carries. If the box has nothing to say it sends `tick` anyway.

Through FTW Cloud's tunnel the box sends a padded frame when a reading lands,
plus a beat while quiet. The relay's fixed cadence applies only to the relay.
The tunnel's envelope, limits and session ids follow Core's
[Cloud protocol](protocol-cloud.md#tunnel).
The browser uses the same Noise and CBOR as iPhone over the service's existing
WebSocket endpoint; its custom QUIC transport is not a WebTransport endpoint.

This constancy is a privacy control, not a performance choice. A delta that
does not fit sends the highest-priority fields and sets `TRUNC`; the rest
follows on the next tick or as a fresh snapshot on the bulk lane. It never
grows the bucket and never fragments, because both would make frame size a
function of what happened in the house.

**Lane 1** carries snapshots and history chunks. Buckets step 1024 / 4096 /
16384. Bulk leaks that a transfer is happening, which is unavoidable and
harmless — what must not leak is the 1 Hz stream, and that lives on lane 0.

## Envelope

```ts
type Envelope = {
  t: string       // message type, stable string
  id?: number     // request id, u32; replies echo it
  b?: unknown     // body
}
```

CBOR, deterministic encoding, text keys.

**Unknown map keys and unknown message types are ignored silently, in both
directions.** This is the rule that lets a newer app talk to an older box and
the reverse. A service worker can pin a bundle for a long time; a hard version
wall would turn that into a white screen. Both sides have a test that feeds in
junk keys.

## Messages

Twenty-three types in v1.

| Type | Direction | Purpose |
|---|---|---|
| `hello` / `hello_ok` | C→B / B→C | Version range, capabilities, clock, boot state, optional first subscription |
| `sub` | C→B | Start the telemetry stream |
| `snap` | B→C | Full site snapshot with the field dictionary |
| `delta` | B→C | Changed fields by id |
| `tick` | B→C | Nothing changed; keeps the cadence constant |
| `hist.query` / `hist.chunk` / `hist.end` | | Time window and resolution |
| `plan.get` / `plan` | | What the box intends to do, slot by slot |
| `price.get` / `price` | | What electricity costs across a window |
| `cmd` / `cmd.ack` / `cmd.result` | | Intent, receipt, and observed outcome |
| `event` | B→C | Something worth surfacing happened |
| `error` | B→C | Stable code with machine-readable args |
| `session.terminate` | B→C | Access revoked or session ended |
| `api.req` / `api.head` / `api.chunk` / `api.end` | | The box's own HTTP API, over this session |

### Starting the stream

A current client includes its first `sub` body in `hello`. A current box
accepts it, sets `hello_ok.subscribed: true`, and follows with the snapshot
without a second client exchange.

This stays compatible in both directions. An older box ignores the unknown
`hello.sub` key and omits `subscribed`, so the client sends the separate `sub`
it always sent. An older client omits `hello.sub`, so a current box waits for
the separate `sub`. A later `sub` changes the same session's cadence, including
the move between 1 Hz while visible and 0.2 Hz while hidden.

## Versioning

The client sends `proto: {min, max}`; the box picks `min(box.max, client.max)`.
If the client is too old the box does **not** error — it replies
`hello_ok{proto: 0, mode: "floor", hint: "app_update"}` and the app degrades
to the frozen subset instead of dying.

Capabilities are a set of names, not version comparisons. The UI gates on
presence; absent means hide, never crash.

Field ids 1–9 are frozen permanently as of v1: mode, grid power, PV power,
battery power, battery state of charge, load power, and their source ids. Any
app, however old, can draw the core view. Freezing them now is free; doing it
later is not.

Ids after the frozen set live in the registry's `extra_fields`: depth for a
node's own view, such as the grid's phase currents and the main fuse. They
travel in the same `delta` and follow the same rules, but an app draws the
core view without them and a box may stop sending one. A delta that does not
fit drops the highest id first, so they give way before the core view.

## Freshness

Every reading points at a **source**, not at its own timestamp:

```ts
sources: {
  [srcId]: {
    kind: string
    name: string
    lastOkMs: number
    staleAfterMs: number
    state: 'live' | 'lagging' | 'stale' | 'down' | 'never'
  }
}
```

The app materialises per-field freshness by looking up `fid → src → sources`.
That keeps deltas at tens of bytes while still answering the question users
actually have: the box is fine, but the inverter went quiet 40 seconds ago.

Ages are computed against the box's **uptime**, not its wall clock. A Pi has
no RTC: after a power cut its clock reads 1970 until NTP answers, which would
make every age wrong by decades. `hello_ok` carries
`{source, syncedAtMs, uptimeMs}` and all ages are uptime deltas.

`dispatchBlockedBy: [srcId]` connects directly to the box's safety invariant
that stale meter data stops dispatch — so the app can say why nothing is
happening instead of looking broken.

## History

Two resolutions in v1: `5m` from the energy ledger (30 days) and `1h` from the
hourly rollup (2 years, the box's only versioned storage contract).

**Downsampling always happens on the box.** One reading per second for a year
is 31 million points; that number never crosses the wire. If a window exceeds
`maxPoints` the box clamps to a coarser resolution and reports `resActual`. It
never fails with "too much data".

Transfer is tile-based — a 5m tile covers 12 hours, a 1h tile covers 7 days —
with an etag per tile. The client sends `have: [{tileId, etag}]` and receives
only the difference. Closed tiles are immutable; the trailing tile is marked
partial and never cached.

Chunks are column-packed int32 little-endian inside CBOR byte strings, so the
client gets an `Int32Array` without parsing. `INT32_MIN` marks a missing
sample — distinct from zero, which is a real reading.

## Prices

Gated on the `price.spot` capability: absent means the app draws no price view
rather than an empty one.

`price.get {fromMs, toMs}` is answered with slots carrying `spotMinor` and
`totalMinor` — integer minor units per kWh, öre or cents. **Money never
crosses as a float.** The box rounds once and nothing rounds again, because a
second rounding is how two screens start disagreeing about what 18.7 öre is.

`totalMinor` is what the household actually pays, tariff and tax included, and
the box computes it because the box holds that configuration. An app that
multiplied spot by its own guess would put a different number under the same
hour than the box's own dashboard does.

Times are wall clock, unlike every age in this protocol: prices are about
hours a person plans around rather than about the box.

`stale` means the answer does not cover the window asked for. That is three
shapes, not one: it begins after the start, it has a hole in the middle, or it
stops short of the end. The box judges all three against the window it was
asked for and sets the one flag for any of them; a slot that starts at or
before `fromMs` covers the head, because that is the slot running at `fromMs`
and it is the price right now.

Tomorrow's rates publish in the afternoon, so a window asked for at breakfast
genuinely ends early, and the box also drops the far end rather than failing an
encode that will not fit a bulk bucket. One failed midday fetch is the second
shape — a store holding 00:00–06:00 and 12:00–24:00. A box that first heard
from the market at breakfast is the third: 06:00–24:00 of a day the app asked
for from midnight, every slot joining the last. A tail-only reading calls the
last two a covered day.

The flag cannot say which shape it is, and the app does not need it to: the app
holds the slots, so it reads the missing hours off them. They are different
sentences to the reader — a day missing its own morning is not a day waiting
for tomorrow — and drawing either as a market that simply went quiet is "never
fake live" with prices in it.

## The box's own API

Gated on `api.passthrough`. The app can ask its box six things by name; the
box's own web page can ask it 132. Every new view therefore meant a box
release, and this ends that: `api.req` carries a method, a path under `/api/`,
a parsed query and an opaque body, and the box runs its own handler in process
and streams the answer back as `api.head`, then `api.chunk`s, then `api.end`.

**This is a security improvement, not a relaxation.** Those 132 routes are
already served on the home LAN with no authentication at all. Reaching them
through a Noise session pinned to an enrolled device, with a role behind it,
is strictly stronger than what households run today.

Bulk lane only. Every field varies in length with what was asked and answered,
and lane 0's fixed size is a privacy control rather than a budget.

**`api.req` has no headers field, deliberately.** The caller's identity rides
on the request context inside the box process, put there by the session that
already authenticated it. With no headers there is no path by which a byte the
client sends becomes a claim about who is asking. Adding the field later opens
exactly that hole.

The query is a parsed map, never a raw string. The box decides what a request
may do from its path, and a path decided over a string that can still carry a
`?` is a parser bug that becomes a privilege bug.

### Telling the two kinds of failure apart

One rule, and it needs no inspection of any body:

- **`api.head` arrived** — the box's HTTP layer answered, and `status` is the
  answer. A 403 or a 500 from a handler is a status, never an `E_` code.
- **`error` arrived on that id** — the passthrough refused, and no handler ran.

They are different message types on the same request id, so the app branches on
type and never on content. The caller decides what a 404 means.

### Four tiers, two doors

**The tier is a fact about the route, declared beside its handler, and never
about the method.** Ask what the code on the other side does:

1. **read** — answers a question, changes nothing, and hands back nothing that
   could be replayed as authority. A shared viewer may ask for it.
2. **configure** — changes a setting. A late execution is the same
   instruction, only later. Owner. Most of these also need a step-up;
   a few household settings skip the ceremony.
3. **actuate** — moves energy, or takes control of what is moving it. A late
   execution here is a *different* instruction.
4. **local** — served only on the box's own page, at home. Either the answer
   carries a credential or a whole file the box cannot vouch for, or doing it
   needs somebody standing at the box. It is not a permission an owner is
   missing: no role and no ceremony changes the answer, which is why it is a
   tier and not a role check.

The method decided this once, and it was wrong twice in the same review.
`GET /api/caldav/credentials` hands out a password that is a write channel into
dispatch; `POST /api/self_tune/start` drives every battery in the house through
±3000 W for minutes. Both read as ordinary from their verb alone. Neither is.
So the verb is not asked.

**A path no route claims is closed.** The box's router matches method and path
together, and anything it does not claim — an unpriced path, or a method a
route was never registered for — falls through to the file server that serves
the box's own page. The session does not carry that page, so the answer is
`E_UNKNOWN_OP`, never a 404. A 404 through the passthrough always means a
handler ran and did not find the thing asked for.

**What that costs, said plainly.** A new *read view* is free for a route the
box already prices as read, and of its 132 routes 55 are — every reading this
app draws a house from among them. It is not free for a route the box has
never priced: that one defaults closed, and serving it means a line in the
box's route table and a box release. This is the right direction to be wrong
in — a route nobody has priced is refused rather than served — but it is a
cost, and it is the reason to read the box's table before planning a view
around a path. The other 77 are 40 configure, 22 actuate and 15 local.

**The passthrough serves read and configure only.** An actuating route reached
through it is refused with `E_USE_CMD`. The reason is the invariant, not taste:
a command carries an expiry and the box revalidates against fresh state before
acting, and an HTTP request has no expiry. A request with no expiry must never
move energy. The rule a builder can apply without asking: *if a late execution
is a different instruction than the one given, it is a `cmd`; if it is merely a
late setting, it is a passthrough.*

The refusal carries an `op` argument **only when a command for that route
exists**. Today four do: `POST /api/mode` names `site.mode.set`,
`POST /api/planner/prefs` names `planner.prefs.set`,
`POST /api/loadpoints/{id}/soc` names `loadpoint.soc.set`, and
`POST /api/loadpoints/{id}/target` names `loadpoint.surplus_only.set` — the
one field of that route's body the session can set; the target and its
deadline still have no command. Every other actuating route names nothing, and
the honest reading of an absent `op` is that the box has no command for it
yet, so that control is not available over the session at all. An app that
assumed `op` was always there would draw a button leading nowhere.

A route whose body replaces a whole document rather than editing part of one —
`POST /api/config` is the one — is refused with `E_WHOLE_DOCUMENT` even for an
owner who has stepped up. A body built from an older idea of that document
silently drops every field the sender never knew about. On the LAN the browser
had just loaded it from this box; a phone on a relay, possibly a year behind,
has no such guarantee.

A `local` route is refused with `E_LOCAL_ONLY`, before the role and before the
ceremony, because neither of them changes it.

`configure` needs role `owner`. Most configure routes also need a `stepUp`
flag. A few household settings — the charging schedule today — skip the
ceremony: the session already proved who is asking, and a second Face ID is
more friction than the table-phone risk is worth. The box names those routes
beside the handler. The app still carries no list of tiers — it asks without
the flag, and a route that still needs a ceremony answers `E_NEEDS_STEP_UP`.

For routes that still need the flag, the box remembers a genuine ceremony for
a few minutes on that session, so a settings screen that writes several things
in a row prompts once. The app does not earn that by sending `stepUp` before a
ceremony; that would be claiming something that had not happened.

The order the box refuses in is fixed, and the app depends on it: whole
document, then role, then ceremony. A viewer who posts a whole document hears
`E_WHOLE_DOCUMENT`, not `E_SCOPE_DENIED` — the route is refused for everybody,
so naming the role would suggest a stronger caller could get through.

**What `stepUp` honestly buys.** The box cannot verify a passkey ceremony
happened: it has no relationship with the authenticator and is deliberately
never a WebAuthn relying party, because that needs an origin and the box is
never an origin. What the flag stops is a phone left unlocked on a table being
picked up and used to reconfigure a house. It stops nothing against a modified
client, which could already send a command today.

The box's contribution today is one line in its own log: the subject, the role,
the method, the path and the flag as it arrived. Nothing counts these, nothing
rate-limits them per enrolment, and none of them reaches the household's event
log. This paragraph claimed all three for a release, which is the worse half of
a security claim — the flag was being described as backed by machinery that was
never written. What the box does is record.

### What will not cross

Only `application/json`, `application/*+json` and `text/*`. Anything else meets
`E_UNSUPPORTED_MEDIA` at the status line, before a byte streams — refused by
class rather than by a list of paths, so a route added next year that streams
an archive meets it without anyone remembering. `GET /api/research/load/dump`
is the one such route the box prices as a read today, and it is where this rule
is tested from. `/api/support/dump` never reaches it: that one is `local`, and
a tier is decided before a handler runs.

Chunks are `API_CHUNK_BYTES` = 12 KiB, chosen so the envelope always lands in
the 16 KiB bulk bucket. Text answers that are merely large stream until the
box's ceiling and then stop with `api.end{truncated: true}`, which the app
treats as a failure — half a document is wrong in a way no caller above can
see. A chunk that arrives out of order fails the request for the same reason.

`hello_ok` carries `role` **and `scopes`**, and the redundancy is deliberate.
The app uses both only to decide what to draw: hiding a button is presentation,
and if the app is wrong and shows one, the box refuses what is behind it.

They are not two spellings of one fact. `role` is what a sentence can name —
"you have view-only access, so this is the owner's to change" — and `scopes` is
that role expanded, by the box, and it is what a control is checked against. An
app that reads only the role has to expand it through a role table of its own,
which is the app deciding what its own grant contains while the box decides
something else; this one did, and a box whose table had moved on was overruled
by an older copy of it.

A box that sends neither is one from before roles existed, and such a box lets
every paired phone do everything — so absent means owner, and the role's own
expansion stands in, or a household would watch its own controls vanish after
an update.

## Commands

Intent and execution are separate, and the gap between them is where safety
lives.

```ts
{
  t: 'cmd',
  b: {
    cmdId: string          // UUIDv7, idempotency key, kept 24 h on the box
    op: string
    args: Record<string, unknown>
    notValidAfterMs: number // mandatory
    expect: { rev: number, guards: Guard[] }
  }
}
```

`notValidAfterMs` is mandatory because a command queued offline must never
execute blindly later. "Charge the battery at 10 kW" arriving three hours late
is rejected with `E_CMD_EXPIRED` — and even inside its window, `guards` are
re-evaluated against fresh state at the dispatch boundary, so it is rejected
with `E_PRECONDITION` if the state of charge has since passed the limit.

`expect.rev` is one global `controlRev` per site, monotonic, bumped by every
mutation of controllable state.

Three separate clocks, three different events — not three versions of one:

| Elapsed | Meaning |
|---|---|
| 1 200 ms | Show "sending…" |
| 5 000 ms without `cmd.ack` | Transport failure: "didn't reach the box" |
| 15 000 ms without `cmd.result` | `unconfirmed` — the box accepted it, the hardware has not confirmed |

`cmd.ack` means the dispatcher accepted the intent and returns a lease with an
absolute expiry. `cmd.result` arrives separately, when the driver has **read
the value back**, carrying `observed: {value, src, uptimeMs}`. The echo of the
requested value is never sent as confirmation — that is the difference between
knowing a command was heard and knowing it happened.

## Errors

Stable `E_`-prefixed codes with `retryable`, `retryAfterMs` and machine-
readable `args`. The box sends codes; the client owns all prose, in every
language. The single exception is push payloads, which must carry rendered
text — those templates are generated from Core's
[push catalogue](../contract/push-catalogue.yaml) and shipped with the
firmware.
