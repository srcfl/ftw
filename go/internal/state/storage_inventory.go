package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SQLiteFileInventory reports page use and sidecar sizes for one SQLite file.
// LivePages counts pages outside SQLite's freelist. It does not estimate free
// space inside a live page.
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

// SQLiteInventory reports the two SQLite files that Core owns today.
type SQLiteInventory struct {
	State SQLiteFileInventory `json:"state"`
	Cache SQLiteFileInventory `json:"cache"`
}

// SQLiteInventory reads page metadata for state.db and cache.db. It does not
// checkpoint, compact, or expose either path.
func (s *Store) SQLiteInventory(ctx context.Context) (SQLiteInventory, error) {
	if s == nil || s.db == nil || s.mainDBPath == "" {
		return SQLiteInventory{}, errors.New("state: storage inventory unavailable")
	}
	stateDB, err := sqliteFileInventory(ctx, s.db, s.mainDBPath)
	if err != nil {
		return SQLiteInventory{}, fmt.Errorf("state inventory: %w", err)
	}

	var cacheDB SQLiteFileInventory
	if s.cache != nil {
		cachePath := filepath.Join(filepath.Dir(s.mainDBPath), "cache.db")
		cacheDB, err = sqliteFileInventory(ctx, s.cache, cachePath)
		if err != nil {
			return SQLiteInventory{}, fmt.Errorf("cache inventory: %w", err)
		}
	}
	return SQLiteInventory{State: stateDB, Cache: cacheDB}, nil
}

func sqliteFileInventory(ctx context.Context, db *sql.DB, path string) (SQLiteFileInventory, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SQLiteFileInventory{}, err
	}
	defer tx.Rollback()

	var out SQLiteFileInventory
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
		return 0, errors.New("not a regular file")
	}
	return info.Size(), nil
}
