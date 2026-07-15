# Calendar integration (CalDAV) — planner constraints + EVSE history

Issue #498. Let the planner act on intent you express in your normal calendar
app, and surface energy history back into that calendar — all on your local
network.

## Mental model

FTW **hosts its own CalDAV server**, in-process, and also runs a CalDAV
**client** against it:

- The **server** (`go/internal/caldavserver`) is pure-Go, built on
  [`emersion/go-webdav`](https://github.com/emersion/go-webdav) (MIT). It ships
  inside the single FTW binary — no sidecar, no second container — and persists
  calendar objects in `state.db`. It binds `:5232` on your LAN so your phone or
  desktop calendar app can subscribe. Because it's in-process it runs everywhere
  FTW does, **including a single-container Home Assistant add-on**.
- The **client** (`go/internal/calendar`) polls a collection on that server and
  maps events onto planner machinery.

```
 Calendar app ──CalDAV(LAN :5232)──▶  FTW
   (phone /                            ├─ caldavserver (in-process, go-webdav)
    Thunderbird)                       └─ calendar client ──▶ away → loadmodel.ProfileAway
                                          (poll/write over          "charge car 80%" → loadpoint target
                                           localhost)               EV session ended → write history event
```

Two directions:

- **Inbound** — you create events in your app; FTW reads them as intents.
- **Outbound** — FTW writes read-only "EVSE history" and "plan" calendars you
  subscribe to (one event per completed charge session; upcoming
  charge/discharge windows).

## Security / network posture

- The CalDAV server listens on **`:5232`** (all interfaces) — i.e. it is
  reachable from **any device on your home network**, not just loopback, so
  phones can sync. It is **not** forwarded to the internet by the FTW deployment
  (no port-forward is created) and is **never** routed through the owner-access
  relay, so it stays on your LAN unless you deliberately forward port `5232` on
  your router. Off your network it simply doesn't sync, then catches up when
  you're home.
- Authentication is HTTP Basic over **plain HTTP** — credentials are
  base64-encoded (not encrypted). This is standard for self-hosted CalDAV on a
  **trusted** home network. If your LAN has guest WiFi or untrusted IoT
  devices, treat this as a weaker boundary: use a strong password, leave the
  feature off (it is opt-in), or put FTW behind a TLS reverse proxy. The server
  fails closed — an empty configured password rejects every request.
- The FTW API surface for this feature (`/api/caldav/*`) is behind the normal
  owner-auth gate; only the CalDAV port itself is on the open LAN.
- DoS hardening: FTW's client caps the CalDAV response it will read (25 MiB) and
  the number of events it parses per poll (10k), and bounds each poll with a
  timeout, so a hostile/MITM'd server can't exhaust the Pi or stall the calendar
  loop. The control loop runs in separate goroutines and is never blocked.

## Setup

No sidecar, nothing to install. FTW **manages the credential for you**
(`caldav.manage_credentials: true`): on first enable it generates a random
password and shows the username + password (with a QR) in **Settings →
Calendar**.

1. In the dashboard, **Settings → Calendar**: tick *Enabled*, save.
2. FTW starts its in-process CalDAV server on `:5232`. Open the **Calendar
   account** panel that appears — copy the username + password, or scan the QR
   to get the subscribe URL onto your phone — and add a CalDAV account in your
   calendar app pointing at the shown URL, e.g.
   `http://<host-ip>:5232/fortytwowatts/energy/` (the tab rewrites `localhost`
   to the dashboard's host for you).

That's the whole flow.

> **Manual credentials.** Set `caldav.manage_credentials: false` and put your
> own `password` in the `caldav:` block (stored in `state.db`, not `config.yaml`
> — see below). The server authenticates against it.

## Writing intents (title keywords)

Events are classified by case-insensitive keyword in the **title**:

| Title example | Meaning |
|---|---|
| `Away`, `Vacation 2 weeks`, `Holiday` | Away interval `[start, end)` → away load profile (~25% load); planner conserves battery. |
| `Charge car 80%` | EV must reach 80% by the event's **start** time. `lp:<id>` selects a loadpoint; no `%` → `ev_default_target_soc_pct`. |

Keyword lists (`away_keywords`, `ev_keywords`) are configurable for other
languages. What FTW parsed is visible at `GET /api/caldav/status`.

**Recurring events work fully.** A weekly *Away* or a daily *Charge car* expands
into its individual occurrences server-side (RFC 4791 `CALDAV:expand`, via
`caldavserver/expand.go`), so the planner sees every occurrence inside its
horizon — not just the first. RRULE, RDATE and EXDATE are all honoured, and if
you edit or delete a single occurrence in your calendar app (a per-instance
`RECURRENCE-ID` override or cancellation) that one occurrence is updated/removed
while the rest of the series is unchanged.

## EVSE history (outbound)

When an EV charge session ends, FTW writes a VEVENT into a **separate**
collection (`history_path`, default `/fortytwowatts/history/`) — e.g.
`EV charged 12.3 kWh`, spanning the charge window. The history collection is
deliberately distinct from the intent calendar so FTW never re-reads its own
events as intents. Subscribe to it read-only. Disable with `evse_history:
false`.

## Plan publishing (outbound, forward-looking)

FTW also publishes the planner's **upcoming** decisions as a read-only calendar
you can subscribe to (`plan_path`, default `/fortytwowatts/plan/`). On each
publish it coalesces the MPC plan into charge/discharge windows — e.g.
`Charge battery ~3.2 kW` from 02:00–05:00 — marked `TENTATIVE` (it's a plan,
not a commitment).

Because the plan re-plans every ~15 min, the publisher **reconciles** rather
than appends: each cycle it PUTs new/changed windows and DELETEs windows that
are no longer planned (or have fallen into the past), keyed by a stable UID,
so your calendar reflects the current plan without piling up stale events.
Only forward-looking windows are published; idle/"hold" slots are omitted.
Disable with `publish_plan: false`; tune cadence with
`plan_publish_interval_s` (default 900). The plan, history and intent
collections are kept distinct so FTW never re-reads its own output as input.

## Config

See the `caldav:` block in `config.example.yaml`. The password is stored in
`state.db` (key `caldav_password`), never written to `config.yaml`. URL,
credentials, keywords and intervals hot-reload; toggling `enabled` needs a
restart. `listen` (default `:5232`) sets the server's bind address.

## Deploy modes & Home Assistant

The CalDAV server is in-process and pure-Go (MIT), so it works in **every**
deploy mode with nothing extra to install:

- **Raspberry Pi image / raw binary / docker-compose (host networking):** the
  server binds `:5232` directly on the host. Subscribe at
  `http://<host-ip>:5232/…`.
- **docker-compose on macOS (bridge networking):** the main service publishes
  `5232:5232` (see `docker-compose.macos.yml`) so phones reach it; keep
  `caldav.url: http://localhost:5232` (the in-container loopback).
- **Home Assistant add-on (single container):** it just works — there is no
  sidecar at all, so no deploy-mode is gated off.

Objects persist in `state.db`, so events survive restarts and image upgrades.

## Implementation

- `go/internal/caldavserver` — the native server: a go-webdav `caldav.Handler`
  (`server.go`) + a `state.db`-backed `caldav.Backend` (`backend.go`) +
  server-side recurrence expansion (`expand.go`).
- `go/internal/calendar` — CalDAV client poll loop (`service.go`), title→intent
  parser (`parse.go`), EVSE session detector + writer (`history.go`), plan
  reconciler (`plan.go`).
- Inbound wiring (`go/cmd/ftw/main.go`): away intervals drive
  `loadmodel.SetProfile` (live) + an away-aware `mpc.Service.Load` predictor
  (horizon); EV deadlines call `loadpoint.Manager.SetTarget`.
- `GET /api/caldav/status` + `GET /api/caldav/credentials`
  (`go/internal/api/api_caldav.go`) back the Settings tab.

### Third-party libraries

The CalDAV stack is built entirely on **Emerson Tan's `emersion/*` libraries**
(all **MIT**-licensed, pure Go — no CGo), which keep FTW a single permissively
licensed static binary:

- [`github.com/emersion/go-webdav`](https://github.com/emersion/go-webdav) — the
  CalDAV **client** (poll/read/write) **and** the CalDAV **server**
  (`caldav` subpackage) behind the in-process native server.
- [`github.com/emersion/go-ical`](https://github.com/emersion/go-ical) —
  iCalendar (RFC 5545) parsing + encoding, and the recurrence set used by
  `expand.go`.
- [`github.com/teambition/rrule-go`](https://github.com/teambition/rrule-go) —
  RRULE/RDATE/EXDATE expansion (reached via go-ical's `RecurrenceSet`).

### Future work

- Verify interop against the full matrix of iOS / Google / Thunderbird (today
  it's proven against FTW's own go-webdav client).
