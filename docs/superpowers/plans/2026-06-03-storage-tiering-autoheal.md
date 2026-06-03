# Resilient Storage: Tiering + Auto-Heal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make SQLite persistence resilient — isolate disposable (re-fetchable) data in a separate `cache.db`, auto-heal corrupt DBs at boot (rebuild cache / restore state from snapshot), and surface every corruption event to `/api/health`.

**Architecture:** `state.Store` opens two files — `state.db` (precious) and `cache.db` (disposable: `prices` + `forecasts`). A boot-time `PRAGMA quick_check` heals corruption: cache → quarantine + rebuild empty; state → restore from a daily `state.db.snapshot` or quarantine + fresh. Heal events flow to the health endpoint.

**Tech Stack:** Go, `modernc.org/sqlite` (pure-Go, no CGo), existing `database/sql` pool.

**Spec:** `docs/superpowers/specs/2026-06-03-storage-tiering-autoheal-design.md`

**Working dir:** `go/` is the module root. Run all `go` commands from there.

---

## File structure

| File | Responsibility | New? |
|---|---|---|
| `go/internal/state/heal.go` | `HealEvent`, `quickCheck`, `openRaw`, `openChecked` (integrity gate + quarantine/rebuild/restore) | NEW |
| `go/internal/state/heal_test.go` | corruption + heal tests, `corruptAt`/`writePopulated` helpers | NEW |
| `go/internal/state/store.go` | `Store` struct (+`cache`, +`healEvents`), `Open` (two DBs), `Close`, `migrate` (tier-split), route price/forecast methods to `cache`, legacy migration, `HealEvents()` | modify |
| `go/internal/state/cost.go` | route `loadPriceSlotsForRange` + `avgSlotPricesForRange` to `s.cache` | modify |
| `go/internal/state/snapshot_state.go` | `SnapshotState(dir)` — atomic `state.db.snapshot` writer | NEW |
| `go/internal/state/snapshot_state_test.go` | snapshot atomicity + validity test | NEW |
| `go/cmd/forty-two-watts/main.go` | `snapshotLoop` (daily + shutdown); pass `Store` heal events into api `Deps` (already has `State`) | modify |
| `go/internal/api/api_health.go` or `api.go` | `storage` object on `GET /api/health` | modify |
| `go/internal/api/*_test.go` | health storage field test | modify |
| `.changeset/storage-tiering-autoheal.md` | minor changeset | NEW |

---

## Task 1: HealEvent type + quickCheck

**Files:**
- Create: `go/internal/state/heal.go`
- Test: `go/internal/state/heal_test.go`

- [ ] **Step 1: Write the failing test**

```go
package state

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openTmp(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := openRaw(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("openRaw: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestQuickCheckHealthyDB(t *testing.T) {
	db := openTmp(t, "ok.db")
	if _, err := db.Exec(`CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (1);`); err != nil {
		t.Fatal(err)
	}
	ok, err := quickCheck(db)
	if err != nil {
		t.Fatalf("quickCheck err: %v", err)
	}
	if !ok {
		t.Error("healthy DB reported corrupt")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/state/ -run TestQuickCheckHealthyDB -v`
Expected: FAIL — `undefined: openRaw` / `undefined: quickCheck`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package state — heal.go: SQLite integrity gate + corruption recovery.
package state

import (
	"database/sql"
	"fmt"
)

// HealEvent records a corruption recovery action taken at boot, for
// surfacing on /api/health. Zero events means clean boot.
type HealEvent struct {
	Tier   string `json:"tier"`   // "state" | "cache"
	Action string `json:"action"` // "rebuilt" | "restored"
	Detail string `json:"detail"`
	AtMs   int64  `json:"at_ms"`
}

const (
	tierState = "state"
	tierCache = "cache"

	healRebuilt  = "rebuilt"
	healRestored = "restored"
)

// sqlitePragmas is the connection-string suffix shared by every DB we open.
const sqlitePragmas = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

// openRaw opens a SQLite file with the standard pragmas + pool sizing.
// It does NOT run migrations or integrity checks.
func openRaw(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+sqlitePragmas)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	return db, nil
}

