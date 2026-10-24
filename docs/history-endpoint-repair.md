# Repair Home use and Solar from bounding power

`ftw-history-endpoints` estimates only incomplete retained minutes. It uses
`Wh = (before_W + after_W) / 2 × duration_hours`, keeping the same mean through
a contiguous gap. Home use endpoints are the closest valid retained minute
source-energy means before and after the gap. Solar endpoints are valid
retained ten-second PV means, expressed as positive generation. Two observed
night-time zeros give 0 Wh. Missing endpoints never imply zero.

The endpoint distance is bounded explicitly (default 15 minutes, maximum 30).
This is historical estimation, not recovered measurement. Changes between
the two readings are unknown. A corrected minute replaces that flow's whole
incomplete minute in the read view; its originally recorded part is not added
a second time. Complete minutes and other flows keep their original evidence.

Build Core and the helper from the same source. Native local Core builds may
retain valid SemVer build metadata while using the installed base-release
slot; keep the normal launcher so updates remain available. Prepare the build
and transfer it before a short stopped-Core binary swap. Do not stop Core
while copying or reviewing the database. Never start a second controlling Core.

Take an independent, integrity-checked SQLite online backup of `history.db`
while the live Core continues collecting. Use SQLite's backup API with a read
snapshot, not a filesystem copy of an active WAL database. Keep matching state
configuration and the prior binary for rollback. Leave disk headroom for the
backup and WAL growth. Then prepare a completed minute range, up to 48 hours:

```sh
ftw-history-endpoints prepare --state /var/lib/ftw/state.db \
  --plan ./endpoint-plan.json --since 2026-10-19T00:00:00+02:00 \
  --until 2026-10-19T23:15:00+02:00 --max-gap 20m \
  --reason 'Owner requested endpoint mean, issue 1750'
```

Inspect `minutes` (flow, whole-minute Wh, endpoint times and powers), `skipped`,
source generation and evidence digest. The boundary timestamps are not raw
polls: their retained resolution is part of the method. Choose the smallest
bound that covers the reviewed gaps. An unsupported edge remains unknown.
Apply the actual reviewed hash and verified independent backup:

```sh
ftw-history-endpoints apply --state /var/lib/ftw/state.db \
  --plan ./endpoint-plan.json --sha256 ACTUAL_REVIEWED_SHA256 \
  --backup /private/backup/history-before.db
```

Apply is safe alongside the live collector. It briefly reserves the SQLite
writer, rechecks historical evidence, inserts one audited metadata record and
invalidates derived daily caches in one transaction. It neither replaces a
live database nor changes raw energy/power/quality rows, device configuration,
control or forecasts. New unrelated measurements survive. An identical retry
is idempotent; tampered, stale or conflicting plans fail without partial writes.

Plans are stored in the existing `energy_ledger_meta` namespace
`endpoint-repair-v1:<sha256>`, with backup hash and application time. This
metadata already participates in portable backup/restore. Original minute
energy in affected quarters is protected from rollup so replacements retain
their boundaries. No new database schema or original quality flags are needed.
Home corrections take read precedence over earlier source-power repairs;
the earlier audit remains present.

Daily totals, system ledger buckets and aggregate charts use the corrections.
Solar has `pv_quality: estimated`; inferred Solar is excluded from the
`measured` fallback. Home use remains `load_quality: estimated`. Current totals
end at the latest committed complete minute, with independent `load_until_ms`
and `pv_until_ms`; the unfinished writer tail is not extrapolated. Aggregate
charts carry estimated/not-for-training markers. Raw samples remain unchanged.

Verify both totals, flow quality, ledger provenance, writer progress and native
update availability after application. If a client has cached a closed-day
aggregate, reload it. To undo a plan, stop Core briefly or take a SQLite writer
transaction, remove only its exact `energy_ledger_meta` key and invalidate the
three derived day-cache tables. Keep later measurements; do not overwrite them
with an older whole-database backup.
