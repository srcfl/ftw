package state

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestVehicleWakeBudgetSurvivesRestartAndIsPerHardware(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1760000000, 0)
	for attempt := 0; attempt < 3; attempt++ {
		allowed, _, err := s.ReserveVehicleWake("tesla:VIN-A", now)
		if err != nil || !allowed {
			t.Fatalf("attempt %d: %v %v", attempt, allowed, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		allowed, retry, err := s.ReserveVehicleWake("tesla:VIN-A", now.Add(time.Second))
		if err != nil || allowed || retry <= 0 {
			t.Fatalf("restart renewed budget: %v %v %v", allowed, retry, err)
		}
		now = now.Add(vehicleWakeGap)
	}
	defer s.Close()
	if allowed, _, err := s.ReserveVehicleWake("tesla:VIN-A", now); allowed || err != nil {
		t.Fatalf("fourth wake: %v %v", allowed, err)
	}
	if allowed, _, err := s.ReserveVehicleWake("tesla:VIN-B", now); !allowed || err != nil {
		t.Fatalf("other VIN: %v %v", allowed, err)
	}
	if allowed, _, err := s.ReserveVehicleWake("tesla:VIN-A", time.Unix(1760000000, 0).Add(vehicleWakeWindow)); !allowed || err != nil {
		t.Fatalf("new window: %v %v", allowed, err)
	}
}

func TestVehicleWakeFailsClosed(t *testing.T) {
	s := freshStore(t)
	now := time.Now()
	if allowed, _, err := s.ReserveVehicleWake("", now); allowed || err == nil {
		t.Fatal("missing identity admitted")
	}
	if err := s.SaveConfig("vehicle_wake:tesla:bad", "broken"); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := s.ReserveVehicleWake("tesla:bad", now); allowed || err == nil {
		t.Fatal("broken checkpoint admitted")
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_wake BEFORE INSERT ON config WHEN NEW.key LIKE 'vehicle_wake:%' BEGIN SELECT RAISE(ABORT, 'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := s.ReserveVehicleWake("tesla:unwritten", now); allowed || err == nil {
		t.Fatal("uncommitted attempt admitted")
	}
}

func TestVehicleWakeConcurrentReservationsAndClockRollback(t *testing.T) {
	s := freshStore(t)
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			allowed, _, _ := s.ReserveVehicleWake("tesla:one", now)
			if allowed {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("concurrent wakes accepted=%d", accepted)
	}
	if allowed, retry, err := s.ReserveVehicleWake("tesla:one", now.Add(-time.Minute)); allowed || retry < time.Minute || err != nil {
		t.Fatalf("clock rollback: %v %v %v", allowed, retry, err)
	}
}
