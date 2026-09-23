package loadpoint

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPauseNeedsFreshStoppedCharger(t *testing.T) {
	now := time.Now()
	h := ManualHold{PowerW: 0, Persistent: true, StartedAt: now}
	st := State{Phases: 3, VoltageV: 230, CommandedKnown: true, CommandedW: 0, CommandedReason: "manual_hold", CommandedSinceMs: now.UnixMilli(), ManualCommandUpdatedAt: now}
	for _, tc := range []struct {
		name    string
		reading ChargerReading
		power   float64
		want    string
	}{
		{"still drawing", ChargerReading{Known: true, UpdatedAt: now, Charging: true, LimitKnown: true, LimitA: 16}, 11000, ManualPausing},
		{"old zero", ChargerReading{Known: true, UpdatedAt: now.Add(-time.Minute), LimitKnown: true, LimitA: 0}, 0, ManualPausing},
		{"car stopped but charger still offers", ChargerReading{Known: true, UpdatedAt: now, LimitKnown: true, LimitA: 16}, 0, ManualPausing},
		{"fresh pause", ChargerReading{Known: true, UpdatedAt: now, LimitKnown: true, LimitA: 0}, 0, ManualPaused},
		{"cloud lost", ChargerReading{Known: true, Unavailable: true, UpdatedAt: now, LimitKnown: true, LimitA: 0}, 0, ManualUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st.CurrentPowerW = tc.power
			got := ManualStatusFrom(h, true, st, tc.reading, now.Add(time.Second))
			if got.State != tc.want {
				t.Fatalf("got %+v want %s", got, tc.want)
			}
			b, _ := json.Marshal(got)
			if !strings.Contains(string(b), `"requested_w":0`) {
				t.Fatalf("pause omitted zero: %s", b)
			}
		})
	}
}

func TestLowerCurrentWaitsForChargerEvenWhilePowerFlows(t *testing.T) {
	now := time.Now()
	h := ManualHold{PowerW: 4140, Persistent: true, StartedAt: now.Add(-time.Hour), UpdatedAt: now}
	st := State{Phases: 3, VoltageV: 230, CurrentPowerW: 11000, CommandedKnown: true, CommandedW: 4140, CommandedReason: "manual_hold", CommandedSinceMs: now.UnixMilli(), ManualCommandUpdatedAt: now}
	ch := ChargerReading{Known: true, Charging: true, LimitKnown: true, LimitA: 16, UpdatedAt: now}
	if got := ManualStatusFrom(h, true, st, ch, now.Add(5*time.Second)); got.State != ManualSent {
		t.Fatalf("old11kW falsely confirmed6A: %+v", got)
	}
	if got := ManualStatusFrom(h, true, st, ch, now.Add(4*time.Minute)); got.State != ManualStalled {
		t.Fatalf("unconfirmed reduction did not time out: %+v", got)
	}
	ch.LimitA = 6
	ch.UpdatedAt = now.Add(10 * time.Second)
	if got := ManualStatusFrom(h, true, st, ch, now.Add(12*time.Second)); got.State != ManualCharging {
		t.Fatalf("confirmed reduction not shown: %+v", got)
	}
}

func TestFreshOldCommandCannotConfirmANewManualChoice(t *testing.T) {
	now := time.Now()
	h := ManualHold{PowerW: 4140, Persistent: true, StartedAt: now.Add(-time.Hour), UpdatedAt: now}
	st := State{Phases: 3, VoltageV: 230, CurrentPowerW: 11000, CommandedKnown: true, CommandedW: 11000, CommandedReason: "manual_hold", ManualCommandUpdatedAt: now.Add(-time.Minute)}
	ch := ChargerReading{Known: true, Charging: true, LimitKnown: true, LimitA: 16, UpdatedAt: now.Add(time.Second)}
	if got := ManualStatusFrom(h, true, st, ch, now.Add(2*time.Second)); got.State != ManualSent {
		t.Fatalf("old command acknowledged new choice: %+v", got)
	}
	// Even an unchanged clamped order must be computed for the new choice.
	st.CommandedReason = "fuse_limit"
	st.CommandedW = 4140
	ch.LimitA = 6
	if got := ManualStatusFrom(h, true, st, ch, now.Add(2*time.Second)); got.State != ManualSent {
		t.Fatalf("old clamp acknowledged new choice: %+v", got)
	}
	st.ManualCommandUpdatedAt = h.UpdatedAt
	if got := ManualStatusFrom(h, true, st, ch, now.Add(2*time.Second)); got.State != ManualCharging || got.LimitReason != "fuse_limit" {
		t.Fatalf("new command not acknowledged: %+v", got)
	}
	h.PowerW = 0
	st.CommandedW = 0
	st.CurrentPowerW = 0
	st.CommandedReason = "manual_hold"
	st.ManualCommandUpdatedAt = now.Add(-time.Minute)
	ch.Charging = false
	ch.LimitA = 0
	if got := ManualStatusFrom(h, true, st, ch, now.Add(2*time.Second)); got.State != ManualPausing {
		t.Fatalf("old pause acknowledged new choice: %+v", got)
	}
}
