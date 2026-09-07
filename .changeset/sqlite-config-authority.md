---
"ftw": minor
---

Store settings and credentials together in SQLite, with durable commits before applying changes. Import YAML once and retain it as a database locator and recovery export. Remove background YAML reloads. Reject stale Settings forms and preserve the previous live settings on a failed write. Capture current settings in backups and keep forecast learning state unchanged.

Mark the migration as state schema 2 so upgrades take a full backup. Returning to a YAML-only Core requires a matching backup restore.
