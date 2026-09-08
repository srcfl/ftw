---
"ftw": minor
---

Use embedded DuckDB for all time-series reads and writes, including the energy ledger. Keep SQLite for configuration and learned state, and retire the FTWDB shadow process.

Core verifies the import of existing SQLite and Parquet history before starting control. Health separates queued ticks from durable commits. State schema 3 requires a full backup; returning to an older Core requires a verified full restore with the matching version.
