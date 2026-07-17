package drivers

import (
	"context"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type deyeInitModbus struct{}

func (deyeInitModbus) Read(_ uint16, count uint16, _ int32) ([]uint16, error) {
	return make([]uint16, count), nil
}

func (deyeInitModbus) WriteSingle(uint16, uint16) error  { return nil }
func (deyeInitModbus) WriteMulti(uint16, []uint16) error { return nil }
func (deyeInitModbus) Close() error                      { return nil }

func TestDeyeInitUsesDefaultAndConfiguredSoCLimits(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"defaults": nil,
		"configured": {
			"soc_min": 80,
			"soc_max": 20, // exercises the safe swap path too
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := NewHostEnv("deye-init", telemetry.NewStore()).WithModbus(deyeInitModbus{})
			driver, err := NewLuaDriver("../../../drivers/deye.lua", env)
			if err != nil {
				t.Fatalf("load Deye driver: %v", err)
			}
			defer driver.Cleanup()

			if err := driver.Init(context.Background(), cfg); err != nil {
				t.Fatalf("Deye driver_init: %v", err)
			}
		})
	}
}
