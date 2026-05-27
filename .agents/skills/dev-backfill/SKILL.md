---
name: dev-backfill
description: Seed a local forty-two-watts state.db with synthetic history so the dashboard has something to render during UI / perf work. Use when the user says "backfill history", "seed the dev DB", "fill /api/energy/daily with test data", or "I need fake days to look at". Do NOT invoke against production data — the tool has a safety gate, respect it.
---

# dev-backfill — seed synthetic history into a local state.db

## What this is for

Generates diurnal / weather-varied synthetic `HistoryPoint` rows in
`state.db` so local dev servers have data to render. Each day has a
distinct weather + load profile (cloud, household-busyness, evening-peak
phase), AR(1) coloured noise on PV/load, and rare appliance / cloud-shadow
events. Output looks plausibly like real telemetry without being real.

## Invocation — it's baked into the main binary

The generator ships inside `forty-two-watts` itself, not a separate
command. There is NO `go/cmd/backfill-history` binary.

```bash
# one-shot: seed 30 days at 5s resolution, then exit.
./forty-two-watts -config ./config.local.yaml -backfill 30
```

Flags:

| flag | default | meaning |
|---|---|---|
| `-backfill N`        | `0` (disabled) | days of history to seed; `N>0` triggers the one-shot and exits WITHOUT starting the service |
| `-backfill-step`     | `5s`           | sample interval (accepts any `time.Duration`) |
| `-backfill-seed`     | `0` (random)   | RNG seed for reproducible runs; prints the effective seed on start |
| `-backfill-force`    | `false`        | bypass the prod-safety gate (see below) |

The `-config` flag resolves `state.path` — the backfill writes to the same
`state.db` the service will open on its next normal start.

## Prod-safety gate — DO NOT override casually

Before inserting anything, the tool counts `history_*` rows whose JSON
payload is NOT the synthetic marker `{"source":"backfill"}`. If that count
is > 0 it refuses with:

```
refusing to backfill: found N non-synthetic history rows (looks like
real data). Pass -backfill-force to override, or point at a clean state.db
```

This is the single guard against pointing at the production Pi's database
and corrupting it. **Never** add `-backfill-force` because the tool
complained — investigate the DB path first (`cfg.State.Path`). Only use
force when the user has explicitly confirmed they want to overwrite a DB
that contains a mixture of real and synthetic rows.

## Typical local-dev flow

```bash
# from repo root, with the local dev config pointing to dev-data/state.db:
wsl -e bash -lc 'cd /mnt/c/code/forty-two-watts/go && \
  go build -o /tmp/ftw-dev ./cmd/forty-two-watts'

# seed 30 days (default step = 5s) — exits after seeding.
wsl -e bash -lc 'cd /mnt/c/code/forty-two-watts/dev-data && \
  /tmp/ftw-dev -config ./config.local.yaml -backfill 30'

# then start the service normally (no -backfill flag).
wsl -e bash -lc 'cd /mnt/c/code/forty-two-watts/dev-data && \
  /tmp/ftw-dev -config ./config.local.yaml \
    -web /mnt/c/code/forty-two-watts/web \
    -drivers /mnt/c/code/forty-two-watts/drivers'
```

Open http://localhost:8080, the history cards will have 30 days of bars.

## How expensive is it?

Row count ≈ `days * 86400 / step_seconds`. 30 days at 5 s ≈ 518 k rows.
Batched inserts of 5000 rows per transaction complete in ~1–2 minutes on
`/mnt/c` (WSL 9P) and ~20 s on a native Linux SSD.

## When a reseed gets stuck on the safety gate

If the user has already backfilled once and wants a different seed, the
tool will happily re-run — every existing row carries the synthetic
marker, so the count of non-synthetic rows is 0. No `-force` needed.

If they've been running the app with live drivers pointed at sims in the
same DB, those rows ARE non-synthetic (driver-recorded) and will block
the next backfill. The right move is to wipe `state.db*` before
reseeding, not to pass `-backfill-force`:

```bash
rm -f dev-data/state.db dev-data/state.db-wal dev-data/state.db-shm
```

## Underlying source

- Generator: `go/internal/devtools/backfill.go`
- CLI wiring: `go/cmd/forty-two-watts/main.go` (flags + one-shot dispatch)
- Safety-gate query: `(*state.Store).CountNonSyntheticHistory` in
  `go/internal/state/store.go`
- Marker: JSON string literal `{"source":"backfill"}` stored in the
  `json` column of every row we write.
