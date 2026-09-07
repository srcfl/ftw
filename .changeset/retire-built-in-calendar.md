---
"ftw": major
---

BREAKING CHANGE: Remove the built-in CalDAV server, its API endpoints, calendar settings and calendar-driven charging and away events.

Use loadpoint targets and ready-by schedules for future charging. Existing goals, calendar data, learned models and forecast archives remain in place. Older config files still load and warn when calendar support was enabled.
