package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Copy one coherent state snapshot without recopying frozen legacy history.
// The selected history database is exported separately. The destination is
// temporary until the complete backup has passed verification and fsync.
func (s *Store) copyStateForBackup(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
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
		if err := copyVerifiedTable(ctx, src, dst, item.name, scanSQLiteBackupTable, func() error { return pauseMaintenance(ctx) }); err != nil {
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
