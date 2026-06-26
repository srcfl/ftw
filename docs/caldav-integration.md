# Calendar integration (CalDAV) — planner constraints + EVSE history

Issue #498. Let the planner act on intent you express in your normal calendar
app, and surface energy history back into that calendar — all on your local
network.

## Mental model

forty-two-watts does **not** host CalDAV. It runs a CalDAV **client** that
talks to a bundled [Radicale](https://radicale.org/) sidecar (or any CalDAV
server). Radicale owns the protocol correctness (PROPFIND/REPORT/sync with
iOS, Google Calendar, Thunderbird, …); 42W just polls a collection and maps
events onto machinery it already has.

```
 Calendar app ──CalDAV(LAN)──▶ Radicale :5232 ◀──poll/write── 42W (internal/calendar)
                                                              │ away → loadmodel.ProfileAway
                                                              │ "charge car 80%" → loadpoint target
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

## Setup

By default 42W **manages the credential for you** (`caldav.manage_credentials:
true`): on first enable it generates a random password, writes the Radicale
htpasswd file itself, and shows the username + password (with a QR) in
**Settings → Calendar**. So the usual flow is just:

1. Start the sidecar (opt-in compose profile):
   ```bash
   docker compose --profile calendar up -d
   ```
2. In the 42W dashboard, **Settings → Calendar**: tick *Enabled*, save.
3. Open the **Calendar account** panel that appears — copy the username +
   password, or scan the QR to get the subscribe URL onto your phone — and add
   a CalDAV account in your calendar app pointing at the shown URL, e.g.
   `http://<host-ip>:5232/fortytwowatts/energy/` (the tab rewrites `localhost`
   to the dashboard's host for you).

> **Manual credentials.** If you'd rather manage the htpasswd yourself, set
> `caldav.manage_credentials: false`, run
> `cp radicale/config/users.example radicale/config/users && htpasswd -B
> radicale/config/users fortytwowatts`, and enter the same username/password in
> the Calendar tab. (For 42W to write the managed file it needs the shared
> `./radicale/config` mount — present in the bundled docker-compose; raw-binary
> deploys fall back to manual.)

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

The calendar feature needs the **Radicale sidecar**, which is **GPLv3**. 42W
keeps it strictly **arm's length** — a separate container reached only over the
CalDAV network protocol — so 42W itself stays cleanly separable and permissively
licensable (MIT/Apache-2.0). The compose file only *references* the public image
(pulled at runtime); Radicale's code is never bundled or redistributed.

The flip side: a sidecar can't live inside a **single-container Home Assistant
add-on**. So when 42W detects it's running as an HA add-on
(`runningAsHAAddon()` — `FTW_HA_ADDON=1`, or a Supervisor token /
`/data/options.json` fallback) it **disables the calendar feature** and the
Settings tab explains why. Override with `FTW_CALDAV_FORCE=1` if you run a
*separate* Radicale add-on and point `caldav.url` at it.

### Native in-process server (`server: native`) — prototype

There's now a second option that sidesteps all of the above: set
`caldav.server: native` and 42W hosts CalDAV **itself**, in-process, using
[`emersion/go-webdav`](https://github.com/emersion/go-webdav) (**MIT**) — see
`go/internal/caldavserver`. No Radicale, no second container, no GPL boundary,
so it works in a single binary / single container **including the HA add-on**.
42W's calendar client still talks CalDAV over `localhost`, so the
inbound/outbound intent logic is identical; only "what it connects to" changes.
It listens on `caldav.listen` (default `:5232`) with the managed credential.

> **Prototype status:** storage is **in-memory** (events are lost on restart)
> and recurrences aren't expanded server-side. Fine for evaluation; not yet a
> production drop-in for Radicale. **TODO(#498):** back it with `state.db` for
> durability. Default stays `radicale` until then.

**TODO (#498):** wire `server: native` into the HA add-on (it's exempt from the
HA-addon disable) and add SQLite persistence. Tracking with the add-on
maintainer (`erikarenhill`).

## Implementation

- `go/internal/calendar` — CalDAV client poll loop (`service.go`), title→intent
  parser (`parse.go`), EVSE session detector + writer (`history.go`).
- Inbound wiring (`go/cmd/forty-two-watts/main.go`): away intervals drive
  `loadmodel.SetProfile` (live) + an away-aware `mpc.Service.Load` predictor
  (horizon); EV deadlines call `loadpoint.Manager.SetTarget`.
- `GET /api/caldav/status` (`go/internal/api/api_caldav.go`) backs the
  Settings tab.
- Deploy: `radicale` service in `docker-compose.yml` (profile `calendar`) +
  `radicale/config/`.
