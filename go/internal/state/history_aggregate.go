package state

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	HistoryResolutionMS      int64 = 10_000
	ArchiveResolutionMS      int64 = 60_000
	OldArchiveResolutionMS   int64 = 300_000
	AggregateRecentRetention       = 24 * time.Hour
	AggregateMinuteRetention       = 30 * 24 * time.Hour
	AggregateRetention             = 2 * 365 * 24 * time.Hour
)

// Core enables this once before collection starts. Import and recovery tools
// keep their explicit raw-write API. All reads understand both formats.
func (s *Store) EnableHistoryAggregation() error {
	var through int64
	if err := s.history.QueryRow(`SELECT COALESCE(MAX(through_ms),0) FROM ts_bucket_days`).Scan(&through); err != nil {
		return err
	}
	w := s.historyWriter
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status.Accepted != 0 {
		return errors.New("history aggregation must be enabled before collection")
	}
	w.aggregateBeforeMS = through
	s.aggregateHistory.Store(true)
	return nil
}

// Reserve an archive boundary without overtaking an accepted tick. The same
// mutex covers admission; no disk IO runs while holding it.
func (s *Store) reserveAggregateCutoff(cutoff int64) int64 {
	w := s.historyWriter
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ts := range w.pendingMeasurementMS {
		cutoff = min(cutoff, bucketStart(ts, ArchiveResolutionMS))
	}
	w.aggregateBeforeMS = max(w.aggregateBeforeMS, cutoff)
	return cutoff
}

// Keep timestamp identities, not raw values, for each hot bucket. Two bytes
// per observed millisecond preserve first-write semantics across ticks and
// restarts. The list is bounded by the 10,000 milliseconds in one bucket.
func addBucketTimestamp(seen []byte, offset int64) ([]byte, bool) {
	n := len(seen) / 2
	i := sort.Search(n, func(i int) bool { return int64(binary.BigEndian.Uint16(seen[2*i:])) >= offset })
	if i < n && int64(binary.BigEndian.Uint16(seen[2*i:])) == offset {
		return seen, false
	}
	seen = append(seen, 0, 0)
	copy(seen[2*i+2:], seen[2*i:])
	binary.BigEndian.PutUint16(seen[2*i:], uint16(offset))
	return seen, true
}

// BucketSummary retains the observed envelope and timestamps. First/Last do
// not imply continuous coverage; missing buckets are never filled. Sum/N is a
// sample-weighted gauge mean, never an energy calculation or a status code.
// The energy ledger consumes the original observations in the same commit.
type BucketSummary struct {
	StartMS      int64   `parquet:"start_ms"`
	ResolutionMS int64   `parquet:"resolution_ms"`
	FirstMS      int64   `parquet:"first_ms"`
	LastMS       int64   `parquet:"last_ms"`
	N            int64   `parquet:"n"`
	Sum          float64 `parquet:"sum"`
	Min          float64 `parquet:"min"`
	Max          float64 `parquet:"max"`
	Last         float64 `parquet:"last"`
}

type metricBucket struct {
	Driver string `parquet:"driver,dict,zstd"`
	Metric string `parquet:"metric,dict,zstd"`
	BucketSummary
}

type bucketKey struct{ driver, metric, start, width int64 }

func bucketStart(ts, width int64) int64 {
	start := ts / width * width
	if ts < 0 && ts%width != 0 {
		start -= width
	}
	return start
}

func (b *BucketSummary) merge(v BucketSummary) {
	if v.N == 0 {
		return
	}
	if b.N == 0 {
		b.FirstMS, b.LastMS, b.Min, b.Max, b.Last = v.FirstMS, v.LastMS, v.Min, v.Max, v.Last
	} else {
		b.FirstMS = min(b.FirstMS, v.FirstMS)
		b.Min, b.Max = min(b.Min, v.Min), max(b.Max, v.Max)
		if v.LastMS >= b.LastMS {
			b.LastMS, b.Last = v.LastMS, v.Last
		}
	}
	b.N += v.N
	b.Sum += v.Sum
}

func (b BucketSummary) validate() error {
	if b.ResolutionMS != HistoryResolutionMS && b.ResolutionMS != ArchiveResolutionMS && b.ResolutionMS != OldArchiveResolutionMS {
		return errors.New("unsupported history resolution")
	}
	if b.N <= 0 || b.StartMS != bucketStart(b.StartMS, b.ResolutionMS) || b.FirstMS < b.StartMS || b.LastMS < b.FirstMS || b.LastMS >= b.StartMS+b.ResolutionMS {
		return errors.New("invalid history bucket bounds")
	}
	for _, v := range []float64{b.Sum, b.Min, b.Max, b.Last} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("non-finite history summary")
		}
	}
	if b.Min > b.Max || b.Last < b.Min || b.Last > b.Max {
		return errors.New("invalid history envelope")
	}
	return nil
}

