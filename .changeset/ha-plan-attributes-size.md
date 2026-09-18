---
"ftw": patch
---

Keep the Home Assistant Plan sensor's attributes under the recorder's 16 KB limit: a compact, rounded 24-hour schedule in the entity and the full schedule on the `plan_schedule_json` topic. Home Assistant stops discarding the attributes and logging a warning on every plan update.
