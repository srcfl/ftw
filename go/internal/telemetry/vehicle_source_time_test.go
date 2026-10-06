package telemetry

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestVehicleSourceTimeControlsFreshnessAndFullCarRecovery(t *testing.T) {
	s := NewStore()
	s.DriverHealthMut("tesla").RecordSuccess()
	now := time.Now()
	emit := func(at time.Time, value float64) {
		s.Update("tesla", DerVehicle, 0, &value, json.RawMessage(fmt.Sprintf(`{"soc":%g,"soc_fresh":true,"soc_observed_at_ms":%d,"charging_state":"Complete"}`, value*100, at.UnixMilli())))
	}
	old := now.Add(-10 * time.Minute)
	emit(old, 0.5)
	if got := s.Get("tesla", DerVehicle); !got.SoCUpdatedAt.Equal(time.UnixMilli(old.UnixMilli())) {
		t.Fatalf("source time replaced: %v", got.SoCUpdatedAt)
	}
	if pick := PickBestVehicle(s, now); pick.Driver != "" {
		t.Fatal("old first response became planning truth")
	}
	emit(old, 0.5)
	if pick := PickBestVehicle(s, now); pick.Driver != "" {
		t.Fatal("cached response renewed observation")
	}
	s.FlushSamples()
	fresh := now.Add(-time.Second)
	emit(fresh, 1)
	pick := PickBestVehicle(s, now)
	if pick.Driver != "tesla" || pick.SoC != 1 {
		t.Fatalf("full car did not replace inferred demand: %+v", pick)
	}
	if samples := s.FlushSamples(); len(samples) != 2 {
		t.Fatalf("fresh source not stored: %+v", samples)
	}
	emit(fresh, 0.1)
	if got := s.Get("tesla", DerVehicle); *got.SoC != 1 || !got.SoCUpdatedAt.Equal(pick.UpdatedAt) {
		t.Fatal("identical source timestamp replaced full-car reading")
	}
}

func TestVehicleInvalidSourceTimeFailsClosed(t *testing.T) {
	for _, stamp := range []string{`null`, `"yesterday"`, `0`, `-1`, fmt.Sprint(time.Now().Add(time.Hour).UnixMilli())} {
		s := NewStore()
		soc := 0.8
		s.Update("tesla", DerVehicle, 0, &soc, json.RawMessage(`{"soc_fresh":true,"soc_observed_at_ms":`+stamp+`}`))
		if got := s.Get("tesla", DerVehicle); got.SoC != nil || !got.SoCUpdatedAt.IsZero() {
			t.Fatalf("invalid source %s admitted", stamp)
		}
	}
}
