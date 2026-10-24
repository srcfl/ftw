package telemetry

import (
	"testing"
	"time"
)

func TestOnlineEVControlPowerRejectsUnqualifiedCache(t *testing.T) {
	for _, tc := range []string{"fresh", "offline", "fault", "stale receipt", "future receipt", "unavailable", "missing health"} {
		t.Run(tc, func(t *testing.T) {
			s := NewStore()
			s.Update("ev", DerEV, 4140, nil, nil)
			s.RecordDriverSuccess("ev")
			now := time.Now()
			r := s.Get("ev", DerEV)
			r.UpdatedAt = now
			want := tc == "fresh"
			switch tc {
			case "offline":
				s.DriverHealthMut("ev").SetOffline()
			case "fault":
				s.DriverHealthMut("ev").SetDeviceFault(true, "test")
			case "stale receipt":
				r.UpdatedAt = now.Add(-16 * time.Second)
			case "future receipt":
				r.UpdatedAt = now.Add(time.Second)
			case "unavailable":
				r.Data = []byte(`{"control_power_available":false}`)
			case "missing health":
				delete(s.health, "ev")
			}
			p, ok := s.OnlineEVControlPower("ev", now, 15*time.Second)
			if ok != want || (ok && p.PowerW != 4140) {
				t.Fatalf("observation=%+v known=%v want %v", p, ok, want)
			}
		})
	}
}
