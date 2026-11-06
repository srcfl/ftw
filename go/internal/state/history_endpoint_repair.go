package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const endpointRepairPrefix = "endpoint-repair-v1:"

type EndpointRepairRequest struct {
	SinceMS  int64  `json:"since_ms"`
	UntilMS  int64  `json:"until_ms"`
	MaxGapMS int64  `json:"max_gap_ms"`
	Reason   string `json:"reason"`
}
type EndpointRepairMinute struct {
	Flow     string  `json:"flow"`
	StartMS  int64   `json:"start_ms"`
	Wh       float64 `json:"wh"`
	BeforeMS int64   `json:"before_ms"`
	AfterMS  int64   `json:"after_ms"`
	BeforeW  float64 `json:"before_w"`
	AfterW   float64 `json:"after_w"`
}
type EndpointRepairPlan struct {
	SHA256         string                 `json:"sha256"`
	EvidenceSHA256 string                 `json:"evidence_sha256"`
	Generation     string                 `json:"generation"`
	Request        EndpointRepairRequest  `json:"request"`
	Minutes        []EndpointRepairMinute `json:"minutes"`
	Skipped        map[string]string      `json:"skipped"`
}
type endpointApplied struct {
	Plan         EndpointRepairPlan `json:"plan"`
	BackupSHA256 string             `json:"backup_sha256"`
	AppliedAt    string             `json:"applied_at"`
}

func endpointDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p EndpointRepairPlan) digest() string { p.SHA256 = ""; return endpointDigest(p) }
func (r EndpointRepairRequest) validate() error {
	if r.SinceMS%60000 != 0 || r.UntilMS%60000 != 0 || r.UntilMS <= r.SinceMS || r.UntilMS-r.SinceMS > 48*3600000 || r.UntilMS > time.Now().Add(-time.Minute).UnixMilli() || r.MaxGapMS < 60000 || r.MaxGapMS > 30*60000 || strings.TrimSpace(r.Reason) == "" {
		return errors.New("choose a completed minute range up to 48h, max-gap 1–30m and a reason")
	}
	return nil
}

type endpointOriginal struct {
	Start   int64
	Covered int64
	First   int64
	Last    int64
	Load    float64
	PV      float64
	Missing HistoryMissing
}

func (s *Store) endpointOriginals(ctx context.Context, from, to int64) ([]endpointOriginal, error) {
	by := map[int64]*endpointOriginal{}
	err := s.walkSiteEnergy(ctx, from, to, func(r siteEnergyRow) error {
		if r.width != 60000 {
			return errors.New("endpoint repair requires retained minute energy")
		}
		b := by[r.start]
		if b == nil {
			b = &endpointOriginal{Start: r.start, First: r.first}
			by[r.start] = b
		}
		b.Covered += r.covered
		b.First = min(b.First, r.first)
		b.Last = max(b.Last, r.last)
		b.Load += r.load
		b.PV += r.pv
		b.Missing |= r.missing
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]endpointOriginal, 0, (to-from)/60000)
	for at := from; at < to; at += 60000 {
		if b := by[at]; b != nil {
			out = append(out, *b)
		} else {
			out = append(out, endpointOriginal{Start: at, Missing: MissingPower})
		}
	}
	return out, nil
}
func endpointOriginalValid(b endpointOriginal, flow string) bool {
	if b.Covered != 60000 || b.First != b.Start || b.Last != b.Start+60000 {
		return false
	}
	if flow == "pv" {
		return b.Missing&MissingPV == 0 && finiteHistoryValue(b.PV) && b.PV >= 0
	}
	return b.Missing&(MissingPower|BalanceMismatch) == 0 && finiteHistoryValue(b.Load) && b.Load >= 0
}

type endpointPower struct {
	At int64
	W  float64
}

