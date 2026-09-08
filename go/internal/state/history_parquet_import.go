package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"github.com/parquet-go/parquet-go"
)

const historyImportRows = 2048

// ImportLegacyParquet imports frozen daily files once. Existing recent samples
// win overlap. The source files remain as evidence after verification.
func (s *Store) ImportLegacyParquet(ctx context.Context, coldDir string) error {
	if coldDir == "" {
		var pending int
		if err := s.history.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_parquet_imports`).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return errors.New("an interrupted Parquet import requires its original cold directory")
		}
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(coldDir, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].parquet"))
	if err != nil {
		return err
	}
	s.ts.allocMu.Lock()
	defer s.ts.allocMu.Unlock()
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	// An interrupted import may have created new catalog entries too.
	defer func() { s.ts.mu.Lock(); s.ts.loaded = false; s.ts.mu.Unlock() }()
	conn, err := s.history.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if err := importHistoryFile(ctx, conn, abs); err != nil {
			return fmt.Errorf("import cold history %s: %w", abs, err)
		}
	}
	var pending int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_parquet_imports`).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return errors.New("an interrupted Parquet source is missing; restore the original source before starting")
	}
	return nil
}

func importHistoryFile(ctx context.Context, conn *sql.Conn, path string) error {
	digest, err := historyFileHash(path)
	if err != nil {
		return err
	}
	for _, table := range []string{"history_parquet_sources", "history_parquet_imports"} {
		var prior string
		err := conn.QueryRowContext(ctx, `SELECT sha256 FROM `+table+` WHERE path=?`, path).Scan(&prior)
		if err == nil {
			if prior != digest {
				return errors.New("previously imported or pending Parquet source changed")
			}
			if table == "history_parquet_sources" {
				return nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	// A bounded reader stages only this file. Its unique index detects duplicate
	// keys across chunk boundaries before any of this file reaches the primary.
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE history_import_source (
		ts_ms BIGINT NOT NULL, driver VARCHAR NOT NULL, metric VARCHAR NOT NULL,
		value DOUBLE NOT NULL CHECK(isfinite(value)), PRIMARY KEY(driver,metric,ts_ms))`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `DROP TABLE IF EXISTS history_import_source`)
	count, err := stageHistoryParquet(ctx, conn, path)
	if err != nil {
		return err
	}
	if after, err := historyFileHash(path); err != nil || after != digest {
		return errors.Join(err, errors.New("Parquet changed during staging"))
	}
	// Bind every committed chunk to immutable source bytes. A retry rechecks
	// all rows, retaining the values verified by an earlier completed chunk.
	if _, err := conn.ExecContext(ctx, `INSERT INTO history_parquet_imports VALUES (?,?) ON CONFLICT DO NOTHING`, path, digest); err != nil {
		return err
	}
	for offset := int64(0); offset < count; offset += historyImportRows {
		if err := importHistoryChunk(ctx, conn, offset, count); err != nil {
			return err
		}
	}
	if after, err := historyFileHash(path); err != nil || after != digest {
		return errors.Join(err, errors.New("Parquet changed during import; restore the original source"))
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_parquet_sources(path,sha256,rows) VALUES (?,?,?)`, path, digest, count); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM history_parquet_imports WHERE path=?`, path); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("history: verified Parquet import", "file", filepath.Base(path), "rows", count, "sha256", digest)
	return nil
}

func stageHistoryParquet(ctx context.Context, conn *sql.Conn, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return 0, err
	}
	pf, err := parquet.OpenFile(f, stat.Size())
	if err != nil {
		return 0, err
	}
	columns := pf.Schema().Columns()
	positions := make([]int, len(columns))
	found := [4]bool{}
	for i, col := range columns {
		positions[i] = -1
		if len(col) != 1 {
			continue
		}
		for j, name := range []string{"ts_ms", "driver", "metric", "value"} {
			if col[0] == name {
				positions[i], found[j] = j, true
			}
		}
	}
	for _, ok := range found {
		if !ok {
			return 0, errors.New("Parquet is missing a sample column")
		}
	}
	reader := parquet.NewReader(pf)
	defer reader.Close()
	buffer := make([]parquet.Row, historyImportRows)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		n, readErr := reader.ReadRows(buffer)
		if readErr != nil && readErr != io.EOF {
			return count, readErr
		}
		if n > 0 {
			err := conn.Raw(func(raw any) error {
				app, err := duckdb.NewAppender(raw.(driver.Conn), "temp", "main", "history_import_source")
				if err != nil {
					return err
				}
				var writeErr error
				for _, row := range buffer[:n] {
					values := make([]driver.Value, 4)
					seen := [4]bool{}
					for _, v := range row {
						p := positions[v.Column()]
						if p < 0 {
							continue
						}
						if v.IsNull() || seen[p] {
							writeErr = errors.New("Parquet contains null or repeated sample fields")
							break
						}
						seen[p] = true
						switch {
						case p == 0 && v.Kind() == parquet.Int64:
							values[p] = v.Int64()
						case (p == 1 || p == 2) && v.Kind() == parquet.ByteArray:
							values[p] = string(v.ByteArray())
						case p == 3 && v.Kind() == parquet.Double:
							value := v.Double()
							if math.IsNaN(value) || math.IsInf(value, 0) {
								writeErr = errors.New("Parquet contains a non-finite sample")
							}
							values[p] = canonicalHistoryFloat(value)
						default:
							writeErr = errors.New("Parquet sample column has the wrong type")
						}
						if writeErr != nil {
							break
						}
					}
					if writeErr != nil {
						break
					}
					if writeErr = app.AppendRow(values...); writeErr != nil {
						break
					}
				}
				return errors.Join(writeErr, app.Close())
			})
			if err != nil {
				return count, err
			}
			count += int64(n)
		}
		if readErr == io.EOF {
			return count, nil
		}
	}
}

func importHistoryChunk(ctx context.Context, conn *sql.Conn, offset, total int64) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	end := min(offset+historyImportRows, total)
	// rowid is stable in this private staging table: nothing deletes or updates
	// its rows. It bounds native results without sorting the entire source.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE import_points AS
	 SELECT * FROM history_import_source WHERE rowid>=? AND rowid<?`, offset, end); err != nil {
		return err
	}
	for _, q := range []string{
		`INSERT INTO ts_drivers(name) SELECT DISTINCT driver FROM import_points ON CONFLICT(name) DO NOTHING`,
		`INSERT INTO ts_metrics(name) SELECT DISTINCT metric FROM import_points ON CONFLICT(name) DO NOTHING`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	var first, last int64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(ts_ms),MAX(ts_ms) FROM import_points`).Scan(&first, &last); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE import_expected AS
	 SELECT d.id AS driver_id,m.id AS metric_id,p.ts_ms,p.value,COALESCE(s.value,p.value) AS expected_value
	 FROM import_points p JOIN ts_drivers d ON d.name=p.driver JOIN ts_metrics m ON m.name=p.metric
	 LEFT JOIN (SELECT * FROM ts_samples WHERE ts_ms>=? AND ts_ms<=?) s
	 ON s.driver_id=d.id AND s.metric_id=m.id AND s.ts_ms=p.ts_ms`, first, last); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ts_samples SELECT driver_id,metric_id,ts_ms,value FROM import_expected ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.expected_value,s.value FROM import_expected p
	 LEFT JOIN (SELECT * FROM ts_samples WHERE ts_ms>=? AND ts_ms<=?) s
	 ON s.driver_id=p.driver_id AND s.metric_id=p.metric_id AND s.ts_ms=p.ts_ms`, first, last)
	if err != nil {
		return err
	}
	var verified int64
	for rows.Next() {
		var expected float64
		var actual sql.NullFloat64
		if err := rows.Scan(&expected, &actual); err != nil {
			rows.Close()
			return err
		}
		if !actual.Valid || historyFloatBits(expected) != historyFloatBits(actual.Float64) {
			rows.Close()
			return errors.New("Parquet sample verification failed")
		}
		verified++
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	if verified != end-offset {
		return errors.New("Parquet row-count verification failed")
	}
	for _, table := range []string{"import_expected", "import_points"} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+table); err != nil {
			return err
		}
	}
	return tx.Commit()
}
