# FTWDB beta candidate

The optional FTWDB sidecar copies five numeric fields from successful live
SQLite history writes: grid power, PV power, battery power, house load and
battery state of charge. Watts keep the site sign convention; SoC stays a
0–1 fraction. SQLite and Parquet still serve history. Config, forecasts, learned
models, schedules and control continue to use their current stores.

This is a bounded session recording. It does not copy old data, driver samples,
SQL imports, retention deletes, the energy ledger or forecast archives. It is
not a complete replica or a backup. Each Core start has a new session ID. On a
normal shutdown or update, Core stops hardware before draining pending memory
work within a two-second I/O budget. An absent or failed sidecar, an exhausted
budget or an abrupt exit can still leave gaps; SQLite keeps the source data.
The shutdown log records acknowledged, dropped and still unconfirmed ticks.

## Enable on a beta test box

Use a Core beta that contains this integration. From the FTW checkout:

```sh
docker compose -f docker-compose.yml -f docker-compose.ftwdb-shadow.yml \
  --profile ftwdb-shadow build ftwdb-shadow
docker compose -f docker-compose.yml -f docker-compose.ftwdb-shadow.yml \
  --profile ftwdb-shadow up -d ftw ftwdb-shadow
```

The overlay builds a pinned FTWDB commit. It gives the sidecar its own data
volume, no network, a 256 MiB memory limit and a quarter CPU. Only the private
Unix socket volume is shared with Core. Both processes use UID 100, GID 101.
There is no startup or health dependency from Core to FTWDB.

For a native Linux install, use the pinned
[systemd service example](https://github.com/srcfl/ftwdb/blob/7bbae63532f695b10aca548bf4ee58c6d7ebb3a8/packaging/systemd/ftwdb-shadow.service)
with the same user as Core. That service listens on
`/run/ftwdb-shadow/ftwdb-shadow.sock`. Pass that path with
`-ftwdb-shadow-socket` or `FTWDB_SHADOW_SOCKET` to Core.
An empty value disables the candidate.
This is an install option; household Settings do not expose an experimental
storage switch.

## Read the result

Read `ftwdb_shadow` from `GET /api/health`. Its state is independent of Core
health. Check these fields together:

- `session`, `started_at` and `scope` identify the covered run.
- `offered_ticks`, `queued_ticks`, `pending_ticks` and `dropped_ticks` show
  collection and overload. `gaps` means at least one offered tick was lost.
- `acknowledged_ticks`, `durable_through_sequence` and `last_ack_at` report
  durable sidecar receipts. A sent batch is not yet an acknowledgement.
  `last_ack_ms` and `max_ack_ms` measure the commit request and durable reply.
- `errors` and `last_error` explain a pause. `sidecar` counters have their own
  `sidecar_checked_at`; they can precede the latest batch acknowledgement.

The queue holds at most 256 small numeric records, plus one pending batch of
at most 128. Core tries a batch every 30 seconds. Connect, encode, socket I/O
and retry happen on a separate goroutine with two-second I/O deadlines. A full
queue drops candidate work and increments its counter. It never waits for the
sidecar from a device or control loop.

The sequence follows delivery of committed writes, not measurement time.
Late and same-time live history writes therefore remain distinct. Retries keep
one source ID, sequence, commit ID and the exact encoded bytes. The sidecar uses
always-sync durability. Core also pauses new writes once its reported store
size reaches 512 MiB. The sidecar enforces its own space limits. Store limits
are test budgets, not a claim that shared-disk I/O has no effect on control.

## Stop the experiment

Stop the sidecar, then recreate Core with the normal Compose file:

```sh
docker compose -f docker-compose.yml -f docker-compose.ftwdb-shadow.yml \
  --profile ftwdb-shadow stop ftwdb-shadow
docker compose -f docker-compose.yml up -d --no-deps ftw
```

Keep the candidate data volume when collecting a report. This flow does not
remove SQLite or Parquet. Do not use `down -v` to disable the experiment.

## Validation

The contract workflow pins the same FTWDB commit as the overlay. It compares
shared byte fixtures, sends committed SQLite history to the real Rust process,
drops an acknowledgement, retries exact bytes, kills the process, checks the
reopened durable receipt and reconciles all copied points offline. Tests also
cover absent, unhealthy, non-durable and full sidecars, a full client queue,
failed SQLite commits, late writes, SI units and concurrent status reads.

Before increasing the scope, measure control latency, CPU, RSS, disk growth,
sync rate and gaps on a real box for at least 72 hours. Test disk pressure and
physical power loss on that hardware. Host tests and SIGKILL do not prove SD-card
power-loss behavior. Keep SQLite/Parquet as the source until those results and
an explicit data migration justify a separate replacement change.
