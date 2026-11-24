package loadpoint

import (
	"testing"
	"time"
)

// Field report (srcfl/ftw#1948): after Charge now the Easee read back the
// order while its load balancing allowed 0 A for 10–14 minutes. The hold
// said the car was not drawing after two minutes, which blames the car.
// While the driver reports that the load balancing holds the start, the
// hold waits up to LoadBalancerRaiseWait; after that it is not drawing.
func TestManualHoldWaitsOutALoadBalancerHold(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 2, 24, 52, 0, time.UTC)
	hold := ManualHold{PowerW: 8280, Persistent: true, StartedAt: t0} // 12 A × 3 × 230 V
	st := State{Phases: 3, VoltageV: 230, CommandedW: 8280, CommandedKnown: true, CommandedReason: "manual_hold", CommandedSinceMs: t0.UnixMilli()}
	zero, four, six := 0.0, 4.0, 6.0
	reading := func(balancer *float64) ChargerReading {
		return ChargerReading{Known: true, LimitA: 12, LimitKnown: true, Reason: "limited by dynamic charger current", BalancerA: balancer}
	}
	for _, tc := range []struct {
		name     string
		balancer *float64
		after    time.Duration
		want     string
	}{
		{"no hold reported", nil, 3 * time.Minute, ManualNotDrawing},
		{"held at 0 A", &zero, 3 * time.Minute, ManualAccepted},
		{"held at 4 A", &four, 14 * time.Minute, ManualAccepted},
		{"held past the wait", &zero, LoadBalancerRaiseWait, ManualNotDrawing},
		{"a limit at the order is no hold", &six, 3 * time.Minute, ManualNotDrawing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := reading(tc.balancer)
			if tc.name == "a limit at the order is no hold" {
				ch.LimitA, hold.PowerW, st.CommandedW = 6, 4140, 4140
				defer func() { hold.PowerW, st.CommandedW = 8280, 8280 }()
			}
			m := ManualStatusFrom(hold, true, st, ch, t0.Add(tc.after))
			if m.State != tc.want {
				t.Fatalf("state %s, want %s", m.State, tc.want)
			}
			if (m.LoadBalancerA == nil) != (tc.balancer == nil) || m.LoadBalancerA != nil && *m.LoadBalancerA != *tc.balancer {
				t.Fatalf("load balancer current %v, want %v", m.LoadBalancerA, tc.balancer)
			}
		})
	}
}

// Codex review on #2050: a hold that starts after a long charge gets the
// full wait, timed from the first zero reading, not from the order.
func TestManualHoldTimesALateLoadBalancerHoldFromZeroDelivery(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 2, 24, 52, 0, time.UTC)
	hold := ManualHold{PowerW: 8280, Persistent: true, StartedAt: t0}
	st := State{Phases: 3, VoltageV: 230, CommandedW: 8280, CommandedKnown: true, CommandedReason: "manual_hold", CommandedSinceMs: t0.UnixMilli()}
	zero := 0.0
	cut := t0.Add(20 * time.Minute)
	for _, tc := range []struct {
		name string
		at   time.Duration
		want string
	}{
		{"three minutes into the hold", 3 * time.Minute, ManualAccepted},
		{"past the wait", LoadBalancerRaiseWait, ManualNotDrawing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := ChargerReading{Known: true, LimitA: 12, LimitKnown: true, BalancerA: &zero,
				NoPowerSince: cut, NoPowerAt: cut.Add(tc.at)}
			m := ManualStatusFrom(hold, true, st, ch, cut.Add(tc.at))
			if m.State != tc.want {
				t.Fatalf("state %s, want %s", m.State, tc.want)
			}
		})
	}
}
