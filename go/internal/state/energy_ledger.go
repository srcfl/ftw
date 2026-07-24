package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	EnergyLedgerSchemaVersion = 1
	EnergyLedgerBucketMS      = int64(5 * 60 * 1000)
	energyLedgerMaxPowerGapMS = int64(2 * 60 * 1000)
	// One megawatt is far above a home site's physical range, but low enough
	// to reject corrupt cumulative-counter reads before they become trusted
	// energy. Reads use the same bound so old bad rows cannot distort totals.
	energyLedgerMaxPlausiblePowerW = 1_000_000.0

	// Keep recent ledger detail aligned with the legacy history hot tier.
	// Older energy is additive, so hourly totals preserve its contract while
	// bounding state.db growth. The API cannot read beyond the hard retention.
	EnergyLedgerDetailedRetention = 30 * 24 * time.Hour
	EnergyLedgerRetention         = 2 * 365 * 24 * time.Hour
	EnergyLedgerRollupBucketMS    = int64(time.Hour / time.Millisecond)
)

var energyLedgerMaintenanceChunkMS = int64(7 * 24 * time.Hour / time.Millisecond)

type EnergyFlow string

const (
	FlowGridImport       EnergyFlow = "grid_import"
	FlowGridExport       EnergyFlow = "grid_export"
	FlowBatteryCharge    EnergyFlow = "battery_charge"
	FlowBatteryDischarge EnergyFlow = "battery_discharge"
	FlowPVGeneration     EnergyFlow = "pv_generation"
	FlowConsumerUse      EnergyFlow = "consumer_use"
	FlowVehicleCharge    EnergyFlow = "vehicle_charge"
	FlowVehicleDischarge EnergyFlow = "vehicle_discharge"
)

type EnergyAssetKind string

const (
	AssetGridMeter        EnergyAssetKind = "grid_meter"
	AssetBattery          EnergyAssetKind = "battery"
	AssetPV               EnergyAssetKind = "pv"
	AssetObservedConsumer EnergyAssetKind = "observed_consumer"
	AssetVehicleCharger   EnergyAssetKind = "vehicle_charger"
)

// EnergyObservation is one current cumulative-counter and/or power reading.
// PowerW is an unsigned magnitude for Flow; sign classification is performed
// by the caller at the site-convention boundary. CounterWh is cumulative and
// directional, so simultaneous import/export or charge/discharge remain two
// independent observations.
type EnergyObservation struct {
	AssetID   string
	DeviceID  string
	AssetKind EnergyAssetKind
	Label     string
	ReadOnly  bool
	Flow      EnergyFlow
	AtMs      int64
	PowerW    *float64
	CounterWh *float64
}

type EnergyAsset struct {
	AssetID     string          `json:"asset_id"`
	DeviceID    string          `json:"device_id,omitempty"`
	Kind        EnergyAssetKind `json:"kind"`
	Label       string          `json:"label,omitempty"`
	ReadOnly    bool            `json:"read_only"`
	FirstSeenMS int64           `json:"first_seen_ms"`
	LastSeenMS  int64           `json:"last_seen_ms"`
}

type EnergyLedgerPoint struct {
	SchemaVersion int        `json:"schema_version"`
	AssetID       string     `json:"asset_id"`
	Flow          EnergyFlow `json:"flow"`
	BucketStartMS int64      `json:"bucket_start_ms"`
	BucketLenMS   int64      `json:"bucket_len_ms"`
	EnergyWh      float64    `json:"energy_wh"`
	Source        string     `json:"source"`
	Quality       string     `json:"quality"`
	Provenance    string     `json:"provenance"`
	SampleCount   int64      `json:"sample_count"`
}

// EnergyHistoryQuery is intentionally bounded by the API before it reaches
// SQL. AssetID empty means a system-wide rollup; otherwise only that stable
// asset is returned. BucketMS must be at least EnergyLedgerBucketMS.
type EnergyHistoryQuery struct {
	AssetID  string
	SinceMS  int64
	UntilMS  int64
	BucketMS int64
	Limit    int
}

func energyAssetID(deviceID string, kind EnergyAssetKind) string {
	return deviceID + "/" + string(kind)
}

// HardwareEnergyAssetID returns the stable ledger identity for one role on a
// registered hardware device. It deliberately accepts no driver/config name.
func HardwareEnergyAssetID(deviceID string, kind EnergyAssetKind) string {
	if strings.TrimSpace(deviceID) == "" {
		return ""
	}
	return energyAssetID(deviceID, kind)
}

