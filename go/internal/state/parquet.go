package state

import (
	"os"
	"runtime"
)

// parquetSampleRow mirrors the long-format schema in column-oriented form.
// Driver and Metric are interned strings (parquet's dictionary encoding makes
// the repetition cheap — typically <2 bytes per row after compression).
type parquetSampleRow struct {
	TsMs   int64   `parquet:"ts_ms"`
	Driver string  `parquet:"driver,dict,zstd"`
	Metric string  `parquet:"metric,dict,zstd"`
	Value  float64 `parquet:"value,zstd"`
}

// syncDir fsyncs a directory so a completed rename survives power loss.
// Best-effort on platforms where directories can't be fsynced (Windows).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}
