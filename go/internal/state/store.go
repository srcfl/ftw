// Package state is SQLite-backed persistent storage for config overrides,
// event log, history snapshots, and battery models.
//
// History uses one table per tier (hot/warm/cold) like the Rust version, but
// the aggregation from hot → warm → cold is pure SQL instead of custom
// bucketing code. See Prune() for the aggregation queries.
package state

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// SchemaVersion identifies the on-disk state format for update rollback.
	// Increase it before a release that cannot safely reopen the same state.db
	// with the prior Core version.
	SchemaVersion = 1
	// HotRetention = 30 days at 5s resolution
	HotRetention = 30 * 24 * time.Hour
	// WarmRetention = 12 months at 15-min buckets
	WarmRetention = 365 * 24 * time.Hour
	// WarmBucketMS = 15-minute bucket size for warm tier
	WarmBucketMS = 15 * 60 * 1000
	// ColdBucketMS = daily bucket size for cold tier
	ColdBucketMS = 24 * 60 * 60 * 1000
)

// Store is the persistent state DB. It wraps two SQLite files:
//   - db:    precious state.db (models, history, devices, config, telemetry)
//   - cache: disposable cache.db (prices, forecasts) — re-fetchable, so it can
//     be quarantined and rebuilt on corruption without losing anything.
//
// See heal.go for the boot-time integrity gate that populates healEvents.
type Store struct {
	db    *sql.DB
	cache *sql.DB
	ts    *internCache

	healEvents []HealEvent

	// mainDBPath is retained so Close can drop a clean-shutdown marker beside the
	// precious state.db (see heal.go) — the marker is what lets the next boot skip
	// the multi-minute integrity check on a large DB. (statePath() the method
	// queries the live DB and can't be used after Close, hence a cached field.)
	// cache.db gets no marker: it is tiny + disposable, so it is always checked.
	mainDBPath string

	// homeLinkFenceMu serializes the append-only emergency revoke markers
	// stored beside state.db. Those markers remain writable when SQLite itself
	// is unavailable and keep a failed revoke closed across restart.
	homeLinkFenceMu     sync.Mutex
	homeLinkFenceRoot   *os.Root
	homeLinkFenceDBName string

	// healMu guards corrupt + verifyCancel. corrupt is set by the background
	// integrity scan when it fails; Close consults it (a corrupt DB must NOT be
	// marked clean). verifyCancel interrupts the in-flight background scan so
	// Close can stop it before closing the DB — otherwise db.Close() blocks on
	// the (multi-minute) scan, the clean marker never gets written, and the next
	// boot is slow. verifyWG lets Close wait for the scan goroutine to unwind.
	healMu       sync.Mutex
	corrupt      bool
	verifyCancel context.CancelFunc
	verifyWG     sync.WaitGroup
}

// Open initializes (or creates) the precious state.db at path plus the
// disposable cache.db beside it, healing either if corrupt (see openChecked),
// then runs all migrations. The connection pragmas (WAL, synchronous(NORMAL),
// foreign_keys, busy_timeout) and a small pool live in openRaw — see heal.go.
func Open(path string) (*Store, error) {
	nowMs := time.Now().UnixMilli()
	cachePath := filepath.Join(filepath.Dir(path), "cache.db")

	// Phase-timed so a slow startup is never a silent hang: each line below
	// reports how long its phase took, so an operator watching the logs can see
	// exactly where a large-DB boot is spending time (integrity gate vs
	// migrations) instead of staring at a frozen "config loaded" line.
	tGate := time.Now()
	db, stEv, err := openChecked(path, tierState, nowMs)
	if err != nil {
		return nil, err
	}
	cache, caEv, err := openChecked(cachePath, tierCache, nowMs)
	if err != nil {
		db.Close()
		return nil, err
	}
	fenceRoot, absolutePath, dbName, err := openHomeLinkFenceRoot(path)
	if err != nil {
		db.Close()
		cache.Close()
		return nil, err
	}
	slog.Info("state: integrity gate complete", "elapsed", time.Since(tGate).Round(time.Millisecond))

	s := &Store{
		db: db, cache: cache, ts: newInternCache(), mainDBPath: absolutePath,
		homeLinkFenceRoot: fenceRoot, homeLinkFenceDBName: dbName,
	}
	for _, ev := range []*HealEvent{stEv, caEv} {
		if ev != nil {
			s.healEvents = append(s.healEvents, *ev)
		}
	}
	tMig := time.Now()
	if err := s.migrate(); err != nil {
		db.Close()
		cache.Close()
		fenceRoot.Close()
		return nil, err
	}
	if err := s.migrateLegacyTierSplit(); err != nil {
		db.Close()
		cache.Close()
		fenceRoot.Close()
		return nil, err
	}
	slog.Info("state: migrations complete", "elapsed", time.Since(tMig).Round(time.Millisecond))

	// Reclaim freelist space left behind by large prunes. No-op on a healthy
	// file; a one-time multi-minute VACUUM on the boot after a bloated DB's
	// first prune. Boot is the only window where no writer can be starved.
	s.CompactIfBloated()

	// Arm the verified-good marker: state.db opened + migrated successfully (it
	// either passed quick_check, was restored from snapshot, or rebuilt fresh), so
	// the next boot can SKIP the (slow on a large DB) integrity check. The marker
	// persists across restarts and crashes — it does NOT depend on a clean Close.
	// Only VerifyInBackground finding corruption removes it, which forces the next
	// boot to run the full check + heal. This is what makes restarts reliably fast.
	writeCleanMarker(path)
	return s, nil
}

// OpenBackupSource opens an existing state.db without integrity healing,
// migrations, cache creation, compaction, or clean-marker writes. It is for an
// offline backup helper that must copy a legacy database byte-for-byte without
// upgrading the source before the matching core is installed.
func OpenBackupSource(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases both DB files. Safe to call multiple times. The verified-good
// marker is NOT managed here — it is armed by Open and removed only by a
// background verify that finds corruption, so fast restarts never depend on this
// running cleanly (a SIGKILLed shutdown still leaves a fast next boot).
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	// Stop the background integrity scan first: db.Close() blocks until every
	// in-flight query finishes, and the scan's quick_check can run for minutes on
	// a large DB. Cancelling it (sqlite3_interrupt) lets the close happen promptly
	// instead of being SIGKILLed mid-close.
	s.healMu.Lock()
	cancel := s.verifyCancel
	s.healMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.verifyWG.Wait()

	var err error
	if s.cache != nil {
		err = s.cache.Close()
	}
	if s.db != nil {
		if e := s.db.Close(); e != nil {
			err = errors.Join(err, e)
		}
	}
	s.homeLinkFenceMu.Lock()
	if s.homeLinkFenceRoot != nil {
		if e := s.homeLinkFenceRoot.Close(); e != nil {
			err = errors.Join(err, e)
		}
		s.homeLinkFenceRoot = nil
	}
	s.homeLinkFenceMu.Unlock()
	return err
}

func openHomeLinkFenceRoot(path string) (*os.Root, string, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve Home Link emergency block root: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve Home Link emergency block parent: %w", err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, "", "", fmt.Errorf("open Home Link emergency block root: %w", err)
	}
	name := filepath.Base(absolute)
	before, err := root.Lstat(name)
	if err != nil {
		root.Close()
		return nil, "", "", fmt.Errorf("inspect Home Link state database: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		root.Close()
		return nil, "", "", errors.New("Home Link state database path is unsafe")
	}
	file, err := root.Open(name)
	if err != nil {
		root.Close()
		return nil, "", "", fmt.Errorf("open Home Link state database: %w", err)
	}
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, pathErr := root.Lstat(name)
	if statErr != nil || pathErr != nil || !os.SameFile(before, opened) ||
		!os.SameFile(opened, after) {
		root.Close()
		return nil, "", "", errors.New("Home Link state database changed while binding its parent")
	}
	if closeErr != nil {
		root.Close()
		return nil, "", "", fmt.Errorf("close Home Link state database: %w", closeErr)
	}
	return root, filepath.Join(parent, name), name, nil
}

