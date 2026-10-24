package state

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func endpointFixture(t *testing.T, night bool, at ...int64) (*Store, EndpointRepairRequest, string) {
	t.Helper()
	s := sourceStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour).UnixMilli()
	if len(at) > 0 {
		base = at[0]
	}
	for at := base - 60000; at <= base+31*60000; at += 5000 {
		if at >= base+10*60000 && at < base+22*60000 {
			continue
		}
		load, pv := 2000.0, 1000.0
		if at >= base+22*60000 {
			load, pv = 4000, 3000
		}
		if night {
			pv = 0
		}
		v := HistorySources{AtMS: at, Topology: "grid+pv", GridW: load - pv, PVW: -pv}
		p := HistoryPoint{TsMs: at, GridW: v.GridW, PVW: v.PVW, LoadW: load, JSON: `{"ev_w":0,"v2x_w":0}`, Sources: &v}
		dashboardTick(t, s, fmt.Sprint(at), &p)
	}
	r := EndpointRepairRequest{base, base + 30*60000, 15 * 60000, "owner-authorized endpoint mean"}
	backup := filepath.Join(t.TempDir(), "history-before.db")
	if _, err := s.history.Exec(`VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	return s, r, backup
}
func TestEndpointRepairMeansBothFlowsWhileLiveAndPreservesOriginals(t *testing.T) {
	ctx := context.Background()
	s, r, backup := endpointFixture(t, false)
	p, err := PrepareEndpointRepair(ctx, s.mainDBPath, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Minutes) == 0 || len(p.Skipped) != 0 {
		t.Fatal(p)
	}
	original, err := s.endpointOriginals(ctx, r.SinceMS, r.UntilMS)
	if err != nil {
		t.Fatal(err)
	}
	beforeDigest := endpointDigest(original)
	// Seed the independent measured PV ledger; the repair must preserve buckets
	// outside its affected windows and replace whole affected system buckets.
	pvLedger := map[int64]float64{}
	for _, b := range original {
		pvLedger[bucketStart(b.Start, 300000)] += b.PV
	}
	for at, wh := range pvLedger {
		insertLedgerEntryTest(t, s, "pv-device", FlowPVGeneration, at, 300000, wh, "hardware_counter", "measured", "counter", 1)
	}
	for _, m := range p.Minutes {
		want := 50.0
		if m.Flow == "pv" {
			want = 2000.0 / 60
		}
		near(t, "endpoint formula", m.Wh, want)
		if m.AfterMS-m.BeforeMS > r.MaxGapMS {
			t.Fatal("bound ignored", m)
		}
	}
	// Simulate new receipts arriving after review: they must survive publication.
	next := r.UntilMS + 120000
	sourceTick(t, s, next, HistorySources{GridW: 3210})
	var through string
	if err := s.history.QueryRow(`SELECT value FROM history_source_cursor WHERE key='through'`).Scan(&through); err != nil {
		t.Fatal(err)
	}
	n, err := ApplyEndpointRepair(ctx, s.mainDBPath, backup, p, p.SHA256)
	if err != nil || n != len(p.Minutes) {
		t.Fatal(n, err)
	}
	again, err := ApplyEndpointRepair(ctx, s.mainDBPath, backup, p, p.SHA256)
	if err != nil || again != n {
		t.Fatal("retry", again, err)
	}
	afterOriginal, _ := s.endpointOriginals(ctx, r.SinceMS, r.UntilMS)
	if endpointDigest(afterOriginal) != beforeDigest {
		t.Fatal("original energy/quality changed")
	}
	var afterThrough string
	_ = s.history.QueryRow(`SELECT value FROM history_source_cursor WHERE key='through'`).Scan(&afterThrough)
	if through != afterThrough {
		t.Fatal("new data lost")
	}
	var plans int
	_ = s.history.QueryRow(`SELECT count(*) FROM energy_ledger_meta WHERE key GLOB 'endpoint-repair-v1:*'`).Scan(&plans)
	if plans != 1 {
		t.Fatal("double counted", plans)
	}
	d, err := s.DailyEnergy(r.SinceMS, r.UntilMS)
	if err != nil || d.Missing&(MissingLoad|MissingPV) != 0 || d.Missing&EndpointEstimatedPV == 0 || d.Missing&EstimatedLoad == 0 {
		t.Fatal("daily", d, err)
	}
	expected := map[string]float64{"load": 0, "pv": 0}
	corrections := map[string]map[int64]float64{"load": {}, "pv": {}}
	for _, m := range p.Minutes {
		corrections[m.Flow][m.StartMS] = m.Wh
	}
	for _, b := range original {
		for _, flow := range []string{"load", "pv"} {
			if wh, ok := corrections[flow][b.Start]; ok {
				expected[flow] += wh
			} else if flow == "load" {
				expected[flow] += b.Load
			} else {
				expected[flow] += b.PV
			}
		}
	}
	near(t, "home total", d.LoadWh, expected["load"])
	near(t, "solar total", d.PVWh, expected["pv"])
	ledger, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{SinceMS: r.SinceMS, UntilMS: r.UntilMS, BucketMS: 300000, Limit: 20})
	if err != nil || truncated {
		t.Fatal(err)
	}
	sums := map[EnergyFlow]float64{}
	for _, v := range ledger {
		if v.Flow == FlowConsumerUse || v.Flow == FlowPVGeneration {
			sums[v.Flow] += v.EnergyWh
			if v.Source == "historical_repair" && v.Provenance != "endpoint_mean_v1" {
				t.Fatal(v)
			}
		}
	}
	near(t, "ledger home", sums[FlowConsumerUse], d.LoadWh)
	near(t, "ledger solar", sums[FlowPVGeneration], d.PVWh)
	points, err := s.LoadHistory(r.SinceMS, r.UntilMS, 30)
	if err != nil {
		t.Fatal(err)
	}
	var marked bool
	for _, v := range points {
		var detail map[string]any
		_ = json.Unmarshal([]byte(v.JSON), &detail)
		if detail["pv_quality"] == "estimated" {
			marked = true
			if v.Missing&EstimatedLoad == 0 {
				t.Fatal("training mark missing")
			}
		}
	}
	if !marked {
		t.Fatal("chart correction missing")
	}
	if err := s.SaveDailyEnergy("endpoint-cache", d); err != nil {
		t.Fatal(err)
	}
	cached, hit, err := s.LoadDailyEnergy("endpoint-cache")
	if err != nil || !hit || cached.Missing&EndpointEstimatedPV == 0 {
		t.Fatal("solar estimate marker lost in cache", cached, hit, err)
	}
	// Retention must preserve the minute boundaries used to replace each flow.
	if err := s.rollSiteEnergy(ctx, r.UntilMS+900000); err != nil {
		t.Fatal(err)
	}
	retained, err := s.DailyEnergy(r.SinceMS, r.UntilMS)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "retained solar", retained.PVWh, d.PVWh)
	near(t, "retained home", retained.LoadWh, d.LoadWh)
	exported := filepath.Join(t.TempDir(), "restored.db")
	if err := s.copyStateForBackup(ctx, exported); err != nil {
		t.Fatal(err)
	}
	if err := s.exportHistoryToSQLite(ctx, exported); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(exported)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.DailyEnergy(r.SinceMS, r.UntilMS)
	if err != nil || got.Missing&(MissingLoad|MissingPV) != 0 {
		t.Fatal(got, err)
	}
	near(t, "restored home", got.LoadWh, d.LoadWh)
	near(t, "restored solar", got.PVWh, d.PVWh)
}
func TestEndpointRepairNightZeroAndBounds(t *testing.T) {
	s, r, backup := endpointFixture(t, true)
	ctx := context.Background()
	p, err := PrepareEndpointRepair(ctx, s.mainDBPath, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Minutes {
		if m.Flow == "pv" && m.Wh != 0 {
			t.Fatal("night must use observed zero endpoints", m)
		}
	}
	bad := p
	bad.Minutes = append([]EndpointRepairMinute(nil), p.Minutes...)
	bad.Minutes[0].Wh++
	if _, err := ApplyEndpointRepair(ctx, s.mainDBPath, backup, bad, p.SHA256); err == nil {
		t.Fatal("tampering accepted")
	}
	if _, err := ApplyEndpointRepair(ctx, s.mainDBPath, s.historyPath, p, p.SHA256); err == nil {
		t.Fatal("source accepted as backup")
	}
	r.MaxGapMS = 5 * 60000
	p, err = PrepareEndpointRepair(ctx, s.mainDBPath, r)
	if err != nil || len(p.Minutes) != 0 || len(p.Skipped) != 2 {
		t.Fatal("long gap was filled", p, err)
	}
}
func TestEndpointRepairRejectsChangedEvidence(t *testing.T) {
	s, r, backup := endpointFixture(t, false)
	ctx := context.Background()
	p, err := PrepareEndpointRepair(ctx, s.mainDBPath, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.history.Exec(`UPDATE history_source_energy SET import_wh=import_wh+1 WHERE start_ms=?`, r.SinceMS); err != nil {
		t.Fatal(err)
	}
	if _, err = ApplyEndpointRepair(ctx, s.mainDBPath, backup, p, p.SHA256); err == nil {
		t.Fatal("stale evidence accepted")
	}
	var n int
	_ = s.history.QueryRow(`SELECT count(*) FROM energy_ledger_meta WHERE key GLOB 'endpoint-repair-v1:*'`).Scan(&n)
	if n != 0 {
		t.Fatal("partial application")
	}
}

func TestEndpointRepairRollsBackWhenCacheInvalidationFails(t *testing.T) {
	s, r, backup := endpointFixture(t, false)
	ctx := context.Background()
	p, err := PrepareEndpointRepair(ctx, s.mainDBPath, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDailyEnergy("guard", DayEnergy{Intervals: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`CREATE TRIGGER endpoint_cache_guard BEFORE DELETE ON energy_daily_partial BEGIN SELECT RAISE(ABORT,'cache locked for test'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEndpointRepair(ctx, s.mainDBPath, backup, p, p.SHA256); err == nil {
		t.Fatal("expected transaction failure")
	}
	var n int
	_ = s.history.QueryRow(`SELECT count(*) FROM energy_ledger_meta WHERE key GLOB 'endpoint-repair-v1:*'`).Scan(&n)
	if n != 0 {
		t.Fatal("audit partially committed")
	}
	if _, hit, err := s.LoadDailyEnergy("guard"); err != nil || !hit {
		t.Fatal("cache partially deleted", hit, err)
	}
}

func TestEndpointRepairLiveTotalsStopAtCommittedMinute(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(-40 * time.Minute).UnixMilli()
	s, r, backup := endpointFixture(t, false, base)
	ctx := context.Background()
	p, err := PrepareEndpointRepair(ctx, s.mainDBPath, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEndpointRepair(ctx, s.mainDBPath, backup, p, p.SHA256); err != nil {
		t.Fatal(err)
	}
	d, err := s.DailyEnergy(base, time.Now().UnixMilli())
	if err != nil || d.Missing&(MissingLoad|MissingPV) != 0 {
		t.Fatal("writer tail voided repaired totals", d, err)
	}
	if d.LoadUntilMS != base+31*60000 || d.PVUntilMS != d.LoadUntilMS {
		t.Fatal("extrapolated unfinished tail", d)
	}
}
