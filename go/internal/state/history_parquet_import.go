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
	"strings"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"github.com/parquet-go/parquet-go"
)

const historyImportRows = 2048

// ImportLegacyParquet imports frozen files while the primary remains open.
// Native instances rotate only after all active connections have closed.
// Existing primary samples win overlap.
func (s *Store) ImportLegacyParquet(ctx context.Context, coldDir string) error {
	s.historyImportMu.Lock()
	defer s.historyImportMu.Unlock()
	// Only this importer owns these disposable directories. A killed process
	// may leave one behind; it contains no authoritative rows or receipts.
	entries, err := os.ReadDir(filepath.Dir(s.historyPath))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), filepath.Base(s.historyPath)+".import-") {
			if err := os.RemoveAll(filepath.Join(filepath.Dir(s.historyPath), entry.Name())); err != nil {
				return err
			}
		}
	}
	if err := s.bindLegacyParquetSources(coldDir); err != nil {
		return err
	}
	rows, err := s.history.QueryContext(ctx, `SELECT m.path,COALESCE(s.rows,0),s.path IS NOT NULL FROM history_parquet_manifest m LEFT JOIN history_parquet_sources s ON s.path=m.path ORDER BY m.path`)
	if err != nil {
		return err
	}
	paths := []string{}
	var completedRows int64
	var totalBytes, completedBytes int64
	bytesKnown := true
	for rows.Next() {
		var path string
		var count int64
		var complete bool
		if err := rows.Scan(&path, &count, &complete); err != nil {
			rows.Close()
			return err
		}
		paths = append(paths, path)
		completedRows += count
		if info, err := os.Stat(path); err == nil {
			totalBytes += info.Size()
			if complete {
				completedBytes += info.Size()
			}
		} else {
			// A verified source may already have been removed. Its old compressed
			// size is unknown; do not present a partial inventory as the total.
			bytesKnown = false
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if s.historyMigration != nil {
		s.historyMigration.update(func(st *HistoryMigrationStatus) {
			st.Phase = "parquet"
			st.Activity = "checking"
			st.FilesTotal = len(paths)
			st.CurrentSource = ""
			st.RowsDone += completedRows
			st.RowsTotal = 0
		})
		if bytesKnown {
			s.historyMigration.startSourceBytes(&totalBytes, &completedBytes)
		}
	}
	for _, path := range paths {
		if err := s.yieldHistoryImport(ctx); err != nil {
			return err
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if err := s.importHistoryFile(ctx, abs, coldDir); err != nil {
			return fmt.Errorf("import cold history %s: %w", abs, err)
		}
		if s.historyMigration != nil {
			s.historyMigration.update(func(st *HistoryMigrationStatus) { st.FilesDone++ })
		}
	}
	var pending int
	if err := s.history.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_parquet_imports`).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return errors.New("an interrupted Parquet source is missing; restore its original source to resume historical import")
	}
	return nil
}

func (s *Store) bindLegacyParquetSources(coldDir string) error {
	if coldDir == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(coldDir, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].parquet"))
	if err != nil {
		return err
	}
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO history_parquet_manifest VALUES (?) ON CONFLICT DO NOTHING`, abs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) importHistoryFile(ctx context.Context, path, coldDir string) error {
	if s.historyMigration != nil {
		name, _ := filepath.Rel(coldDir, path)
		s.historyMigration.update(func(st *HistoryMigrationStatus) {
			st.CurrentSource = filepath.ToSlash(name)
			st.Activity = "checking"
			st.CurrentSourceRowsDone, st.CurrentSourceRowsTotal = 0, 0
		})
	}
	digest, err := historyFileHash(path)
	if err != nil {
		// A verified file may have been removed after a complete backup. Its
		// receipt still proves coverage; unimported missing files stay errors.
		if errors.Is(err, os.ErrNotExist) {
			var complete int
			if checkErr := s.history.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_parquet_sources WHERE path=?`, path).Scan(&complete); checkErr == nil && complete != 0 {
				return nil
			}
		}
		return err
	}
	for _, table := range []string{"history_parquet_sources", "history_parquet_imports"} {
		var prior string
		err := s.history.QueryRowContext(ctx, `SELECT sha256 FROM `+table+` WHERE path=?`, path).Scan(&prior)
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
	if s.historyMigration != nil {
		name, _ := filepath.Rel(coldDir, path)
		s.historyMigration.update(func(st *HistoryMigrationStatus) {
			st.CurrentSource = filepath.ToSlash(name)
			st.CurrentSourceRowsDone = 0
			st.CurrentSourceRowsTotal = 0
			st.RowsTotal = 0
		})
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	sourceBytes := info.Size()
	if err := s.checkpointHistoryImport(ctx); err != nil {
		return err
	}
	// This instance owns all file-sized buffers and may spill to disk. Closing
	// it cannot close the live database or any reader's connection.
	dir, err := os.MkdirTemp(filepath.Dir(s.historyPath), filepath.Base(s.historyPath)+".import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	db, err := sql.Open("duckdb", filepath.Join(dir, "staging.duckdb")+"?threads=1&memory_limit=64MB&max_temp_directory_size=512MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, q := range []string{
		`CREATE SEQUENCE ts_drivers_id START 1`, `CREATE SEQUENCE ts_metrics_id START 1`,
		`CREATE TABLE ts_drivers(id BIGINT DEFAULT nextval('ts_drivers_id'),name VARCHAR UNIQUE)`,
		`CREATE TABLE ts_metrics(id BIGINT DEFAULT nextval('ts_metrics_id'),name VARCHAR UNIQUE)`,
		`CREATE TEMP TABLE history_import_source(ts_ms BIGINT NOT NULL,driver_id BIGINT NOT NULL,metric_id BIGINT NOT NULL,value DOUBLE NOT NULL CHECK(isfinite(value)))`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	count, err := stageHistoryParquet(ctx, conn, path, func(int64) {
		if s.historyMigration != nil {
			s.historyMigration.update(func(*HistoryMigrationStatus) {})
		}
	})
	if err != nil {
		return fmt.Errorf("stage source: %w", err)
	}
	var duplicates int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT ts_ms,LAG(ts_ms) OVER(PARTITION BY driver_id,metric_id ORDER BY ts_ms) AS previous FROM history_import_source) WHERE ts_ms=previous`).Scan(&duplicates); err != nil {
		return fmt.Errorf("validate source keys: %w", err)
	}
	if duplicates != 0 {
		return errors.New("Parquet contains duplicate sample keys")
	}
	if after, err := historyFileHash(path); err != nil || after != digest {
		return errors.Join(err, errors.New("Parquet changed during staging"))
	}
	var offset int64
	if err := s.history.QueryRowContext(ctx, `SELECT rows_done FROM history_parquet_progress WHERE path=?`, path).Scan(&offset); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if offset > count {
		return errors.New("Parquet source is shorter than its committed progress")
	}
	s.historyWriteMu.Lock()
	_, err = s.history.ExecContext(ctx, `INSERT INTO history_parquet_imports VALUES (?,?) ON CONFLICT DO NOTHING`, path, digest)
	s.historyWriteMu.Unlock()
	if err != nil {
		return err
	}
	if s.historyMigration != nil {
		s.historyMigration.update(func(st *HistoryMigrationStatus) {
			st.CurrentSourceRowsDone = offset
			st.CurrentSourceRowsTotal = count
			st.RowsDone += offset
		})
		s.historyMigration.addSourceBytes(estimatedSourceBytes(sourceBytes, offset, count), offset > 0, false)
	}
	for offset < count {
		s.historyActivity("importing")
		if err := s.yieldHistoryImport(ctx); err != nil {
			return err
		}
		end := min(offset+historyImportRows, count)
		rows, err := conn.QueryContext(ctx, `SELECT p.ts_ms,d.name,m.name,p.value FROM history_import_source p JOIN ts_drivers d ON d.id=p.driver_id JOIN ts_metrics m ON m.id=p.metric_id WHERE p.rowid>=? AND p.rowid<? ORDER BY p.rowid`, offset, end)
		if err != nil {
			return err
		}
		batch := make([]Sample, 0, historyImportRows)
		for rows.Next() {
			var sm Sample
			if err := rows.Scan(&sm.TsMs, &sm.Driver, &sm.Metric, &sm.Value); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, sm)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if int64(len(batch)) != end-offset {
			return errors.New("staging row count changed")
		}
		if err := s.mergeHistoricalSamples(ctx, batch, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO history_parquet_progress VALUES (?,?) ON CONFLICT(path) DO UPDATE SET rows_done=excluded.rows_done`, path, end)
			return err
		}); err != nil {
			return fmt.Errorf("import rows at %d: %w", offset, err)
		}
		if s.historyMigration != nil {
			s.historyMigration.update(func(st *HistoryMigrationStatus) { st.CurrentSourceRowsDone = end; st.RowsDone += end - offset })
			s.historyMigration.addSourceBytes(estimatedSourceBytes(sourceBytes, end, count)-estimatedSourceBytes(sourceBytes, offset, count), true, true)
		}
		offset = end
		if offset%(64*historyImportRows) == 0 {
			if err := s.checkpointHistoryImport(ctx); err != nil {
				return err
			}
		}
	}
	s.historyActivity("checking")
	if after, err := historyFileHash(path); err != nil || after != digest {
		return errors.Join(err, errors.New("Parquet changed during import; restore the original source"))
	}
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM history_parquet_progress WHERE path=?`, path); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.historyMigration != nil {
		s.historyMigration.addSourceBytes(sourceBytes-estimatedSourceBytes(sourceBytes, offset, count), false, true)
	}
	slog.Info("history: verified Parquet import", "file", filepath.Base(path), "rows", count, "sha256", digest)
	return nil
}

func estimatedSourceBytes(size, rows, totalRows int64) int64 {
	if totalRows <= 0 {
		return 0
	}
	return int64(float64(size) * float64(min(rows, totalRows)) / float64(totalRows))
}

func stageHistoryParquet(ctx context.Context, conn *sql.Conn, path string, progress ...func(int64)) (int64, error) {
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
	drivers, metrics := map[string]int64{}, map[string]int64{}
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
			staged := make([][]driver.Value, 0, n)
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
				for _, spec := range []struct {
					pos   int
					table string
					ids   map[string]int64
				}{{1, "ts_drivers", drivers}, {2, "ts_metrics", metrics}} {
					name, ok := values[spec.pos].(string)
					if !ok {
						writeErr = errors.New("Parquet is missing an identity field")
						break
					}
					id, ok := spec.ids[name]
					if !ok {
						writeErr = conn.QueryRowContext(ctx, `INSERT INTO `+spec.table+`(name) VALUES (?) ON CONFLICT(name) DO UPDATE SET name=excluded.name RETURNING id`, name).Scan(&id)
						if writeErr != nil {
							break
						}
						spec.ids[name] = id
					}
					values[spec.pos] = id
				}
				if writeErr != nil {
					break
				}
				staged = append(staged, values)
			}
			if writeErr != nil {
				return count, writeErr
			}
			err := conn.Raw(func(raw any) error {
				app, err := duckdb.NewAppender(nativeHistoryConn(raw), "temp", "main", "history_import_source")
				if err != nil {
					return err
				}
				var writeErr error
				for _, values := range staged {
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
			for _, notify := range progress {
				notify(count)
			}
		}
		if readErr == io.EOF {
			return count, nil
		}
	}
}

func importHistoryChunk(ctx context.Context, conn *sql.Conn, offset, total int64) error {
	return importHistoryChunkCommit(ctx, conn, offset, total, nil)
}

func importHistoryChunkCommit(ctx context.Context, conn *sql.Conn, offset, total int64, receipt func(*sql.Tx) error) error {
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
	var first, last int64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(ts_ms),MAX(ts_ms) FROM import_points`).Scan(&first, &last); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE import_expected AS
	 SELECT p.driver_id,p.metric_id,p.ts_ms,p.value,COALESCE(s.value,p.value) AS expected_value
	 FROM import_points p
	 LEFT JOIN (SELECT * FROM ts_samples WHERE ts_ms>=? AND ts_ms<=?) s
	 ON s.driver_id=p.driver_id AND s.metric_id=p.metric_id AND s.ts_ms=p.ts_ms`, first, last); err != nil {
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
	if receipt != nil {
		if err := receipt(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}
