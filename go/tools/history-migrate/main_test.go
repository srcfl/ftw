package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
	_ "modernc.org/sqlite"
)

func TestRealBetaConversionPreservesIDsGoalsAndHotSamples(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	cfg, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.Exec(`CREATE TABLE config(key TEXT PRIMARY KEY,value TEXT NOT NULL);INSERT INTO config VALUES('history_duckdb_generation','test-generation'),('goal','80% by 07:00')`)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Close()
	source, err := sql.Open("duckdb", filepath.Join(dir, "history.duckdb")+"?threads=1&memory_limit=128MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	for _, stmt := range betaSchema {
		if _, err := source.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO history_migrations(name) VALUES('generation:test-generation'),('sqlite-v1')`,
		`INSERT INTO ts_drivers VALUES(7,'meter')`,
		`INSERT INTO ts_metrics VALUES(9,'power','W')`,
		`INSERT INTO ts_samples SELECT 7,9,i,i/10.0 FROM range(1,10001) t(i)`,
		// Leave physical row-ID holes; a resumed copy must not skip nearby data.
		`INSERT INTO ts_samples VALUES(7,9,20000,1)`,
		`DELETE FROM ts_samples WHERE ts_ms=20000`,
		`INSERT INTO ts_series_hour VALUES(7,9,1577836800000,84000,7000,7000,12,1577836860000)`,
		`INSERT INTO history_warm VALUES(1000,123.5,NULL,-12.25,100,0.8,'{"mode":"warm"}')`,
		`INSERT INTO history_cold VALUES(10,100,NULL,0,100,0.8,'{}')`,
		`INSERT INTO energy_daily VALUES('2026-09-01',1,2,3,4,5,6,1000)`,
		`INSERT INTO energy_assets VALUES('ev:stable','easee:stable','vehicle_charger','Car',0,1800000000000,1800000120000)`,
		`INSERT INTO energy_ledger_entries VALUES(1,'ev:stable','vehicle_charge',1800000000000,300000,200,'hardware_counter','measured','counter',2,1800000120000)`,
		`INSERT INTO energy_ledger_cursors VALUES('ev:stable','vehicle_charge','counter',300,1800000120000)`,
		`CHECKPOINT`,
	} {
		if _, err := source.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("FTW_STORAGE_ADMISSION") == "1" {
		for _, q := range []string{
			`INSERT INTO ts_metrics SELECT 1000+i,'metric-'||i,'W' FROM range(10) t(i)`,
			`INSERT INTO ts_samples SELECT 7,1000+(i%10),200000+(i//10),i/10.0 FROM range(1000000) t(i)`,
			`CHECKPOINT`,
		} {
			if _, err := source.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	hot, err := sql.Open("sqlite", filepath.Join(dir, state.HotHistoryFilename))
	if err != nil {
		t.Fatal(err)
	}
	_, err = hot.Exec(`CREATE TABLE history_hot(ts_ms INTEGER PRIMARY KEY,grid_w REAL,pv_w REAL,bat_w REAL,load_w REAL,bat_soc REAL,json TEXT);
 CREATE TABLE ts_drivers(id INTEGER PRIMARY KEY,name TEXT);
 CREATE TABLE ts_metrics(id INTEGER PRIMARY KEY,name TEXT,unit TEXT);
 CREATE TABLE ts_samples(driver_id INTEGER,metric_id INTEGER,ts_ms INTEGER,value REAL);
 CREATE TABLE hot_ticks(id TEXT,ts_ms INTEGER,payload TEXT);
 INSERT INTO ts_drivers VALUES(999,'meter');INSERT INTO ts_metrics VALUES(888,'power','W');
 INSERT INTO ts_samples VALUES(999,888,10001,1000.1),(999,888,10002,1000.2);
 INSERT INTO history_hot VALUES(10002,1000.2,0,0,0,0,'{}');`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		counter := float64(100 + i*100)
		payload, err := json.Marshal(struct{ Observations []state.EnergyObservation }{[]state.EnergyObservation{{AssetID: "ev:stable", DeviceID: "easee:stable", AssetKind: state.AssetVehicleCharger, Label: "Car", Flow: state.FlowVehicleCharge, AtMs: 1800000000000 + int64(i)*60000, CounterWh: &counter}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hot.Exec(`INSERT INTO hot_ticks VALUES(?,?,?)`, fmt.Sprint(i), 1800000000000+int64(i)*60000, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	hot.Close()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	source, err = sql.Open("duckdb", filepath.Join(dir, "history.duckdb")+"?access_mode=read_only&threads=1&memory_limit=128MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	err = state.ConvertBetaHistory(ctx, path, source, func(phase string) {
		if phase == "copy and verify energy_daily" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expected partial copy", err)
	}
	if err := state.ConvertBetaHistory(context.Background(), path, source, func(s string) { t.Log(s) }); err != nil {
		t.Fatal(err)
	}
	if err := state.ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal("retry", err)
	}
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, _ := s.LoadConfig("goal"); got != "80% by 07:00" {
		t.Fatal(got)
	}
	points, err := s.LoadSeries("meter", "power", 0, 20000, 0)
	if err != nil || len(points) != 10002 {
		t.Fatalf("rows=%d err=%v", len(points), err)
	}
	for i, p := range points {
		if p.Value != float64(i+1)/10 {
			t.Fatalf("changed row %d: %+v", i, p)
		}
	}
	db, err := sql.Open("sqlite", state.HistoryDatabasePath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id int64
	if err := db.QueryRow(`SELECT id FROM ts_drivers WHERE name='meter'`).Scan(&id); err != nil || id != 7 {
		t.Fatal("changed identifier", id, err)
	}
	if os.Getenv("FTW_STORAGE_ADMISSION") == "1" {
		var count int64
		if err := db.QueryRow(`SELECT COUNT(*) FROM ts_samples WHERE metric_id>=1000`).Scan(&count); err != nil || count != 1000000 {
			t.Fatal("large interleaved copy differs", count, err)
		}
	}
	var summaryCount int64
	if err := db.QueryRow(`SELECT n FROM ts_series_hour WHERE hour_ms=1577836800000`).Scan(&summaryCount); err != nil || summaryCount != 12 {
		t.Fatal("old summary lost", summaryCount, err)
	}
	if err := db.QueryRow(`SELECT n FROM ts_series_hour WHERE hour_ms=0`).Scan(&summaryCount); err != nil || summaryCount != 10002 {
		t.Fatal("hot sample summary lost", summaryCount, err)
	}
	var energy float64
	if err := db.QueryRow(`SELECT SUM(energy_wh) FROM energy_ledger_entries WHERE asset_id='ev:stable'`).Scan(&energy); err != nil || energy != 300 {
		t.Fatal("hot counter replay changed energy", energy, err)
	}
	var jsonText string
	if err := db.QueryRow(`SELECT json FROM history_warm WHERE ts_ms=1000`).Scan(&jsonText); err != nil || jsonText != `{"mode":"warm"}` {
		t.Fatal("snapshot changed", jsonText, err)
	}
	for _, name := range []string{"history.duckdb", state.HotHistoryFilename} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(fmt.Errorf("original removed: %s: %w", name, err))
		}
	}
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	bytes := int64(usage.Maxrss) * 1024
	if runtime.GOOS == "darwin" {
		bytes = int64(usage.Maxrss)
	}
	t.Logf("kernel peak RSS: %.2f MiB", float64(bytes)/(1<<20))
	if bytes > 256<<20 {
		t.Fatalf("converter peak exceeds 256 MiB: %d bytes", bytes)
	}

}