// quickCheck runs `PRAGMA quick_check` and reports whether the database is
// structurally sound. A healthy DB returns exactly one row, "ok".
func quickCheck(db *sql.DB) (bool, error) {
	rows, err := db.Query("PRAGMA quick_check")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var first string
	n := 0
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return false, err
		}
		if n == 0 {
			first = s
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return n == 1 && first == "ok", nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/state/ -run TestQuickCheckHealthyDB -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/state/heal.go go/internal/state/heal_test.go
git commit -m "feat(state): quickCheck + openRaw SQLite integrity helpers"
```

---

## Task 2: Detect corruption (quickCheck on a damaged file)

**Files:**
- Modify: `go/internal/state/heal_test.go`

- [ ] **Step 1: Write the failing test + helpers**

```go
import (
	"os"
	// (add to existing imports)
)

// writePopulated creates a multi-page DB at path with `rows` rows, then
// checkpoints the WAL into the main file and closes so the bytes are on
// disk and corruptible.
func writePopulated(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE big(id INTEGER PRIMARY KEY, blob TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(`INSERT INTO big(blob) VALUES (printf('%0512d', ?))`, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
}

// corruptAt overwrites 256 bytes at the given offset with 0xFF, damaging a
// b-tree content page (offset must be >= page size so the header survives
// and the file still opens — quick_check is what detects the damage).
func corruptAt(t *testing.T, path string, offset int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	junk := make([]byte, 256)
	for i := range junk {
		junk[i] = 0xFF
	}
	if _, err := f.WriteAt(junk, offset); err != nil {
		t.Fatal(err)
	}
}

func TestQuickCheckDetectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	writePopulated(t, path, 200) // ~100 KB → many pages
	corruptAt(t, path, 8192)     // page 3 with default 4 KB pages

	db, err := openRaw(path)
	if err != nil {
		t.Fatalf("openRaw should still open a header-intact file: %v", err)
	}
	defer db.Close()
	ok, err := quickCheck(db)
	if err != nil {
		// some corruption surfaces as a query error rather than rows — also "not ok"
		ok = false
	}
	if ok {
		t.Error("corrupted DB reported healthy")
	}
}
```

- [ ] **Step 2: Run test to verify it fails (or reveals helper bugs)**

Run: `go test ./internal/state/ -run TestQuickCheckDetectsCorruption -v`
Expected: Initially may need iteration on the corruption offset. The test
passes once `quickCheck` returns `ok=false` for the damaged file. If it
reports healthy, increase `rows` (more pages) or change offset. This is a
real RED→GREEN on the helper, not production code.

- [ ] **Step 3: (no production code — helpers only)**

The production `quickCheck` already exists. This task locks in reproducible
corruption simulation used by later tasks.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/state/ -run TestQuickCheck -v`
Expected: both quickCheck tests PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/state/heal_test.go
git commit -m "test(state): reproducible SQLite corruption + detection"
```

---

## Task 3: openChecked — heal a single DB file

**Files:**
- Modify: `go/internal/state/heal.go`
- Test: `go/internal/state/heal_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestOpenCheckedCleanNoEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clean.db")
	writePopulated(t, path, 10)
	db, ev, err := openChecked(path, tierCache, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ev != nil {
		t.Errorf("clean open produced heal event: %+v", ev)
	}
}

func TestOpenCheckedCacheRebuildsOnCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	writePopulated(t, path, 200)
	corruptAt(t, path, 8192)

	db, ev, err := openChecked(path, tierCache, 1717430000000)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ev == nil || ev.Action != healRebuilt || ev.Tier != tierCache {
		t.Fatalf("want rebuilt/cache event, got %+v", ev)
	}
	// fresh DB: quick_check clean, the old table is gone
	ok, _ := quickCheck(db)
	if !ok {
		t.Error("rebuilt cache is not healthy")
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.db.corrupt-1717430000000")); err != nil {
		t.Errorf("corrupt file not quarantined: %v", err)
	}
}

func TestOpenCheckedStateRestoresFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	writePopulated(t, path, 200)
	// snapshot = a known-good copy
	snap := path + ".snapshot"
	copyFile(t, path, snap)
	corruptAt(t, path, 8192)

	db, ev, err := openChecked(path, tierState, 42)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ev == nil || ev.Action != healRestored {
		t.Fatalf("want restored event, got %+v", ev)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM big`).Scan(&n); err != nil {
		t.Fatalf("restored DB missing data: %v", err)
	}
	if n != 200 {
		t.Errorf("restored row count = %d, want 200", n)
	}
}

func TestOpenCheckedStateFreshWhenNoSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	writePopulated(t, path, 200)
	corruptAt(t, path, 8192)

	db, ev, err := openChecked(path, tierState, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ev == nil || ev.Action != healRebuilt || ev.Tier != tierState {
		t.Fatalf("want rebuilt/state event, got %+v", ev)
	}
}

// copyFile is a test helper.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/state/ -run TestOpenChecked -v`
Expected: FAIL — `undefined: openChecked`.

- [ ] **Step 3: Implement openChecked + quarantine/restore helpers**

```go
// (add to heal.go)
import (
	"errors"
	"io"
	"log/slog"
	"os"
)

// openChecked opens path, verifies integrity, and heals on corruption.
// Returns the live DB, an optional HealEvent (nil = clean), and an error
// only when even the fresh fallback fails.
//
//   - tierCache: corrupt → quarantine + rebuild empty (data re-fetchable).
//   - tierState: corrupt → restore from "<path>.snapshot" if valid,
//     else quarantine + fresh.
func openChecked(path, tier string, nowMs int64) (*sql.DB, *HealEvent, error) {
	db, err := openRaw(path)
	if err == nil {
		ok, qerr := quickCheck(db)
		if qerr == nil && ok {
			return db, nil, nil // clean
		}
		db.Close()
	}

	// Corruption (open error, query error, or quick_check != ok).
	slog.Warn("state: database corrupt, healing", "path", path, "tier", tier)

	if tier == tierState {
		snap := path + ".snapshot"
		if snapshotUsable(snap) {
			if err := quarantine(path, nowMs); err != nil {
				return nil, nil, err
			}
			if err := copyFileRaw(snap, path); err != nil {
				return nil, nil, fmt.Errorf("restore from snapshot: %w", err)
			}
			db, err := openRaw(path)
			if err != nil {
				return nil, nil, err
			}
			ev := &HealEvent{Tier: tier, Action: healRestored, AtMs: nowMs,
				Detail: "state.db was corrupt — restored from last snapshot"}
			return db, ev, nil
		}
	}

	// Rebuild empty (cache always; state only when no usable snapshot).
	if err := quarantine(path, nowMs); err != nil {
		return nil, nil, err
	}
	db, err = openRaw(path)
	if err != nil {
		return nil, nil, err
	}
	detail := "cache.db was corrupt — rebuilt empty, re-fetching"
	if tier == tierState {
		detail = "state.db was corrupt and no snapshot existed — started fresh (history/models lost)"
	}
	ev := &HealEvent{Tier: tier, Action: healRebuilt, AtMs: nowMs, Detail: detail}
	return db, ev, nil
}

// quarantine renames the corrupt DB and its WAL/shm sidecars out of the way.
func quarantine(path string, nowMs int64) error {
	suffix := fmt.Sprintf(".corrupt-%d", nowMs)
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.Rename(p, p+suffix); err != nil {
			return fmt.Errorf("quarantine %s: %w", p, err)
		}
	}
	return nil
}

// snapshotUsable reports whether a snapshot file exists and passes quick_check.
func snapshotUsable(snap string) bool {
	if _, err := os.Stat(snap); err != nil {
		return false
	}
	db, err := openRaw(snap)
	if err != nil {
		return false
	}
	defer db.Close()
	ok, err := quickCheck(db)
	return err == nil && ok
}

// copyFileRaw copies src to dst (dst must not exist or is overwritten).
func copyFileRaw(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/state/ -run TestOpenChecked -v`
Expected: all four PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/state/heal.go go/internal/state/heal_test.go
git commit -m "feat(state): openChecked — quarantine/rebuild/restore on corruption"
```

---

## Task 4: Wire Open to two DBs + tier-split schema + routing

**Files:**
- Modify: `go/internal/state/store.go` (struct, `Open`, `Close`, `migrate`, `SavePrices`/`LoadPrices`/`SaveForecasts`/`LoadForecasts`)
- Modify: `go/internal/state/cost.go` (`loadPriceSlotsForRange`, `avgSlotPricesForRange`)
- Test: `go/internal/state/heal_test.go` (routing test)

- [ ] **Step 1: Write the failing test**

```go
func TestPricesRouteToCacheSurviveStateCorruption(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	// cache.db must exist alongside state.db
	if _, err := os.Stat(filepath.Join(dir, "cache.db")); err != nil {
		t.Fatalf("cache.db not created: %v", err)
	}
	// Prices save + load through the cache handle
	if err := st.SavePrices([]PricePoint{{Zone: "SE3", SlotTsMs: 1000, SlotLenMin: 60, SpotOreKwh: 50, TotalOreKwh: 60, Source: "test", FetchedAtMs: 1}}); err != nil {
		t.Fatalf("SavePrices: %v", err)
	}
	got, err := st.LoadPrices("SE3", 0, 2000)
	if err != nil || len(got) != 1 {
		t.Fatalf("LoadPrices got %d (%v)", len(got), err)
	}
	st.Close()
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/state/ -run TestPricesRoute -v`
Expected: FAIL — `cache.db not created` (Open still single-DB).

- [ ] **Step 3: Implement the split**

In `store.go`, change the struct and `Open`/`Close`:

```go
type Store struct {
	db    *sql.DB // precious — state.db
	cache *sql.DB // disposable — cache.db (prices, forecasts)
	ts    *internCache

	healEvents []HealEvent
}

func Open(path string) (*Store, error) {
	nowMs := time.Now().UnixMilli()
	cachePath := filepath.Join(filepath.Dir(path), "cache.db")

	db, stEv, err := openChecked(path, tierState, nowMs)
	if err != nil {
		return nil, err
	}
	cache, caEv, err := openChecked(cachePath, tierCache, nowMs)
	if err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db, cache: cache, ts: newInternCache()}
	for _, ev := range []*HealEvent{stEv, caEv} {
		if ev != nil {
			s.healEvents = append(s.healEvents, *ev)
		}
	}
	if err := s.migrate(); err != nil {
		db.Close()
		cache.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.cache != nil {
		err = s.cache.Close()
	}
	if s.db != nil {
		if e := s.db.Close(); e != nil {
			err = e
		}
	}
	return err
}

// HealEvents returns corruption-recovery events from this boot (nil = clean).
func (s *Store) HealEvents() []HealEvent {
	if s == nil {
		return nil
	}
	return s.healEvents
}
```

Add imports `path/filepath` and keep `time` in `store.go` (already present).

In `migrate()`, split the schema: run the `prices` and `forecasts`
`CREATE TABLE` statements (currently `store.go:296` and `store.go:310`)
against `s.cache`; run everything else against `s.db`. Concretely, move
those two CREATE strings out of the `s.db` loop into a second loop:

```go
// after the existing s.db migration loop:
cacheSchema := []string{
	`CREATE TABLE IF NOT EXISTS prices ( ... )`,     // move the existing prices DDL here verbatim
	`CREATE TABLE IF NOT EXISTS forecasts ( ... )`,  // move the existing forecasts DDL here verbatim
}
for _, q := range cacheSchema {
	if _, err := s.cache.Exec(q); err != nil {
		return fmt.Errorf("cache migrate: %w", err)
	}
}
```

Route the four methods to `s.cache` — change the receiver field in each
SQL call (`store.go:933 SavePrices`, `:960 LoadPrices`, `:993 SaveForecasts`,
`:1022 LoadForecasts`): replace `s.db.Begin()`/`s.db.Query(...)` with
`s.cache.Begin()`/`s.cache.Query(...)`.

In `cost.go`, change `s.db.Query` → `s.cache.Query` in
`loadPriceSlotsForRange` (`:128`) and `avgSlotPricesForRange` (`:283`).

- [ ] **Step 4: Run to verify it passes (and nothing else broke)**

Run: `go test ./internal/state/ -run TestPricesRoute -v`
Expected: PASS.
Run: `go test ./internal/state/ ./internal/prices/ ./internal/mpc/`
Expected: all PASS (prices/mpc consume Store via the unchanged public API).

- [ ] **Step 5: Commit**

```bash
git add go/internal/state/store.go go/internal/state/cost.go go/internal/state/heal_test.go
git commit -m "feat(state): open state.db + cache.db; route prices/forecasts to cache"
```

---

## Task 5: Legacy migration — move prices/forecasts out of state.db

**Files:**
- Modify: `go/internal/state/store.go` (add `migrateLegacyTierSplit`, call from `Open` after `migrate`)
- Test: `go/internal/state/heal_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestLegacyPricesMigratedToCache(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")

	// Simulate an OLD single-DB install: prices live in state.db.
	{
		db, _ := openRaw(statePath)
		db.Exec(`CREATE TABLE prices (zone TEXT, slot_ts_ms INTEGER, slot_len_min INTEGER,
			spot_ore_kwh REAL, total_ore_kwh REAL, source TEXT, fetched_at_ms INTEGER,
			PRIMARY KEY(zone, slot_ts_ms))`)
		db.Exec(`INSERT INTO prices VALUES ('SE3', 1000, 60, 50, 60, 'old', 1)`)
		db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		db.Close()
	}

	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := st.LoadPrices("SE3", 0, 2000)
	if err != nil || len(got) != 1 || got[0].SpotOreKwh != 50 {
		t.Fatalf("legacy price not migrated to cache: %d rows, err=%v", len(got), err)
	}
	// And the legacy table is gone from state.db.
	var name string
	err = st.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='prices'`).Scan(&name)
	if err == nil {
		t.Error("legacy prices table still present in state.db")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/state/ -run TestLegacyPrices -v`
Expected: FAIL — legacy row not visible via cache-routed LoadPrices.

- [ ] **Step 3: Implement migrateLegacyTierSplit**

```go
// (store.go) call after s.migrate() in Open, before return:
//   if err := s.migrateLegacyTierSplit(); err != nil { ... close ... return }

// migrateLegacyTierSplit moves prices/forecasts rows from a pre-tiering
// state.db into cache.db, then drops them from state.db. Idempotent:
// a no-op once state.db has no such tables. Best-effort — a read failure
// on the (possibly corrupt) source is logged and skipped; the data is
// re-fetchable.
func (s *Store) migrateLegacyTierSplit() error {
	for _, tbl := range []string{"prices", "forecasts"} {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			continue // already migrated / fresh install
		}
		if err != nil {
			return fmt.Errorf("legacy check %s: %w", tbl, err)
		}
		if err := s.copyTableToCache(tbl); err != nil {
			slog.Warn("legacy tier migration: copy failed, skipping (data re-fetchable)",
				"table", tbl, "err", err)
		}
		if _, err := s.db.Exec(`DROP TABLE ` + tbl); err != nil {
			return fmt.Errorf("drop legacy %s: %w", tbl, err)
		}
		slog.Info("migrated legacy table to cache.db", "table", tbl)
	}
	return nil
}

// copyTableToCache streams every row of tbl from state.db into the
// already-created cache.db table via a parameterized INSERT OR IGNORE.
func (s *Store) copyTableToCache(tbl string) error {
	rows, err := s.db.Query(`SELECT * FROM ` + tbl)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	placeholders := make([]string, len(cols))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	insert := fmt.Sprintf(`INSERT OR IGNORE INTO %s (%s) VALUES (%s)`,
		tbl, strings.Join(cols, ","), strings.Join(placeholders, ","))

	tx, err := s.cache.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if _, err := tx.Exec(insert, vals...); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}
```

Ensure `strings` is imported in `store.go` (it is — used by SnapshotTo).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/state/ -run TestLegacyPrices -v`
Expected: PASS.
Run: `go test ./internal/state/`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/state/store.go go/internal/state/heal_test.go
git commit -m "feat(state): migrate legacy prices/forecasts from state.db to cache.db"
```

---

## Task 6: Atomic state.db snapshot writer

**Files:**
- Create: `go/internal/state/snapshot_state.go`
- Test: `go/internal/state/snapshot_state_test.go`

- [ ] **Step 1: Write the failing test**

```go
package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotStateProducesValidCopy(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Put something precious in state.db (devices table exists post-migrate).
	if _, err := st.db.Exec(`INSERT INTO config(key, value) VALUES ('k','v')`); err != nil {
		t.Fatal(err)
	}

	if err := st.SnapshotState(); err != nil {
		t.Fatalf("SnapshotState: %v", err)
	}
	snap := filepath.Join(dir, "state.db.snapshot")
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	db, _ := openRaw(snap)
	defer db.Close()
	ok, err := quickCheck(db)
	if err != nil || !ok {
		t.Errorf("snapshot not healthy: ok=%v err=%v", ok, err)
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM config WHERE key='k'`).Scan(&v); err != nil || v != "v" {
		t.Errorf("snapshot missing precious row: %q err=%v", v, err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/state/ -run TestSnapshotState -v`
Expected: FAIL — `undefined: SnapshotState`.

- [ ] **Step 3: Implement SnapshotState (atomic, reuses SnapshotTo)**

```go
// Package state — snapshot_state.go: periodic recovery snapshot of state.db.
package state

import (
	"fmt"
	"os"
)

// statePath returns the on-disk path of the precious DB. SQLite exposes it
// via `PRAGMA database_list` (the "main" entry).
func (s *Store) statePath() (string, error) {
	rows, err := s.db.Query("PRAGMA database_list")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" {
			return file, nil
		}
	}
	return "", fmt.Errorf("statePath: no main database")
}

// SnapshotState writes a fresh "<state.db>.snapshot" recovery copy, atomically:
// snapshot to a temp file, verify it, then rename over the previous snapshot.
// Reuses SnapshotTo (which already excludes bulky time-series tables).
func (s *Store) SnapshotState() error {
	main, err := s.statePath()
	if err != nil {
		return err
	}
	final := main + ".snapshot"
	tmp := main + ".snapshot.tmp"
	_ = os.Remove(tmp) // SnapshotTo refuses to overwrite

	if err := s.SnapshotTo(tmp); err != nil {
		return fmt.Errorf("snapshot write: %w", err)
	}
	// Verify the temp snapshot before promoting it.
	vdb, err := openRaw(tmp)
	if err != nil {
		return err
	}
	ok, qerr := quickCheck(vdb)
	vdb.Close()
	if qerr != nil || !ok {
		_ = os.Remove(tmp)
		return fmt.Errorf("snapshot failed integrity check")
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("snapshot promote: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/state/ -run TestSnapshotState -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/state/snapshot_state.go go/internal/state/snapshot_state_test.go
git commit -m "feat(state): SnapshotState — atomic recovery snapshot of state.db"
```

---

## Task 7: Snapshot loop in main.go (daily + shutdown)

**Files:**
- Modify: `go/cmd/forty-two-watts/main.go`

- [ ] **Step 1: Locate the existing `rolloffLoop` wiring**

Run: `rg -n "rolloffLoop|go .*Loop\(ctx" go/cmd/forty-two-watts/main.go`
Expected: shows where background loops are started with `ctx` + `st`.

- [ ] **Step 2: Add the snapshot loop function**

```go
// snapshotLoop writes a recovery snapshot of state.db daily and once on
// shutdown. The snapshot is the restore source if state.db corrupts (see
// state.openChecked). cache.db needs none — it's re-fetchable.
func snapshotLoop(ctx context.Context, st *state.Store) {
	// Initial snapshot shortly after boot so a fresh install gets one fast.
	first := time.NewTimer(2 * time.Minute)
	defer first.Stop()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	doSnap := func() {
		if err := st.SnapshotState(); err != nil {
			slog.Warn("state snapshot failed", "err", err)
		} else {
			slog.Info("state snapshot written")
		}
	}
	for {
		select {
		case <-ctx.Done():
			doSnap() // best-effort on graceful shutdown
			return
		case <-first.C:
			doSnap()
		case <-t.C:
			doSnap()
		}
	}
}
```

- [ ] **Step 3: Start it next to the other loops**

After the store is opened and other loops start (near `rolloffLoop`):

```go
go snapshotLoop(ctx, st)
```

- [ ] **Step 4: Verify build + vet**

Run: `go build ./... && go vet ./cmd/forty-two-watts/`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add go/cmd/forty-two-watts/main.go
git commit -m "feat: daily + on-shutdown state.db recovery snapshot loop"
```

---

## Task 8: /api/health storage field

**Files:**
- Modify: health handler (find with `rg -n "handleHealth" go/internal/api/`)
- Test: `go/internal/api/api_test.go` (or the health test file)

- [ ] **Step 1: Write the failing test**

```go
func TestHealthReportsStorageEvents(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	// Force a cache rebuild event: corrupt cache.db before Open.
	st, _ := state.Open(statePath)
	st.Close()
	corruptCacheFile(t, filepath.Join(dir, "cache.db")) // helper: writePopulated+corruptAt equivalent
	st2, _ := state.Open(statePath)
	defer st2.Close()

	srv := New(&Deps{State: st2, Version: "test"})
	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	storage, ok := body["storage"].(map[string]any)
	if !ok {
		t.Fatalf("no storage object in /api/health: %s", w.Body.String())
	}
	if storage["cache"] != "rebuilt" {
		t.Errorf("storage.cache = %v, want rebuilt", storage["cache"])
	}
}
```

(If a corruption helper isn't reachable from the api package, populate +
corrupt `cache.db` inline using `os` + a couple of `INSERT`s through a
throwaway `database/sql` open, mirroring `heal_test.go`.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/api/ -run TestHealthReportsStorage -v`
Expected: FAIL — no `storage` key.

- [ ] **Step 3: Add the storage object to the health handler**

In the health handler, after building the existing response map, add:

```go
// storage: surface DB corruption-recovery events from this boot.
storage := map[string]any{"state": "ok", "cache": "ok"}
if s.deps.State != nil {
	for _, ev := range s.deps.State.HealEvents() {
		storage[ev.Tier] = ev.Action // "rebuilt" | "restored"
		storage["last_event_ms"] = ev.AtMs
		storage["detail"] = ev.Detail
	}
}
resp["storage"] = storage
```

(Match the handler's actual response-map variable name.)

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/api/ -run TestHealthReportsStorage -v`
Expected: PASS.
Run: `go test ./internal/api/`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/api/
git commit -m "feat(api): surface DB corruption-recovery events on /api/health"
```

---

## Task 9: Changeset + operations doc note

**Files:**
- Create: `.changeset/storage-tiering-autoheal.md`
- Modify: `docs/operations.md` (recovery section)

- [ ] **Step 1: Write the changeset**

```markdown
---
"forty-two-watts": minor
---

state: resilient two-tier storage with auto-heal

Disposable, re-fetchable data (spot prices, weather forecasts) now lives in a
separate `cache.db`, isolated from the precious `state.db` (trained models,
energy history, device identity). At boot each database runs `PRAGMA
quick_check`: a corrupt `cache.db` is quarantined and rebuilt empty
(re-fetched within the hour); a corrupt `state.db` is restored from a daily
recovery snapshot, or quarantined and started fresh if none exists. Every
recovery is surfaced on `GET /api/health` under `storage`, so DB corruption
is never a silent, blank-dashboard failure again. Existing installs migrate
automatically — `prices`/`forecasts` move from `state.db` to `cache.db` on
first boot.
```

- [ ] **Step 2: Add an operations recovery note**

In `docs/operations.md`, add a short "Database corruption" subsection: what
`storage.state`/`storage.cache` ≠ `ok` means, where quarantined files land
(`<db>.corrupt-<ms>`), and that `cache.db` is always safe to delete.

- [ ] **Step 3: Commit**

```bash
git add .changeset/storage-tiering-autoheal.md docs/operations.md
git commit -m "docs: changeset + operations note for tiered storage auto-heal"
```

---

## Task 10: Full verification

- [ ] **Step 1: Run the state + consumer suites**

Run: `go test ./internal/state/ ./internal/prices/ ./internal/mpc/ ./internal/api/ ./internal/savings/ -count=1`
Expected: all PASS. (savings + mpc read prices via the Store API — they
must be unaffected by the file split.)

- [ ] **Step 2: vet + build**

Run: `go vet ./... && go build ./...`
Expected: clean.

- [ ] **Step 3: Note on the e2e port conflict**

`make verify` runs the e2e suite which binds `:8080`; on a dev box already
running an instance it fails with `address already in use` — environmental,
not a regression. CI runs it on a clean runner. Verify via the targeted
package runs above + CI.

---

## Self-review notes

- **Spec coverage:** tiering (T4), FX-stays-in-kv (no task — by omission, correct),
  boot integrity + heal (T1–T3), snapshot recovery (T6–T7), health surfacing
  (T8), migration (T5), testing (each task is TDD). Dashboard banner is
  intentionally deferred (the `/api/health` field is the high-leverage piece;
  the banner is a follow-up UI task under `web/` per DESIGN.md).
- **Type consistency:** `HealEvent{Tier,Action,Detail,AtMs}`, `tierState`/
  `tierCache`, `healRebuilt`/`healRestored`, `openChecked`, `openRaw`,
  `quickCheck`, `SnapshotState`, `HealEvents()` used consistently across tasks.
- **Conflict:** PR #414 touches only `migrate()` tail; T4/T5 additions are
  append-only — trivial rebase if it lands first.
