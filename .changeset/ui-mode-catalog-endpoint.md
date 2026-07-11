---
"forty-two-watts": minor
---

Drive the dashboard mode buttons from a server-side catalog. New
`GET /api/modes` returns every selectable mode with its label, tooltip, and
tier (primary / advanced / hidden), derived from `control.ModeCatalog`. The
web dashboard now builds its Strategy buttons from that endpoint instead of a
hand-maintained HTML list, so the UI, the `/api/mode` validator, and the Home
Assistant discovery `select` all derive from the same canonical mode set and
can't drift apart. Adding a new mode to the enum now surfaces everywhere by
construction (a completeness test fails if the catalog omits one).
