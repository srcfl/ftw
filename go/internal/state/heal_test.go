package state

import (
	"database/sql"
	"os"
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
	if _, err := db.Exec(`CREATE TABLE t(x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (1)`); err != nil {
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

// copyFile is a test helper (distinct from production copyFileRaw).
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
	if ok, _ := quickCheck(db); !ok {
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
	copyFile(t, path, path+".snapshot") // known-good copy
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
