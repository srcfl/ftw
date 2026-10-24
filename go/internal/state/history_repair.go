package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Every electrical flow must be selected or explicitly declared absent. In
// particular, select site-side AC battery power, never a cabinet DC counter.
type RepairPowerSource struct {
	Flow   string `json:"flow"`
	Driver string `json:"driver,omitempty"`
	Metric string `json:"metric,omitempty"`
	Absent bool   `json:"absent,omitempty"`
}

type HistoryRepairRequest struct {
	SinceMS  int64               `json:"since_ms"`
	UntilMS  int64               `json:"until_ms"`
	MaxGapMS int64               `json:"max_gap_ms"`
	Reason   string              `json:"reason"`
	Sources  []RepairPowerSource `json:"sources"`
}

type HistoryRepairPlan struct {
	SHA256               string               `json:"sha256"`
	Request              HistoryRepairRequest `json:"request"`
	CreatedAt            string               `json:"created_at"`
	BackupStateSHA256    string               `json:"backup_state_sha256"`
	BackupHistorySHA256  string               `json:"backup_history_sha256"`
	WorkingHistorySHA256 string               `json:"working_history_sha256"`
	WorkingStateSHA256   string               `json:"working_state_sha256"`
	Minutes              []repairMinute       `json:"minutes"`
	Skipped              map[int64]string     `json:"skipped"`
}

func (p HistoryRepairPlan) digest() string {
	p.SHA256 = ""
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (r HistoryRepairRequest) validate() error {
	if r.SinceMS%60000 != 0 || r.UntilMS%60000 != 0 || r.UntilMS <= r.SinceMS || r.UntilMS-r.SinceMS > 48*3600000 || r.UntilMS > time.Now().Add(-time.Minute).UnixMilli() {
		return errors.New("choose completed minute boundaries, at most 48 hours, ending at least one minute ago")
	}
	if r.MaxGapMS < 1000 || r.MaxGapMS > 180000 || strings.TrimSpace(r.Reason) == "" {
		return errors.New("reason and max-gap between 1 and 180 seconds are required")
	}
	seen := map[string]bool{}
	electrical := map[string]bool{}
	for _, v := range r.Sources {
		if v.Flow != "grid" && v.Flow != "pv" && v.Flow != "battery" && v.Flow != "ev" && v.Flow != "v2x" {
			return errors.New("unknown electrical flow")
		}
		if seen[v.Flow] {
			return errors.New("duplicate electrical flow")
		}
		seen[v.Flow] = true
		if v.Absent {
			if v.Flow == "grid" || v.Driver != "" || v.Metric != "" {
				return errors.New("grid cannot be absent; absent flows cannot name a source")
			}
			continue
		}
		if v.Driver == "" || !strings.HasSuffix(v.Metric, "_w") {
			return errors.New("each present flow needs an explicit driver and power metric ending in _w")
		}
		key := v.Driver + ":" + v.Metric
		if electrical[key] {
			return errors.New("one electrical source cannot be counted twice")
		}
		electrical[key] = true
	}
	if len(seen) != 5 {
		return errors.New("select grid, pv, battery, ev and v2x; explicitly use none for absent flows")
	}
	return nil
}

type repairPowerPoint struct {
	at    int64
	watts float64
}

// Aggregates are retained means, not reconstructed raw polls. Linear
// allocation and any bridge are estimates recorded in the correction plan.
func repairIntegral(points []repairPowerPoint, from, to, maxGap int64) (float64, int64, bool) {
	var wh float64
	var covered, interpolated int64
	first := max(1, sort.Search(len(points), func(i int) bool { return points[i].at >= from }))
	for i := first; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if a.at >= to {
			break
		}
		lo, hi := max(from, a.at), min(to, b.at)
		if hi <= lo {
			continue
		}
		dt := b.at - a.at
		if dt <= 0 || dt > maxGap {
			return 0, 0, false
		}
		left := a.watts + (b.watts-a.watts)*float64(lo-a.at)/float64(dt)
		right := a.watts + (b.watts-a.watts)*float64(hi-a.at)/float64(dt)
		wh += (left + right) / 2 * float64(hi-lo) / 3600000
		covered += hi - lo
		if dt > maxLoadEstimateTickMS {
			interpolated += hi - lo
		}
	}
	return wh, interpolated, covered == to-from && finiteHistoryValue(wh)
}