func validEnergyFlow(flow EnergyFlow) bool {
	switch flow {
	case FlowGridImport, FlowGridExport, FlowBatteryCharge, FlowBatteryDischarge,
		FlowPVGeneration, FlowConsumerUse, FlowVehicleCharge, FlowVehicleDischarge:
		return true
	}
	return false
}

func (s *Store) ensureEnergyLedgerVersion() error {
	var raw string
	if err := s.db.QueryRow(`SELECT value FROM energy_ledger_meta WHERE key = 'schema_version'`).Scan(&raw); err != nil {
		return fmt.Errorf("energy ledger schema version: %w", err)
	}
	version, err := strconv.Atoi(raw)
	if err != nil || version != EnergyLedgerSchemaVersion {
		return fmt.Errorf("unsupported energy ledger schema version %q (core supports %d)", raw, EnergyLedgerSchemaVersion)
	}
	return nil
}

func validateEnergyObservation(o EnergyObservation) error {
	if strings.TrimSpace(o.AssetID) == "" || o.AtMs <= 0 || !validEnergyFlow(o.Flow) {
		return fmt.Errorf("invalid energy observation identity/time/flow")
	}
	if o.PowerW == nil && o.CounterWh == nil {
		return errors.New("energy observation requires power or counter")
	}
	for name, v := range map[string]*float64{"power": o.PowerW, "counter": o.CounterWh} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0) {
			return fmt.Errorf("energy observation %s must be finite and non-negative", name)
		}
	}
	return nil
}

// recordEnergyObservationsTx updates ledger cursors and appends bucketed
// deltas. It is called from RecordTickWithEnergy so history, scalar samples,
// cursor movement, and ledger energy commit atomically.
func recordEnergyObservationsTx(tx *sql.Tx, observations []EnergyObservation) error {
	for _, o := range observations {
		if err := validateEnergyObservation(o); err != nil {
			return err
		}
		readOnly := o.ReadOnly || o.AssetKind == AssetObservedConsumer
		if _, err := tx.Exec(`INSERT INTO energy_assets(
			asset_id, device_id, kind, label, read_only, first_seen_ms, last_seen_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(asset_id) DO UPDATE SET
			device_id = CASE WHEN excluded.device_id <> '' THEN excluded.device_id ELSE energy_assets.device_id END,
			kind = excluded.kind, label = excluded.label,
			read_only = excluded.read_only,
			last_seen_ms = MAX(energy_assets.last_seen_ms, excluded.last_seen_ms)`,
			o.AssetID, o.DeviceID, o.AssetKind, o.Label, readOnly, o.AtMs, o.AtMs); err != nil {
			return fmt.Errorf("upsert energy asset: %w", err)
		}
		if err := recordEnergyObservationTx(tx, o); err != nil {
			return fmt.Errorf("record energy %s %s: %w", o.AssetID, o.Flow, err)
		}
	}
	return nil
}

type ledgerCursor struct {
	value float64
	tsMS  int64
}

func loadLedgerCursor(tx *sql.Tx, assetID string, flow EnergyFlow, kind string) (ledgerCursor, bool, error) {
	var c ledgerCursor
	err := tx.QueryRow(`SELECT value, ts_ms FROM energy_ledger_cursors
		WHERE asset_id = ? AND flow = ? AND cursor_kind = ?`, assetID, flow, kind).
		Scan(&c.value, &c.tsMS)
	if errors.Is(err, sql.ErrNoRows) {
		return ledgerCursor{}, false, nil
	}
	return c, err == nil, err
}

func saveLedgerCursor(tx *sql.Tx, o EnergyObservation, kind string, value float64) error {
	_, err := tx.Exec(`INSERT INTO energy_ledger_cursors(asset_id, flow, cursor_kind, value, ts_ms)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(asset_id, flow, cursor_kind) DO UPDATE SET
			value = excluded.value, ts_ms = excluded.ts_ms
		WHERE excluded.ts_ms >= energy_ledger_cursors.ts_ms`, o.AssetID, o.Flow, kind, value, o.AtMs)
	return err
}

