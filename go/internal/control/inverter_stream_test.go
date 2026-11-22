package control

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A hybrid's inverter stream says what its AC stage does and how much more
// it will take from or give to the grid. Dispatch, its fuse protection and
// the battery split never read it: the same site with and without the
// stream gets the same targets, in every mode, tick after tick.
func TestInverterStreamNeverChangesDispatch(t *testing.T) {
	site := func(inverter bool) *telemetry.Store {
		s := seedStore(9000, []struct {
			name          string
			currentW, soc float64
		}{
			{"ferroamp", -1500, 0.6},
			{"solis", 2000, 0.4},
		})
		if inverter {
			for _, name := range []string{"ferroamp", "solis"} {
				r, err := telemetry.ParseInverterReading([]byte(`{"W":-20000,"available_import_W":0,"available_export_W":0,"L1_W":-7000}`))
				if err != nil {
					t.Fatal(err)
				}
				s.UpdateInverter(name, r, 1)
			}
		}
		return s
	}
	caps := map[string]float64{"ferroamp": 15200, "solis": 100000}
	clamped := 0
	for _, mode := range []Mode{ModeSelfConsumption, ModeCharge, ModeIdle, ModePeakShaving} {
		plain, with := site(false), site(true)
		a, b := NewState(0, 50, "ferroamp"), NewState(0, 50, "ferroamp")
		a.Mode, b.Mode = mode, mode
		a.MinDispatchIntervalS, b.MinDispatchIntervalS = 0, 0
		for tick := range 5 {
			// 9 kW import on a 16 A fuse: fuse protection is in play.
			ta, tb := ComputeDispatch(plain, a, caps, 11040), ComputeDispatch(with, b, caps, 11040)
			// Target order follows map order; the targets are what count.
			byDriver := func(x, y DispatchTarget) int { return strings.Compare(x.Driver, y.Driver) }
			slices.SortFunc(ta, byDriver)
			slices.SortFunc(tb, byDriver)
			if !reflect.DeepEqual(ta, tb) {
				t.Fatalf("%s tick %d: targets moved with the inverter stream:\n%+v\n%+v", mode, tick, ta, tb)
			}
			if len(ta) == 0 {
				t.Fatalf("%s tick %d: no targets to compare", mode, tick)
			}
			for _, target := range ta {
				if target.Clamped {
					clamped++
				}
			}
			plain.Update("ferroamp", telemetry.DerMeter, 9000, nil, nil)
			with.Update("ferroamp", telemetry.DerMeter, 9000, nil, nil)
		}
	}
	if clamped == 0 {
		t.Fatal("no target was clamped, so fuse protection was never compared")
	}
}
