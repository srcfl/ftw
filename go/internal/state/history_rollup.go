package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Unlike prepared hourly archives, rollups return a short write deadline so
// their caller can reduce the batch. Busy snapshots still reread their input.
func (s *Store) rollupTransaction(ctx context.Context, prepare, write func(context.Context, *sql.Tx) error) error {
	conflicts := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.HistoryWriterStatus().Pending < historyCommitMaxTicks/2 {
			var err error
			if conflicts < 3 {
				err = s.tryArchiveBatch(ctx, prepare, write)
			} else {
				// Constant live commits can invalidate every read snapshot. One
				// attempt may read under the lock, within the same one-second
				// total budget. The caller shrinks the batch if it cannot fit.
				err = s.tryArchiveBatch(ctx, nil, func(ctx context.Context, tx *sql.Tx) error {
					// Reserve SQLite's writer as well: compatibility writers and
					// another process do not share the Go mutex. No rows change.
					if _, err := tx.ExecContext(ctx, `UPDATE history_receipts SET batch_id=batch_id WHERE 0`); err != nil {
						return err
					}
					if err := prepare(ctx, tx); err != nil {
						return err
					}
					return write(ctx, tx)
				})
			}
			conflicts++
			if !historyWriteBusy(err) {
				return err
			}
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

// Read the full source buckets before acquiring the live writer mutex. The
// same SQLite snapshot binds the averages and newest JSON to the deleted rows.
func (s *Store) pruneChunk(ctx context.Context, src, dst string, fromMS, toMS, bucketMS int64) (int64, error) {
	var points []HistoryPoint
	var deleted int64
	err := s.rollupTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		points = nil
		q := fmt.Sprintf(`SELECT (ts_ms / %d) * %d + %d,
 AVG(grid_w),AVG(pv_w),AVG(bat_w),AVG(load_w),AVG(bat_soc),json,MAX(ts_ms)
 FROM %s WHERE ts_ms>=? AND ts_ms<? GROUP BY ts_ms/%d`, bucketMS, bucketMS, bucketMS/2, src, bucketMS)
		rows, err := tx.QueryContext(ctx, q, fromMS, toMS)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p HistoryPoint
			var newest int64
			if err := rows.Scan(&p.TsMs, &p.GridW, &p.PVW, &p.BatW, &p.LoadW, &p.BatSoC, &p.JSON, &newest); err != nil {
				return err
			}
			points = append(points, p)
		}
		return rows.Err()
	}, func(ctx context.Context, tx *sql.Tx) error {
		for _, p := range points {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO `+dst+` (ts_ms,grid_w,pv_w,bat_w,load_w,bat_soc,json) VALUES(?,?,?,?,?,?,?)`,
				p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM `+src+` WHERE ts_ms>=? AND ts_ms<?`, fromMS, toMS)
		if err != nil {
			return err
		}
		deleted, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func (s *Store) rollupEnergyLedgerWidth(ctx context.Context, fromMS, toMS, width int64) (int64, error) {
	var total int64
	limit := 128
	for {
		var ids []any
		var deleted int64
		err := s.rollupTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
			ids = nil
			rows, err := tx.QueryContext(ctx, `SELECT schema_version,asset_id,flow,bucket_start_ms,bucket_len_ms,source,quality,provenance FROM energy_ledger_entries
 WHERE bucket_len_ms<? AND bucket_start_ms>=? AND bucket_start_ms<? ORDER BY bucket_start_ms,asset_id,flow,schema_version,bucket_len_ms,source,quality,provenance LIMIT ?`, width, fromMS, toMS, limit)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var version, start, length int64
				var asset, flow, source, quality, provenance string
				if err := rows.Scan(&version, &asset, &flow, &start, &length, &source, &quality, &provenance); err != nil {
					return err
				}
				ids = append(ids, version, asset, flow, start, length, source, quality, provenance)
			}
			return rows.Err()
		}, func(ctx context.Context, tx *sql.Tx) error {
			deleted = 0
			if len(ids) == 0 {
				return nil
			}
			in := "(" + strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?,?,?),", len(ids)/8), ",") + ")"
			args := append([]any{width, width, width}, ids...)
			if _, err := tx.ExecContext(ctx, `INSERT INTO energy_ledger_entries(
 schema_version,asset_id,flow,bucket_start_ms,bucket_len_ms,energy_wh,source,quality,provenance,sample_count,observed_at_ms)
 SELECT schema_version,asset_id,flow,(bucket_start_ms/?)*?,?,SUM(energy_wh),source,quality,provenance,SUM(sample_count),MAX(observed_at_ms)
 FROM energy_ledger_entries WHERE (schema_version,asset_id,flow,bucket_start_ms,bucket_len_ms,source,quality,provenance) IN `+in+`
 GROUP BY schema_version,asset_id,flow,4,source,quality,provenance
 ON CONFLICT(schema_version,asset_id,flow,bucket_start_ms,bucket_len_ms,source,quality,provenance)
 DO UPDATE SET energy_wh=energy_ledger_entries.energy_wh+excluded.energy_wh,
 sample_count=energy_ledger_entries.sample_count+excluded.sample_count,
 observed_at_ms=MAX(energy_ledger_entries.observed_at_ms,excluded.observed_at_ms)`, args...); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `DELETE FROM energy_ledger_entries WHERE (schema_version,asset_id,flow,bucket_start_ms,bucket_len_ms,source,quality,provenance) IN `+in, ids...)
			if err != nil {
				return err
			}
			deleted, err = res.RowsAffected()
			return err
		})
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				limit = max(1, limit/2)
			} else {
				return total, err
			}
		} else {
			total += deleted
			if len(ids) == 0 {
				return total, nil
			}
		}
		if err := pauseMaintenance(ctx); err != nil {
			return total, err
		}
	}
}
