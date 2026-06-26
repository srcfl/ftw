# Calendar integration (CalDAV) — planner constraints + EVSE history

Issue #498. Let the planner act on intent you express in your normal calendar
app, and surface energy history back into that calendar — all on your local
network.

## Mental model

There's a CalDAV **server** (where your calendar lives) and 42W's CalDAV
**client** (which reads/writes it and maps events onto planner machinery). The
client is always the same; you choose the server with `caldav.server`:

- **`native` (default)** — 42W hosts CalDAV **itself**, in-process, via
  [`emersion/go-webdav`](https://github.com/emersion/go-webdav) (MIT). No
  sidecar, no extra container, objects persisted in `state.db`. Works in a
  single binary / single container — including a Home Assistant add-on. See
  `go/internal/caldavserver`.
- **`radicale`** — the bundled [Radicale](https://radicale.org/) sidecar
  (GPLv3, separate container). Radicale has more complete protocol support
  (recurrence expansion, wider client interop), so pick it if you need those
  today; it can't run in a single-container HA add-on (see *Deploy modes*).

Either way the client talks CalDAV over `localhost`, so everything below is
identical between the two — only "what it connects to" changes.

```
 Calendar app ──CalDAV(LAN)──▶  CalDAV server  ◀──poll/write── 42W (internal/calendar)
   (phone /                     native (default) │ away → loadmodel.ProfileAway
    Thunderbird)                or Radicale :5232 │ "charge car 80%" → loadpoint target
                                                  └ EV session ended → write history event
```

Two directions:

- **Inbound** — you create events in your app; 42W reads them as intents.
- **Outbound** — 42W writes a read-only "EVSE history" calendar you subscribe
  to (one event per completed charge session).

## Security / network posture

- Radicale listens on **`0.0.0.0:5232`** — i.e. it is reachable from **any
  device on your home network**, not just loopback. It is **not** forwarded to
  the internet by the 42W deployment (no port-forward is created) and is
  **never** routed through the owner-access relay, so it stays on your LAN
  unless you deliberately forward port `5232` on your router. Off your network
  it simply doesn't sync, then catches up when you're home.
- Authentication is HTTP Basic over **plain HTTP** — credentials are
  base64-encoded (not encrypted). This is standard for self-hosted CalDAV on a
  **trusted** home network. If your LAN has guest WiFi or untrusted IoT
  devices, treat this as a weaker boundary: use a strong password, leave the
  feature off (it is opt-in), or put Radicale behind a TLS reverse proxy.
- The 42W API surface for this feature (`/api/caldav/*`) is behind the normal
  owner-auth gate; only the Radicale port itself is on the open LAN.
- DoS hardening: 42W caps the CalDAV response it will read (25 MiB) and the
  number of events it parses per poll (10k), and bounds each poll with a
  timeout, so a hostile/MITM'd server can't exhaust the Pi or stall the calendar
  loop. The control loop runs in separate goroutines and is never blocked by a
  slow calendar server.

## Setup (native server — the default)

No sidecar needed. 42W **manages the credential for you**
(`caldav.manage_credentials: true`): on first enable it generates a random
password and shows the username + password (with a QR) in **Settings →
Calendar**.

1. In the dashboard, **Settings → Calendar**: tick *Enabled*, save.
2. 42W starts its in-process CalDAV server on `:5232`. Open the **Calendar
   account** panel that appears — copy the username + password, or scan the QR
   to get the subscribe URL onto your phone — and add a CalDAV account in your
   calendar app pointing at the shown URL, e.g.
   `http://<host-ip>:5232/fortytwowatts/energy/` (the tab rewrites `localhost`
   to the dashboard's host for you).

That's the whole flow — no extra container.

## Setup (Radicale sidecar — `server: radicale`)

Set `caldav.server: radicale`, then:

1. Start the sidecar: `docker compose --profile calendar up -d`.
2. Credentials work the same (42W writes the Radicale htpasswd into the shared
   `./radicale/config` mount). Tick *Enabled* and read the **Calendar account**
   panel as above.

> **Manual Radicale credentials.** Set `caldav.manage_credentials: false`, run
> `cp radicale/config/users.example radicale/config/users && htpasswd -B
> radicale/config/users fortytwowatts`, and enter the same username/password in
> the Calendar tab.

## Writing intents (title keywords)

Events are classified by case-insensitive keyword in the **title**:

| Title example | Meaning |
|---|---|
| `Away`, `Vacation 2 weeks`, `Holiday` | Away interval `[start, end)` → away load profile (~25% load); planner conserves battery. |
| `Charge car 80%` | EV must reach 80% by the event's **start** time. `lp:<id>` selects a loadpoint; no `%` → `ev_default_target_soc_pct`. |

Keyword lists (`away_keywords`, `ev_keywords`) are configurable for other
languages. What 42W parsed is visible at `GET /api/caldav/status`.

## EVSE history (outbound)

When an EV charge session ends, 42W writes a VEVENT into a **separate**
collection (`history_path`, default `/fortytwowatts/history/`) — e.g.
`EV charged 12.3 kWh`, spanning the charge window. The history collection is
deliberately distinct from the intent calendar so 42W never re-reads its own
events as intents. Subscribe to it read-only. Disable with `evse_history:
false`.

## Plan publishing (outbound, forward-looking)

42W also publishes the planner's **upcoming** decisions as a read-only calendar
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
collections are kept distinct so 42W never re-reads its own output as input.

## Config

See the `caldav:` block in `config.example.yaml`. The password is stored in
`state.db` (key `caldav_password`), never written to `config.yaml`. URL,
credentials, keywords and intervals hot-reload; toggling `enabled` needs a
restart.

## Deploy modes & Home Assistant

**The default `server: native` works in every deploy mode** — docker-compose,
raw binary, and a single-container Home Assistant add-on — because it's
in-process and pure-Go (MIT). Nothing extra to install; objects persist in
`state.db`. Its only current limitation is **no server-side recurrence
expansion** (a recurring event shows only its first occurrence) and that broad
calendar-app interop isn't fully proven yet.

`server: radicale` is the opt-in alternative when you need recurrence /
wider client interop today. Radicale is **GPLv3**, so 42W keeps it strictly
**arm's length** — a separate container reached only over the CalDAV network
protocol — which keeps 42W cleanly separable and permissively licensable
(MIT/Apache-2.0); the compose file only *references* the public image, never
bundling or redistributing it.

The catch with Radicale: a sidecar can't live inside a **single-container HA
add-on**. So 42W detects the add-on (`runningAsHAAddon()` — `FTW_HA_ADDON=1`,
or a Supervisor token / `/data/options.json` fallback) and, **in `radicale`
mode only**, disables the calendar with an explanation in the Settings tab. The
fix there is simply to use `server: native` (the default), which is exempt.
`FTW_CALDAV_FORCE=1` re-enables `radicale` mode for a *separate* Radicale add-on
you point `caldav.url` at.

**TODO (#498):** add server-side recurrence expansion to the native server
(using the vendored `teambition/rrule-go`) and verify interop against iOS /
Google / Thunderbird, then drop the Radicale option if it's no longer needed.
Tracking with the add-on maintainer (`erikarenhill`).

## Implementation

- `go/internal/calendar` — CalDAV client poll loop (`service.go`), title→intent
  parser (`parse.go`), EVSE session detector + writer (`history.go`).
- Inbound wiring (`go/cmd/forty-two-watts/main.go`): away intervals drive
  `loadmodel.SetProfile` (live) + an away-aware `mpc.Service.Load` predictor
  (horizon); EV deadlines call `loadpoint.Manager.SetTarget`.
- `GET /api/caldav/status` (`go/internal/api/api_caldav.go`) backs the
  Settings tab.
- Native server (`server: native`, default): `go/internal/caldavserver` — a
  go-webdav `caldav.Handler` + a `state.db`-backed `caldav.Backend`.
- Deploy: optional `radicale` service in `docker-compose.yml` (profile
  `calendar`) + `radicale/config/`, used only when `server: radicale`.

### Third-party libraries

The CalDAV stack is built entirely on **Emerson Tan's `emersion/*` libraries**
(all **MIT**-licensed, pure Go — no CGo):

- [`github.com/emersion/go-webdav`](https://github.com/emersion/go-webdav) — the
  CalDAV **client** (used in every mode to poll/read/write) **and** the CalDAV
  **server** (`caldav` subpackage) that powers `server: native`.
- [`github.com/emersion/go-ical`](https://github.com/emersion/go-ical) —
  iCalendar (RFC 5545) parsing + encoding.
- [`github.com/teambition/rrule-go`](https://github.com/teambition/rrule-go) —
  pulled in transitively via go-ical; the future home of native recurrence
  expansion.

These keep 42W a single static binary and are why the native server is
permissively licensable (unlike the GPLv3 Radicale sidecar).