// VerifyInBackground runs the integrity check OFF the startup hot path. Call it
// once, after the app is already serving. A clean-shutdown boot skips the
// blocking check (see openChecked) so startup stays fast on a multi-GB DB; this
// is the belt-and-suspenders pass that still catches at-rest corruption (e.g.
// SD-card rot) without making control wait. On failure it logs loudly and arms a
// full check + heal for the next boot: it marks the Store corrupt (so Close
// leaves no clean marker) and removes any existing marker.
func (s *Store) VerifyInBackground() {
	if s == nil || s.db == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.healMu.Lock()
	s.verifyCancel = cancel
	s.healMu.Unlock()
	s.verifyWG.Add(1)
	go func() {
		defer s.verifyWG.Done()
		s.verifyOnce(ctx)
	}()
}

// verifyOnce is the synchronous body of VerifyInBackground (split out so it is
// directly testable). It runs quick_check on state.db and, on failure, arms a
// heal for the next boot. A ctx cancellation (Close interrupting the scan on
// shutdown) is NOT a failure — the check simply didn't finish, so we leave the
// DB's clean status untouched.
func (s *Store) verifyOnce(ctx context.Context) {
	t0 := time.Now()
	ok, err := quickCheckContext(ctx, s.db)
	if err == nil && ok {
		slog.Info("state: background integrity check passed",
			"elapsed", time.Since(t0).Round(time.Millisecond))
		return
	}
	if ctx.Err() != nil {
		slog.Info("state: background integrity check aborted (shutdown)",
			"elapsed", time.Since(t0).Round(time.Millisecond))
		return
	}
	// A malformed image surfaces as an error rather than an "ok != 'ok'" row, so
	// treat ANY non-clean result as corruption: arm a heal for the next boot
	// (don't leave a clean marker). The next boot's openChecked runs the full
	// check and restores from snapshot if the damage is real; a transient error
	// just costs one slower boot.
	slog.Error("state: BACKGROUND INTEGRITY CHECK FAILED — state.db is not clean; "+
		"it will be checked and healed from snapshot on the next restart", "err", err)
	s.healMu.Lock()
	s.corrupt = true
	s.healMu.Unlock()
	if s.mainDBPath != "" {
		_ = os.Remove(cleanMarkerPath(s.mainDBPath))
	}
}

// HealEvents returns the corruption-recovery events from this boot (nil = a
// clean boot). Surfaced on /api/health so DB corruption is never silent.
func (s *Store) HealEvents() []HealEvent {
	if s == nil {
		return nil
	}
	return s.healEvents
}

// SnapshotTo writes a self-contained, defragmented copy of the database
// to dstPath. The source DB remains open for the duration; readers and writers
// continue unimpeded. This compact copy is for automatic corruption recovery;
// it intentionally omits bulky time-series tables. User-visible update
// rollback must use BackupToCompressed instead.
//
// dstPath must not exist — SQLite refuses to overwrite an existing file.
// Safe to call while the Store is serving live traffic.
// snapshotExcludedTables lists tables whose contents are intentionally
// dropped from compact recovery snapshots. They're the bulky time-series stores
// — recoverable from cold parquet roll-off, so excluding them from
// snapshots drops disk + wall-clock cost while retaining the config, planner,
// identity, and model state needed for corruption recovery.
//
// 2026-05-25 measurement on a 1 GB state.db: VACUUM INTO took ~30 s
// and produced a 1 GB snapshot. After this exclusion the same path
// takes ~2 s and produces a ~50 MB snapshot.
var snapshotExcludedTables = map[string]bool{
	"history_hot":  true,
	"history_warm": true,
	"history_cold": true,
	"ts_samples":   true,
	// ~85 kB JSON per replan (measured 485 MB at 30 d retention on a real
	// site) and recoverable from the diagnostics Parquet rolloff — the same
	// rationale as ts_samples. Including it made every snapshot ~470 MB.
	"planner_diagnostics": true,
}

func (s *Store) SnapshotTo(dstPath string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: snapshot on nil store")
	}
	// Single-quote escape for ATTACH DATABASE path literal — bind
	// parameters aren't honoured at the SQL grammar level. The caller
	// constructs dstPath, but defence in depth.
	escaped := strings.ReplaceAll(dstPath, "'", "''")

	// Refuse to overwrite an existing destination. The old VACUUM INTO
	// path errored implicitly; the ATTACH path would happily append
	// into a pre-existing schema, which would silently corrupt a stale
	// snapshot. Caller (createPreUpdateSnapshot) builds a unique
	// timestamped dir per snapshot, so collision is a bug worth
	// surfacing.
	if _, err := os.Stat(dstPath); err == nil {
		return fmt.Errorf("snapshot: destination already exists: %s", dstPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("snapshot: stat dst %s: %w", dstPath, err)
	}

	// Use a single connection so ATTACH state survives across statements.
	// db.Exec acquires a fresh connection each call, which would break
	// the ATTACH binding.
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("snapshot: acquire conn: %w", err)
	}
	defer conn.Close()

	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("ATTACH DATABASE '%s' AS snap", escaped)); err != nil {
		return fmt.Errorf("snapshot: attach %s: %w", dstPath, err)
	}
	// Detach on every exit path so the connection is clean when returned
	// to the pool.
	defer func() { _, _ = conn.ExecContext(ctx, "DETACH DATABASE snap") }()

	// Walk the source schema, replay each essential CREATE on snap, and
	// stream the rows over. sqlite_master.sql carries the original
	// CREATE text — we rewrite the leading identifier so it lands in
	// the attached DB instead of main. Strict-mode tables and WITHOUT
	// ROWID survive unchanged because the substring is purely
	// "TABLE name" → "TABLE snap.name", before any column / option
	// clauses.
	rows, err := conn.QueryContext(ctx,
		`SELECT type, name, tbl_name, sql FROM main.sqlite_master
		 WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		 ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 ELSE 2 END, name`)
	if err != nil {
		return fmt.Errorf("snapshot: list schema: %w", err)
	}
	var items []snapshotSchemaRow
	for rows.Next() {
		var r snapshotSchemaRow
		if err := rows.Scan(&r.objType, &r.name, &r.tblName, &r.sqlText); err != nil {
			_ = rows.Close()
			return fmt.Errorf("snapshot: scan schema: %w", err)
		}
		items = append(items, r)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("snapshot: close schema rows: %w", err)
	}

	for _, r := range items {
		// Skip objects belonging to excluded tables — their CREATE
		// statements would otherwise re-introduce empty placeholder
		// tables on the destination.
		if snapshotExcludedTables[r.name] || snapshotExcludedTables[r.tblName] {
			continue
		}
		snapSQL, err := rewriteCreateForAttachedDB(r.sqlText, r.objType, r.name)
		if err != nil {
			return fmt.Errorf("snapshot: rewrite CREATE for %s: %w", r.name, err)
		}
		if _, err := conn.ExecContext(ctx, snapSQL); err != nil {
			return fmt.Errorf("snapshot: create %s on snap: %w", r.name, err)
		}
	}

	// Copy each essential table's rows over in parent-before-child order.
	// Alphabetic sqlite_master order is not safe when a schema contains FKs.
	copyItems, err := snapshotTableCopyOrder(ctx, conn, items, snapshotExcludedTables)
	if err != nil {
		return fmt.Errorf("snapshot: order tables: %w", err)
	}
	for _, r := range copyItems {
		if r.objType != "table" {
			continue
		}
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO snap.%q SELECT * FROM main.%q", r.name, r.name)); err != nil {
			return fmt.Errorf("snapshot: copy table %s: %w", r.name, err)
		}
	}
	return nil
}

// BackupProgress reports the current phase of a complete database backup.
// TotalBytes is known once SQLite has produced the consistent raw copy.
type BackupProgress struct {
	Phase          string
	CompletedBytes int64
	TotalBytes     int64
}

const (
	BackupPhaseCopying     = "copying_database"
	BackupPhaseCompressing = "compressing_database"
	BackupPhaseSyncing     = "syncing_backup"
)

