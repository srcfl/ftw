// Package state — heal.go: SQLite integrity gate + corruption recovery.
//
// A corrupt state.db used to fail silently and totally: writes errored with
// "database disk image is malformed (11)" while the dashboard just showed
// blank data. This file adds a boot-time integrity check and the recovery
// machinery that heals corruption instead of running broken forever.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// HealEvent records a corruption-recovery action taken at boot, for surfacing
// on /api/health. Zero events means a clean boot.
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
// busy_timeout(5000) lets contenders wait for the WAL lock instead of failing
// SQLITE_BUSY immediately; the small pool (set in openRaw) lets reads run in
// parallel while writers queue safely behind it.
const sqlitePragmas = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

// openRaw opens a SQLite file with the standard pragmas + pool sizing. It does
// NOT run migrations or integrity checks.
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
	return quickCheckContext(context.Background(), db)
}

// quickCheckContext is quickCheck with a cancellable context. The background
// integrity scan (Store.verifyOnce) uses it so a shutdown can interrupt the
// (multi-minute, on a large DB) scan instead of letting db.Close() block on it —
// which would otherwise stop the clean-shutdown marker from being written and
// make the NEXT boot slow. modernc.org/sqlite honours ctx cancellation via
// sqlite3_interrupt, so the in-flight quick_check aborts promptly.
func quickCheckContext(ctx context.Context, db *sql.DB) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
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

// cleanMarkerSuffix names the "verified-good" sidecar file kept next to state.db.
// Its presence means state.db opened + migrated cleanly (or was healed to a good
// state) at some past boot and no background scan has found corruption since, so
// the boot integrity check can be SKIPPED — which matters because `PRAGMA
// quick_check` scans the whole file and takes MINUTES on a multi-GB DB, making
// every restart look hung.
//
// The marker is PERSISTENT, not a clean-shutdown flag: it is armed by Open (after
// a successful open) and survives both clean shutdowns AND crashes — a crash does
// not corrupt a WAL DB, so it must not force a slow re-scan. The only thing that
// removes it is Store.VerifyInBackground finding real corruption, which forces the
// next boot to run the full check + heal. This deliberately decouples fast restarts
// from how the process exited (a clean Close is unreliable to depend on).
const cleanMarkerSuffix = ".clean"

// cleanMarkerPath returns the verified-good marker sidecar for a DB path.
func cleanMarkerPath(dbPath string) string { return dbPath + cleanMarkerSuffix }

// markerPresent reports whether the verified-good marker exists for dbPath. It
// does NOT remove it — the marker persists until corruption is found.
func markerPresent(dbPath string) bool {
	_, err := os.Stat(cleanMarkerPath(dbPath))
	return err == nil
}

// writeCleanMarker arms the verified-good marker for dbPath so the next boot can
// skip the integrity check. Best-effort: on failure the next boot simply runs the
// (correct, slower) check.
func writeCleanMarker(dbPath string) {
	f, err := os.Create(cleanMarkerPath(dbPath))
	if err != nil {
		slog.Warn("state: could not write verified-good marker", "path", dbPath, "err", err)
		return
	}
	_ = f.Close()
}

// openChecked opens path, verifies integrity, and heals on corruption. Returns
// the live DB, an optional HealEvent (nil = clean), and an error only when even
// the fresh fallback fails.
//
//   - tierCache: corrupt → quarantine + rebuild empty (data re-fetchable).
//   - tierState: corrupt → restore from "<path>.snapshot" if valid, else
//     quarantine + fresh.
//
// A present verified-good marker (see cleanMarkerSuffix) short-circuits the
// quick_check so a restart of a large DB is fast. Open re-arms the marker after a
// successful open; only a background verify finding corruption removes it.
func openChecked(path, tier string, nowMs int64) (*sql.DB, *HealEvent, error) {
	db, err := openRaw(path)
	if err == nil {
		// The skip fast path is scoped to the precious state DB: only it grows large
		// enough for quick_check to be slow, and only it has a snapshot to heal from.
		// cache.db is tiny (checked in microseconds) and disposable (rebuilt empty on
		// corruption), so it is ALWAYS checked — no marker, no skip — and its
		// rebuild-on-corruption safety net is never bypassed.
		if tier == tierState && markerPresent(path) {
			slog.Info("state: verified-good marker present, skipping boot integrity check",
				"path", path, "tier", tier)
			return db, nil, nil // verified good at a past boot; trust + skip
		}
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

// quarantine renames the corrupt DB and its WAL/shm sidecars out of the way so
// a fresh file can take their place. Missing sidecars are skipped.
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

// copyFileRaw copies src to dst (dst created/overwritten), flushing to disk.
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
