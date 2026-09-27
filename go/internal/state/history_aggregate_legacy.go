package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrCompactedHistory = errors.New("individual samples cannot correct compacted history; restore the original history before importing corrections")

func rejectCompactedSamples(ctx context.Context, tx *sql.Tx, rs []resolvedSample) error {
	seen := map[int64]bool{}
	for _, r := range rs {
		day := bucketStart(r.ts, 24*time.Hour.Milliseconds())
		if seen[day] {
			continue
		}
		seen[day] = true
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_legacy_bucket_days WHERE day_ms=?)`, day).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return ErrCompactedHistory
		}
	}
	return nil
}
