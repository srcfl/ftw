package drivers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// TestEmitMetricRecordsHealthSuccess guards a read-only telemetry driver
// (MyUplink heat pump) that ONLY calls host.emit_metric — never the
// structured host.emit. Such a driver still polls successfully and emits
// fresh data, so it must register as healthy/online. Before the fix,
// emit_metric buffered the sample but never bumped LastSuccess, so the
// watchdog flipped the driver offline despite live telemetry.
func TestEmitMetricRecordsHealthSuccess(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("metric-only", tel)

	src := `
		function driver_init() end
		function driver_poll()
			host.emit_metric("hp_outdoor_temp_c", 7.3)
			return 60000
		end
		function driver_command() end
		function driver_default_mode() end
		function driver_cleanup() end
	`
	path := filepath.Join(t.TempDir(), "drv.lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	h := tel.DriverHealth("metric-only")
	if h == nil {
		t.Fatal("no health record")
	}
	if h.LastSuccess == nil {
		t.Error("LastSuccess is nil — a metric-only driver must record health success on emit_metric")
	}
	if !h.IsOnline() {
		t.Errorf("driver should be online after emitting a metric, got status %v", h.Status)
	}
}
