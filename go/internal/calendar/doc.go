// Package calendar consumes calendar events as planner constraints (issue
// #498). FTW runs a CalDAV *client* against its own in-process
// native CalDAV server (internal/caldavserver) — it polls a calendar
// collection and maps events into intents the planner already knows how to
// honour:
//
//   - an "away"/vacation event switches the load model to its away profile for
//     the interval (loadmodel.ProfileAway, ~25% load) so the MPC conserves
//     battery while the house is empty; IsAwayAt lets the load predictor apply
//     the away profile per slot across the planning horizon;
//   - an EV "charged-by-departure" event sets the matching loadpoint's target
//     SoC + deadline (loadpoint.Manager.SetTarget), which the MPC loadpoint
//     probe already reads and enforces.
//
// Events are classified by case-insensitive keyword match on the event title
// (SUMMARY), e.g. "Away" / "Vacation" or "Charge car 80%". Keyword lists are
// configurable (config.CalDAV) so non-English calendars work; an explicit
// "lp:<id>" token and an "<n>%" target are honoured when present.
//
// The whole feature is opt-in (config.CalDAV.Enabled) and fail-soft: an
// unreachable server logs a warning and leaves control untouched. Recurrences
// are expanded server-side via the CalDAV calendar-query Expand element, so no
// RRULE math lives here.
package calendar
