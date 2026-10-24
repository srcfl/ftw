package state

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

// This bit describes the derived read view and its day cache, never raw samples.
const EndpointEstimatedPV HistoryMissing = 1 << 9

func (s *Store) endpointReadUntil(ctx context.Context, from, to int64) (int64, error) {
	end := bucketStart(to, 60000)
	if s.history == nil || to < time.Now().Add(-2*time.Minute).UnixMilli() {
		return end, nil
	}
	var last int64
	err := s.history.QueryRowContext(ctx, `SELECT COALESCE(MAX(last_ms),0) FROM history_source_energy WHERE start_ms>=? AND start_ms<?`, from, to).Scan(&last)
	if err != nil {
		return 0, err
	}
	if last > 0 {
		end = min(end, bucketStart(last, 60000))
	}
	return end, nil
}
func (s *Store) endpointPVBuckets(ctx context.Context, from, to, width int64) ([]sourceLoadBucket, map[int64]EndpointRepairMinute, error) {
	all, err := s.endpointRepairs(ctx, from, to)
	if err != nil {
		return nil, nil, err
	}
	repairs := all["pv"]
	if len(repairs) == 0 {
		return nil, repairs, nil
	}
	by := map[int64]*sourceLoadBucket{}
	add := func(r siteEnergyRow) error {
		w := max(width, r.width)
		start := from + (r.start-from)/w*w
		b := by[start]
		if b == nil {
			b = &sourceLoadBucket{start: start, width: w, first: r.first, last: r.last}
			by[start] = b
		}
		if b.width != w || r.first < start || r.last > start+w {
			b.missing |= MissingPV
		}
		b.first = min(b.first, r.first)
		b.last = max(b.last, r.last)
		b.covered += r.covered
		b.n += r.n
		b.wh += r.pv
		b.missing |= r.missing & MissingPV
		return nil
	}
	err = s.walkSiteEnergy(ctx, from, to, func(r siteEnergyRow) error {
		if _, ok := repairs[r.start]; ok && r.width == 60000 {
			return nil
		}
		return add(r)
	})
	if err != nil {
		return nil, nil, err
	}
	for at, p := range repairs {
		_ = add(siteEnergyRow{start: at, width: 60000, first: at, last: at + 60000, covered: 60000, pv: p.Wh})
	}
	var out []sourceLoadBucket
	for _, b := range by {
		if b.covered != b.width || b.first != b.start || b.last != b.start+b.width || b.wh < 0 || !finiteHistoryValue(b.wh) {
			b.missing |= MissingPV
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out, repairs, nil
}
func (s *Store) applyEndpointDailyPV(ctx context.Context, from, to int64, d *DayEnergy) error {
	end, err := s.endpointReadUntil(ctx, from, to)
	if err != nil || end <= from {
		return err
	}
	b, repairs, err := s.endpointPVBuckets(ctx, from, end, end-from)
	if err != nil || len(repairs) == 0 {
		return err
	}
	if len(b) == 1 && b[0].missing&MissingPV == 0 && b[0].covered == end-from {
		d.PVWh = b[0].wh
		d.Missing &^= MissingPV
		d.Missing |= EndpointEstimatedPV
		d.PVUntilMS = end
	}
	return nil
}
func (s *Store) applyHistoricalLoadChart(ctx context.Context, points []HistoryPoint, from, to, width int64) ([]HistoryPoint, error) {
	out, err := s.applyHistoricalLoadChartOnly(ctx, points, from, to, width)
	if err != nil || width < 60000 {
		return out, err
	}
	all, err := s.endpointRepairs(ctx, from, to)
	if err != nil {
		return nil, err
	}
	changedLoad := map[int64]bool{}
	for at := range all["load"] {
		changedLoad[from+(at-from)/width*width] = true
	}
	for i := range out {
		p := &out[i]
		at := from + (p.TsMs-from)/width*width
		if !changedLoad[at] || p.Missing&MissingLoad != 0 {
			continue
		}
		var detail map[string]any
		_ = json.Unmarshal([]byte(p.JSON), &detail)
		if detail == nil {
			detail = map[string]any{}
		}
		detail["history_load_method"] = "endpoint_mean_v1"
		detail["load_quality"] = "estimated"
		detail["forecast_measurement_quality"] = "aggregate_not_for_training"
		raw, _ := json.Marshal(detail)
		p.JSON = string(raw)
	}
	buckets, repairs, err := s.endpointPVBuckets(ctx, from, to, width)
	if err != nil || len(repairs) == 0 {
		return out, err
	}
	changed := map[int64]bool{}
	for at := range repairs {
		changed[from+(at-from)/width*width] = true
	}
	by := map[int64]sourceLoadBucket{}
	for _, b := range buckets {
		by[b.start] = b
	}
	seen := map[int64]bool{}
	for i := range out {
		p := &out[i]
		at := from + (p.TsMs-from)/width*width
		b, ok := by[at]
		if !ok || !changed[at] || b.missing&MissingPV != 0 {
			continue
		}
		p.PVW = -b.wh * 3600000 / float64(b.width)
		p.Missing &^= MissingPV
		p.Missing |= EstimatedLoad
		seen[at] = true
		var detail map[string]any
		_ = json.Unmarshal([]byte(p.JSON), &detail)
		if detail == nil {
			detail = map[string]any{}
		}
		detail["pv_quality"] = "estimated"
		detail["history_pv_method"] = "endpoint_mean_v1"
		detail["forecast_measurement_quality"] = "aggregate_not_for_training"
		raw, _ := json.Marshal(detail)
		p.JSON = string(raw)
	}
	for _, b := range buckets {
		if !changed[b.start] || seen[b.start] || b.missing&MissingPV != 0 {
			continue
		}
		out = append(out, HistoryPoint{TsMs: b.start + b.width - 1, FirstMS: b.start, ResolutionMS: b.width, PVW: -b.wh * 3600000 / float64(b.width), Missing: MissingGrid | MissingBattery | MissingEV | MissingV2X | MissingSoC | MissingLoad | EstimatedLoad, JSON: `{"pv_quality":"estimated","history_pv_method":"endpoint_mean_v1","forecast_measurement_quality":"aggregate_not_for_training"}`})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TsMs < out[j].TsMs })
	return out, nil
}
func (s *Store) applyHistoricalLoadLedger(ctx context.Context, q EnergyHistoryQuery, points []EnergyLedgerPoint) ([]EnergyLedgerPoint, error) {
	out, err := s.applyHistoricalLoadLedgerOnly(ctx, q, points)
	if err != nil {
		return nil, err
	}
	all, err := s.endpointRepairs(ctx, q.SinceMS, q.UntilMS)
	if err != nil {
		return nil, err
	}
	changedLoad := map[int64]bool{}
	for at := range all["load"] {
		changedLoad[q.SinceMS+(at-q.SinceMS)/q.BucketMS*q.BucketMS] = true
	}
	for i := range out {
		if out[i].Flow == FlowConsumerUse && changedLoad[out[i].BucketStartMS] {
			out[i].Provenance = "endpoint_mean_v1"
		}
	}
	if q.AssetID != "" {
		return out, nil
	}
	buckets, repairs, err := s.endpointPVBuckets(ctx, q.SinceMS, q.UntilMS, q.BucketMS)
	if err != nil || len(repairs) == 0 {
		return out, err
	}
	changed := map[int64]bool{}
	for at := range repairs {
		changed[q.SinceMS+(at-q.SinceMS)/q.BucketMS*q.BucketMS] = true
	}
	filtered := out[:0]
	for _, p := range out {
		if p.Flow != FlowPVGeneration || !changed[p.BucketStartMS] {
			filtered = append(filtered, p)
		}
	}
	out = filtered
	for _, b := range buckets {
		if changed[b.start] && b.missing&MissingPV == 0 {
			out = append(out, EnergyLedgerPoint{SchemaVersion: EnergyLedgerSchemaVersion, AssetID: "system", Flow: FlowPVGeneration, BucketStartMS: b.start, BucketLenMS: b.width, EnergyWh: b.wh, Source: "historical_repair", Quality: "estimated", Provenance: "endpoint_mean_v1", SampleCount: b.n})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BucketStartMS < out[j].BucketStartMS })
	return out, nil
}
