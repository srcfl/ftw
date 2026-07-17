package integration_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/drivers"
	ftwmodbus "github.com/srcfl/ftw/go/internal/modbus"
	ftwmqtt "github.com/srcfl/ftw/go/internal/mqtt"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const sharedCatalogID = "591626853fbc5e38dbfe85a83a06835cd71719047a4a369a64ae52e1c6dad6e1"

var lockedDriverHashes = map[string]string{
	"ambibox.lua": "d7e8b9b74fbff52fd9230100e0ce407ff738502bfb4720873d2f54cc7ab78ec1",
	"deye.lua":    "31d606df923536be6504fab9b5ee4da4654cd1167e516bea02320c77b2582b7b",
	"sdm630.lua":  "ce1b514f0a55d19f8a388addac31c2b715dc21df2d4cd76a08964539791b71ab",
	"sungrow.lua": "466a5f8637e6756fc2e1af4197d4edc1845474231413c0016f0e5900acb7b7ac",
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Skipf("%s is not set; run through Zap tools/lua_lab/run_device_simulator_chain.sh", name)
	}
	return value
}

func requiredPort(t *testing.T, name string) int {
	t.Helper()
	value := requiredEnv(t, name)
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("invalid %s=%q", name, value)
	}
	return port
}

func pollCycles(t *testing.T) int {
	t.Helper()
	value := os.Getenv("DEVICE_SIMULATOR_POLL_CYCLES")
	if value == "" {
		return 25
	}
	cycles, err := strconv.Atoi(value)
	if err != nil || cycles < 1 {
		t.Fatalf("invalid DEVICE_SIMULATOR_POLL_CYCLES=%q", value)
	}
	return cycles
}

func lockedDriverPath(t *testing.T, catalogDir string, name string) string {
	t.Helper()
	path := filepath.Join(catalogDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read locked driver %s: %v", name, err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(data))
	if got != lockedDriverHashes[name] {
		t.Fatalf("catalog %s driver %s hash = %s, want %s",
			sharedCatalogID, name, got, lockedDriverHashes[name])
	}
	return path
}

func TestLockedLuaDriversAgainstDeviceSimulator(t *testing.T) {
	catalogDir := requiredEnv(t, "LUA_CATALOG_DIR")
	host := requiredEnv(t, "DEVICE_SIMULATOR_HOST")

	modbusCases := []struct {
		name       string
		driver     string
		portEnv    string
		metricName string
		config     map[string]any
	}{
		{
			name:       "sim-sungrow",
			driver:     "sungrow.lua",
			portEnv:    "DEVICE_SIMULATOR_SUNGROW_PORT",
			metricName: "pv_w_primary",
			config:     map[string]any{},
		},
		{
			name:       "sim-deye",
			driver:     "deye.lua",
			portEnv:    "DEVICE_SIMULATOR_DEYE_PORT",
			metricName: "inverter_temp_c",
			config:     map[string]any{},
		},
		{
			name:       "sim-sdm630",
			driver:     "sdm630.lua",
			portEnv:    "DEVICE_SIMULATOR_SDM630_PORT",
			metricName: "grid_hz",
			config:     map[string]any{},
		},
	}

	for _, tc := range modbusCases {
		t.Run(tc.name, func(t *testing.T) {
			capability, err := ftwmodbus.Dial(host, requiredPort(t, tc.portEnv), 1)
			if err != nil {
				t.Fatalf("dial simulator: %v", err)
			}
			defer capability.Close()

			store := telemetry.NewStore()
			env := drivers.NewHostEnv(tc.name, store).WithModbus(capability)
			env.BatteryCapacityWh = 10_000
			driver, err := drivers.NewLuaDriver(
				lockedDriverPath(t, catalogDir, tc.driver), env,
			)
			if err != nil {
				t.Fatalf("load driver: %v", err)
			}
			defer driver.Cleanup()

			if err := driver.Init(context.Background(), tc.config); err != nil {
				t.Fatalf("driver init: %v", err)
			}
			for cycle := 0; cycle < pollCycles(t); cycle++ {
				if _, err := driver.Poll(context.Background()); err != nil {
					t.Fatalf("driver poll cycle %d: %v", cycle, err)
				}
			}
			if _, _, ok := store.LatestMetric(tc.name, tc.metricName); !ok {
				t.Fatalf("driver did not emit metric %q", tc.metricName)
			}
		})
	}

	t.Run("sim-ambibox", func(t *testing.T) {
		capability, err := ftwmqtt.Dial(
			host,
			requiredPort(t, "DEVICE_SIMULATOR_AMBIBOX_PORT"),
			"",
			"",
			"ftw-lua-simulator-chain",
		)
		if err != nil {
			t.Fatalf("dial simulator: %v", err)
		}
		defer capability.Close()

		store := telemetry.NewStore()
		env := drivers.NewHostEnv("sim-ambibox", store).WithMQTT(capability)
		env.BatteryCapacityWh = 10_000
		driver, err := drivers.NewLuaDriver(
			lockedDriverPath(t, catalogDir, "ambibox.lua"), env,
		)
		if err != nil {
			t.Fatalf("load driver: %v", err)
		}
		defer driver.Cleanup()
		if err := driver.Init(context.Background(), map[string]any{}); err != nil {
			t.Fatalf("driver init: %v", err)
		}

		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			if _, err := driver.Poll(context.Background()); err != nil {
				t.Fatalf("driver poll: %v", err)
			}
			if store.Get("sim-ambibox", telemetry.DerV2X) != nil {
				return
			}
		}
		t.Fatal("driver did not emit V2X telemetry from simulator MQTT topics")
	})
}
