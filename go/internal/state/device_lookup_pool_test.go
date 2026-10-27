package state

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// LookupDeviceByDriverName (called every control tick through
// forecastCurtailment.PendingDrivers) must answer from the in-memory
// deviceCache, so a busy four-connection state.db pool cannot stall the
// tick.
func TestLookupDeviceByDriverNameDoesNotWaitForStatePool(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.RegisterDevice(Device{DriverName: "battery", Make: "Sungrow", Serial: "A1"}); err != nil {
		t.Fatal(err)
	}

	// Occupy every pooled connection, standing in for config writes,
	// forecast archive/prune and backup.
	ctx := context.Background()
	var held []*sql.Conn
	for i := 0; i < 4; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	release := func() {
		for _, c := range held {
			_ = c.Close()
		}
		held = nil
	}
	defer release()

	// The cache already has the answer, without touching SQL.
	cacheStart := time.Now()
	var cached string
	for _, d := range s.CachedDevices() {
		if d.DriverName == "battery" {
			cached = d.DeviceID
		}
	}
	if cached != "sungrow:A1" {
		t.Fatalf("cache device id = %q", cached)
	}
	t.Logf("CachedDevices answered in %v", time.Since(cacheStart))

	done := make(chan *Device, 1)
	go func() { done <- s.LookupDeviceByDriverName("battery") }()
	select {
	case d := <-done:
		t.Logf("lookup returned %+v", d)
	case <-time.After(time.Second):
		release()
		d := <-done
		t.Fatalf("LookupDeviceByDriverName waited over 1s for a state.db connection (returned %+v only after the pool was released)", d)
	}
}
