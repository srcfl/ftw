package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Issue #1441: adding a Zap P1 meter (catalog meter+pv+battery, PV reading
// off by default) froze the chart for good. The Zap never reports PV, so the
// forecast topology kept the household balance invalid on every tick and
// after every restart. The chart needs a measured balance, not proof that an
// optional source is absent or that device identities are confirmed.
func TestHistoryKeepsRunningWithOptionalPVAndUnconfirmedIdentity(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	catalog := []drivers.CatalogEntry{
		{Path: "drivers/zap.lua", Filename: "zap.lua", Capabilities: []string{"meter", "pv", "battery"}},
		{Path: "drivers/sma.lua", Filename: "sma.lua", Capabilities: []string{"pv"}},
		{Path: "drivers/pixii.lua", Filename: "pixii.lua", Capabilities: []string{"battery"}},
	}
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "sourceful-zap", Lua: "drivers/zap.lua", IsSiteMeter: true, Config: map[string]any{"host": "zap.local"}},
		{Name: "sma", Lua: "drivers/sma.lua"},
		{Name: "pixii", Lua: "drivers/pixii.lua", BatteryCapacityWh: 10000},
	}}
	ids := map[string]string{"sourceful-zap": "zap:sn", "sma": "sma:sn"} // pixii identity not yet known
	site := newForecastSiteConfig(st)
	site.identity = func(name string) (string, bool) { id := ids[name]; return id, id != "" }
	site.Configure(cfg, catalog)

	tel := telemetry.NewStore()
	for name, kind := range map[string]telemetry.DerType{"sourceful-zap": telemetry.DerMeter, "sma": telemetry.DerPV, "pixii": telemetry.DerBattery} {
		tel.EnsureDriverHealth(name)
		w := map[telemetry.DerType]float64{telemetry.DerMeter: 1500, telemetry.DerPV: -2000, telemetry.DerBattery: 500}[kind]
		tel.Update(name, kind, w, nil, nil)
		tel.RecordDriverSuccess(name)
	}
	snap := site.Snapshot()
	if r := tel.ForecastMeasurementNow("sourceful-zap", snap.Options); r.Valid {
		t.Fatalf("forecast learning must stay strict: %+v", r)
	}
	point, ok := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "sourceful-zap"},
		time.Now().UnixMilli(), time.Minute, snap.HistoryOptions)
	if !ok || point.LoadW != 3000 || point.PVW != -2000 || point.BatW != 500 {
		t.Fatalf("history stopped: point=%+v available=%v", point, ok)
	}

	// A known source that goes silent still stops history; only optional,
	// never-reported PV is treated as absent.
	tel.DriverHealthMut("pixii").SetOffline()
	if _, ok := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "sourceful-zap"},
		time.Now().UnixMilli(), time.Minute, snap.HistoryOptions); ok {
		t.Fatal("offline battery treated as zero in history")
	}
	tel.Update("pixii", telemetry.DerBattery, 500, nil, nil)
	tel.RecordDriverSuccess("pixii")
	// Once the Zap does report PV, a stale PV reading stops history again.
	tel.Update("sourceful-zap", telemetry.DerPV, -100, nil, nil)
	tel.Get("sourceful-zap", telemetry.DerPV).UpdatedAt = time.Now().Add(-2 * time.Minute)
	if _, ok := buildHistoryPoint(tel, tickPersistControl{SiteMeterDriver: "sourceful-zap"},
		time.Now().UnixMilli(), time.Minute, snap.HistoryOptions); ok {
		t.Fatal("stale reported PV treated as zero in history")
	}
}
