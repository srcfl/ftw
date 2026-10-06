package drivers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestLuaVehicleWakeHostBindingAndReadOnlyBoundary(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		t.Run(map[bool]string{false: "vehicle", true: "read_only"}[readonly], func(t *testing.T) {
			declaration := "false"
			if readonly {
				declaration = "true"
			}
			source := `DRIVER = { read_only = ` + declaration + ` }
function driver_init(config) host.set_make("Tesla"); host.set_sn("VIN-A") end
function driver_poll()
  assert(host.unix_ms() > 1700000000000)
  local ok, retry, err = host.reserve_vehicle_wake()
  assert(retry > 0)
  if DRIVER.read_only then assert(not ok and err) else assert(ok and not err) end
end
function driver_default_mode() end`
			path := filepath.Join(t.TempDir(), "vehicle.lua")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			env := NewHostEnv("renamed-driver", telemetry.NewStore())
			calls := 0
			env.ReserveVehicleWake = func(makeName, serial string) (bool, time.Duration, error) {
				if makeName != "Tesla" || serial != "VIN-A" {
					t.Fatal("reservation lost hardware identity")
				}
				calls++
				return true, 90 * time.Second, nil
			}
			d, err := NewLuaDriver(path, env)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Cleanup()
			if err := d.Init(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 1
			if readonly {
				want = 0
			}
			if calls != want {
				t.Fatalf("reservation calls=%d want=%d", calls, want)
			}
		})
	}
}
