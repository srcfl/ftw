---
"forty-two-watts": minor
---

Calendar-based planner constraints via a built-in CalDAV calendar (#498). 42W
hosts its own in-process, LAN-only CalDAV server (pure-Go, `emersion/go-webdav`,
MIT; objects persist in `state.db`), so no extra container is needed — it even
works as a single-container Home Assistant add-on. Recurring events are expanded
server-side. 42W reads a calendar you keep in your normal calendar app and turns
events into planner intents:

- An **Away** / **Vacation** event switches the load model to its away profile
  for that interval, so the planner conserves battery while the house is empty.
- A **"Charge car 80%"** event (with your departure as the event time) sets the
  matching loadpoint's target SoC + deadline, which the MPC already honours.

42W also **writes** read-only calendars you can subscribe to:

- an EVSE usage history ("EV charged 12.3 kWh", one event per charge session);
- the planner's forward-looking plan — upcoming battery charge/discharge
  windows — reconciled each cycle so it stays current without piling up.

Both read-only calendars can be subscribed to in one tap via a `webcal://`
link (a read-only `.ics` feed served on the CalDAV port).

The feature is opt-in (`caldav.enabled`) and fail-soft — a calendar problem
never blocks control — and stays entirely on your local network. Configure it
under Settings → Calendar (mobile-friendly), which auto-manages the credential
and shows it (with a QR) to add to a phone or desktop calendar app.
