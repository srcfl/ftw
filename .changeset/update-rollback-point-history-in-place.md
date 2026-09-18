---
"ftw": minor
---

Save the Core update rollback point from the settings database and configuration only. History stays in its own file, which the update does not replace and a rollback leaves in place, so the step is bounded by settings size and no longer waits hours on a full history export that could not meet its deadline on a Raspberry Pi. Every update now takes a rollback point. Going back across a history-format change still needs a full backup made before that update.
