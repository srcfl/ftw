package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// Report the values driver_init actually received, with realistic defaults
// that would widen the battery window if the host forgot to inject it.
func driverSoCProbe(version string) []byte {
	source := updateDriverLua(version, "P1-123", `host.emit("meter", {w=1})`)
	init := fmt.Sprintf(`function driver_init(config)
 config = config or {}
 if config.reject_version == %q then error("init rejected this version") end
 host.emit_metric("init_charge_ceil_soc", config.charge_ceil_soc or 0.95)
 host.emit_metric("init_discharge_floor_soc", config.discharge_floor_soc or 0.05)
 host.emit_metric("init_version", %s)
`, version, strings.ReplaceAll(version, ".", ""))
	return []byte(strings.Replace(string(source), "function driver_init(config)", init, 1))
}

func newDriverSoCFixture(t *testing.T, overrides map[string]any) *driverUpdateFixture {
	t.Helper()
	f := newDriverUpdateFixture(t, "running")
	if err := os.WriteFile(f.bundled, driverSoCProbe("1.0.2"), 0o644); err != nil {
		t.Fatal(err)
	}
	min, max := 0.23, 0.81
	f.s.deps.Cfg.Batteries = map[string]config.Battery{"p1": {SoCMin: &min, SoCMax: &max}}
	f.s.deps.Cfg.Drivers[0].Config = maps.Clone(overrides)
	if err := f.s.deps.SaveConfig(f.s.deps.ConfigPath, f.s.deps.Cfg); err != nil {
		t.Fatal(err)
	}
	// The production startup path derives bounds without changing Cfg.Drivers.
	startup := config.WithBatterySoCBounds(f.s.deps.Cfg.Drivers, f.s.deps.Cfg.Batteries)
	if err := f.s.deps.Registry.Restart(context.Background(), startup[0]); err != nil {
		t.Fatal(err)
	}
	f.publishSource("1.0.3", driverSoCProbe("1.0.3"))
	return f
}

func assertDriverSoC(t *testing.T, f *driverUpdateFixture, since time.Time, version, min, max float64, raw map[string]any) {
	t.Helper()
	for name, want := range map[string]float64{"init_version": version, "init_charge_ceil_soc": max, "init_discharge_floor_soc": min} {
		got, at, ok := f.s.deps.Tel.LatestMetric("p1", name)
		if !ok || got != want || at.Before(since) {
			t.Fatalf("Lua %s=%g at %s, want fresh %g after %s", name, got, at, want, since)
		}
	}
	if got := f.s.deps.Cfg.Drivers[0].Config; !reflect.DeepEqual(got, raw) {
		t.Fatalf("runtime bounds leaked into live config: got %v want %v", got, raw)
	}
	loaded, err := config.Load(f.s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Drivers[0].Config; !reflect.DeepEqual(got, raw) {
		t.Fatalf("runtime bounds leaked into saved config: got %v want %v", got, raw)
	}
	t.Logf("Lua init version=%g: discharge_floor_soc=%g charge_ceil_soc=%g; raw config preserved", version, min, max)
}

func TestManagedDriverSoCBoundsAcrossVersions(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  map[string]any
		max  float64
	}{
		{name: "battery-only", max: 0.81},
		{name: "explicit-override", raw: map[string]any{"charge_ceil_soc": 0.77}, max: 0.77},
		{name: "null-is-unset", raw: map[string]any{"charge_ceil_soc": nil, "discharge_floor_soc": nil}, max: 0.81},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDriverSoCFixture(t, tc.raw)
			assertDriverSoC(t, f, time.Time{}, 102, 0.23, tc.max, tc.raw)
			for _, step := range []struct {
				action, body, publish string
				version               float64
			}{
				{"install", `{"repository_id":"test"}`, "", 103},
				{"install", `{"repository_id":"test"}`, "1.0.4", 104},
				{"rollback", `{}`, "", 103},
				{"activate", `{"version":"1.0.4"}`, "", 104},
				{"use_bundled", `{}`, "", 102},
				{"activate", `{"version":"1.0.3"}`, "", 103},
			} {
				if step.publish != "" {
					f.publishSource(step.publish, driverSoCProbe(step.publish))
				}
				since := time.Now()
				f.request(context.Background(), step.action, step.body, 200)
				assertDriverSoC(t, f, since, step.version, 0.23, tc.max, tc.raw)
			}
			// A later battery edit must not be shadowed by previously derived keys.
			min, max := 0.31, 0.72
			f.s.deps.Cfg.Batteries = map[string]config.Battery{"p1": {SoCMin: &min, SoCMax: &max}}
			if err := f.s.deps.SaveConfig(f.s.deps.ConfigPath, f.s.deps.Cfg); err != nil {
				t.Fatal(err)
			}
			wantMax := max
			if tc.name == "explicit-override" {
				wantMax = tc.max
			}
			since := time.Now()
			f.request(context.Background(), "activate", `{"version":"1.0.4"}`, 200)
			assertDriverSoC(t, f, since, 104, min, wantMax, tc.raw)
		})
	}
}

func TestManagedDriverSoCBoundsDuringRecovery(t *testing.T) {
	for _, action := range []string{"install-first", "activate-first", "install", "activate", "rollback", "use_bundled", "save-first", "save-bundled"} {
		t.Run(action, func(t *testing.T) {
			f := newDriverSoCFixture(t, nil)
			ctx := context.Background()
			endpoint, body, rejected, recovered := "install", `{"repository_id":"test"}`, "1.0.3", float64(102)
			if action == "activate-first" {
				// Keep a signed file on disk without selecting it in config.
				if _, err := f.s.deps.DriverRepository.Install(ctx, "test", "esphome-dsmr", ""); err != nil {
					t.Fatal(err)
				}
				if err := f.s.deps.DriverRepository.Deactivate("drivers/esphome-dsmr.lua"); err != nil {
					t.Fatal(err)
				}
				endpoint, body = "activate", `{"version":"1.0.3"}`
			} else if action != "install-first" && action != "save-first" {
				f.request(ctx, "install", body, 200)
				recovered, rejected = 103, "1.0.4"
				f.publishSource("1.0.4", driverSoCProbe("1.0.4"))
				if action == "activate" || action == "rollback" {
					f.request(ctx, "install", body, 200)
					if action == "activate" {
						f.request(ctx, "activate", `{"version":"1.0.3"}`, 200)
						endpoint, body = "activate", `{"version":"1.0.4"}`
					} else {
						endpoint, body, rejected, recovered = "rollback", `{}`, "1.0.3", 104
					}
				} else if action == "use_bundled" || action == "save-bundled" {
					endpoint, body, rejected = "use_bundled", `{}`, "1.0.2"
				}
			}
			var raw map[string]any
			if strings.HasPrefix(action, "save-") {
				f.saveErr = errors.New("disk full")
			} else {
				raw = map[string]any{"reject_version": rejected}
				f.s.deps.Cfg.Drivers[0].Config = maps.Clone(raw)
				if err := f.s.deps.SaveConfig(f.s.deps.ConfigPath, f.s.deps.Cfg); err != nil {
					t.Fatal(err)
				}
			}
			since := time.Now()
			response := f.request(ctx, endpoint, body, 502)
			if strings.Contains(response["error"].(string), "recovery failed") || strings.Contains(response["error"].(string), "rollback failed") {
				t.Fatalf("runtime recovery failed: %v", response)
			}
			assertDriverSoC(t, f, since, recovered, 0.23, 0.81, raw)
		})
	}
}