func recordEnergyObservationTx(tx *sql.Tx, o EnergyObservation) error {
	power, havePower, err := loadLedgerCursor(tx, o.AssetID, o.Flow, "power")
	if err != nil {
		return err
	}
	counter, haveCounter, err := loadLedgerCursor(tx, o.AssetID, o.Flow, "counter")
	if err != nil {
		return err
	}

	if o.CounterWh != nil {
		// If power fallback advanced while the hardware counter was absent,
		// re-baseline on return. Applying the full counter delta would count
		// the fallback interval twice.
		switch {
		case !haveCounter:
			if err := addLedgerMarker(tx, o, "hardware_counter", "gap", "counter_baseline"); err != nil {
				return err
			}
		case havePower && power.tsMS > counter.tsMS:
			// Power fallback already covered everything through power.tsMS.
			// Integrate only the final interval to this returning sample, then
			// re-baseline the counter. Applying its cumulative delta would count
			// the fallback interval twice.
			if o.PowerW != nil {
				if err := integratePower(tx, o, power, *o.PowerW, "counter_resume_fallback"); err != nil {
					return err
				}
			}
			if err := addLedgerMarker(tx, o, "hardware_counter", "recovered", "counter_resumed"); err != nil {
				return err
			}
		case o.AtMs > counter.tsMS:
			delta := *o.CounterWh - counter.value
			if delta >= -1e-6 {
				if delta < 0 {
					delta = 0
				}
				if !plausibleEnergy(delta, o.AtMs-counter.tsMS) {
					if err := addLedgerMarker(tx, o, "hardware_counter", "invalid", "counter_jump"); err != nil {
						return err
					}
				} else {
					quality, provenance := "measured", "counter"
					if o.AtMs-counter.tsMS > energyLedgerMaxPowerGapMS {
						quality, provenance = "recovered", "counter_gap"
					}
					if err := addLedgerInterval(tx, o, counter.tsMS, o.AtMs, delta,
						"hardware_counter", quality, provenance); err != nil {
						return err
					}
				}
			} else {
				if err := addLedgerMarker(tx, o, "hardware_counter", "reset", "counter_reset"); err != nil {
					return err
				}
				if o.PowerW != nil && havePower {
					if err := integratePower(tx, o, power, *o.PowerW, "counter_reset_fallback"); err != nil {
						return err
					}
				}
			}
		}
		if err := saveLedgerCursor(tx, o, "counter", *o.CounterWh); err != nil {
			return err
		}
	} else if o.PowerW != nil && havePower {
		if err := integratePower(tx, o, power, *o.PowerW, "power"); err != nil {
			return err
		}
	} else if o.PowerW != nil {
		if err := addLedgerMarker(tx, o, "power_telemetry", "gap", "power_baseline"); err != nil {
			return err
		}
	}

	// Keep the power cursor warm even while a counter is authoritative so it
	// can cover the exact reset/missing-counter interval without a blind spot.
	if o.PowerW != nil {
		return saveLedgerCursor(tx, o, "power", *o.PowerW)
	}
	return nil
}

func integratePower(tx *sql.Tx, o EnergyObservation, prev ledgerCursor, current float64, provenance string) error {
	dt := o.AtMs - prev.tsMS
	if dt <= 0 {
		return nil
	}
	if dt > energyLedgerMaxPowerGapMS {
		return addLedgerMarker(tx, o, "power_telemetry", "gap", "power_gap")
	}
	wh := (prev.value + current) * 0.5 * float64(dt) / 3_600_000
	return addLedgerInterval(tx, o, prev.tsMS, o.AtMs, wh,
		"power_telemetry", "integrated", provenance)
}

func addLedgerMarker(tx *sql.Tx, o EnergyObservation, source, quality, provenance string) error {
	start := (o.AtMs / EnergyLedgerBucketMS) * EnergyLedgerBucketMS
	return upsertLedgerEntry(tx, o, start, 0, source, quality, provenance)
}

// addLedgerInterval distributes an interval over fixed buckets by temporal
// overlap. Counter deltas across a gap are therefore not dumped into the final
// bucket, while provenance still says that their intra-gap shape was recovered.
func addLedgerInterval(tx *sql.Tx, o EnergyObservation, fromMS, toMS int64, energyWh float64, source, quality, provenance string) error {
	if toMS <= fromMS || energyWh < 0 {
		return nil
	}
	duration := float64(toMS - fromMS)
	for start := (fromMS / EnergyLedgerBucketMS) * EnergyLedgerBucketMS; start < toMS; start += EnergyLedgerBucketMS {
		end := start + EnergyLedgerBucketMS
		overlapStart, overlapEnd := max64(fromMS, start), min64(toMS, end)
		if overlapEnd <= overlapStart {
			continue
		}
		part := energyWh * float64(overlapEnd-overlapStart) / duration
		if err := upsertLedgerEntry(tx, o, start, part, source, quality, provenance); err != nil {
			return err
		}
	}
	return nil
}

