# calendar — CalDAV-client planner constraints (issue #498)

## What it does

Polls a CalDAV calendar collection (served by the bundled **Radicale**
sidecar, or any CalDAV server) and maps events into planner intents. 42W is
a **client only** — it does not host CalDAV. Opt-in (`config.CalDAV.Enabled`)
and fail-soft: an unreachable server never blocks control.

Two intents, both routed onto **existing** machinery:

| Calendar event (title keyword) | Effect |
|---|---|
| "Away" / "Vacation" / "Holiday" | `loadmodel.SetProfile(ProfileAway)` for the interval (live + training); `IsAwayAt(t)` drives the per-slot MPC load override |
| "Charge car 80%" / "EV …" | `loadpoint.Manager.SetTarget(id, soc, departure)` → the MPC loadpoint probe already enforces it |

## Files

- `parse.go` — pure title→intent classifier (`parser`, no config/network dep).
  Keyword substring match (case-insensitive); `(\d{1,3})%` → target SoC;
  `lp:<id>` → explicit loadpoint. EV checked before away (more specific).
- `service.go` — `Service`: poll loop, CalDAV fetch (calendar-query REPORT
  with server-side `Expand` for recurrences), `apply` (profile switch + EV
  target), `IsAwayAt`, `Status`.
- `doc.go`, `*_test.go`.

## Public API

- `New(cfg config.CalDAV, lp LoadpointTargeter, lm LoadProfiler, firstLoadpointID string) *Service`
- `(*Service).Start(ctx)` / `.Stop()` / `.Reload(cfg, firstLoadpointID)`
- `(*Service).IsAwayAt(t) bool` — wired into the MPC load predictor in main.go.
- `(*Service).Status() Status` — rendered by `GET /api/caldav/status`.
- Interfaces `LoadProfiler` (= `*loadmodel.Service`) and `LoadpointTargeter`
  (= `*loadpoint.Manager`) keep the package testable with fakes.

## How it talks to neighbors

- main.go constructs it after `loadSvc` + `lpMgr`, wraps `mpcSvc.Load` with
  `IsAwayAt`+`loadSvc.PredictWith(t, ProfileAway)`, and registers a
  config-reload hook. `api.Deps.CalDAV` exposes `Status()`.
- Recurrences expand server-side (`caldav.CalendarExpandRequest`) — **no RRULE
  math here**.

## What NOT to do

- Do not host CalDAV here — Radicale owns server correctness; this is a client.
- Do not block control on a poll error — record it in `Status`, keep last-good.
- Do not clear a manual/UI EV target when no calendar event is upcoming.
- Away profile switch fires on transition only (idempotent); leaving an away
  window restores `ProfileHome`, intentionally overriding a manual profile
  choice while/after an away event (documented precedence).
