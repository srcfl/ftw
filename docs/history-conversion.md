# Convert DuckDB beta history

Only boxes that selected DuckDB history need this tool. Fresh installations
and older SQLite + Parquet installations start Core without a manual conversion.
The normal Core binary, container and release builds do not include DuckDB.

Use the converter from the same commit as the new Core candidate. Build it on
the target architecture from `go/tools/history-migrate` with `go build -o
ftw-history-migrate .`, or use its Dockerfile from the repository root:

```sh
docker buildx build --platform linux/arm64 \
  -f go/tools/history-migrate/Dockerfile --target export \
  --output type=local,dest=bin/history-migrate-arm64 .
```

The standalone Linux tool requires glibc 2.34 or newer and a compatible C++
runtime. Older Pi installations should use the separate converter container;
do not upgrade the host libraries just to run the tool. Build the same
Dockerfile with `--target runtime --load -t ftw-history-migrate:local`. With
Core stopped, run it against the host data directory:

```sh
docker run --rm --network none \
  -v /path/to/data:/data ftw-history-migrate:local -state /data/state.db
```

Stop Core and any helper that writes its data directory. Preserve a verified
full backup made with the matching old Core, and leave the original state.db,
history.duckdb, its WAL, history-hot.db, its WAL and cold/ in place. Use another
volume for a backup if space is tight; do not extract a large backup alongside
an active control loop on the same SD card. Conversion needs room for both the
originals and the new SQLite file. It never removes the originals.

Run locally on the box, using the real host path rather than a container path:

```sh
./ftw-history-migrate -state /path/to/data/state.db
```

The tool checks the beta generation, fingerprints the sources, copies and
reads back each table, merges any remaining legacy SQLite rows and hot history,
and replays energy observations through the existing ledger rules. Hot catalog
IDs map by name to the selected catalog. Saved device IDs, charging goals,
SoC estimates and model state stay in state.db.

An interruption leaves history.db.converting. Restart the same tool with Core
still stopped. It resumes copied beta tables and skips completed merge phases;
a partial merge phase replays safely. Changed source fingerprints stop the
resume rather than mixing generations. Publication uses a file sync, rename
and directory sync before selecting the new generation. A crash after publication
but before selection checks a synced receipt, source and destination hashes, and
the integrity of the published file before it resumes.

Start the matching new Core only after the tool reports success. Check health,
history writer commits/rejections, device identities, the current charging goal,
retained SoC/session energy, forecast model availability and both recent and old
history. Verify a new portable full backup and restore in a separate directory
before releasing the candidate. Keep the originals until that check completes.

Image-only rollback cannot cross state schema 4. Stop Core and restore the full
backup with its matching old Core version if you need to return. A state-only
snapshot or an old frozen history table is not a complete restore.

Local converter tests use a real DuckDB fixture:

```sh
cd go/tools/history-migrate
go test -v .
```

Core's storage tests run with `cd go && go test ./internal/state
./internal/backup`. No converter or DuckDB installation is needed for those tests.

For the larger backup/restore admission test, including concurrent goal saves,
run `cd go && FTW_STORAGE_ADMISSION=1 go test -v ./internal/state -run
TestStorageAdmission -count=1`. The synthetic fixture contains about 48 MiB of
snapshot JSON. The test logs peak process RSS. Also run it under an enforced
256 MiB process/container limit where the host kernel supports one; an ignored
limit is not evidence of containment. Keep the full test process at Core's
normal IO priority when measuring production goal-save latency. An additional
idle-IO stress run also deprioritizes the goal writes and must be reported
separately.