// BackupToCompressed writes a complete, point-in-time copy of state.db as a
// gzip stream. Unlike SnapshotTo, this is a user-data backup: it includes the
// history and sample tables as well as configuration, models, and identities.
// The self-update rollback flow must use this method; restoring the compact
// recovery snapshot produced by SnapshotTo would intentionally erase recent
// time-series data.
//
// SQLite cannot stream VACUUM INTO, so the complete raw copy is materialised
// next to dstPath, compressed, synced, and removed. dstPath must not exist.
func (s *Store) BackupToCompressed(dstPath string) error {
	return s.BackupToCompressedWithProgress(dstPath, nil)
}

// BackupToCompressedWithProgress is BackupToCompressed with phase and byte
// progress. The callback may take long enough to write a small status file,
// but it must not call back into Store.
func (s *Store) BackupToCompressedWithProgress(dstPath string, report func(BackupProgress)) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: backup on nil store")
	}
	if _, err := os.Stat(dstPath); err == nil {
		return fmt.Errorf("backup: destination already exists: %s", dstPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup: stat dst %s: %w", dstPath, err)
	}

	rawPath := dstPath + ".raw.tmp"
	_ = os.Remove(rawPath)
	defer os.Remove(rawPath)
	reportBackupProgress(report, BackupProgress{Phase: BackupPhaseCopying})
	escaped := strings.ReplaceAll(rawPath, "'", "''")
	if _, err := s.db.Exec(fmt.Sprintf("VACUUM INTO '%s'", escaped)); err != nil {
		return fmt.Errorf("backup to %s: %w", rawPath, err)
	}

	in, err := os.Open(rawPath)
	if err != nil {
		return fmt.Errorf("open backup temp: %w", err)
	}
	defer in.Close()
	rawInfo, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat backup temp: %w", err)
	}
	rawBytes := rawInfo.Size()
	reportBackupProgress(report, BackupProgress{
		Phase:      BackupPhaseCompressing,
		TotalBytes: rawBytes,
	})
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create compressed backup: %w", err)
	}
	committed := false
	defer func() {
		_ = out.Close()
		if !committed {
			_ = os.Remove(dstPath)
		}
	}()

	zw, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("create gzip writer: %w", err)
	}
	progress := &backupProgressReader{
		r:      in,
		total:  rawBytes,
		report: report,
		lastAt: time.Now(),
	}
	if _, err := io.CopyBuffer(zw, progress, make([]byte, 1<<20)); err != nil {
		_ = zw.Close()
		return fmt.Errorf("compress backup: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finish compressed backup: %w", err)
	}
	reportBackupProgress(report, BackupProgress{
		Phase:          BackupPhaseSyncing,
		CompletedBytes: rawBytes,
		TotalBytes:     rawBytes,
	})
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync compressed backup: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close compressed backup: %w", err)
	}
	committed = true
	return nil
}

type backupProgressReader struct {
	r         io.Reader
	total     int64
	completed int64
	report    func(BackupProgress)
	lastAt    time.Time
}

func (r *backupProgressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.completed += int64(n)
	now := time.Now()
	if r.completed == r.total || now.Sub(r.lastAt) >= time.Second {
		reportBackupProgress(r.report, BackupProgress{
			Phase:          BackupPhaseCompressing,
			CompletedBytes: r.completed,
			TotalBytes:     r.total,
		})
		r.lastAt = now
	}
	return n, err
}

func reportBackupProgress(report func(BackupProgress), progress BackupProgress) {
	if report != nil {
		report(progress)
	}
}

type snapshotSchemaRow struct {
	objType string
	name    string
	tblName string
	sqlText string
}

