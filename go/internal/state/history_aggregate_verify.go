package state

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

type bucketEvidence struct {
	summary     BucketSummary
	absoluteSum float64
}
type bucketEvidenceSet map[[2]string]bucketEvidence

func (e bucketEvidenceSet) add(b metricBucket) error {
	key := [2]string{b.Driver, b.Metric}
	v, ok := e[key]
	if !ok && len(e) >= maxSeriesBuckets {
		return ErrHistoryQueryLimit
	}
	v.summary.merge(b.BucketSummary)
	v.absoluteSum += math.Abs(b.Sum)
	e[key] = v
	return nil
}
func (e bucketEvidenceSet) verifyStage(ctx context.Context, db *sql.DB) error {
	got := bucketEvidenceSet{}
	if err := scanStagedBuckets(ctx, db, got.add); err != nil {
		return err
	}
	if len(got) != len(e) {
		return fmt.Errorf("aggregate identities changed: %d != %d", len(got), len(e))
	}
	for key, w := range e {
		a := got[key].summary
		b := w.summary
		if a.N != b.N || a.FirstMS != b.FirstMS || a.LastMS != b.LastMS || a.Min != b.Min || a.Max != b.Max || a.Last != b.Last || math.Abs(a.Sum-b.Sum) > 1e-12*max(1, w.absoluteSum) {
			return fmt.Errorf("aggregate evidence changed for %s/%s", key[0], key[1])
		}
	}
	return nil
}
