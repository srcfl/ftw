package drivers

import (
	"context"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type fakeSerial struct{}

func (fakeSerial) Read(int, time.Duration) ([]byte, error) { return nil, nil }
func (fakeSerial) Close() error                            { return nil }

func serialRetryRegistry(t *testing.T, present *atomic.Bool) *Registry {
	t.Helper()
	r := NewRegistry(telemetry.NewStore())
	r.SerialFactory = func(name string, c *config.SerialConfig) (SerialCap, error) {
		if !present.Load() {
			return OpenSerial(c) // real error for a missing node
		}
		return fakeSerial{}, nil
	}
	t.Cleanup(r.ShutdownAll)
	return r
}

func serialRetryDriver(t *testing.T) config.Driver {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "ttyUSB0")
	return config.Driver{
		Name: "p1",
		Lua:  writeTestDriver(t, registryRestartTestDriver),
		Capabilities: config.Capabilities{
			Serial: &config.SerialConfig{Address: missing, BaudRate: 115200, ReadTimeoutMS: 1000},
		},
	}
}

// A serial site meter whose device node is missing at boot must come back
// on its own once the device appears. The boot loop calls reg.Add once.
func TestBootSpawnFailureIsRetriedWhenSerialDeviceAppears(t *testing.T) {
	var present atomic.Bool
	r := serialRetryRegistry(t, &present)
	cfg := serialRetryDriver(t)

	// Same call the boot loop makes in cmd/ftw.
	err := r.Add(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected boot Add to fail while the device is missing")
	}
	t.Logf("boot Add error: %v", err)

	present.Store(true) // USB enumerates a moment later.

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(r.Names(), "p1") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("driver p1 still not registered 15s after its serial device appeared; registered=%v", r.Names())
}

// An explicit Reload (config save) also recovers the driver at once.
func TestReloadRecoversFailedBootSpawn(t *testing.T) {
	var present atomic.Bool
	r := serialRetryRegistry(t, &present)
	cfg := serialRetryDriver(t)
	if err := r.Add(context.Background(), cfg); err == nil {
		t.Fatal("expected boot Add to fail while the device is missing")
	}
	present.Store(true)
	r.Reload(context.Background(), []config.Driver{cfg}, false)
	if !slices.Contains(r.Names(), "p1") {
		t.Fatalf("Reload did not add p1; registered=%v", r.Names())
	}
}

func pendingSpawnRetry(r *Registry, name string) *spawnRetry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spawnRetries[name]
}

// Each failed retry doubles the wait up to the cap. A user Add starts again
// from the shortest wait. Leaving the config, Remove and shutdown end retries.
func TestSpawnRetryBacksOffAndStopsWhenDriverLeaves(t *testing.T) {
	var present atomic.Bool
	r := serialRetryRegistry(t, &present)
	cfg := serialRetryDriver(t)
	if err := r.Add(context.Background(), cfg); err == nil {
		t.Fatal("expected Add to fail while the device is missing")
	}

	want := spawnRetryMinDelay
	for range 8 {
		e := pendingSpawnRetry(r, "p1")
		if e == nil || e.delay != want {
			t.Fatalf("retry delay = %v, want %v", e, want)
		}
		r.retrySpawn("p1", e)
		want = min(2*want, spawnRetryMaxDelay)
	}
	if e := pendingSpawnRetry(r, "p1"); e == nil || e.delay != spawnRetryMaxDelay {
		t.Fatalf("retry delay after many failures = %v, want cap %v", e, spawnRetryMaxDelay)
	}

	// A stale timer that fires after a newer attempt must do nothing.
	stale := pendingSpawnRetry(r, "p1")
	if err := r.Add(context.Background(), cfg); err == nil {
		t.Fatal("expected Add to fail while the device is missing")
	}
	if e := pendingSpawnRetry(r, "p1"); e == nil || e == stale || e.delay != spawnRetryMinDelay {
		t.Fatalf("user Add did not restart backoff: %+v", e)
	}
	present.Store(true)
	r.retrySpawn("p1", stale)
	if slices.Contains(r.Names(), "p1") {
		t.Fatal("stale retry started the driver")
	}

	present.Store(false)
	r.Reload(context.Background(), nil, false)
	if e := pendingSpawnRetry(r, "p1"); e != nil {
		t.Fatal("retry kept after the driver left the config")
	}

	if err := r.Add(context.Background(), cfg); err == nil {
		t.Fatal("expected Add to fail while the device is missing")
	}
	r.Remove("p1")
	if e := pendingSpawnRetry(r, "p1"); e != nil {
		t.Fatal("retry kept after Remove")
	}

	if err := r.Add(context.Background(), cfg); err == nil {
		t.Fatal("expected Add to fail while the device is missing")
	}
	r.ShutdownAll()
	if e := pendingSpawnRetry(r, "p1"); e != nil {
		t.Fatal("retry kept after ShutdownAll")
	}
	if err := r.Add(context.Background(), cfg); err == nil {
		t.Fatal("expected Add to fail while the device is missing")
	}
	if e := pendingSpawnRetry(r, "p1"); e != nil {
		t.Fatal("retry scheduled after ShutdownAll")
	}
}

// A probe never schedules a retry.
func TestProbeFailureIsNotRetried(t *testing.T) {
	var present atomic.Bool
	r := serialRetryRegistry(t, &present)
	if err := r.AddProbe(context.Background(), serialRetryDriver(t)); err == nil {
		t.Fatal("expected probe to fail while the device is missing")
	}
	if e := pendingSpawnRetry(r, "p1"); e != nil {
		t.Fatal("probe failure scheduled a retry")
	}
}
