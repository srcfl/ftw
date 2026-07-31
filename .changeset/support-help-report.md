---
"ftw": minor
---

Help report: `GET /api/support/report` returns a single Markdown file
describing what FTW is doing and why, reachable from a "Something looks
wrong?" button under the plan chart and from any driver's Diagnose modal.
It exists because answering "why is it discharging when the plan says
charge?" currently takes a screenshot round-trip per question — one thread
needed nine of them to establish that a load forecast said 383 W while the
house was drawing 7.9 kW.

The file opens with `Findings`: automated checks written in the order a
person would run them — stale plan, fallback solver, offline or faulted
devices, active safety limits, and a forecast-versus-reality comparison for
both load and solar. Under that come the live site state, the plan slot
covering *this moment* (stated before anything about the next one, since
confusing the two is what sends these threads sideways), the surrounding
slots with the solver's reasoning, forecast accuracy, driver health,
component versions, and the recent warning and error log.

It is one file rather than a bundle because people paste it into a chat and
ask for help there; a tarball of JSON does not survive that. The plan window
is trimmed to a few hours either side of now and logs to warnings and errors,
which keeps it small enough to upload and to read in full.

`/api/support/dump` is unchanged and remains the deep bundle for cases the
report cannot close.
