# calendar — CalDAV-client planner constraints (issue #498)

## What it does

Polls a CalDAV calendar collection — served by FTW's own in-process native
CalDAV server (`internal/caldavserver`) — and maps events into planner intents.
This package is the **client**; the server is a sibling package. Opt-in
(`config.CalDAV.Enabled`) and fail-soft: an unreachable server never blocks
control.

Two intents, both routed onto **existing** machinery:

| Calendar event (title keyword) | Effect |
|---|---|
| "Away" / "Vacation" / "Holiday" | `loadmodel.SetProfile(ProfileAway)` for the interval (live + training); `IsAwayAt(t)` drives the per-slot MPC load override |
| "Charge car 80%" / "EV …" | `loadpoint.Manager.SetTarget(id, soc, departure)` → the MPC loadpoint probe already enforces it |

It also **writes** two read-only calendars (separate collections): an EVSE
usage history (`history.go`) and the planner's forward-looking charge/discharge
windows (`plan.go`, reconciled each cycle — PUT changed, DELETE stale).

## Files

- `parse.go` — pure title→intent classifier (`parser`, no config/network dep).
  Keyword substring match (case-insensitive); `(\d{1,3})%` → target SoC;
  `lp:<id>` → explicit loadpoint. EV checked before away (more specific).
- `service.go` — `Service`: poll loop, CalDAV fetch (calendar-query REPORT
  with a time-range filter the server expands recurrences within; response-size
  + event-count + timeout DoS caps), `apply` (profile switch + EV target),
  `IsAwayAt`, `Status`, `Credentials`.
- `provision.go` — `GenerateToken`: mints the managed credential (main.go
  persists it to state.db; the native server authenticates against it and the
  Settings tab reveals it +QR).
- `doc.go`, `*_test.go` (incl. `native_e2e_test.go`: the whole feature against
  the in-process server, including a recurring "Away" event).

## Public API

- `New(cfg config.CalDAV, lp LoadpointTargeter, lm LoadProfiler, firstLoadpointID string) *Service`
- `(*Service).Start(ctx)` / `.Stop()` / `.Reload(cfg, firstLoadpointID)`
- `(*Service).IsAwayAt(t) bool` — wired into the MPC load predictor in main.go.
- `(*Service).Status() Status` — rendered by `GET /api/caldav/status`.
- Interfaces `LoadProfiler` (= `*loadmodel.Service`) and `LoadpointTargeter`
  (= `*loadpoint.Manager`) keep the package testable with fakes.

## How it talks to neighbors

- main.go starts the `caldavserver` first, then constructs this client after
  `loadSvc` + `lpMgr`, wraps `mpcSvc.Load` with `IsAwayAt`+
  `loadSvc.PredictWith(t, ProfileAway)`, and registers a config-reload hook.
  `api.Deps.CalDAV` exposes `Status()`.
- Recurrences expand **server-side** in `caldavserver` — **no RRULE math here**.

## What NOT to do

- Do not host CalDAV here — that's `internal/caldavserver`; this is the client.
- Do not block control on a poll error — record it in `Status`, keep last-good.
- Do not clear a manual/UI EV target when no calendar event is upcoming.
- Away profile switch fires on transition only (idempotent); leaving an away
  window restores `ProfileHome`, intentionally overriding a manual profile
  choice while/after an away event (documented precedence).
