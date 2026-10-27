---
"ftw": patch
---

Start Core when history.db is damaged, empty or missing, including on sites that converted beta history. Core moves the old file aside as `history.db.corrupt-<ms>`, starts an empty history and shows the loss in the storage banner and on `/api/health`. Before, Core exited and restarted every 5 seconds, so no driver ran.