func (s *Store) endpointPowers(ctx context.Context, from, to int64, flow string) ([]endpointPower, error) {
	if flow == "load" {
		originals, err := s.endpointOriginals(ctx, bucketStart(from, 60000), bucketStart(to, 60000)+60000)
		if err != nil {
			return nil, err
		}
		var out []endpointPower
		for _, b := range originals {
			if endpointOriginalValid(b, "load") {
				out = append(out, endpointPower{At: b.Start + 30000, W: b.Load * 60})
			}
		}
		return out, nil
	}
	column, mask := "load_w", MissingPower|BalanceMismatch
	if flow == "pv" {
		column = "-pv_w"
		mask = MissingPV
	}
	// Retained ten-second means are explicit endpoints, not invented raw polls.
	rows, err := s.history.QueryContext(ctx, `SELECT e.last_ms,e.`+strings.TrimPrefix(column, "-")+`,COALESCE(q.missing,0) FROM (
 SELECT *, 'dashboard' kind FROM history_dashboard UNION ALL SELECT *, 'partial_dashboard' kind FROM history_dashboard_partial
 ) e LEFT JOIN history_quality q ON q.kind=e.kind AND q.start_ms=e.start_ms AND q.resolution_ms=e.resolution_ms
 WHERE e.resolution_ms=10000 AND e.last_ms>=? AND e.first_ms<=? ORDER BY e.last_ms`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []endpointPower
	for rows.Next() {
		var p endpointPower
		var missing HistoryMissing
		if err := rows.Scan(&p.At, &p.W, &missing); err != nil {
			return nil, err
		}
		if flow == "pv" {
			p.W = -p.W
		}
		if missing&mask == 0 && finiteHistoryValue(p.W) && p.W >= 0 {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}
func (s *Store) planEndpointRepair(ctx context.Context, r EndpointRepairRequest) (EndpointRepairPlan, error) {
	p := EndpointRepairPlan{Request: r, Skipped: map[string]string{}}
	if err := r.validate(); err != nil {
		return p, err
	}
	if err := s.history.QueryRowContext(ctx, `SELECT name FROM history_migrations WHERE name LIKE 'generation:%'`).Scan(&p.Generation); err != nil {
		return p, err
	}
	original, err := s.endpointOriginals(ctx, r.SinceMS, r.UntilMS)
	if err != nil {
		return p, err
	}
	p.EvidenceSHA256 = endpointDigest(original)
	for _, flow := range []string{"load", "pv"} {
		powers, err := s.endpointPowers(ctx, r.SinceMS-r.MaxGapMS, r.UntilMS+r.MaxGapMS, flow)
		if err != nil {
			return p, err
		}
		for i := 0; i < len(original); {
			if endpointOriginalValid(original[i], flow) {
				i++
				continue
			}
			start := i
			for i < len(original) && !endpointOriginalValid(original[i], flow) {
				i++
			}
			from, to := original[start].Start, original[i-1].Start+60000
			before := sort.Search(len(powers), func(j int) bool { return powers[j].At > from }) - 1
			after := sort.Search(len(powers), func(j int) bool { return powers[j].At >= to })
			key := fmt.Sprintf("%s:%d:%d", flow, from, to)
			if before < 0 || after == len(powers) {
				p.Skipped[key] = "missing valid bounding power"
				continue
			}
			a, b := powers[before], powers[after]
			if b.At <= a.At || b.At-a.At > r.MaxGapMS {
				p.Skipped[key] = "bounding observations exceed max-gap"
				continue
			}
			wh := (a.W + b.W) / 2 / 60
			if !finiteHistoryValue(wh) || wh < 0 || !plausibleEnergy(wh, 60000) {
				p.Skipped[key] = "implausible endpoint estimate"
				continue
			}
			for at := from; at < to; at += 60000 {
				p.Minutes = append(p.Minutes, EndpointRepairMinute{flow, at, wh, a.At, b.At, a.W, b.W})
			}
		}
	}
	p.SHA256 = p.digest()
	return p, nil
}

// PrepareEndpointRepair reads the live box without migrations or a long copy.
// Only completed evidence is considered. It never starts equipment control.
func PrepareEndpointRepair(ctx context.Context, statePath string, r EndpointRepairRequest) (EndpointRepairPlan, error) {
	s, err := OpenBackupSource(statePath)
	if err != nil {
		return EndpointRepairPlan{}, err
	}
	defer s.Close()
	if s.history == nil {
		return EndpointRepairPlan{}, errors.New("primary SQLite history required")
	}
	return s.planEndpointRepair(ctx, r)
}
func (s *Store) endpointRepairs(ctx context.Context, from, to int64) (map[string]map[int64]EndpointRepairMinute, error) {
	out := map[string]map[int64]EndpointRepairMinute{"load": {}, "pv": {}}
	if s.history == nil {
		return out, nil
	}
	rows, err := s.history.QueryContext(ctx, `SELECT value FROM energy_ledger_meta WHERE key GLOB 'endpoint-repair-v1:*' ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var a endpointApplied
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, err
		}
		if a.Plan.SHA256 != a.Plan.digest() || len(a.BackupSHA256) != 64 {
			return nil, errors.New("invalid endpoint repair audit")
		}
		for _, m := range a.Plan.Minutes {
			if m.StartMS < from || m.StartMS+60000 > to {
				continue
			}
			if out[m.Flow] == nil || m.StartMS%60000 != 0 || !finiteHistoryValue(m.Wh) || m.Wh < 0 {
				return nil, errors.New("invalid endpoint correction")
			}
			if old, ok := out[m.Flow][m.StartMS]; ok && old != m {
				return nil, errors.New("conflicting endpoint corrections")
			}
			out[m.Flow][m.StartMS] = m
		}
	}
	return out, rows.Err()
}

// ApplyEndpointRepair writes only audited metadata and derived caches, in one
// short SQLite transaction. It never replaces a live database with a snapshot.
// The independently verified backup and exact plan hash are mandatory.
func ApplyEndpointRepair(ctx context.Context, statePath, backupPath string, p EndpointRepairPlan, sha string) (int, error) {
	if len(sha) != 64 || p.SHA256 != sha || p.digest() != sha {
		return 0, errors.New("exact reviewed plan hash required")
	}
	if err := p.Request.validate(); err != nil {
		return 0, err
	}
	if len(p.Minutes) == 0 {
		return 0, errors.New("no supported corrections")
	}
	source := historyDatabasePath(statePath)
	src, err := filepath.EvalSymlinks(source)
	if err != nil {
		return 0, err
	}
	bak, err := filepath.EvalSymlinks(backupPath)
	if err != nil {
		return 0, err
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	bakInfo, err := os.Stat(bak)
	if err != nil {
		return 0, err
	}
	if os.SameFile(srcInfo, bakInfo) {
		return 0, errors.New("backup must be independent")
	}
	backupSHA, err := repairFileHash(bak)
	if err != nil {
		return 0, err
	}
	backup, err := sql.Open("sqlite", ReadOnlyDatabaseURI(bak))
	if err != nil {
		return 0, err
	}
	ok, checkErr := quickCheckContext(ctx, backup)
	if checkErr != nil || !ok {
		backup.Close()
		if checkErr != nil {
			return 0, fmt.Errorf("backup integrity check: %w", checkErr)
		}
		return 0, errors.New("backup integrity check failed")
	}
	var generation string
	err = backup.QueryRowContext(ctx, `SELECT name FROM history_migrations WHERE name=?`, p.Generation).Scan(&generation)
	backup.Close()
	if err != nil {
		return 0, errors.New("backup generation differs")
	}
	s, err := OpenBackupSource(statePath)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	db, err := sql.Open("sqlite", source+"?_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	// Holding the write reservation keeps Core from changing the evidence between
	// review verification and publication, while WAL readers remain available.
	current, err := s.planEndpointRepair(ctx, p.Request)
	if err != nil {
		return 0, err
	}
	if current.SHA256 != sha {
		return 0, errors.New("historical evidence changed; prepare a new plan")
	}
	existing, err := s.endpointRepairs(ctx, p.Request.SinceMS, p.Request.UntilMS)
	if err != nil {
		return 0, err
	}
	for _, m := range p.Minutes {
		if old, ok := existing[m.Flow][m.StartMS]; ok && old != m {
			return 0, errors.New("overlapping plan conflicts")
		}
	}
	var saved string
	err = conn.QueryRowContext(ctx, `SELECT value FROM energy_ledger_meta WHERE key=?`, endpointRepairPrefix+sha).Scan(&saved)
	if err == nil {
		return len(p.Minutes), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	raw, err := json.Marshal(endpointApplied{p, backupSHA, time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return 0, err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO energy_ledger_meta(key,value) VALUES(?,?)`, endpointRepairPrefix+sha, string(raw)); err != nil {
		return 0, err
	}
	for _, table := range []string{"energy_daily", "energy_daily_partial", "history_daily_quality"} {
		if _, err = conn.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return 0, err
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	return len(p.Minutes), nil
}
