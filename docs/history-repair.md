# Repair historical household energy

`ftw-history-repair` reconstructs missing household energy from retained,
explicitly selected power series. It writes durable, audited corrections to
`history_load_estimates` on a **separate working copy**, not the source
databases. Core then reads the corrections in daily household totals, chart
aggregates and the household energy ledger. Original samples, other energy
flows, quality masks, forecasts and control remain unchanged.

These are estimates, not exact recovery of lost measurements. A correction
does not make a missing PV measurement known. The API marks household energy
with `load_quality: estimated`, independently of other missing flows. It keeps
derived energy out of `measured.load_wh` and self-powered percentages. A live
day corrected this way reports `load_until_ms` at its last completed minute.

## Prepare a copy and inspect its plan

Build the tool from the same patched source as Core:

```sh
cd go
go build -o ../bin/ftw-history-repair ./cmd/ftw-history-repair
```

For a deployment, stop Core using the installation's actual service manager
before taking the snapshot, and keep it stopped until the corrected history
is installed or the operation is abandoned. A copy prepared while Core keeps
collecting data can be used for analysis, but must not later replace the live
history: that would discard newer measurements. Keep a verified full FTW
backup and the original release for deployment rollback. This tool additionally
preserves checked copies of the affected state/history databases.

Select a completed range, at most 48 hours, with explicit time zones and
minute boundaries. Select every electrical flow or explicitly declare it
absent. Use site-side **AC** battery power; do not combine cabinet DC energy
with AC grid/PV readings. Check the running box's metric catalogue and physical
topology before using these example driver/metric names:

```sh
bin/ftw-history-repair prepare \
  --state /var/lib/ftw/state.db \
  --workdir ./repair-2026-10-19 \
  --since 2026-10-19T00:00:00+02:00 \
  --until 2026-10-19T14:00:00+02:00 \
  --grid pixii:meter_w --pv sma:pv_w \
  --battery pixii:battery_ac_w --ev easee-cloud:ev_w --v2x none \
  --max-gap 15s --reason 'Reconstruct missing Home use intervals, issue 1750'
```

The work directory must be new. `backup/` contains state/history snapshots;
`corrected/` starts as a separate copy. `plan.json` records their hashes,
selected sources, period, reason, per-minute Wh, interpolation evidence,
unsupported minutes and the exact plan SHA-256. Preparing does not apply
corrections. The originals are opened read-only; both copies are checked
through Core's database integrity/generation checks.

The reconstruction uses retained power means at minute resolution or finer.
It integrates between their observed time centres over the same minute for
every flow, using `grid - PV - battery - EV - V2X`. It does not extrapolate
edges, split coarse hourly evidence, clip signed residuals, or replace an
existing estimate. It refuses duplicate source mappings and non-watt inputs.

The default maximum point gap is 15 seconds. Unsupported minutes remain
unknown. If the owner explicitly accepts bounded interpolation, prepare a
**new** plan with a larger bound, at most 180 seconds, and inspect the changed
minutes and `max_interpolated_ms_per_source` values. This is an estimate of
allocation, not proof of constant power or an instrument's delay. Signed
minute residuals are retained; each displayed aggregate must still have
complete supported coverage and a nonnegative resulting balance.

## Apply the reviewed plan to the copy

After reviewing the actual plan and its source selections, supply its exact
hash:

```sh
bin/ftw-history-repair apply --workdir ./repair-2026-10-19 \
  --sha256 REPLACE_WITH_THE_REVIEWED_PLAN_SHA256
```

The operation verifies the backup hashes and unchanged working inputs,
inserts all correction minutes and clears derived daily caches in one SQLite
transaction. A conflict rolls the transaction back. Retrying the same applied
plan does not add energy again. `apply-result.json` reports before/after
household Wh and quality for the **selected range**, not an assumed full day.
If another gap remains unsupported, the complete total stays unknown and the
accepted subtotal remains available.

The correction rows carry the plan hash, backup hash, timestamp, reason and
source mapping in their method metadata. They survive portable backup/restore
and Core restart. Minute energy needed to interpret a corrected quarter is
retained rather than merged away by energy rollup. Original measurement
coverage remains separate from reconstructed household coverage.

## Validate and deploy separately

Use the patched Core to read `corrected/state.db` and `corrected/history.db`
in an isolated test setup without equipment control. Check the selected
period's daily total, ledger provenance, estimate labels and remaining gaps.
The tool never starts a second Core and has no install or restart action.

For the owner's stopped installation, an operator must verify the generation,
release layout, file ownership, backup and absence of newer source data before
installing the corrected history and patched Core together. The same source
state configuration must still bind that history generation. Do not replace
the owner's configuration with a stale test copy. To abandon or undo the
repair before deployment, discard the working copy; the source is unchanged.
For deployment rollback, restore the verified original history snapshot and
matching original release while Core is stopped. Never copy SQLite files
over a running Core or leave mismatched WAL/SHM files beside a restored DB.
