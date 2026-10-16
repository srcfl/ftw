# ADR 0008: FTW Cloud keeps a readable copy the owner turns on

- Status: proposed. Nothing described here has shipped.
- Date: 2026-10-12
- Decided by: Fredrik on 12 October 2026, decisions 1–5 and 7. Decision 6 is
  the recommendation, taken as the working choice until he answers it.
- Revisits: [ADR 0006](0006-app-uplink.md), which made the relay the only
  remote path. That still holds for homes that do not turn FTW Cloud on.
- Revisited by: [ADR 0010](0010-cloud-entry.md), proposed, which carries
  command frames on this connection. The cloud forwards them and still
  cannot read them.
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

**The start flow has two switches.** The first turns FTW Cloud on; the iPhone
app needs it, and it lists what leaves the house. The second turns statistics
on; it is off by default, and the app works the same either way. A change to
purpose or fields needs a new policy version and fresh consent.

- **Purpose.** To learn how homes with FTW use and shift energy, to improve
  FTW's planning and defaults, and to publish figures about homes as a group.
  Not for advertising, decisions about one home, or handing rows to anyone.
- **Fields.** Per closed hour, the site's energy totals in Wh, rounded to
  10 Wh, under the ledger's flow names; the price zone; which of battery,
  solar and charger the site has; the policy version. Nothing finer than an
  hour, and no names, addresses, IP addresses, serials, coordinates, VINs,
  config or logs.
- **`statsId`.** 128 random bits drawn by the box at consent, derived from
  nothing else. A new consent draws a new one.
- **Retention.** Rows go 13 months after the hour they describe. Withdrawal or
  a purge deletes them within 24 hours. Published aggregates stay, and the
  consent screen says so.
- **Cell rules.** At least 20 homes per figure, nothing finer than a price
  zone or an hour, and no home above 30 % of a cell's sum. Code applies the
  rules; no person reads raw rows.
- **Partners.** Homes under a partner's control stay out unless that
  partner's contract allows them in. Nova keeps B2B partner telemetry.
- **Law.** The service links `statsId` to the home so a purge can reach it,
  which makes the rows personal data (CJEU C-413/23 P). Serving the owner's phones
  is necessary for the service asked for and meets the exemption in LEK
  9 kap. 28 §; statistics are not, so they need consent. A lawyer confirms.

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
| Registry | Until purge, or 90 days without a roster |
| APNs registrations | Until purge, or 30 days without the box |
| Events | A one-hour ring in memory |
| Access logs | 14 days, with a keyed hash instead of the address |
| SSM sessions and deploy log | 24 months |
| Backups | None; boxes refill the service |
| Statistics | 13 months |

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

The statistics switch ships with the start flow in C4. The box records consent
and `statsId` from then on, nothing is sent until C8, and the backfill covers
the gap. The lawyer also confirms that order: if the DPIA in C8 changes the
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
