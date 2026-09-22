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

func TestChargingPeriodResetsOnLostConnectionProof(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identified bool
		generation uint64
		unknown    bool
	}{
		{"unknown identified connection", true, 1, true},
		{"unknown unidentified connection", false, 1, true},
		{"new connection", true, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager()
			now := time.Unix(1700000000, 0)
			m.SetNowFn(func() time.Time { return now })
			m.Load([]Config{{ID: "car"}})
			m.observeConnectionProof("car", 1, false)
			m.Observe("car", true, 4140, 0, true)
			if tc.identified {
				m.byID["car"].sessionDeviceID = "charger"
				m.byID["car"].sessionID = "session"
			}
			now = now.Add(10 * time.Second)
			if on, elapsed := m.ChargingPeriod("car"); !on || elapsed != 10*time.Second {
				t.Fatalf("before connection loss: charging=%v elapsed=%s", on, elapsed)
			}
			m.observeConnectionProof("car", tc.generation, tc.unknown)
			if on, elapsed := m.ChargingPeriod("car"); on || elapsed != 0 {
				t.Fatalf("cached power survived connection loss: charging=%v elapsed=%s", on, elapsed)
			}
			m.observeConnectionProof("car", tc.generation, false)
			if on, _ := m.ChargingPeriod("car"); on {
				t.Fatal("connection proof alone restored charging")
			}
			now = now.Add(time.Second)
			m.Observe("car", true, 4140, 0, true)
			if on, elapsed := m.ChargingPeriod("car"); !on || elapsed != 0 {
				t.Fatalf("fresh observation retained pre-gap duration: charging=%v elapsed=%s", on, elapsed)
			}
		})
	}
}
