# caldavserver — native in-process CalDAV server (#498)

## What it does

FTW's own pure-Go CalDAV server, built on `github.com/emersion/go-webdav`
(**MIT**). It is the **only** CalDAV server FTW uses — there is no sidecar.
Being in-process it ships in the single FTW binary, so the calendar feature
runs everywhere FTW does, including a single-container Home Assistant add-on.

FTW's calendar client (`internal/calendar`) talks CalDAV to it over localhost,
so the inbound/outbound intent logic is independent of transport. Both sides
authenticate with the managed credential.

## Files

- `server.go` — `Server` (go-webdav `caldav.Handler` behind Basic auth on its
  own listener, default `:5232`) + `New` / `NewHandler` / `Start` / `Stop`.
  `basicAuth` is constant-time and fail-closed on an empty password.
- `backend.go` — `backend`: a `caldav.Backend` that (de)serializes iCal +
  computes ETags and delegates persistence to a `Store`. `QueryCalendarObjects`
  runs `caldav.Filter` then expands recurrences (see below).
- `expand.go` — RFC 4791 `CALDAV:expand`. go-webdav v0.7 drops the `<expand>`
  element before the backend sees it, so the expansion window comes from the
  comp-filter time-range the client sends alongside it (`filterTimeRange`).
  Recurring masters fan out into per-occurrence instances (each with a
  `RECURRENCE-ID`, no `RRULE`) via go-ical's `RecurrenceSet` (RRULE/RDATE/EXDATE,
  rrule-go). VEVENTs are grouped by UID so per-instance `RECURRENCE-ID` override
  components replace the matching occurrence (and `STATUS:CANCELLED` deletes it).
- `store.go` — the `Store` interface (`*state.Store` satisfies it; objects live
  in the `caldav_objects` / `caldav_calendars` tables) + `NewMemStore` for
  tests / no-DB fallback.
- `server_test.go` — client↔server round-trip (PUT/REPORT/DELETE), auth, and a
  restart-survival test against a real `state.db`.
- `expand_test.go` — recurrence expansion (end-to-end through the client + the
  pure `expandCalendar`).

## Wired in

`main.go`: when `cfg.CalDAV.Enabled` it starts the server (`caldavserver.New`)
against `state.db`. The end-to-end test that the real `calendar.Service` parses
intents (including recurring ones) from it lives in
`internal/calendar/native_e2e_test.go`.

## Known limits

- Single principal; minimal MKCALENDAR/sync semantics.
- Interop verified against FTW's own go-webdav client, not yet the full matrix
  of iOS / Google / Thunderbird.

## What NOT to do

- Don't put iCal parsing in `state` — it stays storage-only (`data TEXT`); the
  backend owns (de)serialization. Recurrence math lives in `expand.go`, not in
  the client.
