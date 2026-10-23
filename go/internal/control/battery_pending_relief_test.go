package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Ending discharge raises import; ending charge raises export. A prior rise
// still absent from the meter must consume room before either can rise again.
func TestPendingBatteryReliefDoesNotSpendPhaseRoomTwice(t *testing.T) {
	for _, direction := range []float64{1, -1} {
		for _, layer := range []string{"allocation", "manual", "idle"} {
			name := "import"
			if direction < 0 {
				name = "export"
			}
			t.Run(name+"/"+layer, func(t *testing.T) {
				now := time.Now().UTC()
				store := telemetry.NewStore()
				data, _ := json.Marshal(map[string]float64{
					"l1_a": 22.3 * direction, "l2_a": 12.3 * direction, "l3_a": 12.3 * direction,
					"l1_w": 22.3 * 230 * direction, "l2_w": 12.3 * 230 * direction, "l3_w": 12.3 * 230 * direction,
				})
				store.Update("meter", telemetry.DerMeter, 10787*direction, nil, data)
				store.RecordDriverSuccess("meter")
				soc := .5
				actual := -2000 * direction
				store.Update("battery", telemetry.DerBattery, actual, &soc, nil)
				store.RecordDriverSuccess("battery")
				store.Get("meter", telemetry.DerMeter).UpdatedAt = now
				store.Get("battery", telemetry.DerBattery).UpdatedAt = now
				st := phaseState(25, 1)
				st.clock = func() time.Time { return now }
				st.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 5000}}
				st.batteryCommands = map[string]commandLog{"battery": commandLog{}.add(now.Add(-time.Hour), actual).add(now.Add(-2*time.Second), -1000*direction)}
				var out []DispatchTarget
				if layer == "allocation" {
					out = fitBatteryRisesToFuse([]DispatchTarget{{Driver: "battery", TargetW: 0}}, store, st)
				} else {
					st.SlewEnabled = false
					st.MinDispatchIntervalS = 0
					if layer == "manual" {
						st.SetBatteryManualHold(BatteryManualHold{PowerW: 0, ExpiresAt: now.Add(time.Minute)})
					} else {
						st.Mode = ModeIdle
					}
					out = ComputeDispatch(store, st, map[string]float64{"battery": 20000}, 17250)
				}
				if len(out) != 1 {
					t.Fatalf("missing battery target: %+v", out)
				}
				projected := 22.3 + direction*(out[0].TargetW-actual)/690
				if !finiteFuseValue(out[0].TargetW) || projected > 24.01 {
					t.Fatalf("pending rise spent twice: target %.1f W projects %.3f A", out[0].TargetW, projected)
				}
				if math.Abs(out[0].TargetW+827*direction) > 1e-6 {
					t.Fatalf("target %.3f W, want %.3f W", out[0].TargetW, -827*direction)
				}
			})
		}
	}
}

func TestPendingBatteryReliefRespectsAvailableRoomAndBounds(t *testing.T) {
	cases := []struct {
		name                            string
		pending, room, last, keep, want float64
	}{
		{"pending", 1000, 1173, -1000, 1000, -827},
		{"settled", 0, 1173, -1000, 1000, 0},
		{"room_for_both", 1000, 2500, -1000, 1000, 0},
		{"direction_blocked", 1000, 1173, -1000, 0, 0},
		{"battery_floor_bounds_relief", 1000, 1173, -1000, 500, -500},
		{"no_existing_relief", 2000, 1173, 0, 1000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := capBatteryCharge([]DispatchTarget{{Driver: "battery", TargetW: 0}}, map[string]float64{"battery": c.last}, c.pending, c.room, map[string]float64{"battery": c.keep})
			if got := out[0].TargetW; math.Abs(got-c.want) > 1e-6 {
				t.Fatalf("target %.3f want %.3f", got, c.want)
			}
		})
	}
}
