package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEVObservationPreservesSessionWhenCloudIsStale(t *testing.T) {
	now := time.Now()
	health := &telemetry.DriverHealth{Status: telemetry.StatusOk}
	r := &telemetry.DerReading{UpdatedAt: now, SmoothedW: 4140, Data: json.RawMessage(`{"connected":true,"session_wh":600,"session_id":"728"}`)}
	s, ok := currentEVSample(r, health, time.Minute, now, false, "sn:charger")
	if !ok || !s.Connected || !s.RequestActive || s.SessionID != "728" || s.DeviceID != "sn:charger" {
		t.Fatalf("valid session lost: %+v %v", s, ok)
	}
	r.UpdatedAt = now.Add(-2 * time.Minute)
	if _, ok := currentEVSample(r, health, time.Minute, now, false, "sn:charger"); ok {
		t.Fatal("stale reading accepted")
	}
	r.UpdatedAt = now
	for _, body := range []string{`{`, `{}`, `{"connected":true,"session_wh":-1}`} {
		r.Data = json.RawMessage(body)
		if _, ok := currentEVSample(r, health, time.Minute, now, false, "sn:charger"); ok {
			t.Fatalf("bad response became a session reading: %s", body)
		}
	}
	r.Data = json.RawMessage(`{"connected":false}`)
	if s, ok := currentEVSample(r, nil, time.Minute, now, true, ""); !ok || s.Connected {
		t.Fatalf("fresh OCPP unplug lost: %+v %v", s, ok)
	}
}
