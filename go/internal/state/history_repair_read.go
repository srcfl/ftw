package state

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

const historicalRepairMethod = "historical_power_repair_v1:"

type repairMinute struct {
	StartMS        int64   `json:"start_ms"`
	Wh             float64 `json:"wh"`
	InterpolatedMS int64   `json:"max_interpolated_ms_per_source"`
}

func (s *Store) historicalLoadRepairs(ctx context.Context, from, to int64) (map[int64]repairMinute, error) {
	out := map[int64]repairMinute{}
	if s.history == nil {
		return out, nil
	}
	rows, err := s.history.QueryContext(ctx, `SELECT start_ms,resolution_ms,covered_ms,load_wh,method FROM history_load_estimates WHERE start_ms>=? AND start_ms<?`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p repairMinute
		var width, covered int64
		var method string
		if err := rows.Scan(&p.StartMS, &width, &covered, &p.Wh, &method); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(method, historicalRepairMethod) {
			continue
		}
		if width != ArchiveResolutionMS || covered != width || p.StartMS%width != 0 || !finiteHistoryValue(p.Wh) {
			return nil, errors.New("invalid historical load repair evidence")
		}
		out[p.StartMS] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	endpoints, err := s.endpointRepairs(ctx, from, to)
	if err != nil {
		return nil, err
	}
	for at, m := range endpoints["load"] {
		out[at] = repairMinute{StartMS: at, Wh: m.Wh, InterpolatedMS: m.AfterMS - m.BeforeMS}
	}
	return out, nil
}

// walkHouseholdLoad replaces only derived household energy in the read view.
// Original powers, other flows, quality masks and observed coverage stay intact.
func (s *Store) walkHouseholdLoad(ctx context.Context, from, to int64, repairs map[int64]repairMinute, visit func(siteEnergyRow) error) error {
	err := s.walkSiteEnergy(ctx, from, to, func(r siteEnergyRow) error {
		if _, ok := repairs[r.start]; ok && r.width == ArchiveResolutionMS {
			return nil
		}
		if r.missing&MissingPower != 0 {
			r.missing |= MissingLoad
		}
		r.missing &= MissingLoad | BalanceMismatch | EstimatedLoad
		return visit(r)
	})
	if err != nil {
		return err
	}
	starts := make([]int64, 0, len(repairs))
	for start := range repairs {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	for _, start := range starts {
		p := repairs[start]
		if start < from || start+ArchiveResolutionMS > to {
			continue
		}
		if err := visit(siteEnergyRow{start: start, width: ArchiveResolutionMS, first: start, last: start + ArchiveResolutionMS,
			covered: ArchiveResolutionMS, load: p.Wh, missing: EstimatedLoad}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) repairedLoadBuckets(ctx context.Context, from, to, width int64) ([]sourceLoadBucket, bool, error) {
	repairs, err := s.historicalLoadRepairs(ctx, from, to)
	if err != nil || len(repairs) == 0 {
		return nil, false, err
	}
	byStart := map[int64]*sourceLoadBucket{}
	err = s.walkHouseholdLoad(ctx, from, to, repairs, func(r siteEnergyRow) error {
		w := max(width, r.width)
		start := from + (r.start-from)/w*w
		b := byStart[start]
		if b == nil {
			b = &sourceLoadBucket{start: start, width: w, first: r.first, last: r.last}
			byStart[start] = b
		}
		if b.width != w || r.first < start || r.last > start+w {
			b.missing |= MissingLoad
		}
		b.first = min(b.first, r.first)
		b.last = max(b.last, r.last)
		b.covered += r.covered
		b.n += r.n
		b.wh += r.load
		b.missing |= r.missing
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	out := make([]sourceLoadBucket, 0, len(byStart))
	for _, b := range byStart {
		if b.covered != b.width || b.first != b.start || b.last != b.start+b.width || b.wh < 0 || !finiteHistoryValue(b.wh) {
			b.missing |= MissingLoad
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out, true, nil
}

func (s *Store) applyHistoricalDailyLoad(ctx context.Context, from, to int64, d *DayEnergy) error {
	// A historical correction cannot invent the currently unfinished minute.
	end, err := s.endpointReadUntil(ctx, from, to)
	if err != nil {
		return err
	}
	if end <= from {
		return nil
	}
	b, have, err := s.repairedLoadBuckets(ctx, from, end, end-from)
	if err != nil || !have {
		return err
	}
	if len(b) == 1 && b[0].missing&(MissingLoad|BalanceMismatch) == 0 && b[0].covered == end-from {
		d.LoadWh = b[0].wh
		d.Missing &^= MissingLoad | BalanceMismatch
		d.Missing |= EstimatedLoad
		d.LoadUntilMS = end
	}
	return nil
}

func (s *Store) applyHistoricalLoadChartOnly(ctx context.Context, points []HistoryPoint, from, to, width int64) ([]HistoryPoint, error) {
	if width < ArchiveResolutionMS {
		return points, nil
	} // Do not invent raw poll values.
	b, have, err := s.repairedLoadBuckets(ctx, from, to, width)
	if err != nil || !have {
		return points, err
	}
	byStart := map[int64]sourceLoadBucket{}
	seen := map[int64]bool{}
	repairs, err := s.historicalLoadRepairs(ctx, from, to)
	if err != nil {
		return points, err
	}
	changed := map[int64]bool{}
	for start := range repairs {
		changed[from+(start-from)/width*width] = true
	}
	for _, p := range b {
		byStart[p.start] = p
	}
	for i := range points {
		p := &points[i]
		start := from + (p.TsMs-from)/width*width
		if !changed[start] {
			continue
		}
		v, ok := byStart[start]
		if !ok || v.missing&(MissingLoad|BalanceMismatch) != 0 {
			continue
		}
		p.LoadW = v.wh * 3600000 / float64(v.width)
		seen[start] = true
		p.Missing &^= MissingLoad | BalanceMismatch
		p.Missing |= EstimatedLoad
		var detail map[string]any
		if json.Unmarshal([]byte(p.JSON), &detail) == nil && detail != nil {
			detail["history_load_method"] = "historical_power_repair_v1"
			detail["load_quality"] = "estimated"
			if data, err := json.Marshal(detail); err == nil {
				p.JSON = string(data)
			}
		}
	}
	for _, v := range b {
		if !changed[v.start] || seen[v.start] || v.missing&(MissingLoad|BalanceMismatch) != 0 {
			continue
		}
		points = append(points, HistoryPoint{TsMs: v.start + v.width - 1, FirstMS: v.start, ResolutionMS: v.width, LoadW: v.wh * 3600000 / float64(v.width),
			Missing: MissingGrid | MissingPV | MissingBattery | MissingEV | MissingV2X | MissingSoC | EstimatedLoad,
			JSON:    `{"history_load_method":"historical_power_repair_v1","load_quality":"estimated","forecast_measurement_quality":"aggregate_not_for_training"}`})
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].TsMs < points[j].TsMs })
	return points, nil
}

func (s *Store) applyHistoricalLoadLedgerOnly(ctx context.Context, q EnergyHistoryQuery, points []EnergyLedgerPoint) ([]EnergyLedgerPoint, error) {
	if q.AssetID != "" && q.AssetID != ObservedConsumerAssetID {
		return points, nil
	}
	b, have, err := s.repairedLoadBuckets(ctx, q.SinceMS, q.UntilMS, q.BucketMS)
	if err != nil || !have {
		return points, err
	}
	repairs, err := s.historicalLoadRepairs(ctx, q.SinceMS, q.UntilMS)
	if err != nil {
		return nil, err
	}
	changed := map[int64]bool{}
	for start := range repairs {
		changed[q.SinceMS+(start-q.SinceMS)/q.BucketMS*q.BucketMS] = true
	}
	out := points[:0]
	for _, p := range points {
		if p.Flow != FlowConsumerUse || !changed[p.BucketStartMS] {
			out = append(out, p)
		}
	}
	for _, p := range b {
		if !changed[p.start] {
			continue
		}
		if p.missing&(MissingLoad|BalanceMismatch) != 0 {
			continue
		}
		asset := "system"
		if q.AssetID != "" {
			asset = q.AssetID
		}
		out = append(out, EnergyLedgerPoint{SchemaVersion: EnergyLedgerSchemaVersion, AssetID: asset, Flow: FlowConsumerUse,
			BucketStartMS: p.start, BucketLenMS: p.width, EnergyWh: p.wh, Source: "historical_repair", Quality: "estimated", Provenance: "historical_power_repair_v1", SampleCount: p.n})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BucketStartMS < out[j].BucketStartMS })
	return out, nil
}
