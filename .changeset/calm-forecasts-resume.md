---
"ftw": patch
---

Fix forecast evaluation stalling after restart. Give score writes and retention separate time budgets, skip retention sorting when the archive is within its limits, and resume after the last committed page when a step fails. Refresh calibration from saved scores even when later work needs a retry.