func snapshotTableCopyOrder(ctx context.Context, conn *sql.Conn, items []snapshotSchemaRow, excluded map[string]bool) ([]snapshotSchemaRow, error) {
	tables := make([]snapshotSchemaRow, 0)
	byName := make(map[string]snapshotSchemaRow)
	for _, r := range items {
		if r.objType != "table" || excluded[r.name] {
			continue
		}
		tables = append(tables, r)
		byName[r.name] = r
	}

	deps := make(map[string][]string, len(tables))
	for _, r := range tables {
		rows, err := conn.QueryContext(ctx, "PRAGMA main.foreign_key_list("+sqliteIdent(r.name)+")")
		if err != nil {
			return nil, fmt.Errorf("foreign_key_list %s: %w", r.name, err)
		}
		for rows.Next() {
			var id, seq int
			var parent, from, to, onUpdate, onDelete, match string
			if err := rows.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan foreign_key_list %s: %w", r.name, err)
			}
			if parent != r.name && !excluded[parent] {
				deps[r.name] = append(deps[r.name], parent)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close foreign_key_list %s: %w", r.name, err)
		}
	}

	visiting := make(map[string]bool, len(tables))
	visited := make(map[string]bool, len(tables))
	ordered := make([]snapshotSchemaRow, 0, len(tables))
	var visit func(string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("foreign-key cycle involving %s", name)
		}
		row, ok := byName[name]
		if !ok {
			return nil
		}
		visiting[name] = true
		for _, dep := range deps[name] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		ordered = append(ordered, row)
		return nil
	}
	for _, r := range tables {
		if err := visit(r.name); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func sqliteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// rewriteCreateForAttachedDB rewrites the leading object identifier
// in a CREATE statement so it targets the attached snap database
// instead of main. SQLite's sqlite_master.sql column stores the
// statement verbatim, without an explicit database prefix; we add
// "snap." before the object name without touching any of the column
// definitions or table options that follow.
func rewriteCreateForAttachedDB(stmt, objType, name string) (string, error) {
	// Find the object-type keyword and the name token that follows it.
	// Case-insensitive search keeps the rewrite robust against future
	// migrations that capitalise differently.
	lower := strings.ToLower(stmt)
	kw := strings.ToLower(objType)
	idx := strings.Index(lower, " "+kw+" ")
	if idx < 0 {
		// Try start-of-string in case the prefix shape changes.
		if strings.HasPrefix(lower, kw+" ") {
			idx = -1
		} else {
			return "", fmt.Errorf("cannot locate %q in CREATE statement", objType)
		}
	}
	// idx points to the space before the keyword; skip past the
	// keyword token to land on the column where the identifier starts.
	afterKw := idx + 1 + len(kw) + 1
	if idx < 0 {
		afterKw = len(kw) + 1
	}
	// IF NOT EXISTS clause precedes the name; advance past it.
	rest := stmt[afterKw:]
	restLower := strings.ToLower(rest)
	if strings.HasPrefix(restLower, "if not exists ") {
		afterKw += len("if not exists ")
		rest = stmt[afterKw:]
	}
	// Find the unquoted name in `rest`. SQLite identifiers can be
	// bare, quoted with double quotes, or backticked — sqlite_master
	// almost always returns the bare form for our migrations. Locate
	// the first occurrence of `name` and prefix it with "snap.".
	nameIdx := strings.Index(rest, name)
	if nameIdx < 0 {
		return "", fmt.Errorf("cannot locate name %q in CREATE statement", name)
	}
	rewritten := stmt[:afterKw+nameIdx] + "snap." + stmt[afterKw+nameIdx:]
	return rewritten, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		// config: small string key-value for mode, grid_target etc.
		`CREATE TABLE IF NOT EXISTS config (
			key TEXT PRIMARY KEY NOT NULL,
			value TEXT NOT NULL
		)`,
		// events: operational log, ms-precision key (seconds collided)
		`CREATE TABLE IF NOT EXISTS events (
			ts_ms INTEGER PRIMARY KEY NOT NULL,
			event TEXT NOT NULL
		)`,
		// telemetry snapshots for crash recovery
		`CREATE TABLE IF NOT EXISTS telemetry (
			key TEXT PRIMARY KEY NOT NULL,
			json TEXT NOT NULL
		)`,
		// battery models (JSON-serialized), keyed by driver name
		`CREATE TABLE IF NOT EXISTS battery_models (
			name TEXT PRIMARY KEY NOT NULL,
			json TEXT NOT NULL
		)`,
		// History tiers — hot/warm/cold, all keyed by ms timestamp
		`CREATE TABLE IF NOT EXISTS history_hot (
			ts_ms INTEGER PRIMARY KEY NOT NULL,
			grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL,
			json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS history_warm (
			ts_ms INTEGER PRIMARY KEY NOT NULL,
			grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL,
			json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS history_cold (
			ts_ms INTEGER PRIMARY KEY NOT NULL,
			grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL,
			json TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_hot_ts ON history_hot(ts_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_warm_ts ON history_warm(ts_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_cold_ts ON history_cold(ts_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts_ms DESC)`,

		// NB: the `prices` and `forecasts` tables live in the disposable
		// cache.db, not here — see cacheStmts below.

		// ---- Long-format time-series ("recent" tier, last 14 days) ----
		// Drivers + metrics are interned to integer ids to keep rows small.
		// Composite PK is (driver_id, metric_id, ts) WITHOUT ROWID so storage
		// is clustered by driver+metric — typical access pattern is "give me
		// metric X for driver Y over time range Z".
		`CREATE TABLE IF NOT EXISTS ts_drivers (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE
		)`,
		`CREATE TABLE IF NOT EXISTS ts_metrics (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			unit TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS ts_samples (
			driver_id INTEGER NOT NULL,
			metric_id INTEGER NOT NULL,
			ts_ms     INTEGER NOT NULL,
			value     REAL NOT NULL,
			PRIMARY KEY (driver_id, metric_id, ts_ms)
		) WITHOUT ROWID, STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_ts_samples_ts ON ts_samples(ts_ms)`,

		// ---- Devices: hardware-stable identity for each driver ----
		// device_id resolution priority:
		//   1. make + ":" + serial          (canonical, set via host.set_sn)
		//   2. "mac:" + arp_lookup(host)    (L2-stable for TCP devices)
		//   3. "ep:" + protocol + ":" + endpoint  (last resort)
		// Persisted state (battery_models, etc.) is keyed on device_id, so
		// renaming a driver in config or removing/re-adding it doesn't
		// orphan the trained model.
		`CREATE TABLE IF NOT EXISTS devices (
			device_id     TEXT PRIMARY KEY NOT NULL,
			driver_name   TEXT NOT NULL,
			make          TEXT,
			serial        TEXT,
			mac           TEXT,
			endpoint      TEXT,
			first_seen_ms INTEGER NOT NULL,
			last_seen_ms  INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_name ON devices(driver_name)`,

		// ---- Planner diagnostics (one snapshot per replan) ----
		// Persists the full structured output of mpc.Service.Diagnose() so
		// operators can time-travel: load any past moment and see what the
		// DP saw + what it decided + why. Denormalized total_cost_ore +
		// horizon_slots so the timeline UI can render summary rows without
		// unmarshalling every JSON blob.
		//
		// Retention: DiagnosticsRecentRetention (30 d) in SQLite; older
		// rows roll off to <coldDir>/diagnostics/YYYY/MM/DD.parquet via
		// RolloffDiagnosticsToParquet.
		`CREATE TABLE IF NOT EXISTS planner_diagnostics (
			ts_ms          INTEGER PRIMARY KEY NOT NULL,
			reason         TEXT    NOT NULL,
			zone           TEXT    NOT NULL,
			total_cost_ore REAL    NOT NULL,
			horizon_slots  INTEGER NOT NULL,
			json           TEXT    NOT NULL
		) STRICT`,

		// CalDAV objects + collections for the native in-process CalDAV server
		// (#498). One row per calendar object (.ics),
		// keyed by its full path; `collection` is the parent collection path so
		// listing a calendar is an indexed scan. `data` is the raw iCalendar.
		`CREATE TABLE IF NOT EXISTS caldav_calendars (
			path        TEXT PRIMARY KEY NOT NULL,
			name        TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT ''
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS caldav_objects (
			path        TEXT PRIMARY KEY NOT NULL,
			collection  TEXT NOT NULL,
			etag        TEXT NOT NULL,
			data        TEXT NOT NULL,
			modified_ms INTEGER NOT NULL
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_caldav_objects_collection ON caldav_objects(collection)`,

		// Nova federation: one row per local DER we've provisioned in Nova.
		// Keyed on (device_id, der_type) so a hybrid inverter with multiple
		// DERs (battery + pv + meter on the same device_id) has one row per
		// DER. The Nova-generated der_id is stored purely for diagnostics
		// and future control-topic subscriptions; the publish path uses
		// (hardware_id, der_name) which are client-owned.
		// Notification history — one row per attempted push. Populated by
		// a bus subscriber in main.go (see events.NotificationDispatched)
		// so the notifications service itself stays free of storage logic.
		// Retention is unbounded for now; volumes are small (operators
		// configure a threshold + cooldown, not per-tick events) so a
		// house would take years to accumulate even 100k rows.
		`CREATE TABLE IF NOT EXISTS notification_log (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			ts_ms      INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			driver     TEXT NOT NULL DEFAULT '',
			title      TEXT NOT NULL DEFAULT '',
			body       TEXT NOT NULL DEFAULT '',
			priority   INTEGER NOT NULL DEFAULT 0,
			status     TEXT NOT NULL,
			error      TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_log_ts ON notification_log(ts_ms DESC)`,

		// Independently installed Lua drivers. Content lives on disk; SQLite
		// records activation history and the exact previous artifact used for
		// one-click rollback.
		`CREATE TABLE IF NOT EXISTS driver_repo_installs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo_url TEXT NOT NULL,
			repo_id TEXT NOT NULL,
			driver_id TEXT NOT NULL,
			logical_path TEXT NOT NULL,
			version TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			installed_path TEXT NOT NULL,
			previous_installed_path TEXT NOT NULL DEFAULT '',
			installed_at_ms INTEGER NOT NULL,
			active INTEGER NOT NULL DEFAULT 0
		) STRICT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_driver_repo_artifact
			ON driver_repo_installs(repo_id, driver_id, version, sha256)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_driver_repo_active_path
			ON driver_repo_installs(logical_path) WHERE active = 1`,
		`CREATE TABLE IF NOT EXISTS driver_command_results (
			id TEXT PRIMARY KEY NOT NULL,
			driver_name TEXT NOT NULL,
			command TEXT NOT NULL,
			status TEXT NOT NULL,
			code TEXT NOT NULL,
			completed_at_ms INTEGER NOT NULL,
			result_json TEXT NOT NULL
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_driver_command_results_completed
			ON driver_command_results(completed_at_ms DESC)`,

		// Cross-component update audit. The operation key survives a core
		// container recreation, allowing the new process to finish the event
		// that the old process recorded before handing off to the updater.
		`CREATE TABLE IF NOT EXISTS component_updates (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			operation_key TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL,
			component_id TEXT NOT NULL,
			action TEXT NOT NULL,
			from_version TEXT NOT NULL DEFAULT '',
			to_version TEXT NOT NULL DEFAULT '',
			outcome TEXT NOT NULL,
			message TEXT NOT NULL DEFAULT '',
			started_at_ms INTEGER NOT NULL,
			finished_at_ms INTEGER NOT NULL DEFAULT 0
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_component_updates_component
			ON component_updates(kind, component_id, started_at_ms DESC)`,

		`CREATE TABLE IF NOT EXISTS nova_ders (
			device_id   TEXT NOT NULL,
			der_type    TEXT NOT NULL,
			der_name    TEXT NOT NULL,
			der_id      TEXT NOT NULL,
			synced_ms   INTEGER NOT NULL,
			PRIMARY KEY (device_id, der_type)
		) STRICT`,

		// Home Link passkey verifier state stays local. The credential id and
		// public key are verifier data; no private credential material or
		// pairing secret is stored here.
		`CREATE TABLE IF NOT EXISTS homelink_credentials (
			site_id           TEXT NOT NULL,
			credential_id     BLOB NOT NULL,
			public_key        BLOB NOT NULL,
			sign_count        INTEGER NOT NULL CHECK(sign_count BETWEEN 0 AND 4294967295),
			label             TEXT NOT NULL,
			user_handle       BLOB NOT NULL,
			backup_eligible   INTEGER NOT NULL CHECK(backup_eligible IN (0, 1)),
			backup_state      INTEGER NOT NULL CHECK(backup_state IN (0, 1)),
			status            TEXT NOT NULL CHECK(status IN ('active', 'revoked', 'uncertain')),
			revision          INTEGER NOT NULL CHECK(revision > 0),
			created_at_ms     INTEGER NOT NULL,
			updated_at_ms     INTEGER NOT NULL,
			PRIMARY KEY (site_id, credential_id)
		) WITHOUT ROWID, STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_homelink_credentials_site_status
			ON homelink_credentials(site_id, status)`,
		// A revoke intent is a permanent fail-closed tombstone. It is committed
		// before the credential row changes, so a failed or ambiguous later
		// write cannot make the credential active again after restart.
		`CREATE TABLE IF NOT EXISTS homelink_credential_revocations (
			site_id           TEXT NOT NULL,
			credential_id     BLOB NOT NULL,
			started_at_ms     INTEGER NOT NULL,
			PRIMARY KEY (site_id, credential_id)
		) WITHOUT ROWID, STRICT`,
		// A policy block is a permanent fail-closed tombstone. It is committed
		// before a credential row is marked uncertain, so a later failed or
		// ambiguous write cannot make a cloned credential usable after restart.
		`CREATE TABLE IF NOT EXISTS homelink_credential_policy_blocks (
			site_id           TEXT NOT NULL,
			credential_id     BLOB NOT NULL,
			started_at_ms     INTEGER NOT NULL,
			PRIMARY KEY (site_id, credential_id)
		) WITHOUT ROWID, STRICT`,

		// Persistent daily-energy aggregate cache.
		//
		// 2026-05-25 measurement: /api/energy/daily?days=30 took ~25 s
		// on the live Pi cold-start because the handler did one
		// DailyEnergy SQL call per day and the in-memory cache was
		// empty after every restart. Each call walked history_hot +
		// warm + cold for that day's window — slow on a 1 GB state.db.
		//
		// This table stores the integration result so closed days never
		// have to be re-computed. The handler writes a row on first
		// compute and reads it back forever — days are immutable once
		// past the local-midnight rollover, so cache invalidation is
		// trivially "always valid".
		//
		// Today's row is never persisted (the day is in progress); the
		// handler still computes it on every request. Tomorrow's
		// midnight rollover the previous day's final value lands here
		// once, lazily, on the next /api/energy/daily request.
		`CREATE TABLE IF NOT EXISTS energy_daily (
			day               TEXT PRIMARY KEY,
			import_wh         REAL NOT NULL,
			export_wh         REAL NOT NULL,
			pv_wh             REAL NOT NULL,
			bat_charged_wh    REAL NOT NULL,
			bat_discharged_wh REAL NOT NULL,
			load_wh           REAL NOT NULL,
			computed_at_ms    INTEGER NOT NULL
		) STRICT`,

		// ---- Versioned energy ledger ----
		// Energy is stored as non-negative directional quantities. Asset IDs
		// are derived from stable hardware identity (or the reserved site
		// identity for the inferred household consumer), never config names.
		`CREATE TABLE IF NOT EXISTS energy_ledger_meta (
			key   TEXT PRIMARY KEY NOT NULL,
			value TEXT NOT NULL
		) STRICT`,
		`INSERT OR IGNORE INTO energy_ledger_meta(key, value)
			VALUES ('schema_version', '1')`,
		`CREATE TABLE IF NOT EXISTS energy_assets (
			asset_id       TEXT PRIMARY KEY NOT NULL,
			device_id      TEXT NOT NULL DEFAULT '',
			kind           TEXT NOT NULL,
			label          TEXT NOT NULL DEFAULT '',
			read_only      INTEGER NOT NULL DEFAULT 0 CHECK(read_only IN (0, 1)),
			first_seen_ms  INTEGER NOT NULL,
			last_seen_ms   INTEGER NOT NULL
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_energy_assets_device
			ON energy_assets(device_id, kind)`,
		`CREATE TABLE IF NOT EXISTS energy_ledger_entries (
			schema_version INTEGER NOT NULL,
			asset_id       TEXT NOT NULL,
			flow           TEXT NOT NULL,
			bucket_start_ms INTEGER NOT NULL,
			bucket_len_ms   INTEGER NOT NULL CHECK(bucket_len_ms > 0),
			energy_wh      REAL NOT NULL CHECK(energy_wh >= 0),
			source         TEXT NOT NULL,
			quality        TEXT NOT NULL,
			provenance     TEXT NOT NULL,
			sample_count   INTEGER NOT NULL DEFAULT 1 CHECK(sample_count > 0),
			observed_at_ms INTEGER NOT NULL,
			PRIMARY KEY (
				schema_version, asset_id, flow, bucket_start_ms,
				bucket_len_ms, source, quality, provenance
			)
		) WITHOUT ROWID, STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_energy_ledger_time
			ON energy_ledger_entries(bucket_start_ms, asset_id, flow)`,
		`CREATE TABLE IF NOT EXISTS energy_ledger_cursors (
			asset_id   TEXT NOT NULL,
			flow       TEXT NOT NULL,
			cursor_kind TEXT NOT NULL,
			value      REAL NOT NULL,
			ts_ms      INTEGER NOT NULL,
			PRIMARY KEY(asset_id, flow, cursor_kind)
		) WITHOUT ROWID, STRICT`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migration %q: %w", stmt[:40]+"…", err)
		}
	}
	if err := s.ensureEnergyLedgerVersion(); err != nil {
		return err
	}
	// Disposable tier (cache.db): re-fetchable market + weather data. Kept in a
	// separate file so its corruption (or a deliberate flush) never risks the
	// precious state.db — and recovery is just "rebuild empty + re-fetch".
	cacheStmts := []string{
		// Spot prices — one row per time slot per zone. Slot duration is
		// provider-dependent (NordPool 15-min PTU since late 2025; ENTSOE
		// mixed); slot_len_min tells consumers what each row represents.
		`CREATE TABLE IF NOT EXISTS prices (
			zone TEXT NOT NULL,
			slot_ts_ms INTEGER NOT NULL,
			slot_len_min INTEGER NOT NULL DEFAULT 60,
			spot_ore_kwh REAL NOT NULL,
			total_ore_kwh REAL NOT NULL,
			source TEXT NOT NULL,
			fetched_at_ms INTEGER NOT NULL,
			PRIMARY KEY (zone, slot_ts_ms)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_prices_slot ON prices(slot_ts_ms)`,
		// Weather + PV forecasts — one row per hour.
		`CREATE TABLE IF NOT EXISTS forecasts (
			slot_ts_ms INTEGER PRIMARY KEY,
			slot_len_min INTEGER NOT NULL DEFAULT 60,
			cloud_cover_pct REAL,
			temp_c REAL,
			solar_wm2 REAL,
			pv_w_estimated REAL,
			source TEXT NOT NULL,
			fetched_at_ms INTEGER NOT NULL
		)`,
	}
	for _, stmt := range cacheStmts {
		if _, err := s.cache.Exec(stmt); err != nil {
			return fmt.Errorf("cache migration %q: %w", stmt[:40]+"…", err)
		}
	}
	return nil
}