func plausibleEnergy(energyWh float64, durationMS int64) bool {
	if energyWh < 0 || durationMS <= 0 {
		return false
	}
	maxWh := energyLedgerMaxPlausiblePowerW * float64(durationMS) / 3_600_000
	return energyWh <= maxWh+1e-6
}

func upsertLedgerEntry(tx *sql.Tx, o EnergyObservation, bucketStart int64, energyWh float64, source, quality, provenance string) error {
	_, err := tx.Exec(`INSERT INTO energy_ledger_entries(
		schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms,
		energy_wh, source, quality, provenance, sample_count, observed_at_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
	ON CONFLICT(schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, source, quality, provenance)
	DO UPDATE SET energy_wh = energy_ledger_entries.energy_wh + excluded.energy_wh,
		sample_count = energy_ledger_entries.sample_count + 1,
		observed_at_ms = MAX(energy_ledger_entries.observed_at_ms, excluded.observed_at_ms)`,
		EnergyLedgerSchemaVersion, o.AssetID, o.Flow, bucketStart, EnergyLedgerBucketMS,
		energyWh, source, quality, provenance, o.AtMs)
	return err
}

func (s *Store) EnergyAssets() ([]EnergyAsset, error) {
	rows, err := s.db.Query(`SELECT asset_id, device_id, kind, label, read_only,
		first_seen_ms, last_seen_ms FROM energy_assets ORDER BY kind, asset_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EnergyAsset, 0)
	for rows.Next() {
		var a EnergyAsset
		if err := rows.Scan(&a.AssetID, &a.DeviceID, &a.Kind, &a.Label, &a.ReadOnly,
			&a.FirstSeenMS, &a.LastSeenMS); err != nil {
			return out, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LoadEnergyHistory performs the only ledger history SQL. Requested buckets
// may combine stored buckets but never subdivide them: hourly rollups therefore
// remain honest hourly points even when BucketMS asks for five-minute detail.
func (s *Store) LoadEnergyHistory(q EnergyHistoryQuery) ([]EnergyLedgerPoint, bool, error) {
	if q.SinceMS < 0 || q.UntilMS <= q.SinceMS || q.BucketMS < EnergyLedgerBucketMS || q.Limit < 1 {
		return nil, false, errors.New("invalid energy history bounds")
	}
	assetID := q.AssetID
	rows, err := s.db.Query(`WITH aggregated AS (
		SELECT
			? AS schema_version,
			CASE WHEN ? = '' THEN 'system' ELSE asset_id END AS result_asset_id,
			flow,
			CASE WHEN bucket_len_ms > ? THEN bucket_start_ms
				ELSE ? + ((bucket_start_ms - ?) / ?) * ? END AS result_bucket_start,
			CASE WHEN bucket_len_ms > ? THEN bucket_len_ms ELSE ? END AS result_bucket_len,
			SUM(energy_wh) AS energy_wh,
			source, quality, provenance, SUM(sample_count) AS sample_count
		FROM energy_ledger_entries
		WHERE schema_version = ?
		  AND bucket_start_ms < ?
		  AND bucket_start_ms + bucket_len_ms > ?
		  AND (? = '' OR asset_id = ?)
		GROUP BY result_asset_id, flow, result_bucket_start, result_bucket_len,
			source, quality, provenance
	), ranked AS (
		SELECT *, DENSE_RANK() OVER (ORDER BY result_bucket_start) AS bucket_rank
		FROM aggregated
	)
	SELECT schema_version, result_asset_id, flow, result_bucket_start,
		result_bucket_len, energy_wh, source, quality, provenance, sample_count,
		bucket_rank
	FROM ranked
	WHERE bucket_rank <= ?
	ORDER BY result_bucket_start, result_asset_id, flow, source, quality, provenance`,
		EnergyLedgerSchemaVersion, assetID,
		q.BucketMS, q.SinceMS, q.SinceMS, q.BucketMS, q.BucketMS,
		q.BucketMS, q.BucketMS,
		EnergyLedgerSchemaVersion, q.UntilMS, q.SinceMS, assetID, assetID, q.Limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]EnergyLedgerPoint, 0)
	truncated := false
	for rows.Next() {
		var p EnergyLedgerPoint
		var rank int
		if err := rows.Scan(&p.SchemaVersion, &p.AssetID, &p.Flow, &p.BucketStartMS,
			&p.BucketLenMS, &p.EnergyWh, &p.Source, &p.Quality, &p.Provenance,
			&p.SampleCount, &rank); err != nil {
			return out, false, err
		}
		if rank > q.Limit {
			truncated = true
			continue
		}
		if p.EnergyWh > 0 && !plausibleEnergy(p.EnergyWh, p.BucketLenMS) {
			p.Quality = "invalid"
			p.Provenance = "implausible_energy"
		}
		out = append(out, p)
	}
	return out, truncated, rows.Err()
}

// PruneEnergyLedger converts completed five-minute buckets older than 30 days
// to hourly buckets and removes all ledger entries beyond the API's two-year
// horizon. Work is split into week-sized transactions so a missed-maintenance
// backlog cannot hold SQLite's single writer lock for minutes on a Pi.
func (s *Store) PruneEnergyLedger(ctx context.Context, now time.Time) (rolled, expired int64, err error) {
	rollupCutoff := now.UnixMilli() - EnergyLedgerDetailedRetention.Milliseconds()
	rollupCutoff = (rollupCutoff / EnergyLedgerRollupBucketMS) * EnergyLedgerRollupBucketMS
	for {
		var minTS sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT MIN(bucket_start_ms)
			FROM energy_ledger_entries
			WHERE bucket_len_ms < ? AND bucket_start_ms < ?`,
			EnergyLedgerRollupBucketMS, rollupCutoff).Scan(&minTS); err != nil {
			return rolled, expired, err
		}
		if !minTS.Valid {
			break
		}
		chunkEnd := min64(minTS.Int64+energyLedgerMaintenanceChunkMS, rollupCutoff)
		chunkEnd = (chunkEnd / EnergyLedgerRollupBucketMS) * EnergyLedgerRollupBucketMS
		if chunkEnd <= minTS.Int64 {
			chunkEnd = minTS.Int64 + EnergyLedgerRollupBucketMS
		}
		n, err := s.rollupEnergyLedgerChunk(ctx, minTS.Int64, chunkEnd)
		if err != nil {
			return rolled, expired, err
		}
		rolled += n
	}

	expireCutoff := now.UnixMilli() - EnergyLedgerRetention.Milliseconds()
	for {
		var minTS sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT MIN(bucket_start_ms)
			FROM energy_ledger_entries WHERE bucket_start_ms < ?`, expireCutoff).Scan(&minTS); err != nil {
			return rolled, expired, err
		}
		if !minTS.Valid {
			break
		}
		chunkEnd := min64(minTS.Int64+energyLedgerMaintenanceChunkMS, expireCutoff)
		if chunkEnd <= minTS.Int64 {
			chunkEnd = minTS.Int64 + EnergyLedgerRollupBucketMS
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM energy_ledger_entries
			WHERE bucket_start_ms >= ? AND bucket_start_ms < ?`, minTS.Int64, chunkEnd)
		if err != nil {
			return rolled, expired, err
		}
		n, _ := res.RowsAffected()
		expired += n
	}
	return rolled, expired, nil
}

