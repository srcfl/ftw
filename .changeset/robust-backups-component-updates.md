---
"ftw": minor
---

Add verified portable full backups with safe automatic-revert restore, make
pre-update rollback points mandatory, and replace the updater dialog with an
Update Center that versions Core, Optimizer, and signed Drivers independently.

Legacy Compose migration now creates and verifies a full backup before any
deployment change, health-gates the paired Core/updater phase, treats Optimizer
as an optional independent phase, and refreshes Driver metadata without
activating code.