// migrateLegacyTierSplit moves prices/forecasts rows from a pre-tiering
// state.db into cache.db, then drops them from state.db. Idempotent: a no-op
// once state.db has no such tables. Best-effort on copy — a read failure on the
// (possibly corrupt) source is logged and skipped, since the data is
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

// ---- Config key-value ----

// SaveConfig writes a config k/v. Upserts on conflict.
func (s *Store) SaveConfig(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO config (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// LoadConfig returns the value for key, or ok=false if missing.
func (s *Store) LoadConfig(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		return "", false
	}
	return v, true
}

// ---- Events ----

// RecordEvent appends an event at the current ms timestamp. Collision-safe up to 1 per ms.
func (s *Store) RecordEvent(event string) error {
	ts := time.Now().UnixMilli()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO events (ts_ms, event) VALUES (?, ?)`, ts, event)
	return err
}

// RecentEvents returns the N most recent events (most recent first).
func (s *Store) RecentEvents(limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT ts_ms, event FROM events ORDER BY ts_ms DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.TsMs, &e.Event); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Event is one entry from the events log.
type Event struct {
	TsMs  int64
	Event string
}

// ---- Telemetry snapshots ----

// SaveTelemetry stores the latest known state of one DER key for crash recovery.
func (s *Store) SaveTelemetry(key, json string) error {
	_, err := s.db.Exec(`INSERT INTO telemetry (key, json) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET json = excluded.json`, key, json)
	return err
}

// LoadTelemetry returns the most recent saved JSON blob for a key.
func (s *Store) LoadTelemetry(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT json FROM telemetry WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		return "", false
	}
	return v, true
}

// ---- Battery models ----

// SaveBatteryModel stores the JSON-serialized model state for a driver.
// The storage key is the resolved device_id when known (so renames don't
// orphan trained state); falls back to the driver name during cold-start
// before any device has reported its identity.
func (s *Store) SaveBatteryModel(name, json string) error {
	key := s.batteryModelKey(name)
	_, err := s.db.Exec(`INSERT INTO battery_models (name, json) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET json = excluded.json`, key, json)
	return err
}

// LoadAllBatteryModels returns all stored model states keyed by the
// CURRENT driver_name (looked up via the devices table). Rows whose
// device_id has no matching driver in this config are skipped silently —
// they belong to drivers the operator has removed from the YAML.
//
// Pulls all device rows BEFORE opening the battery_models query. Originally
// required because the pool was capped at 1 connection (overlapping Rows on
// the same goroutine deadlocked); now harmless under the larger pool but
// the pattern stays — pre-resolving lookups before the main scan still
// produces simpler, faster code.
func (s *Store) LoadAllBatteryModels() (map[string]string, error) {
	// Step 1: build device_id → driver_name reverse map.
	rev := make(map[string]string)
	if drows, err := s.db.Query(`SELECT device_id, driver_name FROM devices`); err == nil {
		for drows.Next() {
			var id, n string
			if err := drows.Scan(&id, &n); err == nil {
				rev[id] = n
			}
		}
		drows.Close()
	}
	// Step 2: read battery_models, translating keys via rev.
	rows, err := s.db.Query(`SELECT name, json FROM battery_models`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, js string
		if err := rows.Scan(&name, &js); err != nil {
			return out, err
		}
		if n, ok := rev[name]; ok {
			out[n] = js
		} else if !strings.Contains(name, ":") {
			// Legacy driver-name key — pass through (migration covers this on next tick).
			out[name] = js
		}
	}
	return out, rows.Err()
}

// DeleteBatteryModel removes a stored model (used when resetting).
func (s *Store) DeleteBatteryModel(name string) error {
	key := s.batteryModelKey(name)
	_, err := s.db.Exec(`DELETE FROM battery_models WHERE name = ?`, key)
	return err
}

// batteryModelKey resolves a driver name to its canonical storage key:
// device_id when known, otherwise the raw driver name (legacy / cold
// start before identity has been registered).
func (s *Store) batteryModelKey(driverName string) string {
	if dev := s.LookupDeviceByDriverName(driverName); dev != nil && dev.DeviceID != "" {
		return dev.DeviceID
	}
	return driverName
}

// ---- History tiers ----

// HistoryPoint is one row of the history table.
type HistoryPoint struct {
	TsMs   int64
	GridW  float64
	PVW    float64
	BatW   float64
	LoadW  float64
	BatSoC float64
	JSON   string
}

// RecordHistory inserts a new hot-tier entry.
func (s *Store) RecordHistory(p HistoryPoint) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO history_hot (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON,
	)
	return err
}

// BulkRecordHistory writes many HistoryPoints in a single transaction.
// Used by backfill / migration tooling where per-row implicit-commit
// overhead dominates (SQLite on slow filesystems).
func (s *Store) BulkRecordHistory(pts []HistoryPoint) error {
	if len(pts) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO history_hot (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range pts {
		if _, err := stmt.Exec(p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadHistory returns points from ALL tiers in [sinceMs, untilMs], merged + sorted.
// maxPoints=0 returns every raw row. With a limit, rows are bucket-averaged in
// SQL down to at most maxPoints points: W columns are bucket AVGs, ts_ms and
// json come from the latest row in the bucket (json cannot be averaged; a
// representative sample is the same trade-off Prune makes when tiering).
// Downsampling used to fetch every row into Go and keep every Nth — a month
// view materialized >1M rows per request once the hot tier grew.
func (s *Store) LoadHistory(sinceMs, untilMs int64, maxPoints int) ([]HistoryPoint, error) {
	// Union across all three tiers. Dedupe on ts_ms preferring hot over warm over cold.
	// COALESCE to 0 so NULL columns (from partial aggregations) scan cleanly.
	const tierUnion = `
		WITH all_rows AS (
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json, 0 AS tier FROM history_hot
			WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json, 1 FROM history_warm
			WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json, 2 FROM history_cold
			WHERE ts_ms BETWEEN ? AND ?
		),
		deduped AS (
			SELECT * FROM all_rows
			GROUP BY ts_ms
			HAVING tier = MIN(tier)
		)
	`
	var (
		rows *sql.Rows
		err  error
	)
	if maxPoints > 0 && untilMs >= sinceMs {
		// Ceil so the bucket count never exceeds maxPoints. MAX(ts_ms) is an
		// aggregate, so SQLite's bare-column rule makes the un-aggregated
		// json column come from that same newest row.
		bucketMs := (untilMs - sinceMs + int64(maxPoints)) / int64(maxPoints)
		if bucketMs < 1 {
			bucketMs = 1
		}
		rows, err = s.db.Query(tierUnion+`
			SELECT MAX(ts_ms),
			       AVG(COALESCE(grid_w, 0)), AVG(COALESCE(pv_w, 0)), AVG(COALESCE(bat_w, 0)),
			       AVG(COALESCE(load_w, 0)), AVG(COALESCE(bat_soc, 0)), json
			FROM deduped
			GROUP BY (ts_ms - ?) / ?
			ORDER BY 1 ASC
		`, sinceMs, untilMs, sinceMs, untilMs, sinceMs, untilMs, sinceMs, bucketMs)
	} else {
		rows, err = s.db.Query(tierUnion+`
			SELECT ts_ms,
			       COALESCE(grid_w, 0), COALESCE(pv_w, 0), COALESCE(bat_w, 0),
			       COALESCE(load_w, 0), COALESCE(bat_soc, 0), json
			FROM deduped
			ORDER BY ts_ms ASC
		`, sinceMs, untilMs, sinceMs, untilMs, sinceMs, untilMs)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	all := make([]HistoryPoint, 0)
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.TsMs, &p.GridW, &p.PVW, &p.BatW, &p.LoadW, &p.BatSoC, &p.JSON); err != nil {
			return all, err
		}
		all = append(all, p)
	}
	return all, rows.Err()
}

// DayEnergy is the set of Wh totals over a time range, in site convention:
// Import/Export are grid-boundary W split on sign; PV and LoadWh are always
// positive accumulations; BatCharged/BatDischarged split bat_w on sign.
type DayEnergy struct {
	ImportWh        float64
	ExportWh        float64
	PVWh            float64
	BatChargedWh    float64
	BatDischargedWh float64
	LoadWh          float64
	// Intervals is the number of integration intervals that contributed to
	// the totals — i.e. the count of rows that had a predecessor (prev_ts IS
	// NOT NULL). Zero means there was at most one history row in the range, so
	// nothing could be integrated and the totals are a vacuous 0 rather than a
	// real measurement. Callers use this to tell "no data yet" apart from a
	// genuine zero (mirrors the old `len(pts) > 1` guard).
	Intervals int64
}

// DailyEnergy integrates history W columns over [sinceMs, untilMs] and returns
// Wh totals in a single round-trip. The integration uses the value at the
// LATER sample of each interval over that interval's width
// (W[j] * (ts[j]-ts[j-1])) — i.e. a right-endpoint / right-Riemann sum. This
// matches the previous Go loop in handleEnergyDaily exactly; do not "fix" it
// to a left-endpoint sum. Pushing the sums into SQL avoids shipping ~17k
// hot-tier rows per day back to the application — month-view dashboards got
// slow once hot retention grew.
func (s *Store) DailyEnergy(sinceMs, untilMs int64) (DayEnergy, error) {
	const q = `
		WITH all_rows AS (
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w FROM history_hot  WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w FROM history_warm WHERE ts_ms BETWEEN ? AND ?
			UNION ALL
			SELECT ts_ms, grid_w, pv_w, bat_w, load_w FROM history_cold WHERE ts_ms BETWEEN ? AND ?
		),
		lagged AS (
			SELECT ts_ms,
			       COALESCE(grid_w, 0) AS grid_w,
			       COALESCE(pv_w,   0) AS pv_w,
			       COALESCE(bat_w,  0) AS bat_w,
			       COALESCE(load_w, 0) AS load_w,
			       LAG(ts_ms) OVER (ORDER BY ts_ms) AS prev_ts
			FROM all_rows
		)
		SELECT
			COALESCE(SUM((CASE WHEN grid_w > 0 THEN  grid_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN grid_w < 0 THEN -grid_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((-pv_w) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN bat_w > 0 THEN  bat_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN bat_w < 0 THEN -bat_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM(load_w * (ts_ms - prev_ts)) / 3600000.0, 0),
			COUNT(*)
		FROM lagged
		WHERE prev_ts IS NOT NULL
	`
	var d DayEnergy
	err := s.db.QueryRow(q,
		sinceMs, untilMs,
		sinceMs, untilMs,
		sinceMs, untilMs,
	).Scan(
		&d.ImportWh, &d.ExportWh, &d.PVWh,
		&d.BatChargedWh, &d.BatDischargedWh, &d.LoadWh,
		&d.Intervals,
	)
	if err != nil {
		return DayEnergy{}, err
	}
	return d, nil
}

// LoadDailyEnergy returns the persisted aggregate for `day` (YYYY-MM-DD).
// Second return is false when the day isn't cached yet. The caller
// recomputes via DailyEnergy on miss and writes back via SaveDailyEnergy.
//
// Closed days are immutable, so callers can treat hit-rows as
// authoritative — no TTL, no staleness check needed.
func (s *Store) LoadDailyEnergy(day string) (DayEnergy, bool, error) {
	const q = `
		SELECT import_wh, export_wh, pv_wh, bat_charged_wh, bat_discharged_wh, load_wh
		FROM energy_daily WHERE day = ?
	`
	var d DayEnergy
	err := s.db.QueryRow(q, day).Scan(
		&d.ImportWh, &d.ExportWh, &d.PVWh,
		&d.BatChargedWh, &d.BatDischargedWh, &d.LoadWh,
	)
	if err == sql.ErrNoRows {
		return DayEnergy{}, false, nil
	}
	if err != nil {
		return DayEnergy{}, false, err
	}
	return d, true, nil
}

// SaveDailyEnergy persists `de` for `day`. Upserts on conflict — the
// row is the latest authoritative aggregate. Today's row should NOT be
// persisted via this method (the day is still accumulating); callers
// should gate on "is closed day" before saving.
func (s *Store) SaveDailyEnergy(day string, de DayEnergy) error {
	const q = `
		INSERT INTO energy_daily(
			day, import_wh, export_wh, pv_wh, bat_charged_wh, bat_discharged_wh, load_wh, computed_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day) DO UPDATE SET
			import_wh         = excluded.import_wh,
			export_wh         = excluded.export_wh,
			pv_wh             = excluded.pv_wh,
			bat_charged_wh    = excluded.bat_charged_wh,
			bat_discharged_wh = excluded.bat_discharged_wh,
			load_wh           = excluded.load_wh,
			computed_at_ms    = excluded.computed_at_ms
	`
	_, err := s.db.Exec(q, day,
		de.ImportWh, de.ExportWh, de.PVWh,
		de.BatChargedWh, de.BatDischargedWh, de.LoadWh,
		time.Now().UnixMilli(),
	)
	return err
}

// CountHistoryWithoutMarker returns the number of history rows across all
// three tiers whose JSON payload differs from marker. Developer tooling uses
// this as a safety gate before adding synthetic data to an existing database.
func (s *Store) CountHistoryWithoutMarker(marker string) (int, error) {
	const q = `
		SELECT
			(SELECT COUNT(*) FROM history_hot  WHERE json IS NOT ?) +
			(SELECT COUNT(*) FROM history_warm WHERE json IS NOT ?) +
			(SELECT COUNT(*) FROM history_cold WHERE json IS NOT ?)
	`
	var n int
	if err := s.db.QueryRow(q, marker, marker, marker).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// HistoryCounts returns the number of rows in (hot, warm, cold) tiers.
func (s *Store) HistoryCounts() (hot, warm, cold int, err error) {
	row := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM history_hot),
		(SELECT COUNT(*) FROM history_warm),
		(SELECT COUNT(*) FROM history_cold)`)
	err = row.Scan(&hot, &warm, &cold)
	return
}

// pruneChunkSpanMS bounds how much history one prune transaction may age.
// The write lock is held per chunk, not for the whole backlog: a DB that
// missed months of pruning is worked off in ~24 h bites of ~40 k rows
// (sub-second each) instead of one transaction that starves every writer.
// 2026-07-16 incident: the first prune of a 93-day backlog held the write
// lock for 4+ hours and every control-loop tick failed with SQLITE_BUSY.
// Var, not const, so tests can force multiple chunks with small data.
var pruneChunkSpanMS = int64(24 * 60 * 60 * 1000)

// Prune ages old hot rows into warm buckets, old warm into cold daily buckets.
// Chunked and linear: each chunk is one short transaction whose boundaries are
// aligned DOWN to whole buckets, so a bucket is always aggregated from its
// complete row set (a cutoff mid-bucket would otherwise INSERT OR REPLACE the
// bucket twice, each time from a partial slice, keeping only the second).
// Idempotent; safe to call often.
func (s *Store) Prune(ctx context.Context) error {
	nowMs := time.Now().UnixMilli()
	t0 := time.Now()

	// hot → warm (15-min buckets). The bare json column rides along with
	// MAX(ts_ms): SQLite's bare-column rule picks it from the newest row of
	// each bucket in the same linear pass. The previous correlated subquery
	// re-scanned history_hot once per bucket — O(buckets × rows).
	hotAged, hotChunks, err := s.pruneTier(ctx, "history_hot", "history_warm",
		nowMs-HotRetention.Milliseconds(), WarmBucketMS)
	if err != nil {
		return fmt.Errorf("hot→warm: %w", err)
	}

	// warm → cold (1-day buckets)
	warmAged, warmChunks, err := s.pruneTier(ctx, "history_warm", "history_cold",
		nowMs-WarmRetention.Milliseconds(), ColdBucketMS)
	if err != nil {
		return fmt.Errorf("warm→cold: %w", err)
	}

	// Maintenance must never be silent — the 4-hour lock above was invisible
	// precisely because a running/finished prune logged nothing.
	if hotAged > 0 || warmAged > 0 {
		slog.Info("state: history prune complete",
			"hot_rows_aged", hotAged, "warm_rows_aged", warmAged,
			"chunks", hotChunks+warmChunks,
			"elapsed", time.Since(t0).Round(time.Millisecond))
	}
	return nil
}

// pruneTier ages rows older than cutoffMs from src into bucketMs-wide averaged
// buckets in dst, in bounded per-chunk transactions. Returns rows aged and
// chunks used. Table names are compile-time constants at every call site.
func (s *Store) pruneTier(ctx context.Context, src, dst string, cutoffMs, bucketMs int64) (aged int64, chunks int, err error) {
	// Only age complete buckets: align the cutoff down to a bucket boundary.
	cutoffMs = (cutoffMs / bucketMs) * bucketMs

	for {
		if err := ctx.Err(); err != nil {
			return aged, chunks, err
		}
		var minTs sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT MIN(ts_ms) FROM `+src).Scan(&minTs); err != nil {
			return aged, chunks, err
		}
		if !minTs.Valid || minTs.Int64 >= cutoffMs {
			return aged, chunks, nil
		}
		// Chunk upper bound: at most pruneChunkSpanMS of rows, never past the
		// cutoff, always on a bucket boundary.
		chunkEnd := minTs.Int64 + pruneChunkSpanMS
		if chunkEnd > cutoffMs {
			chunkEnd = cutoffMs
		}
		chunkEnd = (chunkEnd / bucketMs) * bucketMs
		if chunkEnd <= minTs.Int64 {
			chunkEnd = (minTs.Int64/bucketMs + 1) * bucketMs
			if chunkEnd > cutoffMs {
				chunkEnd = cutoffMs
			}
		}

		n, err := s.pruneChunk(ctx, src, dst, minTs.Int64, chunkEnd, bucketMs)
		if err != nil {
			return aged, chunks, err
		}
		aged += n
		chunks++

		// Yield between chunks. Short transactions alone are not enough:
		// SQLite's busy handler retries with backoff and no fairness, so a
		// back-to-back chunk loop re-acquires the lock before any waiting
		// writer wins its retry — observed in production as tick-persistence
		// SQLITE_BUSY all through a 93-day backlog migration even with ~1 s
		// chunks. A real pause guarantees every waiter a window.
		select {
		case <-ctx.Done():
			return aged, chunks, ctx.Err()
		case <-time.After(pruneChunkPause):
		}
	}
}

// pruneChunkPause is the writer-fairness gap between prune chunks. Var so
// tests can shrink it.
var pruneChunkPause = 250 * time.Millisecond

// pruneChunk aggregates+deletes src rows in [fromMs, toMs) in one short
// transaction.
func (s *Store) pruneChunk(ctx context.Context, src, dst string, fromMs, toMs, bucketMs int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Bare-column rule: exactly one MAX() aggregate in the grouped inner
	// query makes the un-aggregated json column come from that newest row.
	q := fmt.Sprintf(`
		INSERT OR REPLACE INTO %s (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
		SELECT b_ts, a_grid, a_pv, a_bat, a_load, a_soc, json FROM (
			SELECT (ts_ms / %d) * %d + %d AS b_ts,
			       AVG(grid_w) AS a_grid, AVG(pv_w) AS a_pv, AVG(bat_w) AS a_bat,
			       AVG(load_w) AS a_load, AVG(bat_soc) AS a_soc,
			       json, MAX(ts_ms) AS newest
			FROM %s
			WHERE ts_ms >= ? AND ts_ms < ?
			GROUP BY ts_ms / %d
		)`, dst, bucketMs, bucketMs, bucketMs/2, src, bucketMs)
	if _, err := tx.ExecContext(ctx, q, fromMs, toMs); err != nil {
		return 0, fmt.Errorf("aggregate: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM `+src+` WHERE ts_ms >= ? AND ts_ms < ?`, fromMs, toMs)
	if err != nil {
		return 0, fmt.Errorf("delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// ---- Prices ----

// PricePoint is one time-slot's spot price row. Slot length varies by source:
// Sourceful and NordPool/elprisetjustnu are 15 min in Nordic zones; direct
// ENTSOE varies by zone. Consumers should honor SlotLenMin when plotting or
// aggregating.
type PricePoint struct {
	Zone        string  `json:"zone"`
	SlotTsMs    int64   `json:"slot_ts_ms"`
	SlotLenMin  int     `json:"slot_len_min"`
	SpotOreKwh  float64 `json:"spot_ore_kwh"`
	TotalOreKwh float64 `json:"total_ore_kwh"`
	Source      string  `json:"source"`
	FetchedAtMs int64   `json:"fetched_at_ms"`
}

// SavePrices upserts a batch of price rows (slot duration per-row).
func (s *Store) SavePrices(pts []PricePoint) error {
	if len(pts) == 0 {
		return nil
	}
	tx, err := s.cache.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO prices
		(zone, slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh, source, fetched_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (zone, slot_ts_ms) DO UPDATE SET
			slot_len_min = excluded.slot_len_min,
			spot_ore_kwh = excluded.spot_ore_kwh,
			total_ore_kwh = excluded.total_ore_kwh,
			source = excluded.source,
			fetched_at_ms = excluded.fetched_at_ms`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range pts {
		slot := p.SlotLenMin
		if slot <= 0 {
			slot = 60
		}
		if _, err := stmt.Exec(p.Zone, p.SlotTsMs, slot, p.SpotOreKwh, p.TotalOreKwh, p.Source, p.FetchedAtMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadPrices returns prices for zone in [sinceMs, untilMs], ordered ascending.
func (s *Store) LoadPrices(zone string, sinceMs, untilMs int64) ([]PricePoint, error) {
	rows, err := s.cache.Query(`SELECT zone, slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh, source, fetched_at_ms
		FROM prices
		WHERE zone = ? AND slot_ts_ms BETWEEN ? AND ?
		ORDER BY slot_ts_ms ASC`, zone, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PricePoint{}
	for rows.Next() {
		var p PricePoint
		if err := rows.Scan(&p.Zone, &p.SlotTsMs, &p.SlotLenMin, &p.SpotOreKwh, &p.TotalOreKwh, &p.Source, &p.FetchedAtMs); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- Forecasts ----

// ForecastPoint is one slot's weather + derived PV estimate.
type ForecastPoint struct {
	SlotTsMs      int64    `json:"slot_ts_ms"`
	SlotLenMin    int      `json:"slot_len_min"`
	CloudCoverPct *float64 `json:"cloud_cover_pct,omitempty"`
	TempC         *float64 `json:"temp_c,omitempty"`
	SolarWm2      *float64 `json:"solar_wm2,omitempty"`
	PVWEstimated  *float64 `json:"pv_w_estimated,omitempty"`
	Source        string   `json:"source"`
	FetchedAtMs   int64    `json:"fetched_at_ms"`
}

// SaveForecasts upserts a batch of forecast rows.
func (s *Store) SaveForecasts(pts []ForecastPoint) error {
	if len(pts) == 0 {
		return nil
	}
	tx, err := s.cache.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO forecasts
		(slot_ts_ms, slot_len_min, cloud_cover_pct, temp_c, solar_wm2, pv_w_estimated, source, fetched_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (slot_ts_ms) DO UPDATE SET
			slot_len_min = excluded.slot_len_min,
			cloud_cover_pct = excluded.cloud_cover_pct,
			temp_c = excluded.temp_c,
			solar_wm2 = excluded.solar_wm2,
			pv_w_estimated = excluded.pv_w_estimated,
			source = excluded.source,
			fetched_at_ms = excluded.fetched_at_ms`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range pts {
		slot := p.SlotLenMin
		if slot <= 0 {
			slot = 60
		}
		if _, err := stmt.Exec(p.SlotTsMs, slot, p.CloudCoverPct, p.TempC, p.SolarWm2, p.PVWEstimated, p.Source, p.FetchedAtMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadForecasts returns forecasts in [sinceMs, untilMs], ordered ascending.
func (s *Store) LoadForecasts(sinceMs, untilMs int64) ([]ForecastPoint, error) {
	rows, err := s.cache.Query(`SELECT slot_ts_ms, slot_len_min, cloud_cover_pct, temp_c, solar_wm2, pv_w_estimated, source, fetched_at_ms
		FROM forecasts
		WHERE slot_ts_ms BETWEEN ? AND ?
		ORDER BY slot_ts_ms ASC`, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ForecastPoint{}
	for rows.Next() {
		var p ForecastPoint
		if err := rows.Scan(&p.SlotTsMs, &p.SlotLenMin, &p.CloudCoverPct, &p.TempC, &p.SolarWm2, &p.PVWEstimated, &p.Source, &p.FetchedAtMs); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
