---
"ftw": patch
---

Report the longest step and step count beside each total phase time when a history write or forecast read fails. Archive failures also report connection, preparation, locks, write and commit times before cleanup. This separates one slow operation from many short operations without logging sample values or changing storage behavior.
