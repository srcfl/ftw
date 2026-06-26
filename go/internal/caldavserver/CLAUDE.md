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
- `backend.go` — `backend`: a `caldav.Backend` that (de)serializes iCal +
  computes ETags and delegates persistence to a `Store`. `QueryCalendarObjects`
  reuses `caldav.Filter` for query matching.
- `store.go` — the `Store` interface (`*state.Store` satisfies it; objects live
  in the `caldav_objects` / `caldav_calendars` tables) + `NewMemStore` for
  tests / no-DB fallback.
- `server_test.go` — client↔server round-trip (PUT/REPORT/DELETE), auth, and a
  restart-survival test against a real `state.db`.

## Wired in

`main.go`: when `cfg.CalDAV.ServerMode() == "native"` it starts the server
(`caldavserver.New`) and is **exempt** from the HA-addon disable
(`runningAsHAAddon`). The end-to-end test that the real `calendar.Service`
parses intents from it lives in `internal/calendar/native_e2e_test.go`.

## Remaining gaps (default stays `radicale` until closed)

- **No server-side recurrence expansion** — recurring events return as their
  master VEVENT.
- Single principal; minimal MKCALENDAR/sync semantics. Interop verified against
  42W's own go-webdav client, not yet against iOS/Google/Thunderbird.

Storage IS durable now (persists in `state.db`).

## What NOT to do

- Don't run it alongside Radicale on the same `:5232` — you pick one
  (`caldav.server`). Native mode needs no `radicale` compose service.
- Don't put iCal parsing in `state` — it stays storage-only (`data TEXT`); the
  backend owns (de)serialization.
