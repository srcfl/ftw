package state

import "context"

// HouseholdSubtotal describes accepted, complete energy intervals only. It is
// not a complete range total, an independent meter, or a gap-filled estimate.
type HouseholdSubtotal struct {
	Wh         float64 `json:"wh"`
	CoverageMS int64   `json:"coverage_ms"`
	PeriodMS   int64   `json:"period_ms"`
	// Minimum interval width; older retained evidence can be coarser.
	BucketMS int64  `json:"minimum_bucket_ms"`
	Quality  string `json:"quality"`
	Method   string `json:"method"`
}

// HouseholdEnergySubtotal reads the same independent source energy and legacy
// site intervals as DailyEnergy, without changing their quality or caches.
// Balance signed energy before deciding whether an interval is usable; never
// clip negative samples, interpolate missing sources or bridge recording gaps.
func (s *Store) HouseholdEnergySubtotal(ctx context.Context, from, to int64) (*HouseholdSubtotal, error) {
	if to <= from || s.history == nil {
		return nil, nil
	}
	const width int64 = 5 * 60000
	repairs, err := s.historicalLoadRepairs(ctx, from, to)
	if err != nil {
		return nil, err
	}
	buckets := map[int64]*sourceLoadBucket{}
	err = s.walkHouseholdLoad(ctx, from, to, repairs, func(r siteEnergyRow) error {
		// Retained coarse intervals remain indivisible; never split an older
		// observed interval into invented five-minute measurements.
		w := max(width, r.width)
		start := bucketStart(r.start, w)
		b := buckets[start]
		if b == nil {
			b = &sourceLoadBucket{start: start, width: w, first: r.first, last: r.last}
			buckets[start] = b
		}
		if b.width != w || r.first < start || r.last > start+w || start < from || start+w > to {
			b.missing |= MissingLoad
		}
		b.first = min(b.first, r.first)
		b.last = max(b.last, r.last)
		b.covered += r.covered
		b.wh += r.load
		b.missing |= r.missing
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := HouseholdSubtotal{PeriodMS: to - from, BucketMS: width, Quality: "estimated", Method: "accepted_interval_balance_v1"}
	for _, b := range buckets {
		if b.missing&MissingPower != 0 || b.covered != b.width || b.first != b.start || b.last != b.start+b.width || b.wh < 0 || !finiteHistoryValue(b.wh) {
			continue
		}
		out.Wh += b.wh
		out.CoverageMS += b.covered
	}
	if out.CoverageMS == 0 {
		return nil, nil
	}
	return &out, nil
}