func (s *Store) planHistoryRepair(ctx context.Context, r HistoryRepairRequest) ([]repairMinute, map[int64]string, error) {
	if err := r.validate(); err != nil {
		return nil, nil, err
	}
	original := map[int64]*sourceLoadBucket{}
	err := s.walkSiteEnergy(ctx, r.SinceMS, r.UntilMS, func(v siteEnergyRow) error {
		if v.width != ArchiveResolutionMS {
			return errors.New("repair needs retained minute energy; coarse historical intervals are not split")
		}
		b := original[v.start]
		if b == nil {
			b = &sourceLoadBucket{}
			original[v.start] = b
		}
		b.covered += v.covered
		b.wh += v.load
		b.missing |= v.missing
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	series := map[string][]repairPowerPoint{}
	for _, v := range r.Sources {
		if v.Absent {
			continue
		}
		var unit string
		if err := s.history.QueryRowContext(ctx, `SELECT COALESCE(unit,'') FROM ts_metrics WHERE name=?`, v.Metric).Scan(&unit); err != nil {
			return nil, nil, fmt.Errorf("source %s:%s: %w", v.Driver, v.Metric, err)
		}
		if unit != "" && unit != "W" {
			return nil, nil, errors.New("repair source is not in watts")
		}
		p, err := s.LoadSeriesBucketsOrRawContext(ctx, v.Driver, v.Metric, r.SinceMS-r.MaxGapMS-60000, r.UntilMS+r.MaxGapMS+60000, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, q := range p {
			if q.ResolutionMS > 60000 || !finiteHistoryValue(q.V) {
				return nil, nil, errors.New("repair requires finite power evidence at minute resolution or finer")
			}
			at := q.TsMs
			if q.FirstMS > 0 {
				at = q.FirstMS + (q.TsMs-q.FirstMS)/2
			}
			series[v.Flow] = append(series[v.Flow], repairPowerPoint{at, q.V})
		}
		for i := 1; i < len(series[v.Flow]); i++ {
			if series[v.Flow][i].at <= series[v.Flow][i-1].at {
				return nil, nil, errors.New("conflicting source observation times")
			}
		}
	}
	var minutes []repairMinute
	skipped := map[int64]string{}
	for at := r.SinceMS; at < r.UntilMS; at += 60000 {
		b := original[at]
		if b != nil && b.covered == 60000 && b.missing&MissingPower == 0 && b.wh >= 0 {
			continue
		}
		var existing int
		if err := s.history.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_load_estimates WHERE start_ms=?`, at).Scan(&existing); err != nil {
			return nil, nil, err
		}
		if existing != 0 {
			skipped[at] = "existing estimate preserved"
			continue
		}
		p := repairMinute{StartMS: at}
		valid := true
		for _, v := range r.Sources {
			if v.Absent {
				continue
			}
			wh, bridge, ok := repairIntegral(series[v.Flow], at, at+60000, r.MaxGapMS)
			if !ok {
				skipped[at] = "insufficient bounded power evidence: " + v.Flow
				valid = false
				break
			}
			if v.Flow == "grid" {
				p.Wh += wh
			} else {
				p.Wh -= wh
			}
			p.InterpolatedMS = max(p.InterpolatedMS, bridge)
		}
		if valid {
			if !plausibleEnergy(absRepairWh(p.Wh), 60000) {
				skipped[at] = "implausible reconstructed energy"
				continue
			}
			minutes = append(minutes, p)
		}
	}
	return minutes, skipped, nil
}

func absRepairWh(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func repairFileHash(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("repair files must be regular files, not links")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func repairCopyFile(from, to string) error {
	a, err := os.Open(from)
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(b, a)
	syncErr := b.Sync()
	return errors.Join(copyErr, syncErr, b.Close())
}

func writeRepairJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}

// PrepareHistoryRepair never opens the owner's databases for writing. It
// snapshots state and history, then operates exclusively on the new copies.
func PrepareHistoryRepair(ctx context.Context, statePath, workdir string, r HistoryRepairRequest) (HistoryRepairPlan, error) {
	var plan HistoryRepairPlan
	if err := r.validate(); err != nil {
		return plan, err
	}
	if err := os.Mkdir(workdir, 0700); err != nil {
		return plan, err
	}
	for _, dir := range []string{"backup", "corrected"} {
		if err := os.Mkdir(filepath.Join(workdir, dir), 0700); err != nil {
			return plan, err
		}
	}
	s, err := OpenBackupSource(statePath)
	if err != nil {
		return plan, err
	}
	if s.history == nil {
		s.Close()
		return plan, errors.New("repair requires primary SQLite history")
	}
	if err := EnsureDiskSpace(workdir, 3*s.BackupSourceBytes()+backupScratchHeadroom); err != nil {
		s.Close()
		return plan, err
	}
	_, err = s.db.ExecContext(ctx, `VACUUM INTO ?`, filepath.Join(workdir, "backup", "state.db"))
	if err == nil {
		_, err = s.history.ExecContext(ctx, `VACUUM INTO ?`, filepath.Join(workdir, "backup", HistoryFilename))
	}
	closeErr := s.Close()
	if err != nil {
		return plan, err
	}
	if closeErr != nil {
		return plan, closeErr
	}
	for _, name := range []string{"state.db", HistoryFilename} {
		if err := repairCopyFile(filepath.Join(workdir, "backup", name), filepath.Join(workdir, "corrected", name)); err != nil {
			return plan, err
		}
	}
	plan.BackupStateSHA256, err = repairFileHash(filepath.Join(workdir, "backup", "state.db"))
	if err != nil {
		return plan, err
	}
	plan.BackupHistorySHA256, err = repairFileHash(filepath.Join(workdir, "backup", HistoryFilename))
	if err != nil {
		return plan, err
	}
	c, err := Open(filepath.Join(workdir, "corrected", "state.db"))
	if err != nil {
		return plan, err
	}
	var integrity string
	err = c.history.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity)
	if err == nil && integrity != "ok" {
		err = errors.New("history integrity check failed")
	}
	if err == nil {
		plan.Minutes, plan.Skipped, err = c.planHistoryRepair(ctx, r)
	}
	closeErr = c.Close()
	if err != nil {
		return plan, err
	}
	if closeErr != nil {
		return plan, closeErr
	}
	plan.WorkingHistorySHA256, err = repairFileHash(filepath.Join(workdir, "corrected", HistoryFilename))
	if err != nil {
		return plan, err
	}
	plan.WorkingStateSHA256, err = repairFileHash(filepath.Join(workdir, "corrected", "state.db"))
	if err != nil {
		return plan, err
	}
	plan.Request = r
	plan.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	plan.SHA256 = plan.digest()
	return plan, writeRepairJSON(filepath.Join(workdir, "plan.json"), plan)
}

type HistoryRepairResult struct {
	PlanSHA256     string `json:"plan_sha256"`
	AppliedMinutes int    `json:"applied_minutes"`
	BeforeWh       any    `json:"before_load_wh"`
	AfterWh        any    `json:"after_load_wh"`
	AfterQuality   string `json:"after_quality"`
	Estimated      bool   `json:"load_estimated"`
}

// ApplyHistoryRepair writes the approved additive estimates to the working
// copy only. A matching retry is idempotent; stale/tampered plans fail closed.
func ApplyHistoryRepair(ctx context.Context, workdir, approvedSHA string) (HistoryRepairResult, error) {
	var result HistoryRepairResult
	var p HistoryRepairPlan
	b, err := os.ReadFile(filepath.Join(workdir, "plan.json"))
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(b, &p); err != nil {
		return result, err
	}
	if len(approvedSHA) != 64 || p.SHA256 != approvedSHA || p.digest() != approvedSHA {
		return result, errors.New("exact reviewed plan SHA-256 is required")
	}
	if err = p.Request.validate(); err != nil {
		return result, err
	}
	for _, v := range []struct{ name, want string }{{"state.db", p.BackupStateSHA256}, {HistoryFilename, p.BackupHistorySHA256}} {
		got, err := repairFileHash(filepath.Join(workdir, "backup", v.name))
		if err != nil || got != v.want {
			return result, errors.New("verified backup changed or is unavailable")
		}
	}
	working := filepath.Join(workdir, "corrected", HistoryFilename)
	stateHash, err := repairFileHash(filepath.Join(workdir, "corrected", "state.db"))
	if err != nil || stateHash != p.WorkingStateSHA256 {
		return result, errors.New("working state changed after the plan")
	}
	hash, err := repairFileHash(working)
	if err != nil {
		return result, err
	}
	s, err := Open(filepath.Join(workdir, "corrected", "state.db"))
	if err != nil {
		return result, err
	}
	defer s.Close()
	methodJSON, _ := json.Marshal(map[string]any{"plan_sha256": p.SHA256, "backup_history_sha256": p.BackupHistorySHA256, "request": p.Request, "created_at": p.CreatedAt})
	method := historicalRepairMethod + string(methodJSON)
	if len(p.Minutes) == 0 {
		return result, errors.New("plan has no supported corrections")
	}
	allApplied := true
	for _, v := range p.Minutes {
		var wh float64
		var saved string
		err := s.history.QueryRowContext(ctx, `SELECT load_wh,method FROM history_load_estimates WHERE start_ms=? AND resolution_ms=60000`, v.StartMS).Scan(&wh, &saved)
		if err != nil || saved != method || wh != v.Wh {
			allApplied = false
			break
		}
	}
	if hash != p.WorkingHistorySHA256 && !allApplied {
		return result, errors.New("working history changed after the plan; prepare a new copy")
	}
	beforeSource, err := OpenBackupSource(filepath.Join(workdir, "backup", "state.db"))
	if err != nil {
		return result, err
	}
	before, err := beforeSource.DailyEnergy(p.Request.SinceMS, p.Request.UntilMS)
	beforeSource.Close()
	if err != nil {
		return result, err
	}
	if !allApplied {
		tx, err := s.history.BeginTx(ctx, nil)
		if err != nil {
			return result, err
		}
		defer tx.Rollback()
		for _, v := range p.Minutes {
			if v.StartMS < p.Request.SinceMS || v.StartMS+60000 > p.Request.UntilMS || v.StartMS%60000 != 0 || !finiteHistoryValue(v.Wh) {
				return result, errors.New("invalid correction minute")
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO history_load_estimates(start_ms,resolution_ms,covered_ms,load_wh,method) VALUES(?,60000,60000,?,?)`, v.StartMS, v.Wh, method); err != nil {
				return result, err
			}
		}
		// All these tables are derived caches. Original observations and their
		// quality are untouched; a new Core process recomputes corrected days.
		for _, table := range []string{"energy_daily", "energy_daily_partial", "history_daily_quality"} {
			if _, err = tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
				return result, err
			}
		}
		if err = tx.Commit(); err != nil {
			return result, err
		}
	}
	after, err := s.DailyEnergy(p.Request.SinceMS, p.Request.UntilMS)
	if err != nil {
		return result, err
	}
	result = HistoryRepairResult{p.SHA256, len(p.Minutes), before.Missing.Value(MissingLoad, before.LoadWh), after.Missing.Value(MissingLoad, after.LoadWh), after.Missing.Quality(), after.Missing&EstimatedLoad != 0}
	return result, writeRepairJSON(filepath.Join(workdir, "apply-result.json"), result)
}

// Keep plan order deterministic for callers that construct requests in maps.
func SortRepairSources(sources []RepairPowerSource) {
	sort.Slice(sources, func(i, j int) bool { return sources[i].Flow < sources[j].Flow })
}
