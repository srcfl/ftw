package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const liveBackupTimeout = 2 * time.Hour
const backupScratchHeadroom = 64 << 20

var backupDiskAvail = diskAvail

// OfflineBackup reports whether this store is the read-only helper used by
// ftw-backup. Live Core backups keep their 100 ms yield; the helper does not.
func (s *Store) OfflineBackup() bool {
	return s != nil && s.offlineBackup
}

func (s *Store) backupWorkContext() (context.Context, context.CancelFunc) {
	if s.OfflineBackup() {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), liveBackupTimeout)
}

func (s *Store) backupCopyYield(ctx context.Context) func() error {
	if s.OfflineBackup() {
		return nil
	}
	pause := s.backupPause
	if pause == nil {
		pause = pauseMaintenance
	}
	return func() error { return pause(ctx) }
}

// BackupSourceBytes is the on-disk size of state and history files, including
// WAL and SHM. Used to preflight scratch space before a portable export.
func (s *Store) BackupSourceBytes() int64 {
	if s == nil {
		return 0
	}
	var n int64
	for _, p := range []string{s.mainDBPath, s.historyPath} {
		n += fileSizeOrZero(p) + fileSizeOrZero(p+"-wal") + fileSizeOrZero(p+"-shm")
	}
	return n
}

// stateSourceBytes is the on-disk size of the settings database alone,
// including WAL and SHM. The update rollback point copies only this file.
func (s *Store) stateSourceBytes() int64 {
	if s == nil {
		return 0
	}
	return fileSizeOrZero(s.mainDBPath) + fileSizeOrZero(s.mainDBPath+"-wal") + fileSizeOrZero(s.mainDBPath+"-shm")
}

// HistoryInPlace reports whether history lives in its own database file that
// a state rollback leaves untouched.
func (s *Store) HistoryInPlace() bool {
	return s != nil && s.history != nil
}

func fileSizeOrZero(path string) int64 {
	if path == "" {
		return 0
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// backupCopyScratch is raw export plus gzip of that file, which coexist.
func backupCopyScratch(sourceBytes int64) int64 {
	return 2*sourceBytes + backupScratchHeadroom
}

// At verification, the staged database gzip, outer archive, copied database
// gzip and extracted database coexist. Extra files also occupy the archive
// and one temporary Parquet verification file. Do not assume compression.
func BackupArchiveScratch(sourceBytes, extraBytes int64) int64 {
	return 4*sourceBytes + 2*extraBytes + backupScratchHeadroom
}

// EnsureDiskSpace refuses to start a backup when dir cannot hold needed bytes.
// A probe error (including Windows) does not block; the copy still fails if
// the filesystem fills.
func EnsureDiskSpace(dir string, needed int64) error {
	if needed <= 0 {
		return nil
	}
	avail, err := backupDiskAvail(dir)
	if err != nil {
		return nil
	}
	if avail < needed {
		return fmt.Errorf("backup: need %d bytes free in %s for the raw export, compressed archive and verification extract; have %d", needed, dir, avail)
	}
	return nil
}

// Copy one coherent state snapshot without recopying frozen legacy history.
// The selected history database is exported separately. The destination is
// temporary until the complete backup has passed verification and fsync.
func (s *Store) copyStateForBackup(ctx context.Context, path string) error {
	src, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer src.Rollback()
	rows, err := src.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END,name`)
	if err != nil {
		return err
	}
	var items []snapshotSchemaRow
	excluded := map[string]bool{}
	if s.history != nil {
		for _, name := range sqliteHistoryTables {
			excluded[name] = true
		}
	}
	for rows.Next() {
		var item snapshotSchemaRow
		if err := rows.Scan(&item.objType, &item.name, &item.tblName, &item.sqlText); err != nil {
			rows.Close()
			return err
		}
		if !excluded[item.name] && !excluded[item.tblName] {
			items = append(items, item)
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	dst, err := openBackupDestination(path)
	if err != nil {
		return err
	}
	defer dst.Close()
	dst.SetMaxOpenConns(1)
	for _, item := range items {
		if item.objType != "table" {
			continue
		}
		if _, err := dst.ExecContext(ctx, item.sqlText); err != nil {
			return fmt.Errorf("backup create %s: %w", item.name, err)
		}
		if err := copyVerifiedTable(ctx, src, dst, item.name, scanSQLiteBackupTable, s.backupCopyYield(ctx)); err != nil {
			return err
		}
	}
	// Preserve AUTOINCREMENT high-water marks, including IDs deleted earlier.
	var sequences int
	if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='sqlite_sequence'`).Scan(&sequences); err != nil {
		return err
	}
	if sequences > 0 {
		var destinationSequences int
		if err := dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='sqlite_sequence'`).Scan(&destinationSequences); err != nil {
			return err
		}
		if destinationSequences > 0 {
			rows, err := src.QueryContext(ctx, `SELECT name,seq FROM sqlite_sequence`)
			if err != nil {
				return err
			}
			for rows.Next() {
				var name string
				var sequence int64
				if err := rows.Scan(&name, &sequence); err != nil {
					rows.Close()
					return err
				}
				if excluded[name] {
					continue
				}
				if _, err := dst.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name=?`, name); err != nil {
					rows.Close()
					return err
				}
				if _, err := dst.ExecContext(ctx, `INSERT INTO sqlite_sequence VALUES(?,?)`, name, sequence); err != nil {
					rows.Close()
					return err
				}
			}
			err = errors.Join(rows.Err(), rows.Close())
			if err != nil {
				return err
			}
		}
	}
	for _, item := range items {
		if item.objType != "table" {
			if _, err := dst.ExecContext(ctx, item.sqlText); err != nil {
				return fmt.Errorf("backup schema %s: %w", item.name, err)
			}
		}
	}
	return nil
}

// Flush each bounded batch before producing more backup IO. Leaving scratch
// writes unsynced can accumulate dirty pages in the kernel; a later fsync of
// a charging goal then waits for that backlog on the same SD card.
func openBackupDestination(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path+"?_pragma=journal_mode(DELETE)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2048)&_pragma=temp_store(FILE)")
}

func quoteHistoryIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func scanSQLiteBackupTable(ctx context.Context, db historyQueryer, table string, visit func([]any) error) (int64, error) {
	info, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteHistoryIdentifier(table)+`)`)
	if err != nil {
		return 0, err
	}
	type primaryKey struct {
		order int
		name  string
	}
	var keys []primaryKey
	for info.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := info.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			info.Close()
			return 0, err
		}
		if pk > 0 {
			keys = append(keys, primaryKey{pk, name})
		}
	}
	err = errors.Join(info.Err(), info.Close())
	if err != nil {
		return 0, err
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].order < keys[j].order })
	order := []string{"rowid"}
	if len(keys) > 0 {
		order = nil
		for _, key := range keys {
			order = append(order, quoteHistoryIdentifier(key.name))
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT * FROM `+quoteHistoryIdentifier(table)+` ORDER BY `+strings.Join(order, ","))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	var n int64
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return n, err
		}
		if err := visit(values); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
