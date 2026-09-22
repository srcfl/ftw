package loadpoint

import (
	"testing"
	"time"
)

func TestChargingPeriodUsesFreshObservedSession(t *testing.T) {
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNowFn(func() time.Time { return now })
	cfg := []Config{{ID: "car", DriverName: "test", MaxChargeW: 11000}}
	m.Load(cfg)
	check := func(want bool, duration time.Duration) {
		t.Helper()
		got, elapsed := m.ChargingPeriod("car")
		if got != want || elapsed != duration {
			t.Fatalf("charging=%v elapsed=%s, want %v/%s", got, elapsed, want, duration)
		}
	}
	check(false, 0)
	m.Observe("car", true, 4140, 0, true)
	now = now.Add(20 * time.Second)
	m.Observe("car", true, 4140, 23, true)
	check(true, 20*time.Second)
	m.Load(cfg)
	check(true, 20*time.Second)
	now = now.Add(31 * time.Second)
	check(false, 0)
	m.Observe("car", true, 4140, 23, true)
	check(true, 0) // the observation gap cannot prove continuous charging
	m.Observe("car", true, 0, 23, true)
	check(false, 0)
	now = now.Add(time.Minute)
	m.Observe("car", true, 4140, 23, true)
	check(true, 0)
	m.ObserveSample("car", EVSample{Connected: true, PowerUnavailable: true, PowerAt: now})
	check(false, 0)
	m.Observe("car", false, 0, 0, false)
	check(false, 0)
	m.Observe("car", true, 4140, 0, true)
	check(true, 0)
}

func TestChargingPeriodHonorsSourceCadence(t *testing.T) {
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNowFn(func() time.Time { return now })
	m.Load([]Config{{ID: "car"}})
	m.ObserveSample("car", EVSample{Connected: true, PowerW: 4140, PowerAt: now, PowerMaxAge: 2 * time.Minute})
	now = now.Add(time.Minute)
	if on, _ := m.ChargingPeriod("car"); !on {
		t.Fatal("fresh slow source lost")
	}
	now = now.Add(61 * time.Second)
	if on, _ := m.ChargingPeriod("car"); on {
		t.Fatal("stale source credited")
	}
}
