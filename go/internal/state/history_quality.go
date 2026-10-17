package state

import (
	"context"
	"database/sql"
)

// Missing fields survive chart averaging, retention and energy caching. A
// bucket with any missing contribution stays unknown; measured raw series
// remain available separately. Zero is only a measured zero or an absent flow.
type HistoryMissing uint16

const (
	MissingGrid HistoryMissing = 1 << iota
	MissingPV
	MissingBattery
	MissingLoad
	MissingSoC
	MissingEV
	MissingV2X
	BalanceMismatch
	EstimatedLoad
)
const MissingPower = MissingGrid | MissingPV | MissingBattery | MissingLoad | MissingEV | MissingV2X
const historyQualitySchema = `CREATE TABLE IF NOT EXISTS history_quality(kind TEXT NOT NULL,start_ms INTEGER NOT NULL,resolution_ms INTEGER NOT NULL,missing INTEGER NOT NULL,PRIMARY KEY(kind,start_ms,resolution_ms)) WITHOUT ROWID`
const historyDailyQualitySchema = `CREATE TABLE IF NOT EXISTS history_daily_quality(day TEXT PRIMARY KEY,missing INTEGER NOT NULL,n INTEGER NOT NULL) WITHOUT ROWID`

func (p *HistoryPoint) ClearMissingValues() {
	if p.Missing&MissingGrid != 0 {
		p.GridW = 0
	}
	if p.Missing&MissingPV != 0 {
		p.PVW = 0
	}
	if p.Missing&MissingBattery != 0 {
		p.BatW = 0
	}
	if p.Missing&MissingLoad != 0 {
		p.LoadW = 0
	}
	if p.Missing&MissingSoC != 0 {
		p.BatSoC = 0
	}
}
func (m HistoryMissing) Value(flag HistoryMissing, value float64) any {
	if m&flag != 0 {
		return nil
	}
	return value
}
func (m HistoryMissing) Quality() string {
	if m&BalanceMismatch != 0 {
		return "balance_mismatch"
	}
	if m&MissingPower != 0 {
		return "incomplete"
	}
	if m&EstimatedLoad != 0 {
		return "estimated"
	}
	return "complete"
}
func putHistoryQuality(ctx context.Context, tx *sql.Tx, kind string, start, width int64, missing HistoryMissing) error {
	if missing == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO history_quality VALUES(?,?,?,?) ON CONFLICT(kind,start_ms,resolution_ms) DO UPDATE SET missing=missing|excluded.missing`, kind, start, width, missing)
	return err
}

// SQL has no bitwise-OR aggregate. MAX per bit preserves unknown evidence
// across every source row, including mixed complete/incomplete buckets.
const historyMissingAggregate = `MAX(missing&1)|MAX(missing&2)|MAX(missing&4)|MAX(missing&8)|MAX(missing&16)|MAX(missing&32)|MAX(missing&64)|MAX(missing&128)`
