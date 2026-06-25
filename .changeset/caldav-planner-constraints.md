---
"forty-two-watts": minor
---

Calendar-based planner constraints via a built-in CalDAV calendar (#498). 42W
can now read a calendar you keep in your normal calendar app (served by a
bundled, LAN-only Radicale sidecar) and turn events into planner intents:

- An **Away** / **Vacation** event switches the load model to its away profile
  for that interval, so the planner conserves battery while the house is empty.
- A **"Charge car 80%"** event (with your departure as the event time) sets the
  matching loadpoint's target SoC + deadline, which the MPC already honours.

42W also **writes** a read-only history calendar you can subscribe to: it
records one event per completed EV charging session ("EV charged 12.3 kWh").

The feature is opt-in (`caldav.enabled`) and fail-soft — an unreachable
calendar server never blocks control — and stays entirely on your local
network. Configure it under Settings → Calendar; enable the sidecar with
`docker compose --profile calendar up -d`.
