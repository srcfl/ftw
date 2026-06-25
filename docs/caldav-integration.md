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

- Radicale binds to the LAN (`:5232`) behind HTTP Basic auth (htpasswd).
- It is **never** routed through the owner-access relay and no port-forward to
  `5232` is created. Off your network it simply doesn't sync, then catches up
  when you're home. Nothing here reaches the internet unless you forward the
  port yourself.
- Over plain HTTP on the LAN, Basic-auth credentials are base64-encoded (not
  encrypted) — standard for self-hosted CalDAV on a trusted home network.

## Setup

1. Set a Radicale password (one-time):
   ```bash
   cp radicale/config/users.example radicale/config/users
   htpasswd -B radicale/config/users fortytwowatts
   ```
2. Start the sidecar (opt-in compose profile):
   ```bash
   docker compose --profile calendar up -d
   ```
3. In the 42W dashboard, **Settings → Calendar**: enable, set the same
   username/password, save.
4. Subscribe your calendar app to the URL shown in that tab, e.g.
   `http://<host-ip>:5232/fortytwowatts/energy/` (the tab rewrites `localhost`
   to the dashboard's host for you).

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
