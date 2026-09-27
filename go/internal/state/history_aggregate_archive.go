package state

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
)

const bucketColumns = `start_ms,resolution_ms,first_ms,last_ms,n,sum_value,min_value,max_value,last_value`

func aggregatePaths(cold string, since, until int64) ([]string, error) {
	if cold == "" {
		return nil, nil
	}
	paths, err := filepath.Glob(filepath.Join(cold, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].buckets.parquet"))
	if err != nil {
		return nil, err
	}
	legacy, err := filepath.Glob(filepath.Join(cold, "[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].legacy-buckets.parquet"))
	if err != nil {
		return nil, err
	}
	paths = append(paths, legacy...)
	out := paths[:0]
	for _, p := range paths {
		day, err := aggregateDay(p)
		if err != nil {
			return nil, err
		}
		if day.UnixMilli() <= until && day.Add(24*time.Hour).UnixMilli() > since {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func aggregateDay(path string) (time.Time, error) {
	base := filepath.Base(path)
	if len(base) < 2 {
		return time.Time{}, errors.New("invalid aggregate filename")
	}
	return time.Parse("2006/01/02", filepath.Base(filepath.Dir(filepath.Dir(path)))+"/"+filepath.Base(filepath.Dir(path))+"/"+base[:2])
}

func walkBucketFile(ctx context.Context, path string, visit func(metricBucket) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	pf, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		return err
	}
	r := parquet.NewGenericReader[metricBucket](pf)
	defer r.Close()
	rows := make([]metricBucket, 256)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(rows)
		for _, b := range rows[:n] {
			if b.Driver == "" || b.Metric == "" {
				return errors.New("missing aggregate identity")
			}
			if e := b.validate(); e != nil {
				return e
			}
			if e := visit(b); e != nil {
				return e
			}
		}
		count += int64(n)
		if errors.Is(err, io.EOF) {
			if count != pf.NumRows() {
				return errors.New("aggregate row count differs")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}
