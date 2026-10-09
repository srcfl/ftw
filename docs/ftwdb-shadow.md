# FTWDB experiment retired

Core stores time-series history in SQLite. It no longer sends history to
FTWDB, opens its socket or reads its files. Normal release builds have no
DuckDB dependency. A box that once selected DuckDB history uses the separate
[history converter](history-conversion.md). Fresh and older SQLite sites do
not.

Existing `-ftwdb-shadow-socket` and `FTWDB_SHADOW_SOCKET` settings produce a
retirement notice and are ignored. Remove those settings and stop the old
`ftwdb-shadow` service after Core is healthy. Keep the old volume until a
full backup and restore has been verified. Do not remove Docker volumes as
part of this update.

`GET /api/health` reports `history_storage` for the SQLite writer, including
pending, committed and rejected tick counts. Accepted ticks remain in memory
until committed. An error or a rejected tick must not appear as saved
history. FTWDB shadow files are not imported: they contain only five numeric
fields that already exist in SQLite. See
[storage and migration](architecture.md).
