package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type writeCountingModbus struct {
	writes atomic.Int32
}

func (m *writeCountingModbus) Read(_ uint16, count uint16, _ int32) ([]uint16, error) {
	return make([]uint16, count), nil
}

func (m *writeCountingModbus) WriteSingle(uint16, uint16) error {
	m.writes.Add(1)
	return nil
}

func (m *writeCountingModbus) WriteMulti(uint16, []uint16) error {
	m.writes.Add(1)
	return nil
}

func (m *writeCountingModbus) Close() error { return nil }

func TestReleaseOnZeroHybridsAreWriteInert(t *testing.T) {
	entries, err := LoadCatalog("../../../drivers")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}

	commands := []map[string]any{
		{"action": "init", "power_w": 0},
		{"action": "battery", "power_w": 2000},
		{"action": "battery", "power_w": -2000},
		{"action": "battery", "power_w": 0},
		{"action": "curtail", "power_w": 1000},
		{"action": "curtail_disable", "power_w": 0},
		{"action": "deinit", "power_w": 0},
	}

	for _, name := range []string{"huawei", "ferroamp_modbus"} {
		t.Run(name, func(t *testing.T) {
			path := "../../../drivers/" + name + ".lua"
			modbus := &writeCountingModbus{}
			driver, err := NewLuaDriver(path, NewHostEnv(name, telemetry.NewStore()).WithModbus(modbus))
			if err != nil {
				t.Fatalf("load %s: %v", name, err)
			}
			if err := driver.Init(context.Background(), nil); err != nil {
				t.Fatalf("init %s: %v", name, err)
			}

			commandErrors := make([]error, 0, len(commands))
			for _, command := range commands {
				body, err := json.Marshal(command)
				if err != nil {
					t.Fatal(err)
				}
				commandErrors = append(commandErrors, driver.Command(context.Background(), body))
			}
			defaultErr := driver.DefaultMode()
			driver.Cleanup()

			if got := modbus.writes.Load(); got != 0 {
				t.Fatalf("command/default/cleanup attempted %d Modbus writes", got)
			}
			if !IsReadOnlyDriver(entries, name+".lua") {
				t.Fatalf("%s must declare read_only=true", name)
			}
			for i, err := range commandErrors {
				if !errors.Is(err, ErrReadOnlyDriver) {
					t.Fatalf("command %v: got %v, want read-only refusal", commands[i], err)
				}
			}
			if defaultErr != nil {
				t.Fatalf("default mode: %v", defaultErr)
			}
		})
	}
}
