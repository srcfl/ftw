package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEVBatteryRealizedCreditRequiresFreshAlignedHealthyPower(t *testing.T) {
	for _, name := range []string{"fresh", "missing", "stale", "future", "skew", "fault", "offline", "unavailable", "cached source", "negative", "above demand"} {
		t.Run(name, func(t *testing.T) {
			store, state := evSplitFixture(t, 12000, nil, 10350, "3p")
			now := time.Now()
			state.clock = func() time.Time { return now }
			meter := store.Get("meter", telemetry.DerMeter)
			meter.UpdatedAt = now
			ev := store.Get("car", telemetry.DerEV)
			ev.UpdatedAt = now
			ev.RawW = 4155
			want := 4155.0
			switch name {
			case "missing":
				store.Remove("car")
				want = 0
			case "stale":
				ev.UpdatedAt = now.Add(-16 * time.Second)
				want = 0
			case "future":
				ev.UpdatedAt = now.Add(time.Second)
				want = 0
			case "skew":
				ev.UpdatedAt = now.Add(-11 * time.Second)
				want = 0
			case "fault":
				store.DriverHealthMut("car").SetDeviceFault(true, "test")
				want = 0
			case "offline":
				store.DriverHealthMut("car").SetOffline()
				want = 0
			case "unavailable":
				ev.Data = []byte(`{"control_power_available":false}`)
				want = 0
			case "cached source":
				ev.Data, _ = json.Marshal(map[string]any{"control_power_observed_at": now.Add(-time.Minute).Format(time.RFC3339Nano)})
				want = 0
			case "negative":
				ev.RawW = -1000
				want = 0
			case "above demand":
				ev.RawW = 20000
				want = 10350
			}
			if got := realizedEVForBatteryW(store, state, meter, now); got != want {
				t.Fatalf("credited %.0f W want %.0f W", got, want)
			}
		})
	}
}

func TestEVBatteryUnknownDeliveryKeepsFullReservation(t *testing.T) {
	store, state := evSplitFixture(t, 12000, []float64{22, 21, 20}, 10350, "3p")
	sent(state, "ev", 10350, 10350)
	sent(state, "first", 8000, 8000)
	store.Remove("car")
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 8000}}, store, state, 17250)
	if got := targetW(out, "first"); got != 0 {
		t.Fatalf("unknown EV delivery released battery room: %.0f W", got)
	}
}

func TestEVBatteryFreshSiblingCannotCreditMissingCharger(t *testing.T) {
	store, state := evSplitFixture(t, 12000, nil, 4140, "3p")
	state.evFuseDemands = append(state.evFuseDemands, evFuseDemand{"missing", 6210, "3p"})
	now := time.Now()
	state.clock = func() time.Time { return now }
	if got := realizedEVForBatteryW(store, state, store.Get("meter", telemetry.DerMeter), now); got != 4140 {
		t.Fatalf("credited %.0f W", got)
	}
}
