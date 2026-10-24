package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func historicalRepairFixture(t *testing.T, powerGap bool) (string, HistoryRepairRequest) {
	t.Helper()
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour).UnixMilli()
	for i := int64(0); i <= 90; i++ {
		v := HistorySources{GridW: 2000, PVW: -500, BatteryW: 100, EVW: 400}
		if i == 42 {
			v.Missing = MissingPV
		}
		if i == 66 {
			continue
		} // retained scalar series can repair a writer gap too.
		sourceTick(t, s, base+i*10000, v)
	}
	// Retained pre-source-hold history has rejected PV minutes. Current
	// collection can bridge this short outage as an estimate; preserve that
	// policy and explicitly model the older stored evidence being repaired.
	for _, minute := range []int64{420000, 480000, 660000} {
		if _, err := s.history.Exec(`INSERT INTO history_quality(kind,start_ms,resolution_ms,missing) VALUES('source_energy',?,60000,?)
 ON CONFLICT(kind,start_ms,resolution_ms) DO UPDATE SET missing=missing|excluded.missing`, base+minute, MissingPV|MissingLoad); err != nil {
			t.Fatal(err)
		}
	}
	var samples []Sample
	for at := base - 30000; at <= base+930000; at += 5000 {
		for metric, w := range map[string]float64{"meter_w": 2000, "pv_w": -500, "battery_ac_w": 100, "ev_w": 400} {
			if powerGap && metric == "pv_w" && at >= base+360000 && at <= base+480000 {
				continue
			}
			samples = append(samples, Sample{Driver: "equipment", Metric: metric, TsMs: at, Value: w, Unit: "W"})
		}
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	path := s.mainDBPath
	if err := s.SaveDailyEnergy("stale-cache", DayEnergy{Missing: MissingLoad, Intervals: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r := HistoryRepairRequest{SinceMS: base, UntilMS: base + 900000, MaxGapMS: 15000, Reason: "fixture: reconstruct retained AC source energy",
		Sources: []RepairPowerSource{{Flow: "grid", Driver: "equipment", Metric: "meter_w"}, {Flow: "pv", Driver: "equipment", Metric: "pv_w"}, {Flow: "battery", Driver: "equipment", Metric: "battery_ac_w"}, {Flow: "ev", Driver: "equipment", Metric: "ev_w"}, {Flow: "v2x", Absent: true}}}
	return path, r
}

func TestHistoryRepairCopyRecalculatesHistoricalTotalsAndPreservesOriginal(t *testing.T) {
	ctx := context.Background()
	source, r := historicalRepairFixture(t, false)
	originalState, _ := repairFileHash(source)
	originalHistory, _ := repairFileHash(filepath.Join(filepath.Dir(source), HistoryFilename))
	work := filepath.Join(t.TempDir(), "repair")
	plan, err := PrepareHistoryRepair(ctx, source, work, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Minutes) == 0 || len(plan.Skipped) != 0 {
		t.Fatal("fixture has reconstructable gaps", plan)
	}
	if _, err := ApplyHistoryRepair(ctx, work, "unreviewed"); err == nil {
		t.Fatal("unreviewed plan applied")
	}
	result, err := ApplyHistoryRepair(ctx, work, plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if result.BeforeWh != nil || result.AfterWh == nil || !result.Estimated {
		t.Fatal("historical total not corrected", result)
	}
	near(t, "reconstructed period Wh", result.AfterWh.(float64), 500)
	again, err := ApplyHistoryRepair(ctx, work, plan.SHA256)
	if err != nil || !reflect.DeepEqual(result, again) {
		t.Fatal("retry double counted", again, err)
	}
	if h, _ := repairFileHash(source); h != originalState {
		t.Fatal("source state changed")
	}
	if h, _ := repairFileHash(filepath.Join(filepath.Dir(source), HistoryFilename)); h != originalHistory {
		t.Fatal("source history changed")
	}
	s, err := Open(filepath.Join(work, "corrected", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var quality int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_quality WHERE missing&?!=0`, MissingPV).Scan(&quality); err != nil || quality == 0 {
		t.Fatal("original rejected evidence erased", quality, err)
	}
	var cached int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM energy_daily_partial WHERE day='stale-cache'`).Scan(&cached); err != nil || cached != 0 {
		t.Fatal("stale cache retained", cached, err)
	}
	d, err := s.DailyEnergy(r.SinceMS, r.UntilMS)
	if err != nil || d.Missing&MissingLoad != 0 || d.Missing&EstimatedLoad == 0 {
		t.Fatal("restart lost repair", d, err)
	}
	if d.Missing&MissingPV == 0 {
		t.Fatal("load correction falsely repaired the PV measurement")
	}
	if err := s.SaveDailyEnergy("corrected-cache", d); err != nil {
		t.Fatal(err)
	}
	c, hit, err := s.LoadDailyEnergy("corrected-cache")
	if err != nil || !hit || c.Missing&EstimatedLoad == 0 {
		t.Fatal(c, hit, err)
	}
	near(t, "persisted corrected Wh", c.LoadWh, 500)
	points, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{SinceMS: r.SinceMS, UntilMS: r.UntilMS, BucketMS: 300000, Limit: 10})
	if err != nil || truncated {
		t.Fatal(err)
	}
	var wh float64
	var n int
	var estimated int
	for _, p := range points {
		if p.Flow == FlowConsumerUse {
			wh += p.EnergyWh
			n++
			if p.Quality == "estimated" {
				estimated++
			}
		}
	}
	if n != 3 {
		t.Fatal("historical ledger not corrected", points)
	}
	if estimated == 0 {
		t.Fatal("repair provenance missing from ledger")
	}
	near(t, "ledger Wh", wh, 500)
	limited, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{SinceMS: r.SinceMS, UntilMS: r.UntilMS, BucketMS: 300000, Limit: 1})
	if err != nil || !truncated {
		t.Fatal("repair bypassed limit", err)
	}
	for _, p := range limited {
		if p.BucketStartMS != r.SinceMS {
			t.Fatal("extra buckets exceeded limit", p)
		}
	}
	chart, err := s.LoadHistory(r.SinceMS, r.UntilMS, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range chart {
		if p.Missing&MissingLoad != 0 {
			t.Fatal("chart kept a repaired load gap", p)
		}
	}
	if len(chart) == 0 {
		t.Fatal("corrected chart is empty")
	}
	// Retention must keep the originals needed to interpret the additive
	// minute corrections, while ordinary uncorrected quarters may roll up.
	if err := s.rollSiteEnergy(ctx, r.UntilMS); err != nil {
		t.Fatal(err)
	}
	d, err = s.DailyEnergy(r.SinceMS, r.UntilMS)
	if err != nil || d.Missing&MissingLoad != 0 {
		t.Fatal("retention lost repair", d, err)
	}
	near(t, "retained corrected Wh", d.LoadWh, 500)
	backup := filepath.Join(t.TempDir(), "restored.db")
	if err := s.copyStateForBackup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := s.exportHistoryToSQLite(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	d, err = restored.DailyEnergy(r.SinceMS, r.UntilMS)
	if err != nil || d.Missing&MissingLoad != 0 || d.Missing&EstimatedLoad == 0 {
		t.Fatal("portable backup lost repair", d, err)
	}
	near(t, "restored corrected Wh", d.LoadWh, 500)
}

func TestHistoryRepairAddsDerivedChartIntervalWithoutInventingOtherMeasurements(t *testing.T) {
	source, r := historicalRepairFixture(t, false)
	work := filepath.Join(t.TempDir(), "repair")
	p, err := PrepareHistoryRepair(context.Background(), source, work, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyHistoryRepair(context.Background(), work, p.SHA256); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(work, "corrected", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := p.Minutes[0].StartMS
	for _, table := range []string{"history_dashboard", "history_dashboard_partial"} {
		if _, err := s.history.Exec(`DELETE FROM `+table+` WHERE last_ms>=? AND last_ms<?`, start, start+60000); err != nil {
			t.Fatal(err)
		}
	}
	points, err := s.LoadHistory(r.SinceMS, r.UntilMS, 15)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range points {
		if v.FirstMS == start && v.ResolutionMS == 60000 && v.N == 0 {
			found = true
			if v.Missing&MissingLoad != 0 || v.Missing&MissingGrid == 0 || v.Missing&EstimatedLoad == 0 {
				t.Fatal("derived interval became raw site measurements", v)
			}
		}
	}
	if !found {
		t.Fatal("historical recording gap remained absent from chart")
	}
}

func TestHistoryRepairTransactionRollsBackAllCorrectionsOnConflict(t *testing.T) {
	source, r := historicalRepairFixture(t, false)
	work := filepath.Join(t.TempDir(), "repair")
	p, err := PrepareHistoryRepair(context.Background(), source, work, r)
	if err != nil {
		t.Fatal(err)
	}
	p.Minutes = append(p.Minutes, p.Minutes[0])
	p.SHA256 = p.digest()
	if err := writeRepairJSON(filepath.Join(work, "plan.json"), p); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyHistoryRepair(context.Background(), work, p.SHA256); err == nil {
		t.Fatal("conflicting correction rows accepted")
	}
	s, err := Open(filepath.Join(work, "corrected", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_load_estimates WHERE method GLOB 'historical_power_repair_v1:*'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("partial correction committed", n, err)
	}
}

func TestHistoryRepairLeavesUnsupportedGapsUnknownUnlessBoundExplicitlyAllowsInterpolation(t *testing.T) {
	source, r := historicalRepairFixture(t, true)
	for _, gap := range []int64{15000, 180000} {
		r.MaxGapMS = gap
		work := filepath.Join(t.TempDir(), "repair")
		plan, err := PrepareHistoryRepair(context.Background(), source, work, r)
		if err != nil {
			t.Fatal(err)
		}
		result, err := ApplyHistoryRepair(context.Background(), work, plan.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if gap == 15000 {
			if len(plan.Skipped) == 0 || result.AfterWh != nil {
				t.Fatal("unsupported source gap became known", plan, result)
			}
		} else {
			if result.AfterWh == nil {
				t.Fatal("explicit bounded reconstruction failed", plan, result)
			}
			var bridged int64
			for _, v := range plan.Minutes {
				bridged += v.InterpolatedMS
			}
			if bridged == 0 {
				t.Fatal("interpolation provenance missing")
			}
		}
	}
}

func TestHistoryRepairRejectsTamperedPlanAndChangedBackupOrWorkingCopy(t *testing.T) {
	for _, kind := range []string{"plan", "backup", "working"} {
		t.Run(kind, func(t *testing.T) {
			source, r := historicalRepairFixture(t, false)
			work := filepath.Join(t.TempDir(), "repair")
			p, err := PrepareHistoryRepair(context.Background(), source, work, r)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "plan":
				p.Minutes[0].Wh += 100
				b, _ := json.Marshal(p)
				if err := os.WriteFile(filepath.Join(work, "plan.json"), b, 0600); err != nil {
					t.Fatal(err)
				}
			case "backup":
				f, err := os.OpenFile(filepath.Join(work, "backup", HistoryFilename), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				f.Write([]byte("changed"))
				f.Close()
			case "working":
				s, err := Open(filepath.Join(work, "corrected", "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.history.Exec(`DELETE FROM energy_daily_partial`); err != nil {
					t.Fatal(err)
				}
				s.Close()
			}
			if _, err := ApplyHistoryRepair(context.Background(), work, p.SHA256); err == nil {
				t.Fatal("changed input was accepted")
			}
		})
	}
}

func TestHistoryRepairIntegralDoesNotClipSignedPowerOrExtrapolate(t *testing.T) {
	p := []repairPowerPoint{{0, -1000}, {60000, -1000}, {120000, 3000}, {180000, 3000}}
	wh, _, ok := repairIntegral(p, 0, 180000, 60000)
	if !ok {
		t.Fatal("signed interval rejected")
	}
	near(t, "signed Wh", wh, 50)
	negative, _, ok := repairIntegral(p, 0, 60000, 60000)
	if !ok || negative >= 0 {
		t.Fatal("negative residual clipped", negative, ok)
	}
	if _, _, ok := repairIntegral(p, -1, 180000, 60000); ok {
		t.Fatal("missing edge was extrapolated")
	}
	if _, _, ok := repairIntegral(p, 0, 180000, 15000); ok {
		t.Fatal("long unapproved gaps filled")
	}
}

func TestHistoryRepairRefusesInsufficientCopySpace(t *testing.T) {
	source, r := historicalRepairFixture(t, false)
	probe := backupDiskAvail
	backupDiskAvail = func(string) (int64, error) { return 0, nil }
	defer func() { backupDiskAvail = probe }()
	if _, err := PrepareHistoryRepair(context.Background(), source, filepath.Join(t.TempDir(), "repair"), r); err == nil {
		t.Fatal("repair started without room for backup and working copy")
	}
}
