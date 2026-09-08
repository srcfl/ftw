# FTWDB experiment retired

Core now embeds DuckDB for time-series history, metric samples and the energy
ledger. SQLite still stores configuration. Core no longer sends history to
FTWDB, opens its socket or reads its files.

Existing `-ftwdb-shadow-socket` and `FTWDB_SHADOW_SOCKET` settings produce a
retirement notice. Remove those settings and stop the old `ftwdb-shadow`
service after the new Core has imported history and passed its health checks.
Keep the old volume and test receipts until you have verified a full backup
and restore. Do not remove Docker volumes as part of this update.

`GET /api/health` reports `history_storage.engine = "duckdb"` and the primary
writer's pending, committed and rejected tick counts. Accepted ticks remain
in memory until committed. An error or a rejected tick must not appear as
saved history.

The new startup path reads legacy history from SQLite and daily sample
Parquet files. It does not import the FTWDB shadow files: those contain only
five numeric fields that already exist in SQLite. See
[storage and migration](architecture.md) for the data and backup rules.
