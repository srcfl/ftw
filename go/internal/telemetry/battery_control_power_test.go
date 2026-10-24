package telemetry

import (
	"math"
	"testing"
	"time"
)

func TestOnlineBatteryControlPowerRejectsUnqualifiedCache(t *testing.T) {
	for _, mode := range []string{"fresh", "missing", "offline", "fault", "stale receipt", "future receipt", "stale source", "future source", "source validity", "unavailable", "nonfinite", "missing health", "invalid age"} {
		t.Run(mode, func(t *testing.T) {
			s := NewStore()
			now := time.Now().UTC()
			s.Update("battery", DerBattery, -690, nil, nil)
			s.RecordDriverSuccess("battery")
			r := s.Get("battery", DerBattery)
			r.UpdatedAt = now
			age := 15 * time.Second
			switch mode {
			case "missing":
				s.Remove("battery")
			case "offline":
				s.DriverHealthMut("battery").SetOffline()
			case "fault":
				s.DriverHealthMut("battery").SetDeviceFault(true, "test")
			case "stale receipt":
				r.UpdatedAt = now.Add(-16 * time.Second)
			case "future receipt":
				r.UpdatedAt = now.Add(time.Second)
			case "stale source":
				r.Data = []byte(`{"control_power_observed_at":"` + now.Add(-16*time.Second).Format(time.RFC3339Nano) + `"}`)
			case "future source":
				r.Data = []byte(`{"control_power_observed_at":"` + now.Add(time.Second).Format(time.RFC3339Nano) + `"}`)
			case "source validity":
				r.Data = []byte(`{"power_max_age_s":1,"control_power_observed_at":"` + now.Add(-2*time.Second).Format(time.RFC3339Nano) + `"}`)
			case "unavailable":
				r.Data = []byte(`{"control_power_available":false}`)
			case "nonfinite":
				r.RawW = math.NaN()
			case "missing health":
				delete(s.health, "battery")
			case "invalid age":
				age = 0
			}
			p, ok := s.OnlineBatteryControlPower("battery", now, age)
			if ok != (mode == "fresh") || (ok && p.PowerW != -690) {
				t.Fatalf("observation=%+v known=%v mode=%s", p, ok, mode)
			}
		})
	}
}
