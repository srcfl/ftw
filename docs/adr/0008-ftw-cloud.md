# ADR 0008: FTW Cloud keeps a readable copy the owner turns on

- Status: proposed. Nothing described here has shipped.
- Date: 2026-10-12
- Decided by: Fredrik on 12 October 2026, decisions 1–5 and 7. Decision 6 is
  the recommendation, taken as the working choice until he answers it.
- Revisits: [ADR 0006](0006-app-uplink.md), which made the relay the only
  remote path. That still holds for homes that do not turn FTW Cloud on.
- Turns away from: [#1323](https://github.com/srcfl/ftw/issues/1323), which
  asks that no feature need a Sourceful service.
- Wire format: the normative [FTW Cloud protocol](../protocol-cloud.md).

## Context

The iPhone app needs what the relay path does not give: a first screen from a
cold start without a Noise handshake through the relay; alerts, widgets and
Live Activities that update while the app is closed; and loadpoint and
control state beyond the ten lane 0 fields. [VISION.md](../../VISION.md) asks
us to verify the relay's fit first. It does not fit: lane 0 is a fixed 512 B
frame, everything else is pulled one request at a time, the relay allows four
streams per box, and a closed app is reachable only through APNs, so some
service must hold what the push carries. A cloud paired with every box as a
Noise device would hold a key to every box.

The earlier iOS plan stayed blind, with a sealed push gateway, more lane 0
fields and a sealed widget mailbox. Fredrik also wants statistics across
homes, which blind storage cannot give.

## Decision

**FTW Cloud is a Go service, `ftwcloudd`. For each home whose owner turns it
on, it keeps a readable copy in memory of live flows, loadpoint and control
state, the plan, prices, health, events and 48 hours of 5-minute tiles. It
serves that copy to the home's phones and widgets, and it sends APNs. The box
opens the connection, picks and signs every message, and can revoke at any
time.**

Fredrik decided the following on 12 October 2026.

1. **Hosting and repositories.** AWS eu-north-1 (Stockholm), in an AWS
   account of its own. `ftwcloudd` lives in a public repository,
   `srcfl/ftw-cloud`, under AGPL-3.0-only. The wire format lives in
   `go/protocol` here, under Apache-2.0
   ([#1588](https://github.com/srcfl/ftw/pull/1588), where a lawyer still
   confirms the relicensing). *Why:* a service that reads household data
   should show its code, and a separate module keeps control code out of its
   build.
2. **Readable by Sourceful, not end-to-end encrypted,** so it can compute
   statistics and
   aggregates. In return the service must be very secure. It reads and never
   controls, and never chooses which actions a user is offered. The box signs
   every message. The relay and escrow stay blind and do not change.
3. **The iPhone app is the way into FTW Cloud,** which may become a paid
   service. A household that does not want FTW Cloud runs the box itself,
   updates included, with the box's own page, the webapp over the blind relay
   and ntfy. This turns away from #1323; see below.
4. **Consent at first use,** in plain words. FTW Cloud is part of the app's
   service, on contract (GDPR art. 6(1)(b)). Statistics have their own switch
   in the same flow, off by default, on consent (art. 6(1)(a)) that is free
   in the sense of art. 7(4). A lawyer must confirm both. Because consent comes at first use, this ADR
   sets the statistics rules now.
5. **No padding on cloud traffic.** It uses lane 1 frames without the bulk
   buckets, because lane 0 is always padded. The service reads the content
   anyway, so padding would hide only size and timing from the network, at
   about three times the data.
6. **Apple sees event codes.** *Assumed; Fredrik has not answered.* Alerts
   carry the box-signed code and arguments in the APNs payload, because a
   fetch would need a token and a round trip before the alert shows.
7. **Lane 0 and the relay do not change.**

## What FTW Cloud is not

- **Not the record.** The box is. Older history reaches phones over Noise.
- **Not a control path.** Commands go from the app to the box over Noise. The
  service holds no credential the box accepts for a change of state.
- **Not a source of actions.** Buttons, Siri and App Intents come from app
  code and from event kinds the phone has verified.
- **Not a clock, config source or update channel** for the box.
- **Not an account system.** A phone proves itself with the Noise key it
  paired with the box.
- **Not needed to run the house.**

## How it works

```
box ──wss, TLS 1.3, signed frames──▶ ftwcloudd ◀──wss, Noise IK──── iPhone app
 │                                     │ ◀──https GET, token──────── widgets
 │                                     └──APNs──▶ Apple ──▶ locked phones
 └──Noise via the relay: commands, long history──▶ iPhone app, webapp
```

- The box dials a fixed address over TLS 1.3. Box and service each check
  that the other signed this TLS session's exporter value before any data
  moves, so a proxy that ends TLS in the middle reads no household data. No
  such proxy may sit in front of `ftwcloudd`.
- Each message belongs to one class and each class to one scope. The box
  drops driver names and device capabilities and uses opaque ids.
  The service forwards the box's bytes unchanged, by scope.
- The app runs Noise IK to a service key built into the app and gets the
  current state in the handshake reply. Widgets read with a short-lived token.
- Alerts carry the box-signed event. The phone checks it, renders its own
  text and picks buttons from the verified kind.
- The home box sends about 5 MB a day.
- If the service is down, the app falls back to the relay and iOS alerts
  stop; web push and ntfy go on.

The [protocol](../protocol-cloud.md) has the classes, field tables, cadence,
grant routes, limits and timers.

## Control boundary

**FTW Cloud reads and never controls. It never chooses the actions a user is
offered.** Code enforces this:

- The service can import only `go/protocol` from this repository, and #1588
  checks that `go/protocol` imports nothing from Core, so the service cannot
  link control code. In Core, an import test keeps control, API and loadpoint
  writers out of the uplink package.
- The service may send the box five message types. Anything else closes the
  socket, under a fuzz test. The box issues a grant only when an owner asks.
- A false message meant to make a user tap the wrong action meets three
  limits: buttons come from verified kinds, state is signed, and a `cmd` keeps
  the guards the box checks against fresh state.

Control still needs a relay session, so the app shows the data path and the
control path as two states.

## Access

- **The grant lives on the box.** A missing grant reads as **off**, the
  opposite of `app_link`. An owner grants over an app session; any door may
  revoke. Turning it on or off notifies all owners. A viewer's app shows that
  the home uses FTW Cloud, and viewers get no alerts.
- **Scopes.** Live, history, plan, health and the new `ftw.events.read`.
  Inventory stays out.
- **Roster.** The box signs the list of paired devices, their keys and the
  scopes their roles allow. The service learns devices only from it.
- **Widgets** get a 7-day read token for live data and prices. It is not a
  device key and cannot command.
- **Revocation.** A device revoke reaches the service within a second while
  the box is connected. A grant
  revoke deletes the home there, and the box keeps its key until it holds a
  signed purge receipt.

## Freshness

Data read through FTW Cloud carries the new carrier state `cloud` and never
looks fresher than the box would grade it. The phone trusts a send time only
through an anchor it can check, and shows unanchored data as "cloud, age
unverified", never live. The service cannot sign, so it cannot clear
staleness. The
[protocol](../protocol-cloud.md#freshness) states the rules and sixteen test
cases.

## Statistics

**Product amendment, 17 October 2026.** Everyone who chooses FTW Cloud
contributes to project statistics. The updated Cloud terms name this scope;
there is no separate statistics opt-out within Cloud. Existing users must
accept the new terms before the approved policy can admit their home.
Owners who decline use the box locally without Cloud.

Local-only users get a separate question, with Yes as the suggested action.
Nothing starts before an active answer. No leaves local control working.
(Replaced on 24 October 2026: a box without Cloud shares the two anonymous
reports until its owner turns them off; see "Anonymous daily reports and one
statistics answer" below.)
This statistics-only link publishes no phone keys, pairing codes, Noise key,
read grant, live state or remote access. It reuses the signed upload path.
A policy or purpose change requires a new version and a fresh answer.
This product decision does not establish a lawful basis; C8 stays off until
that question and the DPIA have been reviewed and signed.

- **Purpose.** To learn how homes with FTW use and shift energy, to improve
  FTW's planning and defaults, and to publish figures about homes as a group.
  Not for advertising, decisions about one home, or handing rows to anyone.
- **Fields.** Recorded closed-hour grid import/export, solar generation,
  battery and vehicle charge/discharge; configured battery capacity,
  battery/charger power limits and solar rating. Rounded to 10 Wh or W.
  Missing values stay absent. No names, addresses, location, device ids,
  prices, free text, configuration documents or logs.
- **`statsId`.** 128 random bits drawn by the box at consent, derived from
  nothing else. A new consent draws a new one.
- **Retention.** Rows go 48 hours after the hour they describe. Leaving Cloud, stopping local sharing or
  a purge deletes unpublished rows when Cloud receives the signed request.
  An offline box sends it on reconnection. Published aggregates stay, and the
  consent screen says so.
- **Cell rules.** At least 20 homes per figure, no per-home output, and no
  home above 30 % of a cell's sum. Code applies the rules. No staff route
  reads raw rows; full host administrators can read the database.
- **Partners.** Homes under a partner's control stay out unless that
  partner's contract allows them in. Nova keeps B2B partner telemetry.
- **Law.** The service links `statsId` to the home for deletion, so these
  rows are personal data. The required Cloud model needs its own documented
  lawful-basis assessment. Writing it in terms does not establish contractual
  necessity or make conditional consent freely given. Local sharing proposes
  informed, separate consent. A legal review must settle both paths, other
  household members and terminal-access rules before activation. The review
  draft lives in `ftw-cloud/privacy/project-statistics-dpia.md`.

## Security and retention

The relay protects data by being unable to read it. The service can read it,
so its protection comes from controls: off by default; a preview of the exact
bytes; one code path to a home's data, with canary homes that alert on a
leak; no staff read path, under a test; two-person review and the running
digest published at `/source`; host access only through logged SSM; outbound
traffic only to APNs and AWS; and an erase test. The legal work is a privacy
notice in Swedish and English, terms, a DPIA, records of processing, a DPA
with AWS, a breach process, and export and erase.

**The honest limit:** someone with root on the service host can read memory.
The privacy notice says so.

| Store | Kept |
|---|---|
| Live state | Memory only; readings dropped an hour after the box goes quiet |
| History tiles | Memory only; each 12-hour tile of 5-minute history until 48 hours after it ends or the roster runs out, box online or not |
| Registry | Until purge, or 90 days without a roster |
| APNs registrations | Until purge, or 30 days without the box |
| Events | A one-hour ring in memory |
| Access logs | 14 days, with a keyed hash instead of the address |
| SSM sessions and deploy log | 24 months |
| Backups | None; boxes refill the service |
| Statistics | 48 hours |

## Operations and cost

One `t4g.small` in eu-north-1, apart from the relay and escrow, with SQLite on
an encrypted volume and no standby; a tested drill rebuilds it. The budget is
at most 3,000 lines of Go in the service, about 1,700 in Core and 900 in
Swift. Estimated cost per month: $25–35 for 1,000 homes, $60–150 for 10,000,
and $600–1,600 for 100,000. A wire change needs paired pull requests here and
in `srcfl/ftw-cloud`.

## Local-only operation and #1323

[#1323](https://github.com/srcfl/ftw/issues/1323) asks for a tested rule that
no feature a household needs may depend on a Sourceful service. **This ADR
turns away from that rule for the iPhone app, on purpose.**

- The box stays complete on its own. Control, planning, safety, history, the
  box's own page and ntfy alerts need no Sourceful service, and that should
  become a tested rule.
- The iPhone app does not. Its fast start, alerts, widgets and Live
  Activities need FTW Cloud, which may cost money.
- A household that wants no Sourceful service runs the box itself, updates
  included, with the box's own page and ntfy. One that accepts the blind relay
  adds the webapp and web push.
- The blind push gateway in the earlier iOS plan is shelved. An iOS alert
  needs FTW Cloud.

## What changes from ADR 0006

ADR 0006 said one remote path means one thing to threat-model and one place
freshness can go wrong. For homes that turn FTW Cloud on there are two of
each; the carrier state `cloud` and box signatures answer the second. Pairing
stays one story, because the service learns devices only from the box.

ADR 0006 also removed Home Link partly because the relay learned the
household. FTW Cloud brings back a stable household id at a Sourceful
service, by the owner's choice, and whoever runs both relay and service can
link a relay handle to a home by IP and timing. The privacy notice says so.
Homes without FTW Cloud keep ADR 0006 as it is.

## Milestones

The core comes first: the uplink, the cache and the app's first screen. Each
later step stands on it and ships on its own.

| Step | Content | Proof |
|---|---|---|
| C1 | This ADR, the protocol, `go/protocol` ([#1588](https://github.com/srcfl/ftw/pull/1588)), the registry names | Drift CI green; ADR accepted |
| **C2 Uplink** | Grant, `cloud.key`, preview; signed `live`, `lp`, `prices`, `hb`; roster; import ban | 24 hours of the home box against staging; preview equals received bytes; a dead service leaves control unchanged |
| **C3 Cache** | `ftwcloudd`: uplink, memory cache, roster, app session, deployed in eu-north-1 | A revoke stops the stream within a second; a rebuilt host refills |
| **C4 First screen** | The app's socket to the service, carrier `cloud`, freshness, relay fallback, the start flow | p95 cold start on mobile data, against the relay; a replay shows stale |
| C5 Alerts | Signed events, notification extension, `box.unreachable` | A forged alert shows a generic line |
| C6 Widgets | Extension token, tiles, widget push | A force-quit phone shows tomorrow's prices by 13:30 |
| C7 Live Activity | Signed state in the extension view | A forged state shows "Open FTW" |
| C8 Statistics | Ingest, backfill, cell rules | DPIA signed; withdrawal purges within 24 hours |

The original separate Cloud statistics switch is replaced by the 17 October
product amendment above. The Cloud terms and the local question ship with the
start flow. The box records consent
and `statsId` from then on. The October project policy below needs new
consent and sends only later hours; it does not backfill the earlier gap. The lawyer also confirms that order: if the DPIA in C8 changes the
rules, the app asks again. A public launch needs C2–C4 and the legal work.

## Alternatives considered

- **Stay blind:** no statistics, and every new surface needs a new sealed
  path.
- **The service in this repository:** it would link control code into an
  18 MB binary, beside the box's release keys.
- **A proprietary service:** trust would rest on promises.
- **Cloudflare Workers:** APNs only through undocumented behaviour, and EU-only
  TLS needs an Enterprise plan.
- **API Gateway, IoT Core or Supabase Realtime:** per-message billing, about
  $57,000 to $143,000 a month at 100,000 homes.

## Risks

| Risk | Cheapest way to reduce it |
|---|---|
| One bug exposes every home | One access path, canary homes, no live readings on disk |
| An insider or the deploy path | Two-person review, a published digest, an SSM alert |
| Composing state costs too much CPU on a Pi | Bench in C2 |
| Signatures cost data on a 4G box, about 150 MB a month | Measure in C2 |
| The network sees message sizes and timing | The privacy notice |
| With the copy off, the service sees from the tunnel's timing when the box reads its devices while a phone watches | The privacy notice; a frame goes on every poll whether or not a value moved, frames keep their size, and a hidden session keeps its 5 s |
| Apple sees the home id, event codes and Live Activity state, and an agent's client name in `agent.added` while the copy carries events | The privacy notice; no device or person's names in event arguments; a notice carries only its kind and that name; the M7 spike |
| Claims or defaults drift back | Claim tests; a missing grant reads as off |
| A paid app meets App Store purchase rules | Decide the payment path before charging |
| AGPL code in the App Store | An App Store exception in `ftw-app` before outside code arrives |

### Project statistics policy, October 2026

`ftw-project-stats-2026-10-v3` covers configured capacities, flexible power,
solar rating and closed-hour energy. `ftw-cloud-2026-10-v3` includes that scope
for every Cloud home. An owner reads `GET /api/cloud/stats-policy`, then
accepts the Cloud terms through `POST /api/cloud/grant`. Core records the
scope, random stats id and start time in the same grant. An older Cloud
answer or saved statistics checkbox does not accept this version.

A local owner can instead accept the statistics policy through
`POST /api/cloud/stats`, without granting readable state or remote access.
`DELETE /api/cloud/stats` stops that local contribution. Within Cloud,
statistics cannot be turned off alone; `DELETE /api/cloud` ends Cloud,
requests home deletion and remembers the off choice across restarts.
Phones can need pairing again after the home key is deleted. An app user
who declines its terms pauses Cloud on that phone and keeps its local keys.

The daily anonymous report also waits for the current project choice.
Its saved config switch cannot bypass a missing answer or a local No.
(Replaced on 24 October 2026; see "Anonymous daily reports and one
statistics answer" below.)
A new policy rotates the stats id and starts a new reporting window. An
unset wall clock starts that window only once time is known; no earlier
hours are filled in.

The public view counts installations reporting in the last 24 closed hours.
It sums the recorded hours, which may be incomplete; it does not claim all
FTW users, full-day production or measured control capacity. Configured
limits remain distinct from measured command results. Unknown values stay
absent. Cloud keeps the consented hourly rows for 48 hours and publishes
only the existing group thresholds. Withdrawal deletes unpublished rows
when Cloud receives the signed roster; offline withdrawal takes effect on
reconnect. Published group totals remain. Native consent UI must adopt the
new policy before it can enable this scope.

C8 stays off until its signed DPIA is recorded. The service starts with no
approved statistics policy. Only an explicit operator setting enables the
exact policy in signed `hello_ok`; Core sends no hours without that signal,
even if an owner previously chose to contribute. The owner policy response
reports this readiness so native clients can explain the wait. Pausing the
service policy stops collection while withdrawal and retention still run.
Deploying this code does not sign the DPIA or accept a household's consent.

### Anonymous daily reports and one statistics answer, October 2026

Fredrik decided this on 24 October 2026, so that stats.ftw.energy can show
energy without C8's household rows, and so that statistics are one thing:
"antingen delar man med sig av stats eller inte".

**One answer.** Statistics are one answer on the box that covers both
anonymous daily reports, with no second setting:

- With FTW Cloud they are part of Cloud, with no separate off switch, as in
  the 17 October amendment. A Cloud grant under older terms sends nothing
  until the owner accepts the current ones.
- Without Cloud they are on until the owner turns them off, under Settings →
  FTW app → Anonymous statistics, where the box shows each message. Nobody is
  asked any more; the local question and its "nothing before an answer" rule
  are gone.
- Stored state is read once (`cloudlink.MigrateStatistics`). A local No, a
  fleet report switched off in the config (`fleet_ping.enabled: false`), or
  Cloud turned off before this release (whose screen said that stops
  statistics) stays off. Every other box is on. `fleet_ping.enabled` is no
  longer a switch.
- A box without Cloud no longer starts a statistics-only Cloud link. Those
  already running keep working until turned off. Hourly C8 rows for local
  boxes therefore have no way in; if C8 is ever activated for them, it needs
  its own consent.
- The hourly C8 upload follows the same answer: off, or Cloud under older
  terms, sends no hour whatever the grant carries, and an answer that is off
  takes the stats purpose out of the grant. Under older terms, asking for
  statistics is refused until the owner accepts the current terms.
- A stored grant counts only through the running link, which honours it only
  with the key it was made with.

**Two reports, never one message.** Each leaves at its own random time:

- the fleet report (`go/internal/fleetping`): version, channel, drivers,
  battery size, price zone, install age, as before;
- the energy report (`go/internal/energyreport`): once per closed UTC day,
  1 to 12 hours after it closes, the date and six whole-kWh sums (solar
  generation, grid import and export, battery charge and discharge, vehicle
  charge). A day with a gap in the energy ledger is skipped, never sent in
  part. Never for a site a partner controls.

With a fleet of about eight boxes, a driver set such as Sungrow, Easee,
myuplink and tesla-ha in SE4 points at one home. Energy sent beside it would
be that home's energy again. So the two share no field and never travel
together.

**Path.** Both go through one independent Oblivious HTTP relay (RFC 9458) to
FTW Cloud's gateway, `POST /v1/ohttp` in `ftwcloudd`, as one padded inner
request each (`go/protocol/reportwire`, 1,536 bytes; the relay always sees
1,591). Inside, each report carries a random id and the UTC date it was
sent. The gateway reads the schema, adds the report to its own day totals
(`/v1/fleet/public`, `/v1/energy/public`) and keeps nothing per report but
a hash of its id, for three days, so a replay or a retry counts once
(RFC 9458, section 6.5). A report sent before yesterday is refused. Both
public views list closed days only, so nobody can match a new count to the
report that just arrived.
`go/internal/statsrelay` holds the relay URL and the gateway key as
compile-time pins. The box encrypts only to the pinned key and never asks the
service for one, so every box on a release uses the same key and a key
swapped in at the service opens nothing. Until both pins are set, the energy
report is not sent, and the fleet report posts straight to `/v1/fleet` as
before, but only from a box that said yes: FTW Cloud's terms or a local yes.
A box that shares only by default never posts straight to FTW Cloud, which
would see its address; it waits for the relay (Fredrik, 24 October 2026). A
build with the relay sends the fleet report to FTW Cloud only through it.
`POST /v1/fleet` stays for boxes on older releases; it can go one stable
release after the first that pins the relay. A collector the household set
in `fleet_ping.endpoint` is still posted to directly.

**Who sees what.** The relay sees the box's address, the time and a
ciphertext of fixed length, never a report. Cloudflare's relay, if chosen,
keeps those logs for about 124 days. The gateway sees each report, the
relay's address and the time, never the box's address. FTW Cloud keeps per
date only the counts and sums; a public energy date appears once no report
may still name it, four days after it ends, and needs at least 5 reports. A
sum that one to four of them had any of shows null: five reports with one car
would otherwise publish that car's charge.

Limits, plainly:

- The relay and Sourceful together could join reports to addresses by time.
  The relay's independence is the control.
- FTW Cloud holds readings from homes whose owners turned on the readable
  copy, including 48 hours of history tiles. The same daily sums could be
  computed from them and matched with a report. The gateway does not do this
  and keeps nothing to match with, but that is a rule in code, not one the
  cryptography enforces.
- The floor of five counts reports, not boxes; Fredrik accepted this on
  24 October 2026. An anonymous report cannot prove that a real box sent
  it. The relay alone can drop the other reports and add fakes to isolate
  one home's day, and that shows on our side as missing reports. Anyone
  else can do it only on a day when one real home reported. Rejecting
  replays and listing closed days only remove the cheap attacks. Anonymous
  tokens (Privacy Pass) are the later fix, once the fleet grows or someone
  shows a need.

**Open for Fredrik's lawyer, not decided here.** Reports that are anonymous
to Sourceful through OHTTP should be outside GDPR. Reading data from the
owner's equipment to make them may still need consent under the ePrivacy
rule (Swedish LEK, chapter 9, section 28). If it does, neither the opt-out for
boxes without Cloud nor the requirement with Cloud holds, and both need an
active yes. Fredrik chose this design; it is built as he asked. C8 stays off.