func (s *Store) rollupEnergyLedgerChunk(ctx context.Context, fromMS, toMS int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO energy_ledger_entries(
		schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms,
		energy_wh, source, quality, provenance, sample_count, observed_at_ms
	)
	SELECT schema_version, asset_id, flow,
		(bucket_start_ms / ?) * ?, ?, SUM(energy_wh), source, quality, provenance,
		SUM(sample_count), MAX(observed_at_ms)
	FROM energy_ledger_entries
	WHERE bucket_len_ms < ? AND bucket_start_ms >= ? AND bucket_start_ms < ?
	GROUP BY schema_version, asset_id, flow,
		(bucket_start_ms / ?) * ?, source, quality, provenance
	ON CONFLICT(schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, source, quality, provenance)
	DO UPDATE SET
		energy_wh = energy_ledger_entries.energy_wh + excluded.energy_wh,
		sample_count = energy_ledger_entries.sample_count + excluded.sample_count,
		observed_at_ms = MAX(energy_ledger_entries.observed_at_ms, excluded.observed_at_ms)`,
		EnergyLedgerRollupBucketMS, EnergyLedgerRollupBucketMS, EnergyLedgerRollupBucketMS,
		EnergyLedgerRollupBucketMS, fromMS, toMS,
		EnergyLedgerRollupBucketMS, EnergyLedgerRollupBucketMS); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM energy_ledger_entries
		WHERE bucket_len_ms < ? AND bucket_start_ms >= ? AND bucket_start_ms < ?`,
		EnergyLedgerRollupBucketMS, fromMS, toMS)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
