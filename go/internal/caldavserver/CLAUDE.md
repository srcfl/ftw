# caldavserver — native in-process CalDAV server (#498, PROTOTYPE)

## What it does

A pure-Go CalDAV server built on `github.com/emersion/go-webdav` (**MIT**), an
alternative to the bundled Radicale sidecar (GPLv3). Selected with
`caldav.server: native`. Because it's in-process it ships in the single 42W
binary — no second container — so the calendar feature can run in a
single-container Home Assistant add-on, and the GPL arm's-length constraint
disappears.

42W's existing calendar client (`internal/calendar`) still talks CalDAV over
localhost, so inbound/outbound intent logic is unchanged — this only replaces
*what the client connects to*. Both sides authenticate with the managed
credential.

## Files

- `server.go` — `Server` (go-webdav `caldav.Handler` behind Basic auth on its
  own listener, default `:5232`) + `New` / `NewHandler` / `Start` / `Stop`.
  `basicAuth` is constant-time and fail-closed on an empty password.
- `backend.go` — `memBackend`: an in-memory `caldav.Backend`. `QueryCalendarObjects`
  reuses `caldav.Filter` for query matching.
- `server_test.go` — client↔server round-trip (PUT/REPORT/DELETE) + auth.

## Wired in

`main.go`: when `cfg.CalDAV.ServerMode() == "native"` it starts the server
(`caldavserver.New`) and is **exempt** from the HA-addon disable
(`runningAsHAAddon`). The end-to-end test that the real `calendar.Service`
parses intents from it lives in `internal/calendar/native_e2e_test.go`.

## PROTOTYPE limitations (do not ship as default yet)

- **In-memory storage** — objects are lost on restart. TODO(#498): persist to
  `state.db` (a `caldav_objects` table); the `caldav.Backend` boundary keeps
  that change local to `backend.go`.
- **No server-side recurrence expansion** — recurring events return as their
  master VEVENT.
- Single principal; minimal MKCALENDAR/sync semantics. Tested against 42W's own
  go-webdav client, not yet against iOS/Google/Thunderbird.

## What NOT to do

- Don't run it alongside Radicale on the same `:5232` — you pick one
  (`caldav.server`). Native mode needs no `radicale` compose service.
- Don't treat it as production-ready until storage is persistent.
