package telemetry

import (
	"math"
	"testing"
	"time"
)

func TestOnlineBatteryControlPowersQualifiesDiagnosticContext(t *testing.T) {
	for _, mode := range []string{"fresh", "offline", "fault", "missing health", "stale receipt", "future receipt", "stale source", "source validity", "unavailable", "nonfinite", "invalid age"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			s := NewStore()
			s.Update("battery", DerBattery, 1500, nil, nil)
			s.RecordDriverSuccess("battery")
			r := s.Get("battery", DerBattery)
			r.UpdatedAt = now
			maxAge := 15 * time.Second
			switch mode {
			case "offline":
				s.DriverHealthMut("battery").SetOffline()
			case "fault":
				s.DriverHealthMut("battery").SetDeviceFault(true, "test")
			case "missing health":
				delete(s.health, "battery")
			case "stale receipt":
				r.UpdatedAt = now.Add(-16 * time.Second)
			case "future receipt":
				r.UpdatedAt = now.Add(time.Second)
			case "stale source":
				r.Data = []byte(`{"power_observed_at":"` + now.Add(-16*time.Second).Format(time.RFC3339Nano) + `"}`)
			case "source validity":
				r.Data = []byte(`{"power_max_age_s":1,"power_observed_at":"` + now.Add(-2*time.Second).Format(time.RFC3339Nano) + `"}`)
			case "unavailable":
				r.Data = []byte(`{"control_power_available":false}`)
			case "nonfinite":
				r.RawW = math.NaN()
			case "invalid age":
				maxAge = 0
			}
			// Another DER must not enter battery context.
			s.Update("car", DerEV, 8000, nil, nil)
			s.RecordDriverSuccess("car")
			got := s.OnlineBatteryControlPowers(now, maxAge)
			if mode == "fresh" {
				if len(got) != 1 || got[0].PowerW != 1500 {
					t.Fatalf("observations %+v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("unqualified context %+v", got)
			}
		})
	}
}
