package drivers

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// blockingSerialRegistry fails the first open, then blocks every later open
// until the test sends a result.
func blockingSerialRegistry(t *testing.T) (*Registry, chan struct{}, chan error) {
	t.Helper()
	entered := make(chan struct{}, 4)
	result := make(chan error)
	first := true
	r := NewRegistry(telemetry.NewStore())
	r.SerialFactory = func(name string, c *config.SerialConfig) (SerialCap, error) {
		if first {
			first = false
			return nil, errors.New("no such device")
		}
		entered <- struct{}{}
		if err := <-result; err != nil {
			return nil, err
		}
		return fakeSerial{}, nil
	}
	t.Cleanup(r.ShutdownAll)
	return r, entered, result
}

// startInflightRetry fails a boot Add, then runs its retry until it blocks
// in the serial open. The returned channel closes when the retry returns.
func startInflightRetry(t *testing.T, r *Registry, cfg config.Driver, entered chan struct{}) chan struct{} {
	t.Helper()
	if err := r.Add(context.Background(), cfg); err == nil {
		t.Fatal("expected first Add to fail")
	}
	e := pendingSpawnRetry(r, cfg.Name)
	if e == nil {
		t.Fatal("no retry scheduled")
	}
	e.timer.Stop()
	done := make(chan struct{})
	go func() { r.retrySpawn(cfg.Name, e); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not reach the serial open")
	}
	return done
}

// returnsWithin reports whether done closes within d.
func returnsWithin(done chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// The user disables a failing driver while a retry runs, and the retry then
// succeeds. The disabled driver must not be left running.
func TestReloadDuringInflightRetrySuccessStopsDriver(t *testing.T) {
	r, entered, result := blockingSerialRegistry(t)
	cfg := serialRetryDriver(t)
	done := startInflightRetry(t, r, cfg, entered)

	reloadDone := make(chan struct{})
	go func() {
		disabled := cfg
		disabled.Disabled = true
		r.Reload(context.Background(), []config.Driver{disabled}, false)
		close(reloadDone)
	}()
	if returnsWithin(reloadDone, 300*time.Millisecond) {
		t.Error("Reload returned while the retry attempt was still in flight")
	}
	result <- nil
	<-done
	<-reloadDone
	if slices.Contains(r.Names(), "p1") {
		t.Fatalf("driver disabled in config is running after an in-flight retry: %v", r.Names())
	}
	if e := pendingSpawnRetry(r, "p1"); e != nil {
		t.Fatalf("retry scheduled for a disabled driver: %+v", e)
	}
}

// Same, but the retry fails. No further retry may be scheduled.
func TestReloadDuringInflightRetryFailureStopsRetrying(t *testing.T) {
	r, entered, result := blockingSerialRegistry(t)
	cfg := serialRetryDriver(t)
	done := startInflightRetry(t, r, cfg, entered)

	reloadDone := make(chan struct{})
	go func() {
		r.Reload(context.Background(), nil, false)
		close(reloadDone)
	}()
	returnsWithin(reloadDone, 300*time.Millisecond)
	result <- errors.New("still missing")
	<-done
	<-reloadDone
	if e := pendingSpawnRetry(r, "p1"); e != nil {
		t.Fatalf("retry still scheduled (in %v) for a driver removed from config", e.delay)
	}
}

// ShutdownAll blocks until all poll loops exit, so it must wait for a retry
// that may still start a driver.
func TestShutdownAllWaitsForInflightRetry(t *testing.T) {
	r, entered, result := blockingSerialRegistry(t)
	cfg := serialRetryDriver(t)
	done := startInflightRetry(t, r, cfg, entered)

	shutDone := make(chan struct{})
	go func() { r.ShutdownAll(); close(shutDone) }()
	if returnsWithin(shutDone, 300*time.Millisecond) {
		t.Error("ShutdownAll returned while a retry attempt was still starting the driver")
	}
	result <- nil
	<-done
	<-shutDone
	if slices.Contains(r.Names(), "p1") {
		t.Fatalf("driver running after shutdown: %v", r.Names())
	}
}
