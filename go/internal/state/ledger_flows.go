package state

import "context"

// LedgerFlowBucket is the site energy in one ledger interval, summed across assets.
type LedgerFlowBucket struct {
	StartMs       int64
	LenMs         int64
	ImportWh      float64
	ExportWh      float64
	PVWh          float64
	LoadWh        float64
	EVChargeWh    float64
	EVDischargeWh float64
}

// LedgerFlowBuckets returns ledger intervals that start inside [since, until).
// Empty when the ledger has no energy there.
func (s *Store) LedgerFlowBuckets(ctx context.Context, since, until int64) ([]LedgerFlowBucket, error) {
	if s == nil || s.history == nil || until <= since {
		return nil, nil
	}
	rows, err := s.history.QueryContext(ctx, `
		SELECT bucket_start_ms, bucket_len_ms, flow, SUM(energy_wh)
		FROM energy_ledger_entries
		WHERE schema_version = ?
		  AND bucket_start_ms >= ? AND bucket_start_ms < ?
		  AND energy_wh > 0
		  AND provenance != 'implausible_energy'
		GROUP BY bucket_start_ms, bucket_len_ms, flow
		ORDER BY bucket_start_ms, bucket_len_ms`,
		EnergyLedgerSchemaVersion, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerFlowBucket
	var cur LedgerFlowBucket
	var open bool
	flush := func() {
		if open {
			out = append(out, cur)
		}
	}
	for rows.Next() {
		var start, length int64
		var flow string
		var wh float64
		if err := rows.Scan(&start, &length, &flow, &wh); err != nil {
			return nil, err
		}
		if !plausibleEnergy(wh, length) {
			continue
		}
		if !open || cur.StartMs != start || cur.LenMs != length {
			flush()
			cur = LedgerFlowBucket{StartMs: start, LenMs: length}
			open = true
		}
		switch EnergyFlow(flow) {
		case FlowGridImport:
			cur.ImportWh += wh
		case FlowGridExport:
			cur.ExportWh += wh
		case FlowPVGeneration:
			cur.PVWh += wh
		case FlowConsumerUse:
			cur.LoadWh += wh
		case FlowVehicleCharge:
			cur.EVChargeWh += wh
		case FlowVehicleDischarge:
			cur.EVDischargeWh += wh
		}
	}
	flush()
	return out, rows.Err()
}

// CostSlot is one stored price window that can price ledger energy.
type CostSlot struct {
	StartMs      int64
	EndMs        int64
	ImportOreKwh float64
	SpotOreKwh   float64
}

// CostSlots returns price windows that can cover energy in [since, until).
func (s *Store) CostSlots(ctx context.Context, zone string, since, until int64) ([]CostSlot, error) {
	raw, err := s.loadPriceSlotsForRange(ctx, zone, since, until)
	if err != nil {
		return nil, err
	}
	out := make([]CostSlot, 0, len(raw))
	for _, sl := range raw {
		out = append(out, CostSlot{StartMs: sl.StartMs, EndMs: sl.EndMs, ImportOreKwh: sl.TotalOre, SpotOreKwh: sl.SpotOreKwh})
	}
	return out, nil
}
