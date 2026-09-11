package drivers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestLuaVehicleCachedSoCDoesNotRefreshObservationTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vehicle.lua")
	src := `
local polls = 0

function driver_init(config) end

function driver_poll()
  polls = polls + 1
  host.emit("vehicle", {
    soc = 61,
    soc_fresh = polls == 1,
    charge_limit_pct = 80,
    charging_state = "Charging",
    stale = false,
  })
  return 1000
end
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	store := telemetry.NewStore()
	driver, err := NewLuaDriver(path, NewHostEnv("vehicle-cache", store))
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer driver.Cleanup()
	if err := driver.Init(context.Background(), nil); err != nil {
		t.Fatalf("init driver: %v", err)
	}
	store.DriverHealthMut("vehicle-cache").RecordSuccess()

	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("fresh poll: %v", err)
	}
	first := store.Get("vehicle-cache", telemetry.DerVehicle)
	if first == nil || first.SoC == nil || *first.SoC != 0.61 || first.SoCUpdatedAt.IsZero() {
		t.Fatalf("fresh vehicle reading = %+v", first)
	}
	firstSoCUpdatedAt := first.SoCUpdatedAt
	if samples := store.FlushSamples(); len(samples) != 2 || samples[1].Metric != "vehicle_soc" {
		t.Fatalf("fresh samples = %+v, want power plus SoC", samples)
	}

	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("cached poll: %v", err)
	}
	cached := store.Get("vehicle-cache", telemetry.DerVehicle)
	if cached == nil || cached.SoC == nil || *cached.SoC != 0.61 {
		t.Fatalf("cached vehicle reading = %+v", cached)
	}
	if !cached.SoCUpdatedAt.Equal(firstSoCUpdatedAt) {
		t.Fatalf("cached poll changed SoC observation time: got %v want %v", cached.SoCUpdatedAt, firstSoCUpdatedAt)
	}
	var raw struct {
		SoC      float64 `json:"soc"`
		SoCFresh bool    `json:"soc_fresh"`
	}
	if err := json.Unmarshal(cached.Data, &raw); err != nil {
		t.Fatalf("decode cached data: %v", err)
	}
	if raw.SoC != 61 || raw.SoCFresh {
		t.Fatalf("cached raw data = %+v, want soc=61 and soc_fresh=false", raw)
	}
	if samples := store.FlushSamples(); len(samples) != 1 || samples[0].Metric != "vehicle_w" {
		t.Fatalf("cached samples = %+v, want power only", samples)
	}

	if pick := telemetry.PickBestVehicle(store, firstSoCUpdatedAt.Add(telemetry.VehicleMaxAge)); pick.Driver != "vehicle-cache" {
		t.Fatalf("SoC at freshness boundary should remain usable, got %+v", pick)
	}
	if pick := telemetry.PickBestVehicle(store, firstSoCUpdatedAt.Add(telemetry.VehicleMaxAge+time.Nanosecond)); pick.Driver != "" {
		t.Fatalf("SoC past freshness boundary must be rejected, got %+v", pick)
	}
}
