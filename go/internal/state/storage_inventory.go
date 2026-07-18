package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SQLiteFileInventory is a read-only accounting snapshot for one SQLite file.
// LivePages means pages not present on SQLite's freelist; it does not attempt
// to estimate unused bytes within otherwise-live pages.
type SQLiteFileInventory struct {
	PageSizeBytes  int64 `json:"page_size_bytes"`
	AllocatedPages int64 `json:"allocated_pages"`
	LivePages      int64 `json:"live_pages"`
	FreePages      int64 `json:"free_pages"`
	AllocatedBytes int64 `json:"allocated_bytes"`
	LiveBytes      int64 `json:"live_bytes"`
	FreeBytes      int64 `json:"free_bytes"`
	FileBytes      int64 `json:"file_bytes"`
	WALBytes       int64 `json:"wal_bytes"`
	SHMBytes       int64 `json:"shm_bytes"`
}

// PhysicalBytes is the current filesystem footprint of the database and its
// WAL/SHM sidecars. It is deliberately separate from AllocatedBytes: pages may
// still reside in the WAL, and the main file can remain at a high-water mark.
func (i SQLiteFileInventory) PhysicalBytes() int64 {
	return i.FileBytes + i.WALBytes + i.SHMBytes
}

// SQLiteInventory reports both persistent SQLite tiers without checkpointing,
// compacting, or otherwise mutating either database.
type SQLiteInventory struct {
	State SQLiteFileInventory `json:"state"`
	Cache SQLiteFileInventory `json:"cache"`
}

// SQLiteInventory returns page accounting and sidecar sizes for state.db and
// cache.db. SQLite queries remain in this package; callers only receive numeric
// metadata and never database paths.
func (s *Store) SQLiteInventory(ctx context.Context) (SQLiteInventory, error) {
	if s == nil || s.db == nil {
		return SQLiteInventory{}, errors.New("state: storage inventory unavailable")
	}
	mainPath := s.mainDBPath
	if mainPath == "" {
		var err error
		mainPath, err = s.statePath()
		if err != nil {
			return SQLiteInventory{}, fmt.Errorf("state inventory path: %w", err)
		}
	}
	main, err := sqliteFileInventory(ctx, s.db, mainPath)
	if err != nil {
		return SQLiteInventory{}, fmt.Errorf("state inventory: %w", err)
	}

	var cache SQLiteFileInventory
	if s.cache != nil {
		cachePath := filepath.Join(filepath.Dir(mainPath), "cache.db")
		cache, err = sqliteFileInventory(ctx, s.cache, cachePath)
		if err != nil {
			return SQLiteInventory{}, fmt.Errorf("cache inventory: %w", err)
		}
	}
	return SQLiteInventory{State: main, Cache: cache}, nil
}

func sqliteFileInventory(ctx context.Context, db *sql.DB, path string) (SQLiteFileInventory, error) {
	var out SQLiteFileInventory
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SQLiteFileInventory{}, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&out.PageSizeBytes); err != nil {
		return SQLiteFileInventory{}, err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&out.AllocatedPages); err != nil {
		return SQLiteFileInventory{}, err
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&out.FreePages); err != nil {
		return SQLiteFileInventory{}, err
	}
	if out.FreePages > out.AllocatedPages {
		out.FreePages = out.AllocatedPages
	}
	out.LivePages = out.AllocatedPages - out.FreePages
	out.AllocatedBytes = out.AllocatedPages * out.PageSizeBytes
	out.LiveBytes = out.LivePages * out.PageSizeBytes
	out.FreeBytes = out.FreePages * out.PageSizeBytes

	err = nil
	if out.FileBytes, err = regularFileSize(path); err != nil {
		return SQLiteFileInventory{}, err
	}
	if out.WALBytes, err = regularFileSize(path + "-wal"); err != nil {
		return SQLiteFileInventory{}, err
	}
	if out.SHMBytes, err = regularFileSize(path + "-shm"); err != nil {
		return SQLiteFileInventory{}, err
	}
	return out, nil
}

func regularFileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("not a regular file")
	}
	return info.Size(), nil
}