// Retry receipts wrap these updates, so an uncertain commit cannot add the
// same tick twice. Collapse duplicate source timestamps before aggregation.
func (s *Store) insertAggregateSamples(ctx context.Context, tx *sql.Tx, rs []resolvedSample) error {
	unique := make(map[[3]int64]resolvedSample, len(rs))
	for _, r := range rs {
		k := [3]int64{r.dID, r.mID, r.ts}
		if _, exists := unique[k]; !exists {
			unique[k] = r
		}
	}
	keys := make([][3]int64, 0, len(unique))
	for k := range unique {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		for n := range 3 {
			if keys[i][n] != keys[j][n] {
				return keys[i][n] < keys[j][n]
			}
		}
		return false
	})
	groups := make(map[bucketKey]BucketSummary)
	seenByBucket := make(map[bucketKey][]byte)
	hours := make(map[seriesHourKey]*seriesHourAcc)
	for _, k := range keys {
		r := unique[k]
		hotKey := bucketKey{r.dID, r.mID, bucketStart(r.ts, HistoryResolutionMS), HistoryResolutionMS}
		seen, loaded := seenByBucket[hotKey]
		if !loaded {
			err := tx.QueryRowContext(ctx, `SELECT seen_ms FROM ts_buckets WHERE driver_id=? AND metric_id=? AND start_ms=? AND resolution_ms=?`, hotKey.driver, hotKey.metric, hotKey.start, hotKey.width).Scan(&seen)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var added bool
		seen, added = addBucketTimestamp(seen, r.ts-hotKey.start)
		seenByBucket[hotKey] = seen
		if !added {
			continue
		}
		for _, width := range []int64{HistoryResolutionMS, ArchiveResolutionMS} {
			key := bucketKey{r.dID, r.mID, bucketStart(r.ts, width), width}
			b := groups[key]
			b.StartMS, b.ResolutionMS = key.start, width
			b.merge(BucketSummary{N: 1, Sum: r.v, Min: r.v, Max: r.v, Last: r.v, FirstMS: r.ts, LastMS: r.ts})
			groups[key] = b
		}
		addSeriesHourSample(hours, r.dID, r.mID, r.ts, r.v)
	}
	// Seed legacy source hours before adding new summaries during migration.
	if err := s.seedSeriesHours(ctx, tx, rs); err != nil {
		return err
	}
	for k, b := range groups {
		if err := b.validate(); err != nil {
			return err
		}
		var sealed int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_bucket_days WHERE day_ms=? AND through_ms>?)`, bucketStart(k.start, 24*time.Hour.Milliseconds()), k.start).Scan(&sealed); err != nil {
			return err
		}
		if sealed != 0 {
			return fmt.Errorf("history interval already archived: %d; correction requires a complete bucket", k.start)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO ts_buckets(driver_id,metric_id,start_ms,resolution_ms,first_ms,last_ms,n,sum_value,min_value,max_value,last_value,seen_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(driver_id,metric_id,start_ms,resolution_ms) DO UPDATE SET
 seen_ms=excluded.seen_ms,first_ms=MIN(first_ms,excluded.first_ms),last_ms=MAX(last_ms,excluded.last_ms),
 last_value=CASE WHEN excluded.last_ms>=last_ms THEN excluded.last_value ELSE last_value END,
 n=n+excluded.n,sum_value=sum_value+excluded.sum_value,min_value=MIN(min_value,excluded.min_value),max_value=MAX(max_value,excluded.max_value)`,
			k.driver, k.metric, k.start, k.width, b.FirstMS, b.LastMS, b.N, b.Sum, b.Min, b.Max, b.Last, nonNilBytes(seenByBucket[k]))
		if err != nil {
			return err
		}
		if k.width == HistoryResolutionMS {
			// Keep one actual observation per series after its chart buckets
			// leave SQLite. Status codes must never come from a bucket mean.
			if _, err := tx.ExecContext(ctx, `INSERT INTO ts_latest(driver_id,metric_id,ts_ms,value) VALUES(?,?,?,?)
 ON CONFLICT(driver_id,metric_id) DO UPDATE SET ts_ms=excluded.ts_ms,value=excluded.value WHERE excluded.ts_ms>ts_latest.ts_ms`, k.driver, k.metric, b.LastMS, b.Last); err != nil {
				return err
			}
		}
	}
	if err := upsertSeriesHoursTableTx(ctx, tx, "ts_aggregate_hours", hours); err != nil {
		return err
	}
	return s.upsertSeriesHoursTx(ctx, tx, hours)
}

func nonNilBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
